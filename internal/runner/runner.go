// Package runner is the node-runner: the entrypoint of every node pod. It
// exchanges the pod's identity for a bundle, prepares the workspace, runs the
// node (pi agent, single LLM call, or check), pushes commits and reports the
// result.
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/protocol"
)

// Env vars the runner reads.
const (
	EnvPodURL      = "AI_FLOW_POD_URL"      // control plane pod-facing base URL
	EnvLaunchToken = "AI_FLOW_LAUNCH_TOKEN" // local mode: pre-minted launch token
	EnvSATokenPath = "AI_FLOW_SA_TOKEN"     // k8s mode: projected service account token path
	EnvWorkDir     = "AI_FLOW_WORKDIR"
)

type Runner struct {
	podURL string
	b      *protocol.Bundle
	http   *http.Client
	work   string // scratch root: repo/, flow/
	repo   string // repo checkout, "" when the node has no repo

	progressMu   sync.Mutex
	lastProgress time.Time

	txMu       sync.Mutex // guards txVersion
	txVersion  int64
	txUploadMu sync.Mutex // serializes uploads; guards txUploaded
	txUploaded int64
}

// Main runs one node and exits. It returns an error only when the result
// could not be delivered (so the Job retries).
func Main(ctx context.Context) error {
	podURL := strings.TrimSuffix(os.Getenv(EnvPodURL), "/")
	if podURL == "" {
		return fmt.Errorf("%s is not set", EnvPodURL)
	}
	work := os.Getenv(EnvWorkDir)
	if work == "" {
		work = "/work"
	}
	work, err := filepath.Abs(work)
	if err != nil {
		return err
	}
	r := &Runner{podURL: podURL, work: work, http: &http.Client{Timeout: 2 * time.Minute}}
	b, err := r.exchange(ctx)
	if err != nil {
		return fmt.Errorf("exchange: %w", err)
	}
	r.b = b
	slog.Info("node start", "run", b.RunID, "node", b.Node, "visit", b.Visit, "type", b.Type)

	timeout := time.Duration(b.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	// leave room to push and report before the Job's own deadline
	nodeCtx, cancel := context.WithTimeout(ctx, timeout-timeout/20)
	defer cancel()

	res := r.run(nodeCtx)
	if res.Error == "" && !contains(b.Outcomes, res.Outcome) {
		res.Error = fmt.Sprintf("node produced outcome %q, not one of %v", res.Outcome, b.Outcomes)
	}
	slog.Info("node done", "outcome", res.Outcome, "error", res.Error)
	for attempt := 0; ; attempt++ {
		err := r.post(ctx, "/v1/result", res, nil)
		if err == nil {
			return nil
		}
		if attempt == 5 {
			return fmt.Errorf("report result: %w", err)
		}
		slog.Warn("report result failed; retrying", "err", err)
		time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
	}
}

func (r *Runner) run(ctx context.Context) (res *protocol.Result) {
	defer func() {
		if p := recover(); p != nil {
			res = &protocol.Result{Error: fmt.Sprintf("node-runner panic: %v", p)}
		}
	}()
	b := r.b
	if err := os.MkdirAll(filepath.Join(r.work, "flow"), 0o755); err != nil {
		return &protocol.Result{Error: err.Error()}
	}
	if b.Repo != nil {
		r.progress("Cloning " + b.Repo.Branch)
		repo, err := r.prepareRepo(ctx)
		if err != nil {
			return &protocol.Result{Error: "workspace: " + err.Error()}
		}
		r.repo = repo
	}

	var out *protocol.Result
	switch b.Type {
	case flow.TypeAgent:
		out = r.runAgent(ctx)
	case flow.TypeLLM:
		out = r.runLLM(ctx)
	case flow.TypeCheck:
		out = r.runCheck(ctx)
	default:
		return &protocol.Result{Error: fmt.Sprintf("node type %q does not run in a pod", b.Type)}
	}
	if out.Outputs == nil {
		out.Outputs = map[string]any{}
	}
	if r.repo != "" && out.Error == "" {
		if b.Repo.Write {
			r.progress("Pushing changes")
			sha, err := r.commitAndPush(ctx, out)
			if err != nil {
				out.Error = "push: " + err.Error()
				return out
			}
			out.Commit = sha
		}
		if d, err := r.diffStat(ctx); err == nil {
			out.Diff = d
		}
	}
	return out
}

// ---- control plane client ----

func (r *Runner) exchange(ctx context.Context) (*protocol.Bundle, error) {
	header := http.Header{}
	if tok := os.Getenv(EnvLaunchToken); tok != "" {
		header.Set("Authorization", "Bearer "+tok)
	} else {
		path := os.Getenv(EnvSATokenPath)
		if path == "" {
			path = "/var/run/secrets/ai-flow/token"
		}
		tok, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read pod token: %w", err)
		}
		header.Set("X-Pod-Token", strings.TrimSpace(string(tok)))
	}
	var b protocol.Bundle
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "POST", r.podURL+"/v1/exchange", nil)
		if err != nil {
			return nil, err
		}
		req.Header = header.Clone()
		lastErr = r.do(req, &b)
		if lastErr == nil {
			return &b, nil
		}
		var he *httpError
		if errors.As(lastErr, &he) && he.status < 500 {
			return nil, lastErr
		}
		time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
	}
	return nil, lastErr
}

type httpError struct {
	status int
	body   string
}

func (e *httpError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.status, strings.TrimSpace(e.body)) }

func (r *Runner) do(req *http.Request, out any) error {
	resp, err := r.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return &httpError{resp.StatusCode, string(body)}
	}
	if out != nil {
		return json.Unmarshal(body, out)
	}
	return nil
}

func (r *Runner) post(ctx context.Context, path string, in, out any) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", r.podURL+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.b.Grant)
	return r.do(req, out)
}

func (r *Runner) postRaw(ctx context.Context, path, contentType string, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, "POST", r.podURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+r.b.Grant)
	return r.do(req, nil)
}

// progress reports a one-line status to the UI, at most every 1.5s.
func (r *Runner) progress(text string) {
	r.progressMu.Lock()
	if time.Since(r.lastProgress) < 1500*time.Millisecond {
		r.progressMu.Unlock()
		return
	}
	r.lastProgress = time.Now()
	r.progressMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.post(ctx, "/v1/progress", protocol.Progress{Text: truncate(text, 300)}, nil); err != nil {
		slog.Debug("progress", "err", err)
	}
}

// uploadTranscript stores the transcript so far. Uploads are versioned so a
// slow periodic snapshot never overwrites a newer (e.g. the final) one.
func (r *Runner) uploadTranscript(data []byte) {
	if len(data) == 0 {
		return
	}
	r.txMu.Lock()
	r.txVersion++
	version := r.txVersion
	r.txMu.Unlock()

	r.txUploadMu.Lock()
	defer r.txUploadMu.Unlock()
	if version < r.txUploaded {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := r.postRaw(ctx, "/v1/transcript", "application/x-ndjson", data); err != nil {
		slog.Warn("upload transcript", "err", err)
		return
	}
	r.txUploaded = version
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
