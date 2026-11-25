package app

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/engine"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/ids"
	"github.com/mauza/ai-flow/internal/store"
	"github.com/mauza/ai-flow/internal/storymap"
)

// autoStart lets a scheduled task (or a task its run spawned) start its own
// run. The schedule's current limits decide when; pausing or removing the
// schedule holds the task.
type autoStart struct {
	Schedule  string `json:"schedule"`
	Force     bool   `json:"force,omitempty"`     // asked for by hand: start without waiting for a slot
	Replanned bool   `json:"replanned,omitempty"` // a restart interrupted planning once already
}

func (r autoStart) json() string {
	b, _ := json.Marshal(r)
	return string(b)
}

func kvTaskAutoStart(taskID string) string { return "task/autostart/" + taskID }

func (a *App) autoStartRecord(ctx context.Context, taskID string) (*autoStart, error) {
	raw, err := a.Store.GetKV(ctx, kvTaskAutoStart(taskID))
	if errors.Is(err, store.ErrNotFound) || (err == nil && raw == "") {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rec autoStart
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return nil, nil // a flag from before schedules owned their tasks
	}
	return &rec, nil
}

// scheduleRetry is how soon a skipped or failed trigger is tried again.
const scheduleRetry = 10 * time.Minute

// ScheduleState tracks the last execution of one project schedule in KV.
type ScheduleState struct {
	LastRunAt  int64  `json:"last_run_at,omitempty"`
	RetryAt    int64  `json:"retry_at,omitempty"`    // set while a skipped or failed trigger waits to be tried again
	LastStatus string `json:"last_status,omitempty"` // started | skipped | error
	LastReason string `json:"last_reason,omitempty"`
	LastRunID  string `json:"last_run_id,omitempty"`
	LastTaskID string `json:"last_task_id,omitempty"`
}

// ScheduleView combines a configured ProjectSchedule with its live runtime state.
type ScheduleView struct {
	Project       string                   `json:"project"`
	Name          string                   `json:"name"`
	Enabled       bool                     `json:"enabled"`
	Paused        bool                     `json:"paused"`
	Every         string                   `json:"every"`
	Flow          string                   `json:"flow,omitempty"`
	TaskTitle     string                   `json:"task_title,omitempty"`
	StoryMap      *config.ScheduleStoryMap `json:"storymap,omitempty"`
	AutoStart     bool                     `json:"auto_start,omitempty"`
	MaxActiveRuns int                      `json:"max_active_runs"`
	MaxOpenTasks  int                      `json:"max_open_tasks"`
	MaxOpenPRs    int                      `json:"max_open_prs"`
	NextDueAt     int64                    `json:"next_due_at,omitempty"`
	State         ScheduleState            `json:"state"`
}

func scheduleKVKey(project, name string) string {
	return "schedule/" + project + "/" + name
}

func (a *App) loadScheduleState(ctx context.Context, project, name string) ScheduleState {
	var st ScheduleState
	if raw, err := a.Store.GetKV(ctx, scheduleKVKey(project, name)); err == nil && raw != "" {
		_ = json.Unmarshal([]byte(raw), &st)
	}
	return st
}

func (a *App) saveScheduleState(ctx context.Context, project, name string, st ScheduleState) {
	b, _ := json.Marshal(st)
	if err := a.Store.SetKV(ctx, scheduleKVKey(project, name), string(b)); err != nil {
		slog.Warn("save schedule state", "project", project, "schedule", name, "err", err)
	}
}

func schedulePausedKey(project, name string) string {
	return "schedule-paused/" + project + "/" + name
}

func (a *App) schedulePaused(ctx context.Context, project, name string) bool {
	v, err := a.Store.GetKV(ctx, schedulePausedKey(project, name))
	return err == nil && v != ""
}

// scheduleLive reports whether the schedule may act on its own.
func (a *App) scheduleLive(ctx context.Context, project string, sc *config.ProjectSchedule) bool {
	return sc != nil && sc.IsEnabled() && !a.schedulePaused(ctx, project, sc.Name)
}

