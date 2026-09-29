// Package engine runs flows: a small state-machine interpreter. A run has one
// active node at a time. Pod nodes (llm, agent, check) become Jobs through a
// Launcher; gates wait for a human; switches and actions run in-process.
//
// All transitions happen on the engine's single loop goroutine. Other parts
// (the broker, the API) only record facts — a result arrived, a gate was
// decided — and wake the loop.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/github"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/ids"
	"github.com/mauza/ai-flow/internal/resolve"
	"github.com/mauza/ai-flow/internal/store"
)

// Launcher starts and watches node pods (or local processes). Launch must use
// JobName(spec) as its identity; a repeated Kubernetes launch must reconcile
// the existing Job rather than create another one.
type Launcher interface {
	Launch(ctx context.Context, spec LaunchSpec) (string, error)
	Status(ctx context.Context, name string) (JobStatus, error)
	Kill(ctx context.Context, name string) error
}

type LaunchSpec struct {
	RunID    string
	Seq      int
	Node     string
	Visit    int
	Type     string
	Image    string
	Timeout  time.Duration
	Retries  int
	Secrets  []SecretMount
	Internet bool
}

type SecretMount struct {
	SecretName, Key string
	Env, File       string
}

type JobState string

const (
	JobRunning   JobState = "running"
	JobSucceeded JobState = "succeeded"
	JobFailed    JobState = "failed"
	JobMissing   JobState = "missing"
)

type JobStatus struct {
	State   JobState
	Message string
}

// Hooks lets intake sources (Linear) mirror run progress.
type Hooks interface {
	RunStarted(ctx context.Context, task *store.Task, run *store.Run)
	RunFinished(ctx context.Context, task *store.Task, run *store.Run)
}

type Engine struct {
	cfg      *config.Config
	store    *store.Store
	launcher Launcher
	hub      *hub.Hub
	gh       *github.Client
	hooks    Hooks

	wake    chan struct{}
	mu      sync.Mutex // serializes transitions (loop + synchronous API calls)
	orphans map[string]time.Time

	// OrphanGrace is how long a finished job may go without a posted result
	// before its visit fails (results are posted just before the pod exits).
	OrphanGrace time.Duration
}

func New(cfg *config.Config, st *store.Store, l Launcher, h *hub.Hub, gh *github.Client) *Engine {
	return &Engine{cfg: cfg, store: st, launcher: l, hub: h, gh: gh, wake: make(chan struct{}, 1), orphans: map[string]time.Time{}, OrphanGrace: 15 * time.Second}
}

// Tick runs one pass over active and queued runs (tests drive the engine with it).
func (e *Engine) Tick(ctx context.Context) { e.tick(ctx) }

func (e *Engine) SetHooks(h Hooks) { e.hooks = h }

// Wake asks the loop to look at runs now.
func (e *Engine) Wake() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Loop drives all runs until ctx ends.
func (e *Engine) Loop(ctx context.Context) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		e.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-e.wake:
		}
	}
}

func (e *Engine) tick(ctx context.Context) {
	e.mu.Lock()
	defer e.mu.Unlock()
	active, err := e.store.ListRuns(ctx, store.RunFilter{Statuses: []string{store.RunRunning, store.RunWaiting}, Limit: -1})
	if err != nil {
		slog.Error("list runs", "err", err)
		return
	}
	occupied := 0
	for _, r := range active {
		if err := e.step(ctx, r); err != nil {
			e.handleError(ctx, r, err)
		}
		if r.Status == store.RunRunning || r.Status == store.RunWaiting {
			occupied++
		}
	}
	queued, err := e.store.ListRuns(ctx, store.RunFilter{Statuses: []string{store.RunQueued}, Limit: -1})
	if err != nil {
		slog.Error("list queued runs", "err", err)
		return
	}
	slots := e.cfg.Env.Runs.MaxConcurrent - occupied
	for i := len(queued) - 1; i >= 0 && slots > 0; i-- { // oldest first
		r := queued[i]
		if err := e.begin(ctx, r); err != nil {
			e.handleError(ctx, r, err)
		}
		// Validation failures and runs that finish synchronously consume no
		// slot. A transient failure after entering running still occupies one.
		if r.Status == store.RunRunning || r.Status == store.RunWaiting {
			slots--
		}
	}
}

// Mark deterministic flow/configuration failures at their source. Unmarked
// store/transport errors remain retryable; they must not discard durable work.
type rejection struct{ error }

