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
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/github"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/ids"
	"github.com/mauza/ai-flow/internal/notify"
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
	gh        *github.Client
	hooks     Hooks
	onRunDone []func(ctx context.Context, run *store.Run)
	notifier  notify.Sender

	wake    chan struct{}
	mu      sync.Mutex // serializes transitions (loop + synchronous API calls)
	orphans map[string]time.Time

	// OrphanGrace is how long a finished job may go without a posted result
	// before its visit fails (results are posted just before the pod exits).
	OrphanGrace time.Duration
	// PollInterval spaces the polls of waiting actions (CI, deploys, health).
	PollInterval time.Duration
	// HealthSustain is how long a health check must keep failing before a
	// soak calls the release degraded (check_health with.sustain overrides).
	// Two polls 15s apart fit inside one rollout blip: on web-games, a replica
	// unavailable for one 30s scrape rolled back three healthy releases.
	HealthSustain time.Duration
	// HTTP probes deploy targets and queries metrics.
	HTTP *http.Client

	refresher func(ctx context.Context, namespace, app string) error

	polls    map[string]time.Time            // run/seq → last poll of a waiting action
	health   map[string]map[string]int       // run/seq → check → consecutive violations
	badSince map[string]map[string]time.Time // run/seq → check → first poll of the current violation
	passes   map[string]map[string]int       // run/seq → check → successful evaluations
}

func New(cfg *config.Config, st *store.Store, l Launcher, h *hub.Hub, gh *github.Client) *Engine {
	return &Engine{cfg: cfg, store: st, launcher: l, hub: h, gh: gh, wake: make(chan struct{}, 1), orphans: map[string]time.Time{}, OrphanGrace: 15 * time.Second,
		PollInterval: 15 * time.Second, HTTP: &http.Client{Timeout: 10 * time.Second},
		polls: map[string]time.Time{}, health: map[string]map[string]int{}, badSince: map[string]map[string]time.Time{}, passes: map[string]map[string]int{},
		HealthSustain: 90 * time.Second}
}

// Tick runs one pass over active and queued runs (tests drive the engine with it).
func (e *Engine) Tick(ctx context.Context) { e.tick(ctx) }

func (e *Engine) SetHooks(h Hooks) { e.hooks = h }

// OnRunDone registers a callback invoked asynchronously whenever any run
// reaches a terminal status (succeeded, failed, or canceled), whether or not
// the run is linked to a task.
func (e *Engine) OnRunDone(fn func(ctx context.Context, run *store.Run)) {
	if fn != nil {
		e.onRunDone = append(e.onRunDone, fn)
	}
}

// SetArgoRefresher lets wait_for_deploy ask Argo CD to refresh a project's
// app (deploy.argocd_app) instead of waiting for its git poll.
func (e *Engine) SetArgoRefresher(f func(ctx context.Context, namespace, app string) error) {
	e.refresher = f
}

// SetNotifier enables push notifications for the configured events.
func (e *Engine) SetNotifier(n notify.Sender) { e.notifier = n }

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
	slots := e.cfg.Current().Env.Runs.MaxConcurrent - occupied
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

