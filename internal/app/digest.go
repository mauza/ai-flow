package app

import (
	"context"
	"fmt"
	"html"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mauza/ai-flow/internal/store"
	"github.com/mauza/ai-flow/internal/storymap"
)

// ProjectSummary aggregates product progress, metrics, delivery investment,
// recent runs, open product questions, and retro takeaways for one project so
// humans can decide what is valuable to build next.
type ProjectSummary struct {
	Project     string             `json:"project"`
	Description string             `json:"description,omitempty"`
	Repo        string             `json:"repo,omitempty"`
	Branch      string             `json:"branch,omitempty"`
	GeneratedAt int64              `json:"generated_at"`
	Delivery    DeliverySummary    `json:"delivery"`
	Attention   *AttentionReport   `json:"attention"`
	Maps        []MapHealthSummary `json:"maps"`
	RecentRuns  []RecentRunSummary `json:"recent_runs"`
	Retros      []RetroHighlight   `json:"retros"`
}

type DeliverySummary struct {
	TotalRuns      int      `json:"total_runs"`
	ActiveRuns     int      `json:"active_runs"`
	WaitingRuns    int      `json:"waiting_runs"`
	SucceededRuns  int      `json:"succeeded_runs"`
	FailedRuns     int      `json:"failed_runs"`
	SuccessRatePct *float64 `json:"success_rate_pct,omitempty"`
	TotalCostUSD   float64  `json:"total_cost_usd"`
	TotalTokens    int64    `json:"total_tokens"`
	MedianCycleHrs *float64 `json:"median_cycle_hours,omitempty"`
}

type MapHealthSummary struct {
	ID            string           `json:"id"`
	Title         string           `json:"title"`
	Description   string           `json:"description,omitempty"`
	TasksTotal    int              `json:"tasks_total"`
	TasksDone     int              `json:"tasks_done"`
	Releases      []ReleaseSummary `json:"releases"`
	Metrics       []NamedMetric    `json:"metrics"`
	OpenQuestions []string         `json:"open_questions"`
	RiskyBets     []BetSummary     `json:"risky_bets"` // high value + low confidence, not done
	Outcomes      []OutcomeSummary `json:"outcomes"`   // tasks with recorded post-ship outcomes
}

type ReleaseSummary struct {
	ID            string   `json:"id"`
	Title         string   `json:"title"`
	Goal          string   `json:"goal,omitempty"`
	Status        string   `json:"status,omitempty"`
	Total         int      `json:"total"`
	Done          int      `json:"done"`
	OpenQuestions []string `json:"open_questions,omitempty"`
}

type NamedMetric struct {
	ID        string       `json:"id"`
	Title     string       `json:"title"`
	Kind      string       `json:"kind"`
	Unit      string       `json:"unit,omitempty"`
	Target    *float64     `json:"target,omitempty"`
	Direction string       `json:"direction,omitempty"`
	Value     *float64     `json:"value,omitempty"`
	Series    [][2]float64 `json:"series,omitempty"`
	Detail    string       `json:"detail,omitempty"`
	Error     string       `json:"error,omitempty"`
	OnTarget  *bool        `json:"on_target,omitempty"`
}

type BetSummary struct {
	ID            string   `json:"id"`
	Title         string   `json:"title"`
	Release       string   `json:"release,omitempty"`
	Value         string   `json:"value,omitempty"`
	Confidence    string   `json:"confidence,omitempty"`
	Hypothesis    string   `json:"hypothesis,omitempty"`
	OpenQuestions []string `json:"open_questions,omitempty"`
}

type OutcomeSummary struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Outcome     string `json:"outcome"`
	OutcomeNote string `json:"outcome_note,omitempty"`
}

type RecentRunSummary struct {
	RunID      string  `json:"run_id"`
	FlowName   string  `json:"flow_name"`
	TaskTitle  string  `json:"task_title,omitempty"`
	Status     string  `json:"status"`
	PRURL      string  `json:"pr_url,omitempty"`
	CostUSD    float64 `json:"cost_usd"`
	CreatedAt  int64   `json:"created_at"`
	FinishedAt int64   `json:"finished_at,omitempty"`
	Error      string  `json:"error,omitempty"`
}

type RetroHighlight struct {
	RunID       string            `json:"run_id"`
	FlowName    string            `json:"flow_name"`
	Summary     string            `json:"summary"`
	Suggestions []RetroSuggestion `json:"suggestions,omitempty"`
}