func rejectf(format string, args ...any) error { return rejection{fmt.Errorf(format, args...)} }

func permanent(err error) bool {
	var rejected rejection
	if errors.As(err, &rejected) {
		return true
	}
	var api *github.APIError
	if errors.As(err, &api) {
		if api.Status == 403 && strings.Contains(strings.ToLower(api.Body), "rate limit") {
			return false
		}
		return api.Status >= 400 && api.Status < 500 && api.Status != 408 && api.Status != 429
	}
	return false
}

func (e *Engine) handleError(ctx context.Context, r *store.Run, err error) {
	slog.Error("reconcile run", "run", r.ID, "err", err)
	if permanent(err) {
		if finishErr := e.finish(ctx, r, store.RunFailed, err.Error()); finishErr != nil {
			slog.Error("persist run failure", "run", r.ID, "err", finishErr)
		}
	}
}

// ---- starting runs ----

// CreateRun queues a run of a saved flow version.
func (e *Engine) CreateRun(ctx context.Context, flowName string, version int, taskID string) (*store.Run, error) {
	fv, err := e.store.GetFlow(ctx, flowName, version)
	if err != nil {
		return nil, fmt.Errorf("flow %s: %w", flowName, err)
	}
	res, err := e.resolve(fv)
	if err != nil {
		return nil, err
	}
	if issues := resolve.Validate(res, e.cfg); resolve.HasErrors(issues) {
		var msgs []string
		for _, i := range issues {
			if i.Severity == resolve.Error {
				msgs = append(msgs, i.String())
			}
		}
		return nil, fmt.Errorf("flow has errors: %s", strings.Join(msgs, "; "))
	}
	if taskID == "" {
		taskID = fv.TaskID
	}
	id := ids.New("r")
	r := &store.Run{
		ID: id, FlowName: fv.Name, FlowVersion: fv.Version, TaskID: taskID, Project: res.Flow.Metadata.Project,
		Status: store.RunQueued, Branch: "ai-flow/" + ids.Slug(fv.Name, 40) + "-" + strings.TrimPrefix(id, "r-")[:6], Base: res.Base,
	}
	r.Snapshot, err = e.snapshot(res)
	if err != nil {
		return nil, err
	}
	if err := e.store.CreateRun(ctx, r); err != nil {
		return nil, err
	}
	e.event(ctx, r, "", "queued", fmt.Sprintf("Run queued for %s v%d", fv.Name, fv.Version))
	e.publishRun(r)
	e.Wake()
	return r, nil
}

func (e *Engine) resolve(fv *store.FlowVersion) (*resolve.Resolved, error) {
	f, err := flow.Parse([]byte(fv.YAML))
	if err != nil {
		return nil, rejection{err}
	}
	return resolve.Resolve(f, e.cfg), nil
}

func (e *Engine) begin(ctx context.Context, r *store.Run) error {
	res, err := e.Resolved(ctx, r)
	if err != nil {
		return err
	}
	stamp := store.Now()
	if err := e.store.SetRunState(ctx, r, store.RunRunning, "", stamp); err != nil {
		return err
	}
	r.Status, r.StartedAt = store.RunRunning, stamp
	e.event(ctx, r, "", "started", "Run started")
	if t := e.task(ctx, r); t != nil {
		e.hub.Publish(hub.Event{Type: "task", ID: t.ID})
		if e.hooks != nil {
			started := *r
			go e.hooks.RunStarted(context.Background(), t, &started)
		}
	}
	return e.enter(ctx, r, res, res.Flow.Spec.Start, 0)
}

// ---- the state machine ----