// PauseSchedule holds or releases a schedule without a config change: no
// triggers, and no runs started for tasks it already queued.
func (a *App) PauseSchedule(ctx context.Context, project, name string, paused bool) error {
	if a.findSchedule(project, name) == nil {
		return fmt.Errorf("project %q has no schedule %q", project, name)
	}
	key := schedulePausedKey(project, name)
	var err error
	if paused {
		err = a.Store.SetKV(ctx, key, "1")
	} else {
		err = a.Store.WriteKV(ctx, nil, []string{key})
	}
	if err == nil && a.Hub != nil {
		a.Hub.Publish(hub.Event{Type: "schedule", ID: project})
	}
	return err
}

func (a *App) findSchedule(project, name string) *config.ProjectSchedule {
	p := a.Cfg.Current().Projects[project]
	if p == nil {
		return nil
	}
	for i := range p.Spec.Schedules {
		if p.Spec.Schedules[i].Name == name {
			return &p.Spec.Schedules[i]
		}
	}
	return nil
}

func (st ScheduleState) nextDue(every time.Duration) int64 {
	if st.RetryAt > 0 {
		return st.RetryAt
	}
	if st.LastRunAt == 0 {
		return 0
	}
	return st.LastRunAt + every.Milliseconds()
}

// ListSchedules returns every configured schedule (optionally filtered to one
// project) together with its last execution state and next due timestamp.
func (a *App) ListSchedules(ctx context.Context, project string) []ScheduleView {
	cfg := a.Cfg.Current()
	out := []ScheduleView{}
	for _, pname := range slices.Sorted(maps.Keys(cfg.Projects)) {
		if project != "" && pname != project {
			continue
		}
		p := cfg.Projects[pname]
		for _, sc := range p.Spec.Schedules {
			st := a.loadScheduleState(ctx, pname, sc.Name)
			view := ScheduleView{
				Project:       pname,
				Name:          sc.Name,
				Enabled:       sc.IsEnabled(),
				Every:         sc.Every.Duration.String(),
				Flow:          sc.Flow,
				StoryMap:      sc.StoryMap,
				AutoStart:     sc.AutoStart,
				MaxActiveRuns: sc.EffectiveMaxActiveRuns(),
				MaxOpenTasks:  sc.EffectiveMaxOpenTasks(),
				MaxOpenPRs:    sc.EffectiveMaxOpenPRs(),
				Paused:        a.schedulePaused(ctx, pname, sc.Name),
				State:         st,
			}
			if sc.Task != nil {
				view.TaskTitle = sc.Task.Title
			}
			if view.Enabled && !view.Paused {
				view.NextDueAt = max(st.nextDue(sc.Every.Duration), store.Now())
			}
			out = append(out, view)
		}
	}
	return out
}

// TriggerSchedule runs one configured schedule now. Unless forced it honors
// the backlog guards and the schedule's pause. A trigger the guards skip (or
// that fails) is tried again soon instead of losing its whole interval.
func (a *App) TriggerSchedule(ctx context.Context, project, name string, force bool) (*ScheduleState, error) {
	a.schedMu.Lock()
	defer a.schedMu.Unlock()
	if a.Cfg.Current().Projects[project] == nil {
		return nil, fmt.Errorf("unknown project %q", project)
	}
	sc := a.findSchedule(project, name)
	if sc == nil {
		return nil, fmt.Errorf("project %q has no schedule %q", project, name)
	}
	if !force && !a.scheduleLive(ctx, project, sc) {
		return nil, fmt.Errorf("schedule %q is paused or disabled", name)
	}
	prev := a.loadScheduleState(ctx, project, name)
	st, err := a.runSchedule(ctx, project, *sc, force)
	if err != nil {
		st = ScheduleState{LastStatus: "error", LastReason: err.Error()}
	}
	if st.LastRunAt == 0 {
		st.LastRunAt = prev.LastRunAt
		st.RetryAt = store.Now() + min(scheduleRetry, sc.Every.Duration).Milliseconds()
	}
	if st.LastRunID == "" && st.LastTaskID == "" {
		st.LastRunID, st.LastTaskID = prev.LastRunID, prev.LastTaskID
	}
	a.saveScheduleState(ctx, project, name, st)
	if a.Hub != nil {
		a.Hub.Publish(hub.Event{Type: "schedule", ID: project})
	}
	return &st, err
}

