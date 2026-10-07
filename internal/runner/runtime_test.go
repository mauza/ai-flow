package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/protocol"
)

type runtimeTransport func(*http.Request) (*http.Response, error)

func (f runtimeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func runtimeResponse() *http.Response {
	return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}
}

func runtimeRunner(t *testing.T) *Runner {
	t.Helper()
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, "flow"), 0700); err != nil {
		t.Fatal(err)
	}
	return &Runner{work: work, podURL: "http://broker.invalid", b: &protocol.Bundle{}, http: &http.Client{Transport: runtimeTransport(func(*http.Request) (*http.Response, error) { return runtimeResponse(), nil })}}
}

func TestCheckCancellationPrecedesExitMapping(t *testing.T) {
	for _, mode := range []string{"deadline", "cancel", "already-canceled"} {
		t.Run(mode, func(t *testing.T) {
			r := runtimeRunner(t)
			r.b.Check = &protocol.CheckSpec{Run: "printf started; sleep 30", ExitCodes: map[string]string{"0": "pass", "-1": "explicit-killed", "default": "business-failure"}}
			if mode == "deadline" {
				delete(r.b.Check.ExitCodes, "-1")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var timer *time.Timer
			switch mode {
			case "deadline":
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 100*time.Millisecond)
				defer stop()
			case "cancel":
				timer = time.AfterFunc(100*time.Millisecond, cancel)
				defer timer.Stop()
			default:
				cancel()
			}
			start := time.Now()
			res := r.runCheck(ctx)
			if res.Error == "" || res.Outcome != "" || !strings.Contains(res.Error, ctx.Err().Error()) {
				t.Fatalf("cancellation became an outcome: %+v", res)
			}
			if time.Since(start) > 3*time.Second {
				t.Fatal("sleeping process wasn't promptly killed")
			}
			if mode != "already-canceled" && !strings.Contains(res.LogTail, "started") {
				t.Fatalf("lost output on cancellation: %+v", res)
			}
		})
	}
	for _, script := range []string{"exit 0", "exit 7"} {
		r := runtimeRunner(t)
		r.b.Check = &protocol.CheckSpec{Run: script, ExitCodes: map[string]string{"0": "pass", "default": "fail"}}
		res := r.runCheck(context.Background())
		want := "pass"
		if script == "exit 7" {
			want = "fail"
		}
		if res.Error != "" || res.Outcome != want {
			t.Fatalf("normal exit mapping changed: %+v", res)
		}
	}
}