func (e *Engine) step(ctx context.Context, r *store.Run) error {
	v, err := e.store.LastVisit(ctx, r.ID)
	if errors.Is(err, store.ErrNotFound) {
		res, err := e.Resolved(ctx, r)
		if err != nil {
			return err
		}
		return e.enter(ctx, r, res, res.Flow.Spec.Start, 0)
	}
	if err != nil {
		return err
	}
	switch v.Status {
	case store.VisitSucceeded, store.VisitError:
		return e.advance(ctx, r, v)
	case store.VisitCanceled:
		// Older versions committed visit cancellation separately from the run.
		return e.finish(ctx, r, store.RunCanceled, "canceled")
	case store.VisitWaiting:
		// Repair pre-migration crashes between gate entry and the run update.
		if r.Status != store.RunWaiting {
			if err := e.store.UpdateRun(ctx, r.ID, map[string]any{"status": store.RunWaiting, "current_node": v.Node}); err != nil {
				return err
			}
			r.Status = store.RunWaiting
			e.publishRun(r)
		}
		if v.Deadline > 0 && store.Now() > v.Deadline {
			return e.decide(ctx, r, v, flow.OutcomeTimeout, "timeout")
		}
		return nil
	case store.VisitPending, store.VisitRunning:
		if !flow.PodType(v.Type) {
			res, err := e.Resolved(ctx, r)
			if err != nil {
				return err
			}
			n := res.Nodes[v.Node]
			if n == nil {
				return rejectf("node %q not in snapshot", v.Node)
			}
			return e.execute(ctx, r, res, n, v)
		}
		if v.JobName == "" || v.LaunchState == "pending" {
			res, err := e.Resolved(ctx, r)
			if err != nil {
				return err
			}
			n := res.Nodes[v.Node]
			if n == nil {
				return rejectf("node %q not in snapshot", v.Node)
			}
			return e.launch(ctx, r, res, n, v)
		}
		if v.Deadline > 0 && store.Now() > v.Deadline+60_000 {
			return e.visitError(ctx, r, v, "node exceeded its timeout")
		}
		st, err := e.launcher.Status(ctx, v.JobName)
		if err != nil {
			slog.Warn("job status", "job", v.JobName, "err", err)
			return nil
		}
		if st.State == JobRunning {
			delete(e.orphans, v.JobName)
			return nil
		}
		// The job ended (or vanished) but no result is recorded. Results are
		// posted just before the pod exits, so allow a short grace period.
		since, seen := e.orphans[v.JobName]
		if !seen {
			e.orphans[v.JobName] = time.Now()
			return nil
		}
		grace := e.OrphanGrace
		if st.State == JobMissing {
			grace *= 8
		}
		if time.Since(since) < grace {
			return nil
		}
		delete(e.orphans, v.JobName)
		msg := "node exited without reporting a result"
		switch st.State {
		case JobFailed:
			msg = "node pod failed"
		case JobMissing:
			msg = "node job disappeared"
		}
		if st.Message != "" {
			msg += ": " + st.Message
		}
		return e.visitError(ctx, r, v, msg)
	}
	return nil
}

func (e *Engine) visitError(ctx context.Context, r *store.Run, v *store.Visit, msg string) error {
	updated, err := e.store.UpdateVisitIf(ctx, r.ID, v.Seq, []string{store.VisitPending, store.VisitRunning}, map[string]any{"status": store.VisitError, "error": msg, "finished_at": store.Now()})
	if err != nil {
		return err
	}
	if !updated {
		return nil
	} // a broker result won the race; reconcile next tick
	v.Status, v.Error = store.VisitError, msg
	return e.advance(ctx, r, v)
}

// advance follows the finished visit's outcome.
func (e *Engine) advance(ctx context.Context, r *store.Run, v *store.Visit) error {
	e.publishVisit(r.ID, v.Seq)
	if v.Status == store.VisitError {
		e.event(ctx, r, v.Node, "error", fmt.Sprintf("%s#%d failed: %s", v.Node, v.Visit, v.Error))
		return e.finish(ctx, r, store.RunFailed, fmt.Sprintf("%s: %s", v.Node, v.Error))
	}
	res, err := e.Resolved(ctx, r)
	if err != nil {
		return err
	}
	n := res.Nodes[v.Node]
	if n == nil {
		return rejectf("node %q not in flow", v.Node)
	}
	target, ok := n.Next[v.Outcome]
	if !ok {
		return e.finish(ctx, r, store.RunFailed, fmt.Sprintf("%s: outcome %q has no transition", v.Node, v.Outcome))
	}
	e.event(ctx, r, v.Node, "outcome", fmt.Sprintf("%s#%d → %s", v.Node, v.Visit, v.Outcome))
	return e.enter(ctx, r, res, target, 0)
}

func (e *Engine) enter(ctx context.Context, r *store.Run, res *resolve.Resolved, target string, depth int) error {
	return e.enterWhy(ctx, r, res, target, depth, "")
}