// CreateRun queues a run of a saved flow version. It holds the engine lock so
// a concurrent DeleteFlow cannot remove the flow between the read and the insert.
func (e *Engine) CreateRun(ctx context.Context, flowName string, version int, taskID string) (*store.Run, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	fv, err := e.store.GetFlow(ctx, flowName, version)
	if err != nil {
		return nil, fmt.Errorf("flow %s: %w", flowName, err)
	}
	res, err := e.resolve(fv)
	if err != nil {
		return nil, err
	}
	if issues := resolve.Validate(res, e.cfg.Current()); resolve.HasErrors(issues) {
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
	return resolve.Resolve(f, e.cfg.Current()), nil
}

func (e *Engine) begin(ctx context.Context, r *store.Run) error {
	res, err := e.Resolved(ctx, r)
	if err != nil {
		return err
	}
	if r.Resumes > 0 {
		// A resume queued for a slot: its pending visit is where it goes on.
		if v, err := e.store.LastVisit(ctx, r.ID); err == nil && v.Seq > r.ResumeSeq && v.Status == store.VisitPending {
			if err := e.store.SetRunState(ctx, r, store.RunRunning, "", store.Now()); err != nil {
				return err
			}
			r.Status = store.RunRunning
			e.event(ctx, r, v.Node, "started", "Resumed run started")
			return e.step(ctx, r)
		}
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
	if msg, err := e.runBudgetExceeded(ctx, r); err != nil {
		return err
	} else if msg != "" {
		return e.finish(ctx, r, store.RunFailed, msg)
	}
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
	if v.Type == flow.TypeParallel && (v.Status == store.VisitPending || v.Status == store.VisitRunning) {
		return e.stepParallel(ctx, r, v)
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
			return e.decide(ctx, r, v, flow.OutcomeTimeout, "timeout", "")
		}
		return e.alertStuck(ctx, r, v)
	case store.VisitPending, store.VisitRunning:
		return e.stepActive(ctx, r, v)
	}
	return nil
}

// stepActive moves one unfinished visit along, on the main path or in a
// parallel branch: control-plane nodes execute, pod nodes launch or are
// watched until their result arrives, their deadline passes or their job dies.
func (e *Engine) stepActive(ctx context.Context, r *store.Run, v *store.Visit) error {
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
	e.saveProjectMap(ctx, r, v)
	ln, err := e.laneOf(ctx, res, v)
	if err != nil {
		return err
	}
	// Mid-flight pause: if a human asked to pause after the current step, stop
	// cleanly before launching the next non-terminal main-path node.
	if ln == nil && !flow.Terminal(target) && r.PauseRequested {
		return e.enterPause(ctx, r, v, target)
	}
	return e.enterWhy(ctx, r, res, target, 0, "", ln)
}

func (e *Engine) enterPause(ctx context.Context, r *store.Run, prev *store.Visit, nextNode string) error {
	v, err := e.store.AddVisit(ctx, r.ID, flow.NodePause, flow.TypeGate)
	if err != nil {
		return err
	}
	prompt := fmt.Sprintf("Paused before %s (after %s → %s). Review progress and continue with optional direction, or stop.", nextNode, prev.Node, prev.Outcome)
	outputs, err := json.Marshal(map[string]string{"next_node": nextNode})
	if err != nil {
		return err
	}
	if err := e.store.Transition(ctx, r.ID, v.Seq, map[string]any{
		"status":     store.VisitWaiting,
		"started_at": store.Now(),
		"prompt":     prompt,
		"outputs":    string(outputs),
	}, map[string]any{
		"status":          store.RunWaiting,
		"current_node":    flow.NodePause,
		"pause_requested": 0,
	}); err != nil {
		return err
	}
	r.Status, r.CurrentNode, r.PauseRequested = store.RunWaiting, flow.NodePause, false
	e.event(ctx, r, flow.NodePause, "waiting", fmt.Sprintf("Paused before %s for human direction", nextNode))
	e.publishRun(r)
	e.publishVisit(r.ID, v.Seq)
	e.alert(ctx, config.NotifyGate, r, "paused before "+nextNode, prompt, 4, "raised_hand")
	return nil
}

func (e *Engine) enter(ctx context.Context, r *store.Run, res *resolve.Resolved, target string, depth int) error {
	return e.enterWhy(ctx, r, res, target, depth, "", nil)
}

// lane is a parallel branch in progress; nil means the run's main path.
type lane struct {
	fork int    // seq of the parallel visit
	name string // the branch (its first node)
	join string // where every branch of the parallel node ends
}

// laneOf returns the branch v runs in, or nil on the main path.
func (e *Engine) laneOf(ctx context.Context, res *resolve.Resolved, v *store.Visit) (*lane, error) {
	if v.ForkSeq == 0 {
		return nil, nil
	}
	fv, err := e.store.GetVisit(ctx, v.RunID, v.ForkSeq)
	if err != nil {
		return nil, err
	}
	fork := res.Nodes[fv.Node]
	if fork == nil {
		return nil, rejectf("parallel node %q not in snapshot", fv.Node)
	}
	return &lane{fork: v.ForkSeq, name: v.Lane, join: fork.Join}, nil
}

// enterWhy moves the run (or one parallel branch, when ln is set) to target;
// why explains an on_exhausted jump. A branch that reaches its join stops
// there: stepParallel continues once every branch has arrived.
func (e *Engine) enterWhy(ctx context.Context, r *store.Run, res *resolve.Resolved, target string, depth int, why string, ln *lane) error {
	if depth > 20 {
		return rejectf("too many chained transitions (on_exhausted cycle?)")
	}
	if ln != nil && target == ln.join {
		e.Wake()
		return nil
	}
	switch target {
	case flow.Success:
		if ln != nil {
			return rejectf("branch %q cannot end the run with $success", ln.name)
		}
		return e.finish(ctx, r, store.RunSucceeded, "")
	case flow.Fail:
		var last *store.Visit
		var err error
		if ln != nil {
			last, err = e.store.LaneHead(ctx, r.ID, ln.fork, ln.name)
		} else {
			last, err = e.store.LastVisit(ctx, r.ID)
		}
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
		} else if ln == nil && last != nil && isReportingStep(res.Nodes[last.Node], last) {
			if root := e.rootFailureReason(ctx, r, last); root != "" {
				msg = root + " (reported via " + last.Node + ")"
			}
		}
		return e.finish(ctx, r, store.RunFailed, msg)
	}
	n := res.Nodes[target]
	if n == nil {
		return rejectf("unknown node %q", target)
	}
	if n.MaxVisits > 0 {
		count, err := e.visitCount(ctx, r, target)
		if err != nil {
			return err
		}
		limit := n.MaxVisits + r.ExtraVisits
		if count >= limit {
			why := fmt.Sprintf("%s already ran %d times (max_visits)", target, limit)
			e.event(ctx, r, target, "exhausted", why+" → "+n.OnExhausted)
			return e.enterWhy(ctx, r, res, n.OnExhausted, depth+1, why, ln)
		}
	}
	var v *store.Visit
	var err error
	if ln != nil {
		v, err = e.store.AddLaneVisit(ctx, r.ID, target, n.Type, ln.fork, ln.name)
	} else {
		v, err = e.store.AddVisit(ctx, r.ID, target, n.Type)
		r.CurrentNode, r.Status = target, store.RunRunning
	}
	if err != nil {
		return err
	}
	e.publishRun(r)
	return e.execute(ctx, r, res, n, v)
}

