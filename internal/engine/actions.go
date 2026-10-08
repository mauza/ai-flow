package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/github"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/resolve"
	"github.com/mauza/ai-flow/internal/store"
)

// execAction runs an action visit. A waiting action that is not finished
// records its progress and stays running; the engine polls it again on a
// later tick (at most every PollInterval) until it finishes or its deadline
// passes, which emits `timeout`.
func (e *Engine) execAction(ctx context.Context, r *store.Run, res *resolve.Resolved, n *resolve.Node, v *store.Visit) error {
	spec := flow.Actions[n.Action]
	key := fmt.Sprintf("%s/%d", r.ID, v.Seq)
	if spec.Waits && v.Status == store.VisitRunning {
		if v.Deadline > 0 && store.Now() > v.Deadline {
			e.forget(key)
			return e.finishAction(ctx, r, v, &actionResult{outcome: flow.OutcomeTimeout,
				summary: fmt.Sprintf("Gave up after %s: %s", shortDuration(n.Timeout.Duration), strings.TrimSpace(v.Progress))})
		}
		if last, ok := e.polls[key]; ok && time.Since(last) < e.PollInterval {
			return nil
		}
	}
	e.polls[key] = time.Now()
	out, err := e.runAction(ctx, r, res, n, v)
	if err != nil {
		return err
	}
	if out.waiting {
		fields := map[string]any{"progress": out.summary}
		if v.Deadline == 0 {
			fields["deadline"] = time.Now().Add(n.Timeout.Duration).UnixMilli()
		}
		if out.summary != v.Progress {
			e.hub.Publish(hub.Event{Type: "progress", ID: r.ID, Seq: v.Seq, Text: out.summary})
		}
		return e.store.UpdateVisit(ctx, r.ID, v.Seq, fields)
	}
	e.forget(key)
	return e.finishAction(ctx, r, v, out)
}

func (e *Engine) forget(key string) {
	delete(e.polls, key)
	delete(e.health, key)
	delete(e.passes, key)
}

func (e *Engine) finishAction(ctx context.Context, r *store.Run, v *store.Visit, out *actionResult) error {
	outputs, err := json.Marshal(out.outputs)
	if err != nil {
		return err
	}
	if out.outputs == nil {
		outputs = []byte("{}")
	}
	if err := e.store.UpdateVisit(ctx, r.ID, v.Seq, map[string]any{"status": store.VisitSucceeded, "outcome": out.outcome, "summary": out.summary, "outputs": string(outputs), "finished_at": store.Now()}); err != nil {
		return err
	}
	v.Status, v.Outcome = store.VisitSucceeded, out.outcome
	return e.advance(ctx, r, v)
}

func waiting(format string, a ...any) *actionResult {
	return &actionResult{waiting: true, summary: fmt.Sprintf(format, a...)}
}

func (e *Engine) repoOf(res *resolve.Resolved) (github.Repo, error) {
	if e.gh == nil {
		return github.Repo{}, rejectf("github is not configured")
	}
	g := e.cfg.Current().Catalog.Grants[res.Repo]
	if g == nil {
		return github.Repo{}, rejectf("flow has no repo")
	}
	repo, err := github.ParseRepoURL(g.URL)
	if err != nil {
		return github.Repo{}, rejection{err}
	}
	return repo, nil
}

var prNumberRe = regexp.MustCompile(`/pull/(\d+)$`)

