// Package launcher starts node visits: as Kubernetes Jobs, or as local child
// processes for development without a cluster.
package launcher

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mauza/ai-flow/internal/engine"
	"github.com/mauza/ai-flow/internal/grant"
)

// Local runs `ai-flow node` as a child process. There is no isolation: use it
// for development and tests only.
type Local struct {
	podURL  string
	dataDir string
	signer  *grant.Signer

	mu    sync.Mutex
	procs map[string]*proc
}

type proc struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func NewLocal(podURL, dataDir string, signer *grant.Signer) *Local {
	return &Local{podURL: podURL, dataDir: dataDir, signer: signer, procs: map[string]*proc{}}
}

func (l *Local) Launch(ctx context.Context, s engine.LaunchSpec) (string, error) {
	name := JobName(s)
	tok, err := l.signer.Mint(grant.Claims{Kind: "launch", Run: s.RunID, Seq: s.Seq, Node: s.Node}, s.Timeout+time.Hour)
	if err != nil {
		return "", err
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	work := filepath.Join(l.dataDir, "work", name)
	logs := filepath.Join(l.dataDir, "logs")
	for _, d := range []string{work, logs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", err
		}
	}
	logf, err := os.Create(filepath.Join(logs, name+".log"))
	if err != nil {
		return "", err
	}
	cmd := exec.Command(exe, "node")
	cmd.Env = append(os.Environ(),
		"AI_FLOW_POD_URL="+l.podURL,
		"AI_FLOW_LAUNCH_TOKEN="+tok,
		"AI_FLOW_WORKDIR="+work,
	)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		logf.Close()
		return "", err
	}
	p := &proc{cmd: cmd, done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		logf.Close()
		close(p.done)
	}()
	l.mu.Lock()
	l.procs[name] = p
	l.mu.Unlock()
	return name, nil
}

func (l *Local) Status(ctx context.Context, name string) (engine.JobStatus, error) {
	l.mu.Lock()
	p, ok := l.procs[name]
	l.mu.Unlock()
	if !ok {
		return engine.JobStatus{State: engine.JobMissing}, nil
	}
	select {
	case <-p.done:
		if p.err != nil {
			return engine.JobStatus{State: engine.JobFailed, Message: p.err.Error() + " (see " + filepath.Join(l.dataDir, "logs", name+".log") + ")"}, nil
		}
		return engine.JobStatus{State: engine.JobSucceeded}, nil
	default:
		return engine.JobStatus{State: engine.JobRunning}, nil
	}
}

func (l *Local) Kill(ctx context.Context, name string) error {
	l.mu.Lock()
	p, ok := l.procs[name]
	l.mu.Unlock()
	if ok && p.cmd.Process != nil {
		syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	}
	return nil
}

// JobName is a DNS-1123 name unique per visit: af-<run>-<seq>-<node>.
func JobName(s engine.LaunchSpec) string {
	node := strings.ReplaceAll(s.Node, "_", "-")
	name := fmt.Sprintf("af-%s-%d-%s", strings.TrimPrefix(s.RunID, "r-"), s.Seq, node)
	if len(name) > 52 { // leave room for the pod suffix
		name = strings.TrimRight(name[:52], "-")
	}
	return name
}