// stepParallel drives each branch of the parallel visit fv one step. Once
// every branch has reached the join, the parallel visit succeeds with
// "joined" and the run continues at the join on the main path. Any branch
// error fails the run (and finish stops the other branches).
func (e *Engine) stepParallel(ctx context.Context, r *store.Run, fv *store.Visit) error {
	res, err := e.Resolved(ctx, r)
	if err != nil {
		return err
	}
	n := res.Nodes[fv.Node]
	if n == nil {
		return rejectf("node %q not in snapshot", fv.Node)
	}
	if fv.Status == store.VisitPending {
		if err := e.store.UpdateVisit(ctx, r.ID, fv.Seq, map[string]any{"status": store.VisitRunning, "started_at": store.Now()}); err != nil {
			return err
		}
		fv.Status = store.VisitRunning
		e.event(ctx, r, n.ID, "parallel", fmt.Sprintf("%s#%d started %d branches: %s", n.ID, fv.Visit, len(n.Branches), strings.Join(n.Branches, ", ")))
		e.publishVisit(r.ID, fv.Seq)
	}
	arrived := 0
	var fastFail *store.Visit
	for _, b := range n.Branches {
		ln := &lane{fork: fv.Seq, name: b, join: n.Join}
		head, err := e.store.LaneHead(ctx, r.ID, fv.Seq, b)
		switch {
		case errors.Is(err, store.ErrNotFound):
			err = e.enterWhy(ctx, r, res, b, 0, "", ln)
		case err != nil:
		case head.Status == store.VisitPending || head.Status == store.VisitRunning:
			err = e.stepActive(ctx, r, head)
		case head.Status == store.VisitSucceeded:
			var done bool
			if done, err = e.laneArrived(ctx, r, res, ln, head); done {
				arrived++
				if n.FailFast && isFailFastOutcome(res.Nodes[head.Node], head.Outcome) && fastFail == nil {
					fastFail = head
				}
			} else if err == nil {
				err = e.advance(ctx, r, head)
			}
		case head.Status == store.VisitError:
			err = e.advance(ctx, r, head)
		default:
			err = e.finish(ctx, r, store.RunCanceled, "canceled")
		}
		if err != nil {
			return err
		}
		if store.RunDone(r.Status) {
			return nil
		}
	}
	if fastFail != nil && arrived < len(n.Branches) {
		if err := e.cancelSiblingLanes(ctx, r, fv, fastFail.Lane, fmt.Sprintf("canceled by parallel fail_fast (%s → %s)", fastFail.Node, fastFail.Outcome)); err != nil {
			return err
		}
		ok, err := e.store.UpdateVisitIf(ctx, r.ID, fv.Seq, []string{store.VisitPending, store.VisitRunning}, map[string]any{
			"status": store.VisitSucceeded, "outcome": flow.OutcomeJoined, "finished_at": store.Now(),
			"summary": fmt.Sprintf("Fail-fast triggered by %s → %s; sibling branches canceled", fastFail.Node, fastFail.Outcome),
		})
		if err != nil || !ok {
			return err
		}
		fv.Status, fv.Outcome = store.VisitSucceeded, flow.OutcomeJoined
		return e.advance(ctx, r, fv)
	}
	if arrived < len(n.Branches) {
		return nil
	}
	ok, err := e.store.UpdateVisitIf(ctx, r.ID, fv.Seq, []string{store.VisitPending, store.VisitRunning}, map[string]any{
		"status": store.VisitSucceeded, "outcome": flow.OutcomeJoined, "finished_at": store.Now(),
		"summary": fmt.Sprintf("All %d branches reached %s", len(n.Branches), n.Join),
	})
	if err != nil || !ok {
		return err
	}
	fv.Status, fv.Outcome = store.VisitSucceeded, flow.OutcomeJoined
	return e.advance(ctx, r, fv)
}