// runSchedule does one trigger's work. LastRunAt is set when the interval was
// used: work was queued, or there was none to queue.
func (a *App) runSchedule(ctx context.Context, project string, sc config.ProjectSchedule, force bool) (ScheduleState, error) {
	slots := math.MaxInt
	if !force {
		reason, free, err := a.scheduleBlockedReason(ctx, project, sc)
		if err != nil {
			return ScheduleState{}, err
		}
		if reason != "" {
			return ScheduleState{LastStatus: "skipped", LastReason: reason}, nil
		}
		slots = free
	}
	var rec *autoStart
	if sc.AutoStart {
		rec = &autoStart{Schedule: sc.Name, Force: force}
	}
	now := store.Now()
	switch {
	case strings.TrimSpace(sc.Flow) != "":
		if a.Engine == nil {
			return ScheduleState{}, fmt.Errorf("engine is not configured")
		}
		run, err := a.Engine.CreateRun(ctx, sc.Flow, 0, "")
		if err != nil {
			return ScheduleState{}, err
		}
		return ScheduleState{LastRunAt: now, LastStatus: "started", LastRunID: run.ID,
			LastReason: fmt.Sprintf("Started run %s of %s", run.ID, sc.Flow)}, nil
	case sc.StoryMap != nil:
		if rec == nil {
			slots = math.MaxInt
		}
		return a.runStoryMapSchedule(ctx, project, sc, rec, slots)
	case sc.Task != nil && strings.TrimSpace(sc.Task.Title) != "":
		t := &store.Task{
			ID:         ids.New("t"),
			Source:     "schedule",
			Identifier: sc.Name + "-" + time.UnixMilli(now).UTC().Format("2006-01-02"),
			Title:      sc.Task.Title,
			Body:       sc.Task.Body,
			Project:    project,
		}
		if err := a.createScheduledTask(ctx, t, rec); err != nil {
			return ScheduleState{}, err
		}
		return ScheduleState{LastRunAt: now, LastStatus: "started", LastTaskID: t.ID,
			LastReason: "Created scheduled task " + t.ID}, nil
	}
	return ScheduleState{}, fmt.Errorf("schedule %q has neither flow, task, nor storymap", sc.Name)
}

// createScheduledTask creates and plans t. The record goes first: planning may
// finish before the next write.
func (a *App) createScheduledTask(ctx context.Context, t *store.Task, rec *autoStart) error {
	if rec != nil {
		if err := a.Store.SetKV(ctx, kvTaskAutoStart(t.ID), rec.json()); err != nil {
			return err
		}
	}
	if err := a.CreateTask(ctx, t, true); err != nil {
		if rec != nil {
			_ = a.Store.WriteKV(ctx, nil, []string{kvTaskAutoStart(t.ID)})
		}
		return err
	}
	return nil
}

