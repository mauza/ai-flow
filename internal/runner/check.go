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
	cmd.Env = append(cleanEnv(), "HOME="+filepath.Join(r.work, "flow", "home"), "CI=true", "NO_COLOR=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	out := &lockedBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	start := time.Now()
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			if ctx.Err() != nil {
				return &protocol.Result{Error: "check timed out", LogTail: tail(out.String(), 8000)}
			}
			return &protocol.Result{Error: err.Error(), LogTail: tail(out.String(), 8000)}
		}
		code = ee.ExitCode()
	}
	outcome, ok := c.ExitCodes[strconv.Itoa(code)]
	if !ok {
		outcome = c.ExitCodes["default"]
	}
	logTail := tail(out.String(), 8000)
	r.uploadTranscript([]byte(out.String()))
	what := "`" + firstLine(c.Run) + "`"
	if strings.Contains(strings.TrimSpace(c.Run), "\n") {
		what = "script"
	}
	return &protocol.Result{
		Outcome: outcome,
		Summary: fmt.Sprintf("%s exited %d after %s", what, code, time.Since(start).Round(time.Second)),
		Outputs: map[string]any{"exit_code": code, "log_tail": tail(out.String(), 4000)},
		LogTail: logTail,
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.b.Len() > 4<<20 { // cap captured output at 4 MiB
		return len(p), nil
	}
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