// ProjectSummary builds a decision-oriented health report for one project.
func (a *App) ProjectSummary(ctx context.Context, project string) (*ProjectSummary, error) {
	cfg := a.Cfg.Current()
	p := cfg.Projects[project]
	if p == nil {
		return nil, fmt.Errorf("unknown project %q", project)
	}
	s := &ProjectSummary{
		Project:     project,
		Description: p.Spec.Description,
		Branch:      p.Spec.Base,
		GeneratedAt: store.Now(),
		Maps:        []MapHealthSummary{},
		RecentRuns:  []RecentRunSummary{},
		Retros:      []RetroHighlight{},
	}
	if src, err := a.ProductSource(project); err == nil {
		s.Repo = src.Repo.String()
	}

	att, err := a.Attention(ctx, project)
	if err != nil {
		return nil, err
	}
	s.Attention = att

	tasks, _ := a.Store.ListTasks(ctx)
	taskByID := map[string]*store.Task{}
	for _, t := range tasks {
		taskByID[t.ID] = t
	}

	runs, err := a.Store.ListRuns(ctx, store.RunFilter{Project: project, Limit: 500})
	if err != nil {
		return nil, err
	}
	var cycleHours []float64
	finished := 0
	for i, r := range runs {
		s.Delivery.TotalRuns++
		s.Delivery.TotalCostUSD += r.CostUSD
		s.Delivery.TotalTokens += r.Tokens
		switch r.Status {
		case store.RunRunning, store.RunQueued:
			s.Delivery.ActiveRuns++
		case store.RunWaiting:
			s.Delivery.WaitingRuns++
		case store.RunSucceeded:
			s.Delivery.SucceededRuns++
			finished++
			if r.FinishedAt > 0 && r.CreatedAt > 0 {
				cycleHours = append(cycleHours, float64(r.FinishedAt-r.CreatedAt)/3.6e6)
			}
		case store.RunFailed:
			s.Delivery.FailedRuns++
			finished++
		case store.RunCanceled:
			finished++
		}
		if i < 12 {
			title := r.FlowName
			if t := taskByID[r.TaskID]; t != nil {
				title = t.Title
			}
			s.RecentRuns = append(s.RecentRuns, RecentRunSummary{
				RunID: r.ID, FlowName: r.FlowName, TaskTitle: title, Status: r.Status,
				PRURL: r.PRURL, CostUSD: r.CostUSD, CreatedAt: r.CreatedAt,
				FinishedAt: r.FinishedAt, Error: clip(r.Error, 240),
			})
		}
		if len(s.Retros) < 3 {
			if retro, err := a.GetRetro(ctx, r.ID); err == nil && retro != nil && retro.Status == "done" && retro.Report != nil {
				var top []RetroSuggestion
				for _, sg := range retro.Report.Suggestions {
					if sg.Priority == "high" || len(top) < 2 {
						top = append(top, sg)
					}
					if len(top) >= 3 {
						break
					}
				}
				s.Retros = append(s.Retros, RetroHighlight{
					RunID: r.ID, FlowName: r.FlowName, Summary: retro.Report.Summary, Suggestions: top,
				})
			}
		}
	}
	s.Delivery.TotalCostUSD = math.Round(s.Delivery.TotalCostUSD*100) / 100
	if finished > 0 {
		rate := math.Round(1000*float64(s.Delivery.SucceededRuns)/float64(finished)) / 10
		s.Delivery.SuccessRatePct = &rate
	}
	if len(cycleHours) > 0 {
		sort.Float64s(cycleHours)
		med := cycleHours[len(cycleHours)/2]
		if len(cycleHours)%2 == 0 {
			med = (cycleHours[len(cycleHours)/2-1] + cycleHours[len(cycleHours)/2]) / 2
		}
		med = math.Round(med*10) / 10
		s.Delivery.MedianCycleHrs = &med
	}

	// Story maps from local workspace if present.
	if a.Workspaces != nil {
		work := a.Workspaces.WorkDir(project)
		if _, err := os.Stat(filepath.Join(work, filepath.FromSlash(storymap.Root))); err == nil {
			if maps, err := storymap.List(work); err == nil {
				mctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				for _, ms := range maps {
					l, err := storymap.Load(work, ms.ID)
					if err != nil {
						continue
					}
					works, _ := a.MapWorks(ctx, project, l.ID)
					mvals := a.MapMetrics(mctx, project, l, works)
					valByID := map[string]MetricValue{}
					for _, mv := range mvals {
						valByID[mv.ID] = mv
					}
					mh := MapHealthSummary{
						ID: l.ID, Title: l.Map.Title, Description: l.Map.Description,
						TasksTotal: len(l.Tasks),
						Releases:   []ReleaseSummary{}, Metrics: []NamedMetric{},
						OpenQuestions: []string{}, RiskyBets: []BetSummary{}, Outcomes: []OutcomeSummary{},
					}
					relCount := map[string][2]int{}
					for _, ut := range l.Tasks {
						c := relCount[ut.Release]
						c[0]++
						if ut.Status == "done" {
							mh.TasksDone++
							c[1]++
						}
						relCount[ut.Release] = c
						if ut.Status != "done" {
							for _, q := range ut.OpenQuestions {
								mh.OpenQuestions = append(mh.OpenQuestions, fmt.Sprintf("%s: %s", ut.Title, q))
							}
							if (ut.Value == "high" && ut.Confidence == "low") || (ut.Confidence == "low" && len(ut.OpenQuestions) > 0) {
								mh.RiskyBets = append(mh.RiskyBets, BetSummary{
									ID: ut.ID, Title: ut.Title, Release: ut.Release,
									Value: ut.Value, Confidence: ut.Confidence,
									Hypothesis: ut.Hypothesis, OpenQuestions: ut.OpenQuestions,
								})
							}
						}
						if ut.Outcome != "" {
							mh.Outcomes = append(mh.Outcomes, OutcomeSummary{
								ID: ut.ID, Title: ut.Title, Outcome: ut.Outcome, OutcomeNote: ut.OutcomeNote,
							})
						}
					}
					for _, rel := range l.Map.Releases {
						c := relCount[rel.ID]
						mh.Releases = append(mh.Releases, ReleaseSummary{
							ID: rel.ID, Title: rel.Title, Goal: rel.Goal, Status: rel.Status,
							Total: c[0], Done: c[1], OpenQuestions: rel.OpenQuestions,
						})
						if rel.Status != "released" {
							for _, q := range rel.OpenQuestions {
								mh.OpenQuestions = append(mh.OpenQuestions, fmt.Sprintf("[%s] %s", rel.Title, q))
							}
						}
					}
					for _, mt := range l.Map.Metrics {
						mv := valByID[mt.ID]
						nm := NamedMetric{
							ID: mt.ID, Title: mt.Title, Kind: mt.Kind, Unit: mt.Unit,
							Target: mt.Target, Direction: mt.Direction,
							Value: mv.Value, Series: mv.Series, Detail: mv.Detail, Error: mv.Error,
						}
						if mt.Target != nil && mv.Value != nil {
							ok := *mv.Value >= *mt.Target
							if mt.Direction == "down" {
								ok = *mv.Value <= *mt.Target
							}
							nm.OnTarget = &ok
						}
						mh.Metrics = append(mh.Metrics, nm)
					}
					s.Maps = append(s.Maps, mh)
				}
			}
		}
	}
	return s, nil
}