func (a *App) runStoryMapSchedule(ctx context.Context, project string, sc config.ProjectSchedule, rec *autoStart, slots int) (ScheduleState, error) {
	if a.Workspaces == nil {
		return ScheduleState{}, fmt.Errorf("product workspaces are not configured")
	}
	if a.GitHub != nil {
		if src, err := a.ProductSource(project); err == nil {
			pctx, cancel := context.WithTimeout(ctx, 45*time.Second)
			_, err := a.Workspaces.Ensure(pctx, project, src)
			if err == nil {
				_, err = a.Workspaces.Pull(pctx, project, src)
			}
			cancel()
			if err != nil {
				slog.Warn("storymap schedule: using the last pulled map", "project", project, "err", err)
			}
		}
	}
	workDir := a.Workspaces.WorkDir(project)
	var mapIDs []string
	if m := strings.TrimSpace(sc.StoryMap.Map); m != "" && m != "*" {
		mapIDs = []string{m}
	} else {
		sums, err := storymap.List(workDir)
		if err != nil {
			return ScheduleState{}, err
		}
		for _, s := range sums {
			if s.Error == "" {
				mapIDs = append(mapIDs, s.ID)
			}
		}
	}
	maxPick := min(sc.StoryMap.EffectiveMaxPerTick(), slots)
	now := store.Now()
	var created []*store.Task
	var picked []string
	for _, mapID := range mapIDs {
		if len(created) >= maxPick {
			break
		}
		loaded, err := storymap.Load(workDir, mapID)
		if err != nil {
			slog.Warn("storymap schedule load failed", "project", project, "map", mapID, "err", err)
			continue
		}
		works, err := a.MapWorks(ctx, project, mapID)
		if err != nil {
			return ScheduleState{}, err
		}
		handled, failed := cardHistory(works, sc.StoryMap.EffectiveMaxAttempts(), now-sc.Every.Duration.Milliseconds())
		for _, card := range selectNextStoryMapTasks(loaded, sc.StoryMap.Release, handled, sc.StoryMap.ShouldSkipOpenQuestions(), maxPick-len(created)) {
			t, err := a.sendToFlow(ctx, project, mapID, "task", card.ID, "", true, rec, retryNote(failed[card.ID]))
			if err != nil {
				return ScheduleState{}, err
			}
			created = append(created, t)
			picked = append(picked, mapID+"/"+card.ID)
		}
	}
	if len(created) == 0 {
		return ScheduleState{LastRunAt: now, LastStatus: "skipped", LastReason: "no ready todo user tasks in storymap"}, nil
	}
	return ScheduleState{LastRunAt: now, LastStatus: "started", LastTaskID: created[0].ID,
		LastReason: fmt.Sprintf("Queued %d storymap task(s): %s", len(created), strings.Join(picked, ", "))}, nil
}

// cardHistory sorts a map's cards by what earlier work on them means for the
// next pick. A card is handled while any work on it is open or succeeded, when
// a human canceled it, when its last failure is newer than retryBefore (leaving
// time to resume it), or after maxAttempts failures. failed holds the newest
// failure of every card.
func cardHistory(works []*MapWork, maxAttempts int, retryBefore int64) (handled map[string]bool, failed map[string]*MapWork) {
	handled, failed = map[string]bool{}, map[string]*MapWork{}
	attempts := map[string]int{}
	for _, w := range works { // newest first
		failure := w.Task.Status == store.TaskFailed || w.Task.Status == store.TaskPlanFailed
		if w.Run != nil && w.Run.Status == store.RunCanceled {
			failure = false
		}
		for _, id := range w.Scope.Tasks {
			if !failure || w.Task.UpdatedAt > retryBefore {
				handled[id] = true
				continue
			}
			if attempts[id]++; attempts[id] >= maxAttempts {
				handled[id] = true
			}
			if failed[id] == nil {
				failed[id] = w
			}
		}
	}
	return handled, failed
}

// retryNote tells the planner what went wrong the last time.
func retryNote(w *MapWork) string {
	if w == nil {
		return ""
	}
	why, where := w.Task.Error, ""
	if w.Run != nil {
		why = firstNonEmpty(w.Run.Error, why)
		where = fmt.Sprintf(" (run %s)", w.Run.ID)
	}
	return fmt.Sprintf("\n\n## Earlier attempt\n\nAn earlier attempt at this failed%s: %s\n\nPlan around that failure. Start from the base branch; the earlier branch is not merged.\n",
		where, firstNonEmpty(clip(strings.TrimSpace(why), 600), "no reason recorded"))
}

// selectNextStoryMapTasks picks up to limit ready "todo" user tasks ordered by
// release slice (in map.Releases order), then journey backbone order, then card order.
func selectNextStoryMapTasks(l *storymap.Loaded, releaseFilter string, handled map[string]bool, skipOpenQuestions bool, limit int) []storymap.Task {
	if l == nil || limit <= 0 {
		return nil
	}
	var releases []string
	if releaseFilter != "" {
		releases = []string{releaseFilter}
	} else {
		for _, r := range l.Map.Releases {
			if r.Status == "released" {
				continue
			}
			releases = append(releases, r.ID)
		}
		releases = append(releases, "")
	}
	actRank := map[string]int{}
	idx := 0
	for _, ph := range l.Map.Journey {
		for _, act := range ph.Activities {
			actRank[act.ID] = idx
			idx++
		}
	}
	var out []storymap.Task
	for _, rel := range releases {
		var sliceCards []storymap.Task
		for _, t := range l.Tasks {
			if t.Release != rel {
				continue
			}
			if t.Status != "" && t.Status != "todo" {
				continue
			}
			if handled[t.ID] {
				continue
			}
			if skipOpenQuestions && len(t.OpenQuestions) > 0 {
				continue
			}
			sliceCards = append(sliceCards, t)
		}
		slices.SortStableFunc(sliceCards, func(a, b storymap.Task) int {
			ra, okA := actRank[a.Activity]
			rb, okB := actRank[b.Activity]
			if !okA {
				ra = 1 << 20
			}
			if !okB {
				rb = 1 << 20
			}
			if ra != rb {
				return ra - rb
			}
			return a.Order - b.Order
		})
		for _, card := range sliceCards {
			out = append(out, card)
			if len(out) >= limit {
				return out
			}
		}
	}
	return out
}