// laneArrived reports whether the branch whose latest visit is head has
// reached its join, following the same on_exhausted jumps enterWhy takes.
func (e *Engine) laneArrived(ctx context.Context, r *store.Run, res *resolve.Resolved, ln *lane, head *store.Visit) (bool, error) {
	n := res.Nodes[head.Node]
	if n == nil {
		return false, rejectf("node %q not in snapshot", head.Node)
	}
	target := n.Next[head.Outcome]
	for depth := 0; depth <= 20; depth++ {
		if target == ln.join {
			return true, nil
		}
		next := res.Nodes[target]
		if next == nil || next.MaxVisits == 0 {
			return false, nil
		}
		count, err := e.visitCount(ctx, r, target)
		if err != nil || count < next.MaxVisits+r.ExtraVisits {
			return false, err
		}
		target = next.OnExhausted
	}
	return false, nil
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
		e.alert(ctx, config.NotifyGate, r, n.ID+" needs a decision", prompt, 4, "raised_hand")
		return nil
	case flow.TypeParallel:
		return e.stepParallel(ctx, r, v)
	case flow.TypeSwitch, flow.TypeJoin:
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
		return e.execAction(ctx, r, res, n, v)
	}
	return rejectf("unknown node type %q", n.Type)
}

// visitCount counts node's visits toward max_visits. Visits before the latest
// resume do not count: each resume gets one fresh bounded window.
func (e *Engine) visitCount(ctx context.Context, r *store.Run, node string) (int, error) {
	vs, err := e.store.Visits(ctx, r.ID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, v := range vs {
		if v.Node == node && v.Seq > r.ResumeSeq {
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
		grant := e.cfg.Current().Catalog.Grants[name]
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
		if e.cfg.Current().Env.Runs.Local {
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
		pods, err := e.liveJobs(ctx, r.ID)
		if err != nil {
			return err
		}
		active := 0
		for _, v := range pods {
			if !visitDone(v.Status) {
				active++
			}
		}
		for _, v := range pods {
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
				if !updated && active == 1 {
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
	e.forgetRun(r.ID)
	switch status {
	case store.RunSucceeded:
		e.event(ctx, r, "", "finished", "Run succeeded")
		e.alert(ctx, config.NotifySucceeded, r, "succeeded", r.PRURL, 3, "white_check_mark")
	case store.RunCanceled:
		e.event(ctx, r, "", "finished", "Run canceled")
	default:
		e.event(ctx, r, "", "finished", "Run failed: "+msg)
		e.alert(ctx, config.NotifyFailed, r, "failed", msg, 4, "x")
	}
	e.publishRun(r)
	var fresh *store.Run
	loadFresh := func() *store.Run {
		if fresh != nil {
			return fresh
		}
		fresh, _ = e.store.GetRun(ctx, r.ID)
		if fresh == nil {
			cp := *r
			fresh = &cp
		}
		return fresh
	}
	if t := e.task(ctx, r); t != nil {
		e.hub.Publish(hub.Event{Type: "task", ID: t.ID})
		if e.hooks != nil {
			go e.hooks.RunFinished(context.Background(), t, loadFresh())
		}
	}
	if len(e.onRunDone) > 0 {
		doneRun := loadFresh()
		for _, fn := range e.onRunDone {
			go fn(context.Background(), doneRun)
		}
	}
	e.Wake() // a slot freed up
	return nil
}

// ---- API entry points ----

// MaxExtraVisits caps how many extra loop rounds a human can grant via gates
// so loops remain bounded even under repeated revisions.
const MaxExtraVisits = 10

// Decide records a human's gate decision and optional note. The note becomes the
// gate's outputs.note, so the next step sees it and later ones can reference it.
func (e *Engine) Decide(ctx context.Context, runID string, seq int, outcome, who, note string) error {
	return e.DecideWithOptions(ctx, runID, seq, outcome, who, note, false)
}

// DecideWithOptions records a gate decision. When extendVisits is true, the
// run's bounded nodes receive +1 loop visit (up to MaxExtraVisits) so a human
// requesting another revision does not immediately hit max_visits.
func (e *Engine) DecideWithOptions(ctx context.Context, runID string, seq int, outcome, who, note string, extendVisits bool) error {
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
	if v.Node == flow.NodePause {
		return e.decidePause(ctx, r, v, outcome, who, note)
	}
	res, err := e.Resolved(ctx, r)
	if err != nil {
		return err
	}
	n := res.Nodes[v.Node]
	if n == nil || !slices.Contains(n.Outcomes, outcome) {
		return fmt.Errorf("%q is not an outcome of %s", outcome, v.Node)
	}
	return e.decideOpts(ctx, r, v, outcome, who, note, extendVisits)
}

func (e *Engine) decidePause(ctx context.Context, r *store.Run, v *store.Visit, outcome, who, note string) error {
	if outcome != flow.OutcomeContinue && outcome != flow.OutcomeStop {
		return fmt.Errorf("%q is not an outcome of %s (use continue or stop)", outcome, flow.NodePause)
	}
	var outMap map[string]string
	json.Unmarshal(v.Outputs, &outMap)
	if outMap == nil {
		outMap = map[string]string{}
	}
	nextNode := outMap["next_node"]
	note = strings.TrimSpace(note)
	summary := fmt.Sprintf("Decided %q", outcome)
	if who != "" {
		summary += " by " + who
	}
	if note != "" {
		summary += ": " + note
		outMap["note"] = note
	}
	rawOut, err := json.Marshal(outMap)
	if err != nil {
		return err
	}
	runFields := map[string]any{"status": store.RunRunning}
	if note != "" {
		runFields["steer_note"] = note
		runFields["steer_seq"] = v.Seq
		r.SteerNote, r.SteerSeq = note, v.Seq
	}
	if err := e.store.Transition(ctx, r.ID, v.Seq, map[string]any{
		"status": store.VisitSucceeded, "outcome": outcome, "decided_by": who, "summary": summary, "outputs": string(rawOut), "finished_at": store.Now(),
	}, runFields); err != nil {
		return err
	}
	r.Status = store.RunRunning
	v.Status, v.Outcome = store.VisitSucceeded, outcome
	e.publishVisit(r.ID, v.Seq)
	if outcome == flow.OutcomeStop {
		msg := "stopped at pause gate"
		if note != "" {
			msg += ": " + note
		}
		return e.finish(ctx, r, store.RunCanceled, msg)
	}
	res, err := e.Resolved(ctx, r)
	if err != nil {
		return err
	}
	e.event(ctx, r, flow.NodePause, "outcome", fmt.Sprintf("%s → %s (entering %s)", flow.NodePause, outcome, nextNode))
	return e.enterWhy(ctx, r, res, nextNode, 0, "", nil)
}

func (e *Engine) decide(ctx context.Context, r *store.Run, v *store.Visit, outcome, who, note string) error {
	return e.decideOpts(ctx, r, v, outcome, who, note, false)
}

func (e *Engine) decideOpts(ctx context.Context, r *store.Run, v *store.Visit, outcome, who, note string, extendVisits bool) error {
	summary := fmt.Sprintf("Decided %q", outcome)
	if who == "timeout" {
		summary = "Nobody decided before the timeout"
	} else if who != "" {
		summary += " by " + who
	}
	outputs := "{}"
	if note = strings.TrimSpace(note); note != "" {
		summary += ": " + note
		b, err := json.Marshal(map[string]string{"note": note})
		if err != nil {
			return err
		}
		outputs = string(b)
	}
	runFields := map[string]any{"status": store.RunRunning}
	extended := false
	if extendVisits && r.ExtraVisits < MaxExtraVisits {
		r.ExtraVisits++
		runFields["extra_visits"] = r.ExtraVisits
		extended = true
	}
	if err := e.store.Transition(ctx, r.ID, v.Seq, map[string]any{
		"status": store.VisitSucceeded, "outcome": outcome, "decided_by": who, "summary": summary, "outputs": outputs, "finished_at": store.Now(),
	}, runFields); err != nil {
		return err
	}
	r.Status = store.RunRunning
	v.Status, v.Outcome = store.VisitSucceeded, outcome
	if extended {
		e.event(ctx, r, v.Node, "extended", fmt.Sprintf("Human granted +1 loop round (extra_visits=%d)", r.ExtraVisits))
	}
	return e.advance(ctx, r, v)
}

// Steer updates mid-flight human steering controls on an active run: request a
// pause after the current step, and/or record a steering note that subsequent
// steps receive in their context without canceling the active step.
func (e *Engine) Steer(ctx context.Context, runID string, pause *bool, note *string, who string) (*store.Run, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if store.RunDone(r.Status) {
		return nil, fmt.Errorf("run is %s", r.Status)
	}
	fields := map[string]any{}
	var notes []string
	if pause != nil {
		if *pause {
			fields["pause_requested"] = 1
			notes = append(notes, "pause requested after current step")
		} else {
			fields["pause_requested"] = 0
			notes = append(notes, "pause request cleared")
		}
	}
	if note != nil {
		trimmed := strings.TrimSpace(*note)
		seq := 0
		if last, err := e.store.LastVisit(ctx, runID); err == nil {
			seq = last.Seq
		}
		fields["steer_note"] = trimmed
		fields["steer_seq"] = seq
		if trimmed != "" {
			notes = append(notes, "note: "+trimmed)
		} else {
			notes = append(notes, "steering note cleared")
		}
	}
	if len(fields) == 0 {
		return r, nil
	}
	if err := e.store.UpdateRun(ctx, runID, fields); err != nil {
		return nil, err
	}
	msg := strings.Join(notes, "; ")
	if who != "" {
		msg = who + ": " + msg
	}
	e.event(ctx, r, r.CurrentNode, "steer", msg)
	fresh, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	e.publishRun(fresh)
	e.Wake()
	return fresh, nil
}

// DeleteFlow removes a flow with its versions, chat and finished runs. It
// fails with *store.ErrFlowActive while a run of the flow is queued, running
// or waiting. Objects such as transcripts are the caller's to remove.
func (e *Engine) DeleteFlow(ctx context.Context, name string) (*store.DeletedFlow, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.store.DeleteFlow(ctx, name)
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
	pods, err := e.liveJobs(ctx, runID)
	if err != nil {
		return err
	}
	// Errored visits too: a timed-out pod can outlive its visit.
	for _, v := range pods {
		if err := e.launcher.Kill(ctx, v.JobName); err != nil {
			return err
		}
	}
	if err := e.finish(ctx, r, store.RunCanceled, "canceled"); err != nil {
		return err
	}
	for _, v := range pods {
		e.publishVisit(runID, v.Seq)
	}
	return nil
}

// liveJobs returns the pod visits whose Job may still exist: unfinished ones,
// and errored ones (a timed-out pod can outlive its visit). Kill is idempotent.
func (e *Engine) liveJobs(ctx context.Context, runID string) ([]*store.Visit, error) {
	vs, err := e.store.Visits(ctx, runID)
	if err != nil {
		return nil, err
	}
	var out []*store.Visit
	for _, v := range vs {
		if flow.PodType(v.Type) && v.JobName != "" && v.Status != store.VisitSucceeded && v.Status != store.VisitCanceled {
			out = append(out, v)
		}
	}
	return out, nil
}

// Resume re-opens a failed or canceled run at node (default: the node of its
// last visit). The run keeps its branch, pinned settings, visits and spend; the
// note reaches the steps that follow through their context.
func (e *Engine) Resume(ctx context.Context, runID, node, note, who string) (*store.Run, error) {
	return e.ResumeOn(ctx, runID, 0, false, node, note, who)
}

// ResumeOn resumes a run, optionally on another saved version of its flow:
// the run keeps its id, branch and history, and continues under the new
// version's steps. This is how a flow bug found mid-run (a broken check, a
// missing conflict route) is fixed without starting over on a new branch.
// refresh re-pins the run's own version to the current runtimes and catalog
// (a runtime image fixed since the run started).
func (e *Engine) ResumeOn(ctx context.Context, runID string, version int, refresh bool, node, note, who string) (*store.Run, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if r.Status != store.RunFailed && r.Status != store.RunCanceled {
		return nil, fmt.Errorf("run is %s: %w", r.Status, store.ErrNotResumable)
	}
	var res *resolve.Resolved
	var repin *store.Repin
	if version == 0 && refresh {
		version = r.FlowVersion
	}
	if version != 0 && (version != r.FlowVersion || refresh) {
		fv, err := e.store.GetFlow(ctx, r.FlowName, version)
		if err != nil {
			return nil, fmt.Errorf("%s v%d: %w", r.FlowName, version, err)
		}
		if res, err = e.resolve(fv); err != nil {
			return nil, fmt.Errorf("cannot resume on v%d: %w", version, err)
		}
		res.Base = r.Base // the run's branch was cut from it
		if issues := resolve.Validate(res, e.cfg.Current()); resolve.HasErrors(issues) {
			return nil, fmt.Errorf("cannot resume on v%d: %v", version, issues)
		}
		snap, err := e.snapshot(res)
		if err != nil {
			return nil, err
		}
		repin = &store.Repin{FlowVersion: version, Snapshot: snap}
	} else if res, err = e.Resolved(ctx, r); err != nil {
		return nil, fmt.Errorf("cannot resume: %w", err)
	}
	if node == "" {
		last, err := e.store.LastVisit(ctx, runID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			node = res.Flow.Spec.Start
		case err != nil:
			return nil, err
		default:
			node = last.Node
		}
	}
	n := res.Nodes[node]
	if n == nil {
		return nil, fmt.Errorf("%q is not a node of %s v%d", node, r.FlowName, max(version, r.FlowVersion))
	}
	if lanes, _ := res.Lanes(); lanes[node].Fork != "" {
		return nil, fmt.Errorf("%q runs inside a branch of %q; resume at %q to rerun its branches", node, lanes[node].Fork, lanes[node].Fork)
	}
	active, err := e.store.ListRuns(ctx, store.RunFilter{Statuses: []string{store.RunRunning, store.RunWaiting}, Limit: -1})
	if err != nil {
		return nil, err
	}
	// With every slot busy the run waits its turn like a new one; it used to
	// be refused, and a canceled run's slot went to the next queued run.
	queued := len(active) >= e.cfg.Current().Env.Runs.MaxConcurrent
	note = strings.TrimSpace(note)
	v, err := e.store.ResumeRun(ctx, runID, node, n.Type, note, repin, queued)
	if err != nil {
		return nil, err
	}
	msg := fmt.Sprintf("Resumed at %s#%d", node, v.Visit)
	if repin != nil && version != r.FlowVersion {
		msg = fmt.Sprintf("Resumed on v%d (was v%d) at %s#%d", version, r.FlowVersion, node, v.Visit)
	} else if repin != nil {
		msg = fmt.Sprintf("Resumed with current runtimes and catalog at %s#%d", node, v.Visit)
	}
	if who != "" {
		msg += " by " + who
	}
	if note != "" {
		msg += ": " + note
	}
	if queued {
		msg += " (queued: every run slot is busy)"
	}
	e.event(ctx, r, node, "resumed", msg)
	fresh, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	e.publishRun(fresh)
	e.publishVisit(runID, v.Seq)
	if t := e.task(ctx, fresh); t != nil {
		e.hub.Publish(hub.Event{Type: "task", ID: t.ID})
		if e.hooks != nil {
			started := *fresh
			go e.hooks.RunStarted(context.Background(), t, &started)
		}
	}
	e.Wake()
	return fresh, nil
}

func visitDone(s string) bool {
	return s == store.VisitSucceeded || s == store.VisitError || s == store.VisitCanceled
}

// runBudgetExceeded enforces flow wall-clock and run-level token caps.
func (e *Engine) runBudgetExceeded(ctx context.Context, r *store.Run) (string, error) {
	res, err := e.Resolved(ctx, r)
	if err != nil {
		return "", nil // let normal reconciliation surface drift/validation errors
	}
	if bd := res.Flow.Spec.Budget; bd != nil {
		if bd.Wall.Duration > 0 && r.StartedAt > 0 && store.Now() > r.StartedAt+bd.Wall.Duration.Milliseconds() {
			return fmt.Sprintf("flow wall-clock budget %s exceeded", shortDuration(bd.Wall.Duration)), nil
		}
		if bd.Tokens > 0 && r.Tokens >= bd.Tokens {
			return fmt.Sprintf("flow token budget %d exceeded (%d used)", bd.Tokens, r.Tokens), nil
		}
	}
	if p := res.Project; p != nil && p.Spec.Budget != nil && p.Spec.Budget.TokensPerRun > 0 && r.Tokens >= p.Spec.Budget.TokensPerRun {
		return fmt.Sprintf("project token cap %d per run exceeded (%d used)", p.Spec.Budget.TokensPerRun, r.Tokens), nil
	}
	return "", nil
}

// isReportingStep reports whether a visit before $fail is a reporting/comment
// step whose own happy outcome ("done", "reported", "ready") would mask the
// real reason the flow took a failure path.
func isReportingStep(n *resolve.Node, v *store.Visit) bool {
	if v == nil {
		return false
	}
	if n != nil && n.Type == flow.TypeAction && n.Action == "comment_task" {
		return true
	}
	switch v.Outcome {
	case "done", "reported", "ready", "drafted":
		return strings.Contains(v.Node, "report") || strings.Contains(v.Node, "publish") || strings.Contains(v.Node, "comment") || strings.Contains(v.Node, "handoff")
	}
	return false
}

// rootFailureReason walks recent events and visits (after the latest resume)
// to recover the underlying cause when a failure-reporting step routes to $fail.
func (e *Engine) rootFailureReason(ctx context.Context, r *store.Run, last *store.Visit) string {
	if evs, err := e.store.Events(ctx, "run_id", r.ID, 50); err == nil {
		for _, ev := range evs {
			if ev.Kind == "exhausted" && ev.Message != "" {
				return ev.Message
			}
		}
	}
	vs, err := e.store.Visits(ctx, r.ID)
	if err != nil {
		return ""
	}
	for i := len(vs) - 1; i >= 0; i-- {
		v := vs[i]
		if v.Seq >= last.Seq || v.Seq <= r.ResumeSeq {
			continue
		}
		if v.Status == store.VisitError && v.Error != "" {
			return fmt.Sprintf("%s#%d: %s", v.Node, v.Visit, v.Error)
		}
		switch v.Outcome {
		case "fail", "failed", "defects", "changes", "degraded", "timeout", "stuck", "blocked", "limit", "cannot_reproduce", "inconclusive":
			msg := fmt.Sprintf("%s#%d → %s", v.Node, v.Visit, v.Outcome)
			if v.Summary != "" {
				msg += ": " + oneLine(v.Summary, 240)
			}
			return msg
		}
	}
	return ""
}

func isFailFastOutcome(n *resolve.Node, outcome string) bool {
	if n != nil && n.Type == flow.TypeCheck && outcome != "pass" {
		return true
	}
	switch outcome {
	case "fail", "failed", "defects", "blocked", "stuck", "timeout", "limit":
		return true
	}
	return false
}

func (e *Engine) cancelSiblingLanes(ctx context.Context, r *store.Run, fv *store.Visit, winnerLane, reason string) error {
	vs, err := e.store.Visits(ctx, r.ID)
	if err != nil {
		return err
	}
	for _, v := range vs {
		if v.ForkSeq != fv.Seq || v.Lane == winnerLane || visitDone(v.Status) {
			continue
		}
		if _, err := e.store.UpdateVisitIf(ctx, r.ID, v.Seq, []string{store.VisitPending, store.VisitRunning, store.VisitWaiting},
			map[string]any{"status": store.VisitCanceled, "error": reason, "finished_at": store.Now()}); err != nil {
			return err
		}
		e.publishVisit(r.ID, v.Seq)
		if flow.PodType(v.Type) && v.JobName != "" {
			if err := e.launcher.Kill(ctx, v.JobName); err != nil {
				return fmt.Errorf("kill sibling job %s: %w", v.JobName, err)
			}
		}
	}
	return nil
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

// ---- notifications ----

// alert pushes a notification about r in the background. Delivery is best
// effort: a failed push is logged and never affects the run.
func (e *Engine) alert(ctx context.Context, event string, r *store.Run, what, body string, priority int, tags ...string) {
	if e.notifier == nil || !e.cfg.Current().Env.Notify.Sends(event) {
		return
	}
	label := r.FlowName
	if t := e.task(ctx, r); t != nil {
		label = firstNonEmpty(t.Identifier, t.Title)
	}
	m := notify.Message{Title: label + ": " + what, Body: tailStr(strings.TrimSpace(body), 1000), Priority: priority, Tags: tags}
	if m.Body == "" {
		m.Body = fmt.Sprintf("Run %s of %s v%d", r.ID, r.FlowName, r.FlowVersion)
	}
	if base := strings.TrimSuffix(e.cfg.Current().Env.Server.PublicURL, "/"); base != "" {
		m.Click = base + "/runs/" + r.ID
		label := "Open run"
		if event == config.NotifyGate || event == config.NotifyStuck {
			label = "Review & decide"
		}
		m.Actions = append(m.Actions, notify.Action{Action: "view", Label: label, URL: m.Click})
		if r.PRURL != "" {
			m.Actions = append(m.Actions, notify.Action{Action: "view", Label: "Open PR", URL: r.PRURL})
		}
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := e.notifier.Send(ctx, m); err != nil {
			slog.Warn("notify", "event", event, "run", r.ID, "err", err)
		}
	}()
}

// shortDuration renders 4h0m0s as 4h, 1h30m0s as 1h30m, and keeps
// sub-minute values exact (0s, 45s, 300ms).
func shortDuration(d time.Duration) string {
	if d < time.Minute {
		return d.Round(time.Millisecond).String()
	}
	s := d.Round(time.Minute).String()
	s = strings.TrimSuffix(s, "0s")
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// alertStuck sends one reminder per gate visit that has waited longer than
// notify.stuckAfter. The kv marker keeps restarts from repeating it.
func (e *Engine) alertStuck(ctx context.Context, r *store.Run, v *store.Visit) error {
	after := e.cfg.Current().Env.Notify.StuckAfter.Duration
	if e.notifier == nil || !e.cfg.Current().Env.Notify.Sends(config.NotifyStuck) || v.StartedAt == 0 || store.Now()-v.StartedAt < after.Milliseconds() {
		return nil
	}
	key := fmt.Sprintf("notify/stuck/%s/%d", r.ID, v.Seq)
	if _, err := e.store.GetKV(ctx, key); !errors.Is(err, store.ErrNotFound) {
		return err // already sent, or a read error to retry next tick
	}
	if err := e.store.SetKV(ctx, key, fmt.Sprint(store.Now())); err != nil {
		return err
	}
	e.alert(ctx, config.NotifyStuck, r, fmt.Sprintf("%s still waiting after %s", v.Node, shortDuration(after)), v.Prompt, 4, "hourglass")
	return nil
}