// RenderDigestMarkdown produces a clean, shareable Markdown status brief for a project.
func RenderDigestMarkdown(s *ProjectSummary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s — Project Status & Value Digest\n\n", s.Project)
	if s.Description != "" {
		fmt.Fprintf(&b, "%s\n\n", strings.TrimSpace(s.Description))
	}
	fmt.Fprintf(&b, "_Generated %s", time.UnixMilli(s.GeneratedAt).UTC().Format("2006-01-02 15:04 UTC"))
	if s.Repo != "" {
		fmt.Fprintf(&b, " · `%s` (%s)", s.Repo, s.Branch)
	}
	b.WriteString("_\n\n")

	// Delivery & Investment
	b.WriteString("## Delivery & Investment\n\n")
	rate := "—"
	if s.Delivery.SuccessRatePct != nil {
		rate = fmt.Sprintf("%.1f%%", *s.Delivery.SuccessRatePct)
	}
	cycle := "—"
	if s.Delivery.MedianCycleHrs != nil {
		cycle = fmt.Sprintf("%.1fh", *s.Delivery.MedianCycleHrs)
	}
	fmt.Fprintf(&b, "- **Runs:** %d total (%d succeeded, %d failed, %d active, %d waiting for human)\n",
		s.Delivery.TotalRuns, s.Delivery.SucceededRuns, s.Delivery.FailedRuns, s.Delivery.ActiveRuns, s.Delivery.WaitingRuns)
	fmt.Fprintf(&b, "- **Success rate:** %s · **Median cycle time:** %s\n", rate, cycle)
	fmt.Fprintf(&b, "- **Total spend:** $%.2f (%d tokens)\n\n", s.Delivery.TotalCostUSD, s.Delivery.TotalTokens)

	// Needs human attention
	if s.Attention != nil && s.Attention.Counts.Total > 0 {
		b.WriteString("## Needs Human Attention\n\n")
		for _, g := range s.Attention.Gates {
			fmt.Fprintf(&b, "- **[Decision]** `%s` on `%s` (%s): %s\n", g.Node, g.FlowName, g.RunID, oneLineClip(g.Prompt, 180))
		}
		for _, f := range s.Attention.FailedRuns {
			fmt.Fprintf(&b, "- **[Failed run]** `%s` (%s): %s\n", f.FlowName, f.RunID, oneLineClip(f.Error, 180))
		}
		for _, sh := range s.Attention.ShippedUnmarked {
			pr := ""
			if sh.PRURL != "" {
				pr = " · " + sh.PRURL
			}
			fmt.Fprintf(&b, "- **[Verify shipment]** %s (%s)%s\n", sh.Title, sh.MapTitle, pr)
		}
		for _, q := range s.Attention.OpenQuestions {
			fmt.Fprintf(&b, "- **[Open question · %s]** %s: %s\n", q.Kind, q.Title, strings.Join(q.Questions, " | "))
		}
		b.WriteString("\n")
	}

	// Story maps, releases & metrics
	for _, m := range s.Maps {
		fmt.Fprintf(&b, "## Product Map: %s (%d/%d tasks done)\n\n", m.Title, m.TasksDone, m.TasksTotal)
		if m.Description != "" {
			fmt.Fprintf(&b, "%s\n\n", strings.TrimSpace(m.Description))
		}
		if len(m.Releases) > 0 {
			b.WriteString("### Releases\n\n| Release | Status | Progress | Goal |\n|---|---|---|---|\n")
			for _, r := range m.Releases {
				fmt.Fprintf(&b, "| %s | %s | %d/%d | %s |\n", r.Title, orNone(r.Status), r.Done, r.Total, strings.ReplaceAll(oneLineClip(r.Goal, 120), "|", "\\|"))
			}
			b.WriteString("\n")
		}
		if len(m.Metrics) > 0 {
			b.WriteString("### Metrics\n\n| Metric | Kind | Current | Target | Status |\n|---|---|---|---|---|\n")
			for _, mt := range m.Metrics {
				cur := "—"
				if mt.Error != "" {
					cur = "error"
				} else if mt.Value != nil {
					cur = fmtMetric(*mt.Value, mt.Unit)
				}
				tgt := "—"
				if mt.Target != nil {
					dir := "≥"
					if mt.Direction == "down" {
						dir = "≤"
					}
					tgt = dir + " " + fmtMetric(*mt.Target, mt.Unit)
				}
				st := mt.Detail
				if mt.OnTarget != nil {
					if *mt.OnTarget {
						st = "On target"
					} else {
						st = "Off target"
					}
				}
				fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", mt.Title, mt.Kind, cur, tgt, st)
			}
			b.WriteString("\n")
		}
		if len(m.RiskyBets) > 0 {
			b.WriteString("### High-Uncertainty Bets (Validate Before Over-Investing)\n\n")
			for _, bet := range m.RiskyBets {
				fmt.Fprintf(&b, "- **%s** (value=%s, confidence=%s)", bet.Title, orNone(bet.Value), orNone(bet.Confidence))
				if bet.Hypothesis != "" {
					fmt.Fprintf(&b, " — _%s_", oneLineClip(bet.Hypothesis, 180))
				}
				b.WriteString("\n")
			}
			b.WriteString("\n")
		}
		if len(m.Outcomes) > 0 {
			b.WriteString("### Recorded Post-Ship Outcomes\n\n")
			for _, o := range m.Outcomes {
				fmt.Fprintf(&b, "- **%s** → `%s`", o.Title, o.Outcome)
				if o.OutcomeNote != "" {
					fmt.Fprintf(&b, ": %s", oneLineClip(o.OutcomeNote, 200))
				}
				b.WriteString("\n")
			}
			b.WriteString("\n")
		}
	}

	if len(s.RecentRuns) > 0 {
		b.WriteString("## Recent Runs\n\n| Run | Task | Status | Cost | PR |\n|---|---|---|---|---|\n")
		for _, r := range s.RecentRuns {
			pr := "—"
			if r.PRURL != "" {
				pr = fmt.Sprintf("[PR](%s)", r.PRURL)
			}
			fmt.Fprintf(&b, "| `%s` | %s | %s | $%.2f | %s |\n", r.RunID, strings.ReplaceAll(oneLineClip(r.TaskTitle, 80), "|", "\\|"), r.Status, r.CostUSD, pr)
		}
		b.WriteString("\n")
	}

	if len(s.Retros) > 0 {
		b.WriteString("## Recent Retro Takeaways\n\n")
		for _, rt := range s.Retros {
			fmt.Fprintf(&b, "- **%s (`%s`)**: %s\n", rt.FlowName, rt.RunID, oneLineClip(rt.Summary, 240))
			for _, sg := range rt.Suggestions {
				fmt.Fprintf(&b, "  - [%s · %s] %s\n", orNone(sg.Priority), sg.Kind, oneLineClip(sg.Change, 180))
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// RenderDigestHTML produces a self-contained standalone HTML page that can be
// downloaded and shared with stakeholders without cluster access.
func RenderDigestHTML(s *ProjectSummary) string {
	var b strings.Builder
	esc := html.EscapeString
	rate := "—"
	if s.Delivery.SuccessRatePct != nil {
		rate = fmt.Sprintf("%.1f%%", *s.Delivery.SuccessRatePct)
	}
	cycle := "—"
	if s.Delivery.MedianCycleHrs != nil {
		cycle = fmt.Sprintf("%.1fh", *s.Delivery.MedianCycleHrs)
	}
	fmt.Fprintf(&b, `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s — ai-flow Project Digest</title>
<style>
:root{color-scheme:light dark;--bg:#0d1017;--card:#151923;--border:#262d3d;--text:#e7eaf2;--muted:#8a93a8;--accent:#7c9cff;--good:#34d399;--bad:#f87171;--warn:#fbbf24;--gate:#c4a1ff;}
@media (prefers-color-scheme:light){:root{--bg:#f6f7fa;--card:#ffffff;--border:#e1e5ee;--text:#151a25;--muted:#5c6578;--accent:#4f6ef7;--good:#0f9f6e;--bad:#dc3b3b;--warn:#b7791f;--gate:#7c4ddb;}}
*{box-sizing:border-box}body{margin:0;padding:28px 20px 56px;background:var(--bg);color:var(--text);font:14px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;}
.wrap{max-width:920px;margin:0 auto}h1{margin:0 0 6px;font-size:24px}h2{margin:28px 0 12px;font-size:17px}h3{margin:16px 0 8px;font-size:14px;color:var(--muted);text-transform:uppercase;letter-spacing:.04em}
.sub{color:var(--muted);margin-bottom:18px}.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(160px,1fr));gap:12px;margin-bottom:16px}
.card{background:var(--card);border:1px solid var(--border);border-radius:10px;padding:14px 16px}.kpi{font-size:22px;font-weight:700;margin-top:4px}
.good{color:var(--good)}.bad{color:var(--bad)}.warn{color:var(--warn)}.gate{color:var(--gate)}.muted{color:var(--muted)}
table{width:100%%;border-collapse:collapse;background:var(--card);border:1px solid var(--border);border-radius:10px;overflow:hidden}
th,td{text-align:left;padding:9px 12px;border-bottom:1px solid var(--border);vertical-align:top}th{font-size:12px;color:var(--muted);text-transform:uppercase;letter-spacing:.04em}
tr:last-child td{border-bottom:0}.pill{display:inline-block;padding:2px 8px;border-radius:999px;font-size:12px;font-weight:600;border:1px solid var(--border)}
a{color:var(--accent);text-decoration:none}a:hover{text-decoration:underline}ul{margin:6px 0 0;padding-left:20px}li{margin:4px 0}
.spark{width:120px;height:28px;display:block;margin-top:6px}.spark path{fill:none;stroke:var(--accent);stroke-width:2}
</style>
</head>
<body><div class="wrap">
<h1>%s</h1>
<div class="sub">%s%sGenerated %s</div>
<div class="grid">
  <div class="card"><div class="muted">Runs</div><div class="kpi">%d</div><div class="muted">%d ok · %d failed · %d waiting</div></div>
  <div class="card"><div class="muted">Success rate</div><div class="kpi">%s</div><div class="muted">Median cycle: %s</div></div>
  <div class="card"><div class="muted">Total spend</div><div class="kpi">$%.2f</div><div class="muted">%d tokens</div></div>
  <div class="card"><div class="muted">Needs attention</div><div class="kpi %s">%d</div><div class="muted">%d gates · %d questions</div></div>
</div>
`,
		esc(s.Project), esc(s.Project),
		esc(s.Description), ternaryStr(s.Description != "", " · ", ""),
		esc(time.UnixMilli(s.GeneratedAt).UTC().Format("2006-01-02 15:04 UTC")),
		s.Delivery.TotalRuns, s.Delivery.SucceededRuns, s.Delivery.FailedRuns, s.Delivery.WaitingRuns,
		esc(rate), esc(cycle),
		s.Delivery.TotalCostUSD, s.Delivery.TotalTokens,
		ternaryStr(s.Attention != nil && s.Attention.Counts.Total > 0, "gate", "good"),
		attentionCount(s.Attention), gateCount(s.Attention), questionCount(s.Attention),
	)

	if s.Attention != nil && s.Attention.Counts.Total > 0 {
		b.WriteString("<h2>Needs Human Attention</h2><div class=\"card\"><ul>")
		for _, g := range s.Attention.Gates {
			fmt.Fprintf(&b, "<li><strong class=\"gate\">Decision (%s)</strong> on <code>%s</code>: %s</li>", esc(g.Node), esc(g.FlowName), esc(oneLineClip(g.Prompt, 200)))
		}
		for _, f := range s.Attention.FailedRuns {
			fmt.Fprintf(&b, "<li><strong class=\"bad\">Failed</strong> <code>%s</code> (%s): %s</li>", esc(f.FlowName), esc(f.RunID), esc(oneLineClip(f.Error, 200)))
		}
		for _, sh := range s.Attention.ShippedUnmarked {
			fmt.Fprintf(&b, "<li><strong class=\"good\">Verify shipment</strong> %s (%s)</li>", esc(sh.Title), esc(sh.MapTitle))
		}
		for _, q := range s.Attention.OpenQuestions {
			fmt.Fprintf(&b, "<li><strong class=\"warn\">Open question</strong> %s: %s</li>", esc(q.Title), esc(strings.Join(q.Questions, " | ")))
		}
		b.WriteString("</ul></div>")
	}

	for _, m := range s.Maps {
		fmt.Fprintf(&b, "<h2>Product Map: %s <span class=\"muted\">(%d/%d done)</span></h2>", esc(m.Title), m.TasksDone, m.TasksTotal)
		if len(m.Metrics) > 0 {
			b.WriteString("<div class=\"grid\">")
			for _, mt := range m.Metrics {
				cur := "—"
				cls := ""
				if mt.Error != "" {
					cur = "error"
					cls = "bad"
				} else if mt.Value != nil {
					cur = fmtMetric(*mt.Value, mt.Unit)
					if mt.OnTarget != nil {
						cls = ternaryStr(*mt.OnTarget, "good", "bad")
					}
				}
				fmt.Fprintf(&b, "<div class=\"card\"><div class=\"muted\">%s · %s</div><div class=\"kpi %s\">%s</div>", esc(mt.Title), esc(mt.Kind), cls, esc(cur))
				if mt.Target != nil {
					dir := "≥"
					if mt.Direction == "down" {
						dir = "≤"
					}
					fmt.Fprintf(&b, "<div class=\"muted\">Target %s %s</div>", dir, esc(fmtMetric(*mt.Target, mt.Unit)))
				}
				if svg := renderSparklineSVG(mt.Series); svg != "" {
					b.WriteString(svg)
				}
				if mt.Detail != "" {
					fmt.Fprintf(&b, "<div class=\"muted\">%s</div>", esc(mt.Detail))
				}
				b.WriteString("</div>")
			}
			b.WriteString("</div>")
		}
		if len(m.Releases) > 0 {
			b.WriteString("<table><thead><tr><th>Release</th><th>Status</th><th>Progress</th><th>Goal</th></tr></thead><tbody>")
			for _, r := range m.Releases {
				fmt.Fprintf(&b, "<tr><td><strong>%s</strong></td><td><span class=\"pill\">%s</span></td><td>%d/%d</td><td>%s</td></tr>",
					esc(r.Title), esc(orNone(r.Status)), r.Done, r.Total, esc(r.Goal))
			}
			b.WriteString("</tbody></table>")
		}
		if len(m.RiskyBets) > 0 {
			b.WriteString("<h3>High-Uncertainty Bets</h3><div class=\"card\"><ul>")
			for _, bet := range m.RiskyBets {
				fmt.Fprintf(&b, "<li><strong>%s</strong> (value=%s, confidence=%s) %s</li>",
					esc(bet.Title), esc(orNone(bet.Value)), esc(orNone(bet.Confidence)), esc(bet.Hypothesis))
			}
			b.WriteString("</ul></div>")
		}
		if len(m.Outcomes) > 0 {
			b.WriteString("<h3>Recorded Post-Ship Outcomes</h3><div class=\"card\"><ul>")
			for _, o := range m.Outcomes {
				fmt.Fprintf(&b, "<li><strong>%s</strong> → <code>%s</code> %s</li>", esc(o.Title), esc(o.Outcome), esc(o.OutcomeNote))
			}
			b.WriteString("</ul></div>")
		}
	}

	if len(s.RecentRuns) > 0 {
		b.WriteString("<h2>Recent Runs</h2><table><thead><tr><th>Run</th><th>Task</th><th>Status</th><th>Cost</th><th>PR</th></tr></thead><tbody>")
		for _, r := range s.RecentRuns {
			pr := "—"
			if r.PRURL != "" {
				pr = fmt.Sprintf("<a href=\"%s\" target=\"_blank\" rel=\"noreferrer\">PR</a>", esc(r.PRURL))
			}
			fmt.Fprintf(&b, "<tr><td><code>%s</code></td><td>%s</td><td><span class=\"pill\">%s</span></td><td>$%.2f</td><td>%s</td></tr>",
				esc(r.RunID), esc(r.TaskTitle), esc(r.Status), r.CostUSD, pr)
		}
		b.WriteString("</tbody></table>")
	}

	b.WriteString("</div></body></html>\n")
	return b.String()
}

func renderSparklineSVG(pts [][2]float64) string {
	if len(pts) < 2 {
		return ""
	}
	x0, x1 := pts[0][0], pts[0][0]
	y0, y1 := pts[0][1], pts[0][1]
	for _, p := range pts[1:] {
		if p[0] < x0 {
			x0 = p[0]
		}
		if p[0] > x1 {
			x1 = p[0]
		}
		if p[1] < y0 {
			y0 = p[1]
		}
		if p[1] > y1 {
			y1 = p[1]
		}
	}
	dx, dy := x1-x0, y1-y0
	if dx == 0 {
		dx = 1
	}
	if dy == 0 {
		dy = 1
	}
	var d strings.Builder
	for i, p := range pts {
		x := (p[0] - x0) / dx * 120
		y := 26 - ((p[1]-y0)/dy)*24
		if i == 0 {
			fmt.Fprintf(&d, "M%.1f,%.1f", x, y)
		} else {
			fmt.Fprintf(&d, " L%.1f,%.1f", x, y)
		}
	}
	return fmt.Sprintf(`<svg class="spark" viewBox="0 0 120 28" preserveAspectRatio="none"><path d="%s"/></svg>`, d.String())
}

func fmtMetric(v float64, unit string) string {
	n := fmt.Sprintf("%.2f", v)
	n = strings.TrimRight(strings.TrimRight(n, "0"), ".")
	if unit == "%" {
		return n + "%"
	}
	if unit != "" {
		return n + " " + unit
	}
	return n
}

func ternaryStr(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}

func attentionCount(a *AttentionReport) int {
	if a == nil {
		return 0
	}
	return a.Counts.Total
}

func gateCount(a *AttentionReport) int {
	if a == nil {
		return 0
	}
	return a.Counts.Gates
}

func oneLineClip(s string, n int) string { return clip(oneLine(s), n) }

func questionCount(a *AttentionReport) int {
	if a == nil {
		return 0
	}
	return a.Counts.OpenQuestions
}
