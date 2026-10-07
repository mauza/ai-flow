package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mauza/ai-flow/internal/protocol"
	"github.com/mauza/ai-flow/internal/resolve"
)

// runCheck runs the node's command and maps the exit code to an outcome.
func (r *Runner) runCheck(ctx context.Context) *protocol.Result {
	c := r.b.Check
	if c == nil || c.Run == "" {
		return &protocol.Result{Error: "check node has no command"}
	}
	dir := r.repo
	if dir == "" {
		dir = filepath.Join(r.work, "flow")
	}
	r.progress("$ " + firstLine(c.Run))
	outputsFile := filepath.Join(r.work, "check-outputs.json")
	os.Remove(outputsFile)
	cmd := exec.CommandContext(ctx, "bash", "-o", "pipefail", "-c", c.Run)
	cmd.Dir = dir
	cmd.Env = append(cleanEnv(r.b.SecretEnv...), "HOME="+filepath.Join(r.work, "flow", "home"), "CI=true", "NO_COLOR=1", "AI_FLOW_OUTPUTS="+outputsFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	out := &lockedBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	start := time.Now()
	err := cmd.Run()
	ctxErr := ctx.Err()
	output := out.String()
	r.uploadTranscript([]byte(output))
	// A killed process usually returns ExitError (-1). Cancellation must win
	// over both an explicit -1 mapping and the default business outcome.
	if ctxErr != nil {
		return &protocol.Result{Error: "check interrupted: " + ctxErr.Error(), LogTail: tail(output, 8000)}
	}
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return &protocol.Result{Error: err.Error(), LogTail: tail(output, 8000)}
		}
		code = ee.ExitCode()
	}
	outcome, ok := c.ExitCodes[strconv.Itoa(code)]
	if !ok {
		outcome = c.ExitCodes["default"]
	}
	logTail := tail(output, 8000)
	what := "`" + firstLine(c.Run) + "`"
	if strings.Contains(strings.TrimSpace(c.Run), "\n") {
		what = "script"
	}
	outputs, err := checkOutputs(outputsFile, r.b.Outputs)
	if err != nil {
		return &protocol.Result{Error: err.Error(), LogTail: logTail}
	}
	outputs["exit_code"], outputs["log_tail"] = code, tail(output, 4000)
	return &protocol.Result{
		Outcome: outcome,
		Summary: fmt.Sprintf("%s exited %d after %s", what, code, time.Since(start).Round(time.Second)),
		Outputs: outputs,
		LogTail: logTail,
	}
}

const maxCheckOutputs = 256 << 10

// checkOutputs reads the JSON object a check command may write to
// $AI_FLOW_OUTPUTS. No file means no structured outputs: a failing command may
// stop before writing it. A file that is not a JSON object, sets a reserved
// field, sets an undeclared field or has a wrong type fails the step, so a
// broken command is never mistaken for a result.
func checkOutputs(path string, declared map[string]any) (map[string]any, error) {
	out := map[string]any{}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read $AI_FLOW_OUTPUTS: %w", err)
	}
	if len(data) > maxCheckOutputs {
		return nil, fmt.Errorf("$AI_FLOW_OUTPUTS is %d bytes; the limit is %d", len(data), maxCheckOutputs)
	}
	if err := json.Unmarshal(data, &out); err != nil || out == nil {
		return nil, fmt.Errorf("$AI_FLOW_OUTPUTS must hold one JSON object: %v", err)
	}
	for k, v := range out {
		if k == "exit_code" || k == "log_tail" {
			return nil, fmt.Errorf("$AI_FLOW_OUTPUTS: %q is set by the runner", k)
		}
		typ, ok := declared[k]
		if !ok {
			return nil, fmt.Errorf("$AI_FLOW_OUTPUTS: %q is not declared in the node's outputs", k)
		}
		schema, err := resolve.OutputSchema(typ)
		if err != nil {
			return nil, fmt.Errorf("outputs.%s: %w", k, err)
		}
		if err := resolve.CheckValue(schema, v); err != nil {
			return nil, fmt.Errorf("$AI_FLOW_OUTPUTS: %s: %w", k, err)
		}
	}
	return out, nil
}

type lockedBuffer struct {
	mu      sync.Mutex
	b       bytes.Buffer
	dropped int64
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := min(len(p), maxCapturedTranscript-256-l.b.Len())
	l.b.Write(p[:n])
	l.dropped += int64(len(p) - n)
	return len(p), nil
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.dropped > 0 {
		return l.b.String() + fmt.Sprintf("\n[transcript truncated: limit_bytes=%d dropped_bytes=%d]\n", maxCapturedTranscript, l.dropped)
	}
	return l.b.String()
}