// enterWhy moves the run to target; why explains an on_exhausted jump.
func (e *Engine) enterWhy(ctx context.Context, r *store.Run, res *resolve.Resolved, target string, depth int, why string) error {
	if depth > 20 {
		return rejectf("too many chained transitions (on_exhausted cycle?)")
	}
	switch target {
	case flow.Success:
		return e.finish(ctx, r, store.RunSucceeded, "")
	case flow.Fail:
		last, err := e.store.LastVisit(ctx, r.ID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		msg := "flow ended in $fail"
		if last != nil {
			msg = fmt.Sprintf("%s → %s", last.Node, last.Outcome)
			if last.Summary != "" {
				msg += ": " + last.Summary
			}
		}
		if why != "" {
			msg = why + " (last step: " + msg + ")"
		}
		return e.finish(ctx, r, store.RunFailed, msg)
	}
	n := res.Nodes[target]
	if n == nil {
		return rejectf("unknown node %q", target)
	}
	if n.MaxVisits > 0 {
		count, err := e.visitCount(ctx, r.ID, target)
		if err != nil {
			return err
		}
		if count >= n.MaxVisits {
			why := fmt.Sprintf("%s already ran %d times (max_visits)", target, n.MaxVisits)
			e.event(ctx, r, target, "exhausted", why+" → "+n.OnExhausted)
			return e.enterWhy(ctx, r, res, n.OnExhausted, depth+1, why)
		}
	}
	v, err := e.store.AddVisit(ctx, r.ID, target, n.Type)
	if err != nil {
		return err
	}
	r.CurrentNode, r.Status = target, store.RunRunning
	e.publishRun(r)
	return e.execute(ctx, r, res, n, v)
}

// execute resumes the same durable visit; it never appends a second visit.
func (e *Engine) execute(ctx context.Context, r *store.Run, res *resolve.Resolved, n *resolve.Node, v *store.Visit) error {
	switch n.Type {
	case flow.TypeLLM, flow.TypeAgent, flow.TypeCheck:
		return e.launch(ctx, r, res, n, v)
	case flow.TypeGate:
		fields := map[string]any{"status": store.VisitWaiting, "started_at": store.Now()}
		if n.Timeout.Duration > 0 {
			fields["deadline"] = time.Now().Add(n.Timeout.Duration).UnixMilli()
		}
		tc, err := e.checkedTemplateContext(ctx, r, res, n)
		if err != nil {
			return err
		}
		prompt, _ := RenderText(n.Prompt, tc)
		fields["prompt"] = prompt
		if err := e.store.Transition(ctx, r.ID, v.Seq, fields, map[string]any{"status": store.RunWaiting}); err != nil {
			return err
		}
		r.Status = store.RunWaiting
		e.event(ctx, r, n.ID, "waiting", fmt.Sprintf("%s is waiting for a decision", n.ID))
		e.publishRun(r)
		e.publishVisit(r.ID, v.Seq)
		return nil
	case flow.TypeSwitch:
		outcome, why, err := e.evalSwitch(ctx, r, res, n)
		if err != nil {
			return err
		}
		if err := e.store.UpdateVisit(ctx, r.ID, v.Seq, map[string]any{"status": store.VisitSucceeded, "outcome": outcome, "summary": why, "started_at": store.Now(), "finished_at": store.Now()}); err != nil {
			return err
		}
		v.Status, v.Outcome = store.VisitSucceeded, outcome
		return e.advance(ctx, r, v)
	case flow.TypeAction:
		if n.Action == "comment_task" && v.Status == store.VisitRunning {
			return e.visitError(ctx, r, v, "comment delivery uncertain after interruption; inspect the task before retrying manually")
		}
		out, err := e.runAction(ctx, r, res, n, v)
		if err != nil {
			return err
		}
		outputs, err := json.Marshal(out.outputs)
		if err != nil {
			return err
		}
		if err := e.store.UpdateVisit(ctx, r.ID, v.Seq, map[string]any{"status": store.VisitSucceeded, "outcome": out.outcome, "summary": out.summary, "outputs": string(outputs), "finished_at": store.Now()}); err != nil {
			return err
		}
		v.Status, v.Outcome = store.VisitSucceeded, out.outcome
		return e.advance(ctx, r, v)
	}
	return rejectf("unknown node type %q", n.Type)
}

func (e *Engine) visitCount(ctx context.Context, runID, node string) (int, error) {
	vs, err := e.store.Visits(ctx, runID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, v := range vs {
		if v.Node == node {
			n++
		}
	}
	return n, nil
}

func (e *Engine) launch(ctx context.Context, r *store.Run, res *resolve.Resolved, n *resolve.Node, v *store.Visit) error {
	snap, err := e.loadSnapshot(ctx, r)
	if err != nil {
		return err
	}
	image, ok := snap.Images[n.Runtime]
	if !ok || image == "" {
		return e.visitError(ctx, r, v, fmt.Sprintf("unknown runtime %q", n.Runtime))
	}
	spec := LaunchSpec{RunID: r.ID, Seq: v.Seq, Node: n.ID, Visit: v.Visit, Type: n.Type, Image: image, Timeout: n.Timeout.Duration}
	if n.Retry != nil {
		spec.Retries = n.Retry.Limit
	}
	for _, g := range n.Grants {
		name, _, _ := strings.Cut(g, ":")
		grant := e.cfg.Catalog.Grants[name]
		if grant == nil {
			continue
		}
		switch grant.Kind {
		case config.GrantSecret:
			if grant.SecretRef == nil {
				return rejectf("secret grant %s has no reference", name)
			}
			spec.Secrets = append(spec.Secrets, SecretMount{SecretName: grant.SecretRef.Name, Key: grant.SecretRef.Key, Env: grant.Env, File: grant.File})
		case config.GrantEgress:
			spec.Internet = spec.Internet || grant.Allow == "internet"
		}
	}
	// Write intent before the external call. The Kubernetes launcher uses the
	// same deterministic name and treats AlreadyExists as success.
	name := JobName(spec)
	if v.LaunchState == "pending" {
		st, err := e.launcher.Status(ctx, name)
		if err != nil {
			return err
		}
		if st.State != JobMissing {
			return e.store.UpdateVisit(ctx, r.ID, v.Seq, map[string]any{"job_name": name, "launch_state": "launched"})
		}
		if e.cfg.Env.Runs.Local {
			return e.visitError(ctx, r, v, "local launch delivery uncertain after interruption; refusing to start a duplicate process")
		}
	}
	deadline := v.Deadline
	if deadline == 0 {
		deadline = time.Now().Add(n.Timeout.Duration + 2*time.Minute).UnixMilli()
	}
	if store.Now() > deadline {
		return e.visitError(ctx, r, v, "job launch exceeded its deadline")
	}
	if err := e.store.UpdateVisit(ctx, r.ID, v.Seq, map[string]any{"job_name": name, "launch_state": "pending", "deadline": deadline}); err != nil {
		return err
	}
	name, err = e.launcher.Launch(ctx, spec)
	if err != nil {
		return fmt.Errorf("launch (will reconcile): %w", err)
	}
	if err := e.store.UpdateVisit(ctx, r.ID, v.Seq, map[string]any{"job_name": name, "launch_state": "launched"}); err != nil {
		return err
	}
	e.event(ctx, r, n.ID, "launched", fmt.Sprintf("%s#%d started (%s)", n.ID, v.Visit, n.Type))
	e.publishVisit(r.ID, v.Seq)
	return nil
}

// JobName is the deterministic launcher identity, persisted before submission.
// Keep compatible with launcher.JobName (the launcher imports engine).
func JobName(s LaunchSpec) string {
	node := strings.ReplaceAll(s.Node, "_", "-")
	name := fmt.Sprintf("af-%s-%d-%s", strings.TrimPrefix(s.RunID, "r-"), s.Seq, node)
	if len(name) > 52 {
		name = strings.TrimRight(name[:52], "-")
	}
	return name
}

func (e *Engine) finish(ctx context.Context, r *store.Run, status, msg string) error {
	if store.RunDone(r.Status) {
		return nil
	}
	if status == store.RunFailed {
		v, err := e.store.LastVisit(ctx, r.ID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if v != nil && flow.PodType(v.Type) && v.JobName != "" && v.Status != store.VisitSucceeded {
			// Cleanup uses the persisted identity, never current authorization.
			// Record failure first so a failed Kill remains cleanup work after
			// restart, even if configuration is repaired or a result arrives.
			if !visitDone(v.Status) {
				updated, err := e.store.UpdateVisitIf(ctx, r.ID, v.Seq,
					[]string{store.VisitPending, store.VisitRunning, store.VisitWaiting},
					map[string]any{"status": store.VisitError, "error": msg, "finished_at": store.Now()})
				if err != nil {
					return err
				}
				if !updated {
					return nil // a result won the race; reconcile it on the next tick
				}
				e.publishVisit(r.ID, v.Seq)
			}
			if err := e.launcher.Kill(ctx, v.JobName); err != nil {
				return fmt.Errorf("stop job %s before failing run: %w", v.JobName, err)
			}
		}
	}
	stamp := store.Now()
	if err := e.store.SetRunState(ctx, r, status, msg, stamp); err != nil {
		return err
	}
	r.Status, r.Error, r.FinishedAt = status, msg, stamp
	switch status {
	case store.RunSucceeded:
		e.event(ctx, r, "", "finished", "Run succeeded")
	case store.RunCanceled:
		e.event(ctx, r, "", "finished", "Run canceled")
	default:
		e.event(ctx, r, "", "finished", "Run failed: "+msg)
	}
	e.publishRun(r)
	if t := e.task(ctx, r); t != nil {
		e.hub.Publish(hub.Event{Type: "task", ID: t.ID})
		if e.hooks != nil {
			fresh, _ := e.store.GetRun(ctx, r.ID)
			if fresh == nil {
				fresh = r
			}
			go e.hooks.RunFinished(context.Background(), t, fresh)
		}
	}
	e.Wake() // a slot freed up
	return nil
}

// ---- API entry points ----

// Decide records a human's gate decision.
func (e *Engine) Decide(ctx context.Context, runID string, seq int, outcome, who string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if store.RunDone(r.Status) {
		return fmt.Errorf("run is %s", r.Status)
	}
	v, err := e.store.GetVisit(ctx, runID, seq)
	if err != nil {
		return err
	}
	if v.Status != store.VisitWaiting {
		return fmt.Errorf("%s#%d is not waiting for a decision", v.Node, v.Visit)
	}
	last, err := e.store.LastVisit(ctx, runID)
	if err != nil {
		return err
	}
	if last.Seq != seq {
		return fmt.Errorf("visit %d is no longer current", seq)
	}
	res, err := e.Resolved(ctx, r)
	if err != nil {
		return err
	}
	n := res.Nodes[v.Node]
	if n == nil || !contains(n.Outcomes, outcome) {
		return fmt.Errorf("%q is not an outcome of %s", outcome, v.Node)
	}
	return e.decide(ctx, r, v, outcome, who)
}

func (e *Engine) decide(ctx context.Context, r *store.Run, v *store.Visit, outcome, who string) error {
	summary := fmt.Sprintf("Decided %q", outcome)
	if who == "timeout" {
		summary = "Nobody decided before the timeout"
	} else if who != "" {
		summary += " by " + who
	}
	if err := e.store.Transition(ctx, r.ID, v.Seq, map[string]any{
		"status": store.VisitSucceeded, "outcome": outcome, "decided_by": who, "summary": summary, "finished_at": store.Now(),
	}, map[string]any{"status": store.RunRunning}); err != nil {
		return err
	}
	r.Status = store.RunRunning
	v.Status, v.Outcome = store.VisitSucceeded, outcome
	return e.advance(ctx, r, v)
}

// Cancel stops a run.
func (e *Engine) Cancel(ctx context.Context, runID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if store.RunDone(r.Status) {
		return nil
	}
	v, err := e.store.LastVisit(ctx, runID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if err == nil && !visitDone(v.Status) {
		if v.JobName != "" {
			if err := e.launcher.Kill(ctx, v.JobName); err != nil {
				return err
			}
		}
	}
	if err := e.finish(ctx, r, store.RunCanceled, "canceled"); err != nil {
		return err
	}
	if v != nil {
		e.publishVisit(runID, v.Seq)
	}
	return nil
}

func visitDone(s string) bool {
	return s == store.VisitSucceeded || s == store.VisitError || s == store.VisitCanceled
}

// ---- helpers ----

func (e *Engine) task(ctx context.Context, r *store.Run) *store.Task {
	t, err := e.optionalTask(ctx, r)
	if err != nil {
		slog.Error("read linked task", "run", r.ID, "err", err)
	}
	return t
}

func (e *Engine) optionalTask(ctx context.Context, r *store.Run) (*store.Task, error) {
	if r.TaskID == "" {
		return nil, nil
	}
	t, err := e.store.GetTask(ctx, r.TaskID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (e *Engine) event(ctx context.Context, r *store.Run, node, kind, msg string) {
	if err := e.store.AddEvent(ctx, &store.Event{RunID: r.ID, TaskID: r.TaskID, FlowName: r.FlowName, Node: node, Kind: kind, Message: msg}); err != nil {
		slog.Error("record event", "run", r.ID, "kind", kind, "err", err)
	}
}

func (e *Engine) publishRun(r *store.Run) {
	e.hub.Publish(hub.Event{Type: "run", ID: r.ID})
}

func (e *Engine) publishVisit(runID string, seq int) {
	e.hub.Publish(hub.Event{Type: "visit", ID: runID, Seq: seq})
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