// queuedTask is a task waiting to start its own run.
type queuedTask struct {
	task  *store.Task // with the process-local planning state applied
	rec   *autoStart
	sched *config.ProjectSchedule
}

// queuedTasks lists the project's tasks that will start their own run, oldest
// first: being planned, ready, or with planning interrupted by a restart.
func (a *App) queuedTasks(ctx context.Context, project string) ([]queuedTask, error) {
	tasks, err := a.Store.ListTasks(ctx)
	if err != nil {
		return nil, err
	}
	var out []queuedTask
	for _, t := range tasks {
		if t.Project != project {
			continue
		}
		state, planning := a.TaskState(t)
		if !planning && state.Status != store.TaskFlowReady && state.Status != TaskPlanInterrupted {
			continue
		}
		rec, err := a.autoStartRecord(ctx, t.ID)
		if err != nil {
			return nil, err
		}
		if rec == nil || (state.Status == TaskPlanInterrupted && rec.Replanned) {
			continue
		}
		sched := a.findSchedule(project, rec.Schedule)
		if !rec.Force && !a.scheduleLive(ctx, project, sched) {
			continue
		}
		runs, err := a.Store.ListRuns(ctx, store.RunFilter{TaskID: t.ID, Limit: 1})
		if err != nil {
			return nil, err
		}
		if len(runs) == 0 {
			out = append(out, queuedTask{state, rec, sched})
		}
	}
	slices.SortStableFunc(out, func(x, y queuedTask) int { return cmp.Compare(x.task.CreatedAt, y.task.CreatedAt) })
	return out, nil
}

func (a *App) activeRuns(ctx context.Context, project string) (int, error) {
	runs, err := a.Store.ListRuns(ctx, store.RunFilter{
		Project:  project,
		Statuses: []string{store.RunQueued, store.RunRunning, store.RunWaiting},
		Limit:    100,
	})
	return len(runs), err
}

// openPRs counts the project's unmerged ai-flow pull requests.
func (a *App) openPRs(ctx context.Context, project string) (int, error) {
	if a.GitHub == nil {
		return 0, nil
	}
	src, err := a.ProductSource(project)
	if err != nil {
		return 0, nil // not on GitHub, so ai-flow opens no pull requests there
	}
	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	heads, err := a.GitHub.OpenPRHeads(pctx, src.Repo)
	if err != nil {
		return 0, fmt.Errorf("listing open pull requests: %w", err)
	}
	n := 0
	for _, h := range heads {
		if strings.HasPrefix(h, "ai-flow/") {
			n++
		}
	}
	return n, nil
}