func TestApprovedEnvReachesCheckAndAgentOnly(t *testing.T) {
	t.Setenv("APPROVED_RUNTIME_TEST", "test-value")
	t.Setenv("UNAPPROVED_RUNTIME_TEST", "must-not-inherit")
	t.Setenv(EnvLaunchToken, "test-launch-token")
	t.Setenv("OPENAI_API_KEY", "test-ungranted-key")
	probe := `test "$APPROVED_RUNTIME_TEST" = test-value && test -z "$UNAPPROVED_RUNTIME_TEST$AI_FLOW_LAUNCH_TOKEN$OPENAI_API_KEY"`
	r := runtimeRunner(t)
	r.b.SecretEnv = []string{"APPROVED_RUNTIME_TEST"}
	r.b.Check = &protocol.CheckSpec{Run: probe, ExitCodes: map[string]string{"0": "pass", "default": "fail"}}
	if res := r.runCheck(context.Background()); res.Error != "" || res.Outcome != "pass" {
		t.Fatalf("check env: %+v", res)
	}

	pi := filepath.Join(t.TempDir(), "fake-pi")
	script := "#!/bin/sh\n" + probe + ` || exit 19
printf '%s' '{"outcome":"done","summary":"env checked","outputs":{},"transcript_key":"forged-by-agent"}' > "$AI_FLOW_RESULT"
printf '%s\n' '{"type":"test_event"}'
`
	if err := os.WriteFile(pi, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AI_FLOW_PI", pi)
	r.b.LLM = &protocol.LLMAccess{Models: []protocol.ModelInfo{{Name: "test"}}, Config: &flow.LLMConfig{}}
	r.b.Outcomes = []string{"done"}
	r.b.Type = flow.TypeAgent
	if res := r.run(context.Background()); res.Error != "" || res.Outcome != "done" {
		t.Fatalf("agent env: %+v", res)
	} else if want := protocol.FinalTranscriptKey(r.b.RunID, r.b.Seq, r.b.Node, []byte("{\"type\":\"test_event\"}\n")); res.TranscriptKey != want {
		t.Fatalf("agent selected the artifact: got %q want %q", res.TranscriptKey, want)
	}
	for _, kv := range cleanEnv() {
		if strings.HasPrefix(kv, "APPROVED_RUNTIME_TEST=") {
			t.Fatal("secret leaked without approval")
		}
	}
}

func TestRunnerResultSelectsOnlyAcknowledgedFinalUpload(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusInternalServerError} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			r := runtimeRunner(t)
			r.b.RunID, r.b.Seq, r.b.Node, r.b.Type = "runtime", 2, "check", flow.TypeCheck
			r.b.Check = &protocol.CheckSpec{Run: "printf own-attempt", ExitCodes: map[string]string{"0": "pass"}}
			r.finalTranscriptKey = "earlier-runner-execution"
			var data []byte
			var delivered []string
			r.http.Transport = runtimeTransport(func(req *http.Request) (*http.Response, error) {
				switch req.URL.Path {
				case "/v1/transcript":
					if req.Header.Get(protocol.TranscriptFinalHeader) != "true" {
						t.Error("final marker missing")
					}
					var err error
					data, err = io.ReadAll(req.Body)
					resp := runtimeResponse()
					resp.StatusCode = status
					return resp, err
				case "/v1/result":
					var result protocol.Result
					if err := json.NewDecoder(req.Body).Decode(&result); err != nil {
						return nil, err
					}
					delivered = append(delivered, result.TranscriptKey)
				}
				return runtimeResponse(), nil
			})
			res := r.run(context.Background())
			if res.Error != "" || res.Outcome != "pass" {
				t.Fatalf("check failed: %+v", res)
			}
			want := ""
			if status == http.StatusNoContent {
				want = protocol.FinalTranscriptKey(r.b.RunID, r.b.Seq, r.b.Node, data)
			}
			if res.TranscriptKey != want {
				t.Fatalf("key=%q want=%q", res.TranscriptKey, want)
			}
			for range 2 {
				if err := r.post(context.Background(), "/v1/result", res, nil); err != nil {
					t.Fatal(err)
				}
			}
			if len(delivered) != 2 || delivered[0] != want || delivered[1] != want {
				t.Fatalf("result retries changed selection: %v", delivered)
			}
		})
	}
}

func TestTranscriptCaptureBoundAndMetadata(t *testing.T) {
	capture := &transcriptCapture{}
	line := []byte(`{"type":"test","text":"` + strings.Repeat("x", 100000) + `"}`)
	for range 100 {
		capture.record(line)
	}
	data, version := capture.snapshot(0)
	if len(data) > maxCapturedTranscript || cap(capture.data) > maxCapturedTranscript {
		t.Fatalf("capture exceeded bound: data=%d capacity=%d", len(data), cap(capture.data))
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte{'\n'})
	for _, line := range lines {
		if !json.Valid(line) {
			t.Fatal("truncation split a JSON event")
		}
	}
	var marker struct {
		Type          string `json:"type"`
		DroppedEvents int64  `json:"dropped_events"`
		DroppedBytes  int64  `json:"dropped_bytes"`
	}
	if err := json.Unmarshal(lines[len(lines)-1], &marker); err != nil {
		t.Fatal(err)
	}
	if marker.Type != "transcript_truncated" || marker.DroppedEvents != int64(100-(len(lines)-1)) || marker.DroppedBytes != marker.DroppedEvents*int64(len(line)+1) {
		t.Fatalf("inaccurate loss metadata: %+v", marker)
	}
	if data, _ := capture.snapshot(version); data != nil {
		t.Fatal("unchanged snapshot recopied")
	}
	data[0] = 'x'
	if capture.data[0] != '{' {
		t.Fatal("snapshot aliases live collector")
	}
}

func TestPiEventWriterDrainsOversizedLines(t *testing.T) {
	r := runtimeRunner(t)
	capture := &transcriptCapture{}
	w := &piEventWriter{r: r, transcript: capture}
	w.Write([]byte("{\"type\":"))
	w.Write([]byte("\"first\"}\n"))
	chunk := bytes.Repeat([]byte{'x'}, 32<<10)
	for range 200 {
		w.Write(chunk)
	}
	w.Write([]byte("\n{\"type\":\"after\"}\n"))
	if cap(w.line) > maxPiEventBytes || w.size != 0 {
		t.Fatalf("unbounded line: cap=%d size=%d", cap(w.line), w.size)
	}
	data, _ := capture.snapshot(0)
	if !bytes.HasPrefix(data, []byte("{\"type\":\"first\"}\n")) || !bytes.Contains(data, []byte(`"dropped_events":2`)) {
		t.Fatalf("missing prefix/loss marker: %s", data)
	}
}