// mergePR merges the run's pull request. It is idempotent: a PR that is
// already merged (say, before a restart) reports its merge commit.
func (e *Engine) mergePR(ctx context.Context, r *store.Run, res *resolve.Resolved, with map[string]string) (*actionResult, error) {
	repo, err := e.repoOf(res)
	if err != nil {
		return nil, err
	}
	m := prNumberRe.FindStringSubmatch(strings.TrimSuffix(r.PRURL, "/"))
	if m == nil {
		return nil, rejectf("the run has no pull request to merge; open one first (open_pull_request)")
	}
	number, _ := strconv.Atoi(m[1])
	method := with["method"]
	if method == "" {
		method = "merge"
	}
	if method != "merge" && method != "squash" && method != "rebase" {
		return nil, rejectf("merge method %q (merge, squash or rebase)", method)
	}
	pr, err := e.gh.PR(ctx, repo, number)
	if err != nil {
		return nil, err
	}
	merged := func(sha string) *actionResult {
		out := map[string]any{"sha": sha, "number": number, "url": r.PRURL}
		// The merge's first parent is what the deploy branch held (and ran)
		// before this release: the target of a rollback.
		if prev, err := e.gh.FirstParent(ctx, repo, sha); err == nil {
			out["previous_sha"] = prev
		} else {
			slog.Warn("merge parent", "run", r.ID, "sha", sha, "err", err)
		}
		return &actionResult{outcome: "merged", summary: fmt.Sprintf("Merged #%d as %s", number, short(sha)), outputs: out}
	}
	switch {
	case pr.Merged:
		return merged(pr.MergeCommitSHA), nil
	case pr.State != "open":
		return &actionResult{outcome: "blocked", summary: fmt.Sprintf("#%d is closed without being merged", number)}, nil
	case pr.Mergeable == nil:
		return waiting("GitHub is still computing whether #%d can merge", number), nil
	case !*pr.Mergeable:
		return &actionResult{outcome: "conflict", summary: fmt.Sprintf("#%d has merge conflicts with its base (%s)", number, pr.MergeableState)}, nil
	case pr.MergeableState == "blocked" || pr.MergeableState == "behind" || pr.MergeableState == "draft":
		return &actionResult{outcome: "blocked", summary: fmt.Sprintf("GitHub will not merge #%d yet: %s (required reviews, checks or an out-of-date branch)", number, pr.MergeableState)}, nil
	}
	sha, err := e.gh.MergePR(ctx, repo, number, method, pr.Head.SHA)
	var api *github.APIError
	if errors.As(err, &api) {
		switch api.Status {
		case 409:
			return waiting("#%d changed while merging; retrying", number), nil
		case 405, 422:
			return &actionResult{outcome: "blocked", summary: fmt.Sprintf("GitHub refused to merge #%d: %s", number, strings.TrimSpace(api.Body))}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return merged(sha), nil
}

// waitForChecks waits for every CI check reported on a commit.
func (e *Engine) waitForChecks(ctx context.Context, r *store.Run, res *resolve.Resolved, v *store.Visit, with map[string]string) (*actionResult, error) {
	repo, err := e.repoOf(res)
	if err != nil {
		return nil, err
	}
	ref := strings.TrimSpace(with["ref"])
	if ref == "" {
		head, err := e.gh.BranchHead(ctx, repo, r.Branch)
		var api *github.APIError
		if errors.As(err, &api) && api.Status == 404 {
			return nil, rejectf("run branch %s has no commits to check", r.Branch)
		}
		if err != nil {
			return nil, err
		}
		ref = head
	}
	settle := 2 * time.Minute
	if s := strings.TrimSpace(with["settle"]); s != "" {
		var d flow.Duration
		if err := d.UnmarshalJSON([]byte(strconv.Quote(s))); err != nil {
			return nil, rejectf("settle: %v", err)
		}
		settle = d.Duration
	}
	want := fieldsOf(with["checks"])
	all, err := e.gh.Checks(ctx, repo, ref)
	if err != nil {
		return nil, err
	}
	var checks []github.Check
	seen := map[string]bool{}
	for _, c := range all {
		if len(want) == 0 || contains(want, c.Name) {
			checks = append(checks, c)
			seen[c.Name] = true
		}
	}
	missing := 0
	for _, w := range want {
		if !seen[w] {
			missing++
		}
	}
	outputs := map[string]any{"ref": ref, "failed": []string{}, "checks": []string{}}
	if len(checks) == 0 || missing > 0 {
		started := time.UnixMilli(v.StartedAt)
		if v.StartedAt > 0 && time.Since(started) >= settle {
			if len(want) > 0 {
				return &actionResult{outcome: "failed", summary: fmt.Sprintf("Required checks never reported on %s: %s", short(ref), strings.Join(want, ", ")),
					outputs: map[string]any{"ref": ref, "failed": want, "checks": []string{}}}, nil
			}
			return &actionResult{outcome: "none", summary: fmt.Sprintf("No CI reported on %s within %s", short(ref), shortDuration(settle)), outputs: outputs}, nil
		}
		return waiting("Waiting for CI to report on %s", short(ref)), nil
	}
	var failed, names []string
	done := 0
	for _, c := range checks {
		names = append(names, c.Name+": "+c.Note)
		if c.Done {
			done++
			if !c.OK {
				failed = append(failed, c.Name)
			}
		}
	}
	if done < len(checks) {
		return waiting("CI on %s: %d/%d checks finished", short(ref), done, len(checks)), nil
	}
	outputs["checks"] = names
	if len(failed) > 0 {
		outputs["failed"] = failed
		return &actionResult{outcome: "failed", summary: fmt.Sprintf("CI failed on %s: %s", short(ref), strings.Join(failed, ", ")), outputs: outputs}, nil
	}
	return &actionResult{outcome: "passed", summary: fmt.Sprintf("All %d checks passed on %s", len(checks), short(ref)), outputs: outputs}, nil
}

// waitForDeploy waits until every version endpoint reports the expected commit.
func (e *Engine) waitForDeploy(ctx context.Context, r *store.Run, res *resolve.Resolved, with map[string]string) (*actionResult, error) {
	d, err := e.deployOf(r)
	if err != nil {
		return nil, err
	}
	if len(d.Versions) == 0 {
		return nil, rejectf("project %s has no deploy.versions to check", r.Project)
	}
	expect := strings.ToLower(strings.TrimSpace(with["expect"]))
	if expect == "" {
		if expect, err = e.lastMerge(ctx, r, res); err != nil {
			return nil, err
		}
	}
	var pending []string
	var version string
	for _, p := range d.Versions {
		got, err := e.probeVersion(ctx, p)
		if err != nil {
			pending = append(pending, fmt.Sprintf("%s: %v", p.URL, err))
			continue
		}
		version = got
		if !sameCommit(got, expect) {
			pending = append(pending, fmt.Sprintf("%s reports %s", p.URL, short(got)))
		}
	}
	if len(pending) > 0 {
		e.refreshArgo(ctx, d)
		return waiting("Waiting for %s to go live; %s", short(expect), strings.Join(pending, "; ")), nil
	}
	return &actionResult{outcome: "deployed", summary: fmt.Sprintf("%s is live on every version endpoint", short(expect)), outputs: map[string]any{"version": version}}, nil
}

// lastMerge is the merge commit of the run's latest merge_pull_request.
func (e *Engine) lastMerge(ctx context.Context, r *store.Run, res *resolve.Resolved) (string, error) {
	visits, err := e.store.Visits(ctx, r.ID)
	if err != nil {
		return "", err
	}
	for i := len(visits) - 1; i >= 0; i-- {
		v := visits[i]
		if n := res.Nodes[v.Node]; n != nil && n.Action == "merge_pull_request" && v.Outcome == "merged" {
			var out struct{ SHA string }
			json.Unmarshal(v.Outputs, &out)
			if out.SHA != "" {
				return strings.ToLower(out.SHA), nil
			}
		}
	}
	return "", rejectf("nothing merged in this run yet; set with.expect")
}

func (e *Engine) probeVersion(ctx context.Context, p config.VersionProbe) (string, error) {
	body, err := e.get(ctx, p.URL)
	if err != nil {
		return "", err
	}
	if p.Field == "" {
		return strings.ToLower(strings.TrimSpace(string(body))), nil
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("not JSON")
	}
	s, _ := doc[p.Field].(string)
	if s == "" {
		return "", fmt.Errorf("no %q field", p.Field)
	}
	return strings.ToLower(strings.TrimSpace(s)), nil
}

// checkHealth soaks the deployment: each poll runs every health query and
// probes the deploy URL. A check that fails twice in a row is degraded. Query
// errors are inconclusive (a metrics outage must not roll a deploy back); the
// app is healthy only once the soak has passed and every check has been
// evaluated successfully at least twice.
func (e *Engine) checkHealth(ctx context.Context, r *store.Run, v *store.Visit, with map[string]string) (*actionResult, error) {
	d, err := e.deployOf(r)
	if err != nil {
		return nil, err
	}
	soak := 10 * time.Minute
	if s := strings.TrimSpace(with["duration"]); s != "" {
		var dur flow.Duration
		if err := dur.UnmarshalJSON([]byte(strconv.Quote(s))); err != nil {
			return nil, rejectf("duration: %v", err)
		}
		soak = dur.Duration
	}
	key := fmt.Sprintf("%s/%d", r.ID, v.Seq)
	if e.health[key] == nil {
		e.health[key], e.passes[key] = map[string]int{}, map[string]int{}
	}
	streak, passes := e.health[key], e.passes[key]
	var now, unknown []string
	record := func(name string, bad bool, detail string) {
		passes[name]++
		if bad {
			streak[name]++
			now = append(now, fmt.Sprintf("%s: %s", name, detail))
		} else {
			streak[name] = 0
		}
	}
	for _, h := range d.Health {
		val, err := e.promQuery(ctx, h.Query)
		if err != nil {
			unknown = append(unknown, fmt.Sprintf("%s: %v", h.Name, err))
			continue
		}
		record(h.Name, val > h.Max, fmt.Sprintf("%.4g (max %.4g)", val, h.Max))
	}
	if d.URL != "" {
		_, err := e.get(ctx, d.URL)
		record("http "+d.URL, err != nil, fmt.Sprint(err))
	}
	var degraded []string
	for name, n := range streak {
		if n >= 2 {
			degraded = append(degraded, name)
		}
	}
	if len(degraded) > 0 {
		return &actionResult{outcome: "degraded", summary: "Degraded: " + strings.Join(now, "; "),
			outputs: map[string]any{"violations": now, "polls": maxCount(passes)}}, nil
	}
	elapsed := time.Since(time.UnixMilli(v.StartedAt))
	confirmed := true
	for _, h := range d.Health {
		confirmed = confirmed && passes[h.Name] >= 2
	}
	if v.StartedAt > 0 && elapsed >= soak && confirmed {
		return &actionResult{outcome: "healthy", summary: fmt.Sprintf("Healthy for %s across %d checks", shortDuration(soak), len(passes)),
			outputs: map[string]any{"violations": []string{}, "polls": maxCount(passes)}}, nil
	}
	status := "all checks ok"
	if len(now) > 0 {
		status = "watching " + strings.Join(now, "; ")
	}
	if len(unknown) > 0 {
		status += "; inconclusive " + strings.Join(unknown, "; ")
	}
	return waiting("Soaking %s/%s: %s", shortDuration(elapsed.Truncate(time.Minute)), shortDuration(soak), status), nil
}

func maxCount(m map[string]int) int {
	n := 0
	for _, v := range m {
		n = max(n, v)
	}
	return n
}

func (e *Engine) deployOf(r *store.Run) (*config.Deploy, error) {
	p := e.cfg.Current().Projects[r.Project]
	if p == nil || p.Spec.Deploy == nil {
		return nil, rejectf("project %q has no deploy configuration", r.Project)
	}
	return p.Spec.Deploy, nil
}

// promQuery runs a PromQL instant query and returns the highest value (0 for
// an empty result).
func (e *Engine) promQuery(ctx context.Context, q string) (float64, error) {
	base := strings.TrimSuffix(e.cfg.Current().Env.Metrics.URL, "/")
	if base == "" {
		return 0, fmt.Errorf("metrics.url is not configured")
	}
	body, err := e.get(ctx, base+"/api/v1/query?query="+url.QueryEscape(q))
	if err != nil {
		return 0, err
	}
	var resp struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string          `json:"resultType"`
			Result     json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, fmt.Errorf("bad response")
	}
	if resp.Status != "success" {
		return 0, fmt.Errorf("%s", resp.Error)
	}
	var values [][2]any
	switch resp.Data.ResultType {
	case "vector":
		var series []struct {
			Value [2]any `json:"value"`
		}
		json.Unmarshal(resp.Data.Result, &series)
		for _, s := range series {
			values = append(values, s.Value)
		}
	case "scalar":
		var v [2]any
		json.Unmarshal(resp.Data.Result, &v)
		values = append(values, v)
	default:
		return 0, fmt.Errorf("unsupported result type %q", resp.Data.ResultType)
	}
	best := 0.0
	for i, v := range values {
		s, _ := v[1].(string)
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(f) {
			continue
		}
		if i == 0 || f > best {
			best = f
		}
	}
	return best, nil
}

func (e *Engine) get(ctx context.Context, u string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := e.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return body, nil
}

func sameCommit(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	if len(a) < 7 || len(b) < 7 {
		return a == b
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func fieldsOf(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' })
}

// refreshArgo asks Argo CD to re-read the project's app now rather than at its
// next git poll, so a write-back (deploy or rollback) rolls out sooner.
func (e *Engine) refreshArgo(ctx context.Context, d *config.Deploy) {
	if e.refresher == nil || d.ArgoCDApp == "" {
		return
	}
	if err := e.refresher(ctx, d.ArgoCDNamespace, d.ArgoCDApp); err != nil {
		slog.Warn("argo refresh", "app", d.ArgoCDApp, "err", err)
	}
}

// rollbackDeploy dispatches the project's rollback workflow for a release and
// waits for it. The dispatch is recorded in kv first-thing after it succeeds,
// so polls and restarts never dispatch twice.
func (e *Engine) rollbackDeploy(ctx context.Context, r *store.Run, res *resolve.Resolved, v *store.Visit, with map[string]string) (*actionResult, error) {
	d, err := e.deployOf(r)
	if err != nil {
		return nil, err
	}
	if d.Rollback == nil {
		return nil, rejectf("project %s has no deploy.rollback workflow", r.Project)
	}
	repo, err := e.repoOf(res)
	if err != nil {
		return nil, err
	}
	sha := strings.ToLower(strings.TrimSpace(with["sha"]))
	if sha == "" {
		if sha, err = e.lastMerge(ctx, r, res); err != nil {
			return nil, err
		}
	}
	previous := e.lastPrevious(ctx, r, res)
	key := fmt.Sprintf("dispatch/%s/%d", r.ID, v.Seq)
	state, err := e.store.GetKV(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		at := time.Now()
		err := e.gh.DispatchWorkflow(ctx, repo, d.Rollback.Workflow, d.Branch, map[string]string{"sha": sha})
		var api *github.APIError
		if errors.As(err, &api) && api.Status >= 400 && api.Status < 500 {
			return &actionResult{outcome: "failed", summary: fmt.Sprintf("GitHub refused to start %s: %s", d.Rollback.Workflow, strings.TrimSpace(api.Body))}, nil
		}
		if err != nil {
			return nil, err
		}
		if err := e.store.SetKV(ctx, key, strconv.FormatInt(at.UnixMilli(), 10)); err != nil {
			return nil, err
		}
		return waiting("Dispatched %s to roll back %s", d.Rollback.Workflow, short(sha)), nil
	}
	if err != nil {
		return nil, err
	}
	at, runID := state, int64(0)
	if i := strings.IndexByte(state, ':'); i >= 0 {
		at = state[:i]
		runID, _ = strconv.ParseInt(state[i+1:], 10, 64)
	}
	ms, _ := strconv.ParseInt(at, 10, 64)
	if runID == 0 {
		runs, err := e.gh.DispatchedRuns(ctx, repo, d.Rollback.Workflow, d.Branch)
		if err != nil {
			return nil, err
		}
		// The oldest run created after the dispatch (allowing for clock skew).
		since := time.UnixMilli(ms).Add(-15 * time.Second)
		for _, run := range runs {
			if !run.CreatedAt.Before(since) && (runID == 0 || run.ID < runID) {
				runID = run.ID
			}
		}
		if runID == 0 {
			return waiting("Waiting for GitHub to start %s", d.Rollback.Workflow), nil
		}
		if err := e.store.SetKV(ctx, key, fmt.Sprintf("%d:%d", ms, runID)); err != nil {
			return nil, err
		}
	}
	run, err := e.gh.Run(ctx, repo, runID)
	if err != nil {
		return nil, err
	}
	outputs := map[string]any{"url": run.HTMLURL, "previous": previous}
	if run.Status != "completed" {
		return waiting("%s %s (%s)", d.Rollback.Workflow, strings.ReplaceAll(run.Status, "_", " "), run.HTMLURL), nil
	}
	if run.Conclusion != "success" {
		return &actionResult{outcome: "failed", summary: fmt.Sprintf("%s ended %s: %s", d.Rollback.Workflow, run.Conclusion, run.HTMLURL), outputs: outputs}, nil
	}
	return &actionResult{outcome: "rolled_back", summary: fmt.Sprintf("Rolled back %s; %s should go live again", short(sha), short(previous)), outputs: outputs}, nil
}

// lastPrevious is the previous_sha of the run's latest merge ("" if unknown).
func (e *Engine) lastPrevious(ctx context.Context, r *store.Run, res *resolve.Resolved) string {
	visits, err := e.store.Visits(ctx, r.ID)
	if err != nil {
		return ""
	}
	for i := len(visits) - 1; i >= 0; i-- {
		v := visits[i]
		if n := res.Nodes[v.Node]; n != nil && n.Action == "merge_pull_request" && v.Outcome == "merged" {
			var out struct {
				Previous string `json:"previous_sha"`
			}
			json.Unmarshal(v.Outputs, &out)
			return out.Previous
		}
	}
	return ""
}