// scheduleBlockedReason says why sc may not queue more work, or how many more
// runs it may queue. Tasks that will start their own run count as active, so
// two schedules due together cannot both take the last slot; tasks that wait
// for a human to start them do not count at all.
func (a *App) scheduleBlockedReason(ctx context.Context, project string, sc config.ProjectSchedule) (string, int, error) {
	active, err := a.activeRuns(ctx, project)
	if err != nil {
		return "", 0, err
	}
	queued, err := a.queuedTasks(ctx, project)
	if err != nil {
		return "", 0, err
	}
	load, limit := active+len(queued), sc.EffectiveMaxActiveRuns()
	if load >= limit {
		return fmt.Sprintf("%d run(s) active and %d queued in project %s (max %d)", active, len(queued), project, limit), 0, nil
	}
	isQueued := map[string]bool{}
	for _, q := range queued {
		isQueued[q.task.ID] = true
	}
	tasks, err := a.Store.ListTasks(ctx)
	if err != nil {
		return "", 0, err
	}
	open := 0
	for _, t := range tasks {
		if t.Project != project {
			continue
		}
		state, planning := a.TaskState(t)
		if planning || isQueued[t.ID] || state.Status == store.TaskRunning {
			open++
		}
	}
	if max := sc.EffectiveMaxOpenTasks(); open >= max {
		return fmt.Sprintf("%d open task(s) already in project %s (max %d)", open, project, max), 0, nil
	}
	prs, err := a.openPRs(ctx, project)
	if err != nil {
		return "", 0, err
	}
	if max := sc.EffectiveMaxOpenPRs(); prs >= max {
		return fmt.Sprintf("%d ai-flow pull request(s) wait to be merged or closed (max %d)", prs, max), 0, nil
	}
	return "", limit - load, nil
}

// startQueued starts the runs the project's queued tasks are waiting for, as
// far as their schedules' limits allow.
func (a *App) startQueued(ctx context.Context, project string) error {
	a.schedMu.Lock()
	defer a.schedMu.Unlock()
	queued, err := a.queuedTasks(ctx, project)
	if err != nil || len(queued) == 0 {
		return err
	}
	active, err := a.activeRuns(ctx, project)
	if err != nil {
		return err
	}
	prs := -1 // not counted yet
	for _, q := range queued {
		if q.task.Status == TaskPlanInterrupted {
			q.rec.Replanned = true
			if err := a.Store.SetKV(ctx, kvTaskAutoStart(q.task.ID), q.rec.json()); err != nil {
				return err
			}
			a.PlanTask(q.task.ID)
			continue
		}
		if q.task.Status != store.TaskFlowReady {
			continue
		}
		if !q.rec.Force {
			if active >= q.sched.EffectiveMaxActiveRuns() {
				continue
			}
			if prs < 0 {
				if prs, err = a.openPRs(ctx, project); err != nil {
					return err
				}
			}
			if prs >= q.sched.EffectiveMaxOpenPRs() {
				continue
			}
		}
		if _, err := a.Engine.CreateRun(ctx, q.task.FlowName, 0, q.task.ID); err != nil {
			msg := "auto-start: " + err.Error()
			a.Store.UpdateTask(ctx, q.task.ID, map[string]any{"status": store.TaskPlanFailed, "error": msg})
			a.Store.AddEvent(ctx, &store.Event{TaskID: q.task.ID, FlowName: q.task.FlowName, Kind: "plan", Message: msg})
			a.Hub.Publish(hub.Event{Type: "task", ID: q.task.ID})
			continue
		}
		active++
	}
	return nil
}

// RunScheduleLoop starts queued runs, triggers due schedules and follows up on
// delivered pull requests.
func (a *App) RunScheduleLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for n := 0; ; n++ {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.tickSchedules(ctx)
			if n%10 == 0 {
				a.reconcileLedgers(ctx)
			}
		}
	}
}

func (a *App) tickSchedules(ctx context.Context) {
	cfg := a.Cfg.Current()
	now := store.Now()
	for _, pname := range slices.Sorted(maps.Keys(cfg.Projects)) {
		scheds := slices.Clone(cfg.Projects[pname].Spec.Schedules)
		if len(scheds) == 0 {
			continue
		}
		if err := a.startQueued(ctx, pname); err != nil {
			slog.Warn("start queued runs", "project", pname, "err", err)
		}
		last := map[string]int64{}
		due := scheds[:0]
		for _, sc := range scheds {
			st := a.loadScheduleState(ctx, pname, sc.Name)
			if !a.scheduleLive(ctx, pname, &sc) || now < st.nextDue(sc.Every.Duration) {
				continue
			}
			last[sc.Name] = st.LastRunAt
			due = append(due, sc)
		}
		// The schedule that has waited longest gets the free slot.
		slices.SortStableFunc(due, func(x, y config.ProjectSchedule) int { return cmp.Compare(last[x.Name], last[y.Name]) })
		for _, sc := range due {
			res, err := a.TriggerSchedule(ctx, pname, sc.Name, false)
			if err != nil {
				slog.Warn("scheduled trigger failed", "project", pname, "schedule", sc.Name, "err", err)
			} else if res.LastStatus == "started" {
				slog.Info("scheduled trigger started", "project", pname, "schedule", sc.Name, "run", res.LastRunID, "task", res.LastTaskID)
			}
		}
	}
}