func TestTranscriptUploadsCoalesceAndJoin(t *testing.T) {
	for _, completion := range []string{"success", "timeout", "both-timeout"} {
		t.Run(completion, func(t *testing.T) {
			r := runtimeRunner(t)
			r.http.Timeout = 200 * time.Millisecond
			capture := &transcriptCapture{}
			capture.record([]byte(`{"type":"initial"}`))
			started := make(chan context.Context, 1)
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			var active, peak, calls atomic.Int32
			var final []byte
			r.http.Transport = runtimeTransport(func(req *http.Request) (*http.Response, error) {
				n := active.Add(1)
				defer active.Add(-1)
				for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
				}
				if calls.Add(1) == 1 {
					if req.Header.Get(protocol.TranscriptFinalHeader) != "" {
						t.Error("periodic upload marked final")
					}
					started <- req.Context()
					select {
					case <-release:
						return runtimeResponse(), nil
					case <-req.Context().Done():
						return nil, req.Context().Err()
					}
				}
				if req.Header.Get(protocol.TranscriptFinalHeader) != "true" {
					t.Error("final upload not marked final")
				}
				if completion == "both-timeout" {
					<-req.Context().Done()
					return nil, req.Context().Err()
				}
				var err error
				final, err = io.ReadAll(req.Body)
				return runtimeResponse(), err
			})
			stop := r.startTranscriptUploads(capture, time.Millisecond)
			var stopOnce sync.Once
			finish := func() { stopOnce.Do(stop) }
			defer func() { unblock(); finish() }()
			var requestCtx context.Context
			select {
			case requestCtx = <-started:
			case <-time.After(time.Second):
				t.Fatal("upload did not start")
			}
			for range 1000 {
				capture.record([]byte(`{"type":"later"}`))
			}
			done := make(chan struct{})
			go func() { defer close(done); finish() }()
			// Shutdown must wait, rather than cancel this request or queue another.
			select {
			case <-requestCtx.Done():
				t.Fatal("shutdown canceled in-flight upload")
			case <-done:
				t.Fatal("shutdown did not wait for in-flight upload")
			case <-time.After(30 * time.Millisecond):
			}
			if calls.Load() != 1 {
				t.Fatalf("queued concurrent uploads: %d", calls.Load())
			}
			if completion == "success" {
				unblock()
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("upload shutdown exceeded request deadlines")
			}
			if active.Load() != 0 || peak.Load() != 1 || calls.Load() != 2 {
				t.Fatalf("uploader didn't join: active=%d peak=%d calls=%d", active.Load(), peak.Load(), calls.Load())
			}
			if completion != "both-timeout" && bytes.Count(final, []byte(`"later"`)) != 1000 {
				t.Fatal("final upload is stale")
			}
		})
	}
}

func TestCapturedCheckOutputHasHardBound(t *testing.T) {
	var out lockedBuffer
	p := bytes.Repeat([]byte{'x'}, 2*maxCapturedTranscript)
	if n, err := out.Write(p); n != len(p) || err != nil {
		t.Fatalf("output writer disrupted subprocess: %d %v", n, err)
	}
	out.Write([]byte("more"))
	s := out.String()
	if len(s) > maxCapturedTranscript || !strings.Contains(s, "transcript truncated:") || !strings.Contains(s, "dropped_bytes=") {
		t.Fatalf("missing bound/metadata: size=%d", len(s))
	}
}

func TestPiWaitBoundsInheritedOutputPipe(t *testing.T) {
	r := runtimeRunner(t)
	t.Setenv("AI_FLOW_PI", "/bin/bash")
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	t.Cleanup(func() {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(string(data))
		if err == nil {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	shim := NewShim(&protocol.LLMAccess{Models: []protocol.ModelInfo{{Name: "test"}}, Config: &flow.LLMConfig{}}, "test")
	defer shim.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	capture := &transcriptCapture{}
	start := time.Now()
	err := r.runPi(ctx, []string{"-c", `sleep 30 & printf '%s' "$!" > "$PID_FILE"; printf '%s\n' '{"type":"before-exit"}'`}, append(cleanEnv(), "PID_FILE="+pidFile), capture, shim)
	if err == nil || time.Since(start) > 8*time.Second {
		t.Fatalf("inherited pipe was not bounded by WaitDelay: %v after %s", err, time.Since(start))
	}
	data, _ := capture.snapshot(0)
	if !bytes.Contains(data, []byte("before-exit")) {
		t.Fatal("lost final stdout")
	}
}
