package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/github"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/metrics"
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
		// The deadline counts from the start: an action whose polls keep
		// erroring never records one, and must still time out.
		deadline := v.Deadline
		if deadline == 0 && v.StartedAt > 0 {
			deadline = v.StartedAt + n.Timeout.Duration.Milliseconds()
		}
		if deadline > 0 && store.Now() > deadline {
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
			fields["deadline"] = v.StartedAt + n.Timeout.Duration.Milliseconds()
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
	delete(e.badSince, key)
	delete(e.passes, key)
}

// forgetRun drops the poll state of every waiting action of a finished run
// (one canceled or failed mid-wait never reaches forget).
func (e *Engine) forgetRun(runID string) {
	prefix := runID + "/"
	for k := range e.polls {
		if strings.HasPrefix(k, prefix) {
			e.forget(k)
		}
	}
	for k := range e.health {
		if strings.HasPrefix(k, prefix) {
			e.forget(k)
		}
	}
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

// releaseInFlight names another run of the same project that merged into the
// deploy branch and is still verifying that release (waiting for the rollout,
// soaking, rolling back). Merging now would put two releases in production at
// once: on web-games three runs merged within 25 minutes, each soak read the
// next rollout as its own release failing, and their rollbacks undid each
// other. Runs waiting at a gate don't hold the deploy target.
func (e *Engine) releaseInFlight(ctx context.Context, r *store.Run) (string, error) {
	d, err := e.deployOf(r, nil)
	if err != nil || d.Branch != r.Base {
		return "", nil // merging this run does not deploy
	}
	runs, err := e.store.ListRuns(ctx, store.RunFilter{Statuses: []string{store.RunRunning}, Limit: -1})
	if err != nil {
		return "", err
	}
	for _, o := range runs {
		if o.ID == r.ID || o.Project != r.Project || o.Base != r.Base {
			continue
		}
		res, err := e.Resolved(ctx, o)
		if err != nil {
			continue // that run fails on its own
		}
		if sha, err := e.lastMerge(ctx, o, res); err == nil && sha != "" {
			return fmt.Sprintf("run %s's release %s is still being verified (%s)", o.ID, short(sha), o.CurrentNode), nil
		}
	}
	return "", nil
}

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
	// Merge only the commit CI passed on: a push after the pass (or a check
	// of some other ref) is untested. Conflict routes back through CI.
	if passed, err := e.ciPassedRef(ctx, r, res); err != nil {
		return nil, err
	} else if _, deployErr := e.deployOf(r, res); passed != pr.Head.SHA && (passed != "" || deployErr == nil) {
		// Without a deploy config (no CI required by validation) a run
		// that never ran CI may still merge.
		what := "no CI pass is recorded in this run"
		if passed != "" {
			what = "CI passed on " + short(passed)
		}
		return &actionResult{outcome: "conflict", summary: fmt.Sprintf(
			"#%d head is %s but %s: run CI on the current head before merging", number, short(pr.Head.SHA), what)}, nil
	}
	if why, err := e.mergeRefused(ctx, r, res, repo, number); err != nil {
		return nil, err
	} else if why != "" {
		return &actionResult{outcome: "blocked", summary: fmt.Sprintf("ai-flow will not merge #%d: %s", number, why)}, nil
	}
	if other, err := e.releaseInFlight(ctx, r); err != nil {
		return nil, err
	} else if other != "" {
		return waiting("Waiting to merge #%d: %s", number, other), nil
	}
	// CI tested the branch against the base as it was. If the base moved
	// since (often another run's release, merged while this one waited), the
	// pass is stale: web-games Play anywhere merged on a green CI from before
	// accounts landed, and the combination failed its release build. Behind
	// is reported as conflict, which flows route to update-branch → CI.
	if behind, err := e.gh.BehindBy(ctx, repo, r.Base, pr.Head.SHA); err != nil {
		return nil, err
	} else if behind > 0 {
		return &actionResult{outcome: "conflict", summary: fmt.Sprintf(
			"#%d is %d commit(s) behind %s, so its CI pass predates them: merge origin/%s into the branch and run CI again before merging",
			number, behind, r.Base, r.Base)}, nil
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

// ciPassedRef is the commit the run's latest passing wait_for_checks
// checked, or "" when none passed.
func (e *Engine) ciPassedRef(ctx context.Context, r *store.Run, res *resolve.Resolved) (string, error) {
	vs, err := e.store.Visits(ctx, r.ID)
	if err != nil {
		return "", err
	}
	for i := len(vs) - 1; i >= 0; i-- {
		v := vs[i]
		if n := res.Nodes[v.Node]; n == nil || n.Action != "wait_for_checks" || v.Outcome != "passed" {
			continue
		}
		var out struct {
			Ref string `json:"ref"`
		}
		json.Unmarshal(v.Outputs, &out)
		return out.Ref, nil
	}
	return "", nil
}

// failureLogs collects the failing lines of each failed Actions job's log.
// Best effort: CI without job logs (other providers) just has none.
func (e *Engine) failureLogs(ctx context.Context, repo github.Repo, checks []github.Check) string {
	var parts []string
	for _, c := range checks {
		if !c.Done || c.OK || c.JobID == 0 {
			continue
		}
		log, err := e.gh.JobLog(ctx, repo, c.JobID)
		if err != nil {
			slog.Warn("ci job log", "job", c.JobID, "err", err)
			continue
		}
		parts = append(parts, "== "+c.Name+"\n"+failureExcerpt(log, 3000))
	}
	return strings.Join(parts, "\n\n")
}

var (
	logStamp = regexp.MustCompile(`^\d{4}-\d\d-\d\dT[\d:.]+Z |\x1b\[[0-9;]*[A-Za-z]`)
	// Strong markers say a test or job failed; weak ones only matter when
	// there are none (passing tests can be named "…undefined…").
	logFailure = regexp.MustCompile(`^(not ok |--- FAIL|FAIL\b|panic:)|##\[error\]`)
	logWeak    = regexp.MustCompile(`(?i)(error:|\berror\b.*:|assert|expected|Cannot )`)
)

// failureExcerpt keeps the lines around failures in a CI log, within max
// bytes, falling back to the log's tail.
func failureExcerpt(log string, limit int) string {
	lines := strings.Split(strings.ReplaceAll(log, "\r\n", "\n"), "\n")
	for i, l := range lines {
		lines[i] = logStamp.ReplaceAllString(l, "") // timestamps and colour codes
	}
	keep := make([]bool, len(lines))
	hits := 0
	mark := func(re *regexp.Regexp, before, after int) {
		for i, l := range lines {
			if re.MatchString(strings.TrimSpace(l)) {
				hits++
				for j := max(0, i-before); j <= min(len(lines)-1, i+after); j++ {
					keep[j] = true
				}
			}
		}
	}
	mark(logFailure, 0, 8) // a TAP failure's details follow it
	if hits == 0 {
		mark(logWeak, 2, 3)
	}
	var out []string
	if hits == 0 {
		out = lines[max(0, len(lines)-40):]
	} else {
		gap := false
		for i, l := range lines {
			if keep[i] {
				out, gap = append(out, l), false
			} else if !gap && len(out) > 0 {
				out, gap = append(out, "…"), true
			}
		}
	}
	text := strings.TrimSpace(strings.Join(out, "\n"))
	if len(text) > limit {
		text = "…" + text[len(text)-limit:]
	}
	return text
}

// deployPipelineFailure reports CI on the merged commit that finished and
// failed (the build/release workflow), as a timeout: the release cannot go
// live, and the timeout route is where flows handle that.
func (e *Engine) deployPipelineFailure(ctx context.Context, res *resolve.Resolved, d *config.Deploy, sha string) *actionResult {
	repo, err := e.repoOf(res)
	if err != nil {
		return nil
	}
	checks, err := e.gh.Checks(ctx, repo, sha)
	if err != nil {
		return nil // inconclusive; keep waiting
	}
	if len(d.Pipeline) > 0 {
		checks = slices.DeleteFunc(checks, func(c github.Check) bool { return !slices.Contains(d.Pipeline, c.Name) })
	}
	var failed []string
	for _, c := range checks {
		if c.Done && !c.OK {
			failed = append(failed, c.Name)
		}
	}
	if len(failed) == 0 {
		return nil
	}
	out := map[string]any{"failed": failed}
	if logs := e.failureLogs(ctx, repo, checks); logs != "" {
		out["log_tail"] = logs
	}
	return &actionResult{outcome: flow.OutcomeTimeout, outputs: out, summary: fmt.Sprintf(
		"%s will not go live: its deploy pipeline failed (%s), so no release was built", short(sha), strings.Join(failed, ", "))}
}

// runPR fetches the run's pull request, or nil when it has none or GitHub
// does not answer (callers treat that as "unknown").
func (e *Engine) runPR(ctx context.Context, repo github.Repo, r *store.Run) (*github.PRState, int) {
	m := prNumberRe.FindStringSubmatch(strings.TrimSuffix(r.PRURL, "/"))
	if m == nil {
		return nil, 0
	}
	number, _ := strconv.Atoi(m[1])
	pr, err := e.gh.PR(ctx, repo, number)
	if err != nil {
		slog.Warn("pull request", "run", r.ID, "err", err)
		return nil, number
	}
	return pr, number
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
		if len(want) == 0 || slices.Contains(want, c.Name) {
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
	if len(checks) == 0 && strings.TrimSpace(with["ref"]) == "" {
		// GitHub runs no pull_request workflows on a PR that conflicts with
		// its base, so this would end in "none" after settle. Say why, on the
		// failed route, which goes back to a step that can merge the base.
		if pr, number := e.runPR(ctx, repo, r); pr != nil && pr.State == "open" && pr.Mergeable != nil && !*pr.Mergeable {
			outputs["failed"] = []string{"merge conflict with " + r.Base}
			return &actionResult{outcome: "failed", outputs: outputs, summary: fmt.Sprintf(
				"#%d conflicts with %s, so GitHub runs no CI on it. Merge origin/%s into the branch (it is current in the checkout; no fetch needed), resolve the conflicts keeping both sides' work, re-run the tests and commit.",
				number, r.Base, r.Base)}, nil
		}
	}
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
		summary := fmt.Sprintf("CI failed on %s: %s", short(ref), strings.Join(failed, ", "))
		// The fix step needs the failure, not just the job name: on web-games
		// it ran the suite 30 times, never saw the 3 failing tests, and gave up.
		if logs := e.failureLogs(ctx, repo, checks); logs != "" {
			outputs["log_tail"] = logs
		}
		if strings.TrimSpace(with["ref"]) == "" && r.PRURL != "" {
			// That failure only existed in the merge with main.
			summary += fmt.Sprintf(". PR CI runs on the merge of the branch into %s: to reproduce, merge origin/%s into the checkout first (no fetch needed).", r.Base, r.Base)
		}
		return &actionResult{outcome: "failed", summary: summary, outputs: outputs}, nil
	}
	return &actionResult{outcome: "passed", summary: fmt.Sprintf("All %d checks passed on %s", len(checks), short(ref)), outputs: outputs}, nil
}

// waitForDeploy waits until every version endpoint reports the expected commit.
func (e *Engine) waitForDeploy(ctx context.Context, r *store.Run, res *resolve.Resolved, with map[string]string) (*actionResult, error) {
	d, err := e.deployOf(r, res)
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
		// A failed build on the merged commit never deploys: say so now, with
		// the failure, instead of timing out. A web-games release waited 20
		// minutes for an image whose Docker build had failed in 2.
		if failed := e.deployPipelineFailure(ctx, res, d, expect); failed != nil {
			return failed, nil
		}
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
// probes the deploy URL. A check that fails on two polls in a row spanning at
// least the sustain window (HealthSustain, with.sustain) is degraded. Query
// errors are inconclusive (a metrics outage must not roll a deploy back); the
// app is healthy only once the soak has passed and every check has been
// evaluated successfully at least twice and the latest poll is clean.
func (e *Engine) checkHealth(ctx context.Context, r *store.Run, res *resolve.Resolved, v *store.Visit, with map[string]string) (*actionResult, error) {
	d, err := e.deployOf(r, res)
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
	sustain := e.HealthSustain
	if s := strings.TrimSpace(with["sustain"]); s != "" {
		var dur flow.Duration
		if err := dur.UnmarshalJSON([]byte(strconv.Quote(s))); err != nil {
			return nil, rejectf("sustain: %v", err)
		}
		sustain = dur.Duration
	}
	key := fmt.Sprintf("%s/%d", r.ID, v.Seq)
	if e.health[key] == nil {
		e.health[key], e.passes[key], e.badSince[key] = map[string]int{}, map[string]int{}, map[string]time.Time{}
	}
	streak, passes, since := e.health[key], e.passes[key], e.badSince[key]
	var now, unknown []string
	record := func(name string, bad bool, detail string) {
		passes[name]++
		if bad {
			if streak[name] == 0 {
				since[name] = time.Now()
			}
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
	for _, p := range d.Probes {
		body, err := e.get(ctx, p.URL)
		if err == nil && !bytes.Contains(body, []byte(p.Contains)) {
			err = fmt.Errorf("the page lacks %q", p.Contains)
		}
		record("page "+p.URL, err != nil, fmt.Sprint(err))
	}
	var degraded []string
	for name, n := range streak {
		if n >= 2 && time.Since(since[name]) >= sustain {
			degraded = append(degraded, name)
		}
	}
	if len(degraded) > 0 {
		return &actionResult{outcome: "degraded", summary: "Degraded: " + strings.Join(now, "; "),
			outputs: map[string]any{"violations": now, "polls": maxCount(passes)}}, nil
	}
	elapsed := time.Since(time.UnixMilli(v.StartedAt))
	// Healthy needs every check evaluated twice since this process started
	// watching (counters are in memory, so a restart re-earns them) and a
	// clean latest poll: a violation that is younger than the sustain window
	// when the soak ends keeps the soak going until it clears or degrades.
	confirmed := len(now) == 0
	for name := range streak {
		confirmed = confirmed && passes[name] >= 2
	}
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

// deployOf is the project's deploy config as pinned when the run started (its
// resolved snapshot), so editing health checks, versions or the rollback
// workflow never changes a release mid-run; resume with refresh re-pins it.
// Without a pinned one (res nil, or no deploy config then) it is the current.
func (e *Engine) deployOf(r *store.Run, res *resolve.Resolved) (*config.Deploy, error) {
	if res != nil && res.Project != nil && res.Project.Spec.Deploy != nil {
		return res.Project.Spec.Deploy, nil
	}
	p := e.cfg.Current().Projects[r.Project]
	if p == nil || p.Spec.Deploy == nil {
		return nil, rejectf("project %q has no deploy configuration", r.Project)
	}
	return p.Spec.Deploy, nil
}

// promQuery runs a PromQL instant query and returns the highest value across
// its series (0 for an empty result).
func (e *Engine) promQuery(ctx context.Context, q string) (float64, error) {
	values, err := metrics.Instant(ctx, e.HTTP, e.cfg.Current().Env.Metrics.URL, q)
	if err != nil || len(values) == 0 {
		return 0, err
	}
	return slices.Max(values), nil
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

// rollbackPickup is how long a dispatched rollback may take to show up as a
// workflow run before the dispatch is presumed lost and sent again.
var rollbackPickup = 3 * time.Minute

// rollbackDeploy dispatches the project's rollback workflow for a release and
// waits for it. The dispatch is recorded in kv before it is sent, so polls
// and restarts look for its run rather than dispatching twice.
func (e *Engine) rollbackDeploy(ctx context.Context, r *store.Run, res *resolve.Resolved, v *store.Visit, with map[string]string) (*actionResult, error) {
	d, err := e.deployOf(r, res)
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
	run, pending, err := e.dispatchWorkflow(ctx, r, v, repo, d.Rollback.Workflow, d.Branch, map[string]string{"sha": sha}, "to roll back "+short(sha))
	if err != nil || pending != nil {
		return pending, err
	}
	outputs := map[string]any{"url": run.HTMLURL, "previous": previous}
	if run.Conclusion != "success" {
		return &actionResult{outcome: "failed", summary: fmt.Sprintf("%s ended %s: %s", d.Rollback.Workflow, run.Conclusion, run.HTMLURL), outputs: outputs}, nil
	}
	return &actionResult{outcome: "rolled_back", summary: fmt.Sprintf("Rolled back %s; %s should go live again", short(sha), short(previous)), outputs: outputs}, nil
}

// runWorkflow dispatches one of the project's listed workflows on its base
// branch, where only merged code runs, and waits for it.
func (e *Engine) runWorkflow(ctx context.Context, r *store.Run, res *resolve.Resolved, v *store.Visit, with map[string]string) (*actionResult, error) {
	name := strings.TrimSpace(with["workflow"])
	p := e.cfg.Current().Projects[r.Project]
	if p == nil || p.Spec.Workflows[name] == nil {
		return nil, rejectf("project %q lists no workflow %q", r.Project, name)
	}
	wf := p.Spec.Workflows[name]
	repo, err := e.repoOf(res)
	if err != nil {
		return nil, err
	}
	var inputs map[string]string
	if note := strings.TrimSpace(with["note"]); note != "" && wf.Note != "" {
		inputs = map[string]string{wf.Note: note}
	}
	base := p.Spec.Base
	if base == "" {
		base = "main"
	}
	run, pending, err := e.dispatchWorkflow(ctx, r, v, repo, wf.File, base, inputs, "on "+base)
	if err != nil || pending != nil {
		return pending, err
	}
	outputs := map[string]any{"url": run.HTMLURL}
	if run.Conclusion != "success" {
		return &actionResult{outcome: "failed", summary: fmt.Sprintf("%s ended %s: %s", name, run.Conclusion, run.HTMLURL), outputs: outputs}, nil
	}
	return &actionResult{outcome: "succeeded", summary: fmt.Sprintf("%s succeeded: %s", name, run.HTMLURL), outputs: outputs}, nil
}

// dispatchWorkflow starts a workflow once for this visit and returns its run when
// it has completed. Until then, or when GitHub refuses, it returns the result to
// report instead. The dispatch is recorded in kv before it is sent, so polls
// after a lost reply or a restart follow the same run.
func (e *Engine) dispatchWorkflow(ctx context.Context, r *store.Run, v *store.Visit, repo github.Repo, workflow, branch string, inputs map[string]string, what string) (*github.WorkflowRun, *actionResult, error) {
	key := fmt.Sprintf("dispatch/%s/%d", r.ID, v.Seq)
	state, err := e.store.GetKV(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		// Record the dispatch before sending it: if the reply is lost (or
		// the process dies) the next poll looks for the run instead of
		// dispatching a second time.
		at := time.Now()
		if err := e.store.SetKV(ctx, key, strconv.FormatInt(at.UnixMilli(), 10)); err != nil {
			return nil, nil, err
		}
		err := e.gh.DispatchWorkflow(ctx, repo, workflow, branch, inputs)
		var api *github.APIError
		if errors.As(err, &api) && api.Status >= 400 && api.Status < 500 {
			return nil, &actionResult{outcome: "failed", summary: fmt.Sprintf("GitHub refused to start %s: %s", workflow, strings.TrimSpace(api.Body))}, nil
		}
		if err != nil {
			slog.Warn("workflow dispatch", "run", r.ID, "err", err)
			return nil, waiting("Dispatching %s %s (reply lost: %v)", workflow, what, err), nil
		}
		return nil, waiting("Dispatched %s %s", workflow, what), nil
	}
	if err != nil {
		return nil, nil, err
	}
	at, runID := state, int64(0)
	if i := strings.IndexByte(state, ':'); i >= 0 {
		at = state[:i]
		runID, _ = strconv.ParseInt(state[i+1:], 10, 64)
	}
	ms, _ := strconv.ParseInt(at, 10, 64)
	if runID == 0 {
		runs, err := e.gh.DispatchedRuns(ctx, repo, workflow, branch)
		if err != nil {
			return nil, nil, err
		}
		// The oldest run created after the dispatch (allowing for clock skew).
		since := time.UnixMilli(ms).Add(-15 * time.Second)
		for _, run := range runs {
			if !run.CreatedAt.Before(since) && (runID == 0 || run.ID < runID) {
				runID = run.ID
			}
		}
		if runID == 0 {
			if time.Since(time.UnixMilli(ms)) > rollbackPickup {
				// No run ever appeared: the dispatch did not land. Send it
				// again on the next poll.
				if err := e.store.WriteKV(ctx, nil, []string{key}); err != nil {
					return nil, nil, err
				}
				return nil, waiting("GitHub never started %s; dispatching it again", workflow), nil
			}
			return nil, waiting("Waiting for GitHub to start %s", workflow), nil
		}
		if err := e.store.SetKV(ctx, key, fmt.Sprintf("%d:%d", ms, runID)); err != nil {
			return nil, nil, err
		}
	}
	run, err := e.gh.Run(ctx, repo, runID)
	if err != nil {
		return nil, nil, err
	}
	if run.Status != "completed" {
		return nil, waiting("%s %s (%s)", workflow, strings.ReplaceAll(run.Status, "_", " "), run.HTMLURL), nil
	}
	return run, nil, nil
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

// queryMetrics evaluates Prometheus/VictoriaMetrics instant queries once for
// discovery/monitoring flows (without requiring a deploy config).
func (e *Engine) queryMetrics(ctx context.Context, r *store.Run, res *resolve.Resolved, with map[string]string) (*actionResult, error) {
	if strings.TrimSpace(e.cfg.Current().Env.Metrics.URL) == "" {
		return &actionResult{outcome: "unavailable", summary: "metrics.url is not configured", outputs: map[string]any{"alerts": []string{}, "summary": "metrics unavailable"}}, nil
	}
	var queries []config.HealthQuery
	if raw := strings.TrimSpace(with["queries"]); raw != "" {
		if err := json.Unmarshal([]byte(raw), &queries); err != nil {
			return nil, rejectf("query_metrics with.queries: %v", err)
		}
	} else {
		p := res.Project
		if p == nil && r.Project != "" {
			p = e.cfg.Current().Projects[r.Project]
		}
		if p != nil {
			if p.Spec.Observability != nil {
				queries = append(queries, p.Spec.Observability.Queries...)
			}
			if p.Spec.Deploy != nil {
				queries = append(queries, p.Spec.Deploy.Health...)
			}
		}
	}
	if len(queries) == 0 {
		return &actionResult{outcome: "unavailable", summary: "no observability or deploy health queries configured", outputs: map[string]any{"alerts": []string{}, "summary": "no metrics queries configured"}}, nil
	}
	var lines, alerts, errs []string
	okCount := 0
	for _, q := range queries {
		val, err := e.promQuery(ctx, q.Query)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", q.Name, err))
			continue
		}
		okCount++
		line := fmt.Sprintf("%s=%.4g (max %.4g)", q.Name, val, q.Max)
		lines = append(lines, line)
		if val > q.Max {
			alerts = append(alerts, fmt.Sprintf("%s: %.4g > max %.4g", q.Name, val, q.Max))
		}
	}
	if okCount == 0 {
		return &actionResult{outcome: "unavailable", summary: "all metrics queries failed: " + strings.Join(errs, "; "),
			outputs: map[string]any{"alerts": []string{}, "summary": strings.Join(errs, "; ")}}, nil
	}
	summaryText := strings.Join(lines, "; ")
	if len(errs) > 0 {
		summaryText += "; errors: " + strings.Join(errs, "; ")
	}
	if len(alerts) > 0 {
		return &actionResult{outcome: "alert", summary: fmt.Sprintf("%d metric alert(s): %s", len(alerts), strings.Join(alerts, "; ")),
			outputs: map[string]any{"alerts": alerts, "summary": summaryText}}, nil
	}
	return &actionResult{outcome: "ok", summary: fmt.Sprintf("All %d metric check(s) within threshold", okCount),
		outputs: map[string]any{"alerts": []string{}, "summary": summaryText}}, nil
}

type taskSpawnItem struct {
	Key      string `json:"key"`
	Title    string `json:"title"`
	Body     string `json:"body"`
	Category string `json:"category"`
}

// createTasks spawns bounded, deduplicated child tasks from with.items,
// skipping duplicates and entries already shipped/rejected in the project ledger.
func (e *Engine) createTasks(ctx context.Context, r *store.Run, res *resolve.Resolved, with map[string]string) (*actionResult, error) {
	if e.taskCreator == nil {
		return nil, rejectf("task creator is not configured")
	}
	raw := strings.TrimSpace(with["items"])
	if raw == "" || raw == "[]" {
		return &actionResult{outcome: "none", summary: "No candidate tasks supplied", outputs: map[string]any{"created": 0, "skipped": 0, "task_ids": []string{}}}, nil
	}
	items := parseTaskSpawnItems(raw)
	if len(items) == 0 {
		return &actionResult{outcome: "none", summary: "No parseable candidate tasks supplied", outputs: map[string]any{"created": 0, "skipped": 0, "task_ids": []string{}}}, nil
	}
	limit := 3
	if m := strings.TrimSpace(with["max"]); m != "" {
		if n, err := strconv.Atoi(m); err == nil && n > 0 {
			limit = min(n, 10)
		}
	}
	plan := true
	if p := strings.ToLower(strings.TrimSpace(with["plan"])); p == "false" || p == "0" || p == "no" {
		plan = false
	}
	source := strings.TrimSpace(with["source"])
	if source == "" {
		source = "flow"
	}
	ledger := e.Ledger(ctx, r.Project)
	var taskIDs []string
	skipped := 0
	for _, it := range items {
		if len(taskIDs) >= limit {
			skipped++
			continue
		}
		title := strings.TrimSpace(it.Title)
		if title == "" {
			skipped++
			continue
		}
		key := normalizeLedgerKey(it.Key)
		if key == "" {
			key = normalizeLedgerKey(title)
		}
		if _, suppressed := ledgerSuppressed(ledger, key); suppressed {
			skipped++
			continue
		}
		body := strings.TrimSpace(it.Body)
		if body == "" {
			body = fmt.Sprintf("Spawned from discovery run %s (%s).", r.ID, r.FlowName)
		}
		tid, created, err := e.taskCreator(ctx, ChildTask{Project: r.Project, Source: source, Key: key, Title: title, Body: body, Plan: plan, ParentTaskID: r.TaskID})
		if err != nil {
			return nil, err
		}
		if !created {
			skipped++
			continue
		}
		taskIDs = append(taskIDs, tid)
		// Now, not after the loop: a later error retries the action, which
		// then skips this item as a duplicate.
		if err := e.UpsertLedger(ctx, r.Project, LedgerEntry{Key: key, Title: title, Category: it.Category,
			Status: LedgerProposed, Summary: oneLine(body, 200), RunID: r.ID}); err != nil {
			return nil, err
		}
	}
	if taskIDs == nil {
		taskIDs = []string{}
	}
	out := map[string]any{"created": len(taskIDs), "skipped": skipped, "task_ids": taskIDs}
	if len(taskIDs) == 0 {
		return &actionResult{outcome: "none", summary: fmt.Sprintf("Skipped all %d candidate(s) (duplicate or already in ledger)", skipped), outputs: out}, nil
	}
	return &actionResult{outcome: "created", summary: fmt.Sprintf("Created %d task(s) (%d skipped): %s", len(taskIDs), skipped, strings.Join(taskIDs, ", ")), outputs: out}, nil
}

func parseTaskSpawnItems(raw string) []taskSpawnItem {
	var objs []taskSpawnItem
	if json.Unmarshal([]byte(raw), &objs) == nil && len(objs) > 0 {
		hasTitle := false
		for _, o := range objs {
			if strings.TrimSpace(o.Title) != "" {
				hasTitle = true
				break
			}
		}
		if hasTitle {
			return objs
		}
	}
	var strs []string
	if json.Unmarshal([]byte(raw), &strs) == nil && len(strs) > 0 {
		out := make([]taskSpawnItem, 0, len(strs))
		for _, s := range strs {
			if s = strings.TrimSpace(s); s != "" {
				title, body, _ := strings.Cut(s, " — ")
				out = append(out, taskSpawnItem{Title: strings.TrimSpace(title), Body: strings.TrimSpace(body)})
			}
		}
		return out
	}
	var out []taskSpawnItem
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(line, "-*• "))
		if line == "" {
			continue
		}
		title, body, _ := strings.Cut(line, " — ")
		out = append(out, taskSpawnItem{Title: strings.TrimSpace(title), Body: strings.TrimSpace(body)})
	}
	return out
}
