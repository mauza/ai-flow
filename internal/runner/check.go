package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mauza/ai-flow/internal/protocol"
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
	cmd := exec.CommandContext(ctx, "bash", "-o", "pipefail", "-c", c.Run)
	cmd.Dir = dir
	cmd.Env = append(cleanEnv(r.b.SecretEnv...), "HOME="+filepath.Join(r.work, "flow", "home"), "CI=true", "NO_COLOR=1")
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
	return &protocol.Result{
		Outcome: outcome,
		Summary: fmt.Sprintf("%s exited %d after %s", what, code, time.Since(start).Round(time.Second)),
		Outputs: map[string]any{"exit_code": code, "log_tail": tail(output, 4000)},
		LogTail: logTail,
	}
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