// reconcileLedgers settles ledger entries whose pull request was merged
// (shipped) or closed (rejected) since.
func (a *App) reconcileLedgers(ctx context.Context) {
	if a.Engine == nil {
		return
	}
	for _, project := range slices.Sorted(maps.Keys(a.Cfg.Current().Projects)) {
		for _, it := range a.Engine.Ledger(ctx, project) {
			if it.Status != engine.LedgerPROpen {
				continue
			}
			switch a.prOutcome(ctx, it.PRURL) {
			case "merged":
				it.Status, it.Summary = engine.LedgerShipped, "Pull request merged"
			case "closed":
				it.Status, it.Summary = engine.LedgerRejected, "Pull request closed without merging"
			default:
				continue
			}
			it.UpdatedAt = 0 // now
			if err := a.Engine.UpsertLedger(ctx, project, it); err != nil {
				slog.Warn("reconcile ledger", "project", project, "key", it.Key, "err", err)
			}
		}
	}
}

// maxChildAttempts bounds how often a failed or reverted improvement may be
// proposed again.
const maxChildAttempts = 3

// CreateTaskFromFlow creates a child task for a flow's create_tasks action. A
// key already used in the project returns created=false with that task's ID,
// unless the earlier task failed or its change was reverted. The child of a
// scheduled task starts on its own under the same schedule.
func (a *App) CreateTaskFromFlow(ctx context.Context, c engine.ChildTask) (taskID string, created bool, err error) {
	source := firstNonEmpty(c.Source, "flow")
	var externalID string
	for n := 1; c.Key != ""; n++ {
		externalID = c.Project + "/" + c.Key
		if n > 1 {
			externalID += fmt.Sprintf("#%d", n)
		}
		prev, err := a.Store.TaskByExternal(ctx, source, externalID)
		if errors.Is(err, store.ErrNotFound) {
			break
		}
		if err != nil {
			return "", false, err
		}
		again, err := a.mayRepropose(ctx, prev, c)
		if err != nil {
			return "", false, err
		}
		if !again || n >= maxChildAttempts {
			return prev.ID, false, nil
		}
	}
	t := &store.Task{
		ID:         ids.New("t"),
		Source:     source,
		ExternalID: externalID,
		Identifier: c.Key,
		Title:      strings.TrimSpace(c.Title),
		Body:       c.Body,
		Project:    c.Project,
	}
	var rec *autoStart
	if c.Plan && c.ParentTaskID != "" {
		parent, err := a.autoStartRecord(ctx, c.ParentTaskID)
		if err != nil {
			return "", false, err
		}
		if parent != nil {
			rec = &autoStart{Schedule: parent.Schedule}
		}
	}
	if rec != nil {
		err = a.createScheduledTask(ctx, t, rec)
	} else {
		err = a.CreateTask(ctx, t, c.Plan)
	}
	if err != nil {
		return "", false, err
	}
	return t.ID, true, nil
}

func (a *App) mayRepropose(ctx context.Context, prev *store.Task, c engine.ChildTask) (bool, error) {
	switch prev.Status {
	case store.TaskFailed, store.TaskPlanFailed:
		runs, err := a.Store.ListRuns(ctx, store.RunFilter{TaskID: prev.ID, Limit: 1})
		if err != nil {
			return false, err
		}
		return len(runs) == 0 || runs[0].Status != store.RunCanceled, nil
	case store.TaskSucceeded:
		if a.Engine == nil {
			return false, nil
		}
		for _, it := range a.Engine.Ledger(ctx, c.Project) {
			if it.Key == c.Key {
				return it.Status == engine.LedgerReverted, nil
			}
		}
	}
	return false, nil
}
