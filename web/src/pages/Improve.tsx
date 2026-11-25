import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import {
  AlertTriangle,
  Check,
  CheckCircle2,
  ClipboardCheck,
  GitCommitHorizontal,
  Play,
  RefreshCw,
  ServerCrash,
  Sparkles,
  ThumbsDown,
  ThumbsUp,
  TrendingUp,
  Wrench,
  X,
  XCircle,
} from "lucide-react";
import { api, ApiError, type FailureCluster, type ImprovementProposal, type RunGrade, type TrendPoint } from "../api";
import DiffView, { diffRows } from "../DiffView";
import { timeAgo, tokens, usd } from "../format";
import { useResource } from "../hooks";
import { Empty, Pill, Spinner, useToast } from "../ui";

type TabID = "overview" | "proposals" | "themes" | "grades";

export default function Improve() {
  const [project, setProject] = useState("");
  const [tab, setTab] = useState<TabID>("overview");
  const [busyAction, setBusyAction] = useState<string>();
  const toast = useToast();

  const overview = useResource(() => api.overview(), []);
  const summary = useResource(
    () => api.improvementSummary(project),
    [project],
    (e) => e.type === "improvement" || e.type === "run" || e.type === "config",
  );
  const proposals = useResource(
    () => api.improvementProposals(),
    [],
    (e) => e.type === "improvement" || e.type === "config",
  );

  const [rubricFilter, setRubricFilter] = useState("");
  const [blameFilter, setBlameFilter] = useState("");
  const [passFilter, setPassFilter] = useState("");
  const grades = useResource(
    () => api.improvementGrades({ project, rubric: rubricFilter, blame: blameFilter, pass: passFilter, limit: 200 }),
    [project, rubricFilter, blameFilter, passFilter],
    (e) => e.type === "improvement",
  );

  const reloadAll = () => {
    summary.reload();
    proposals.reload();
    grades.reload();
  };

  const gradeUngraded = async () => {
    setBusyAction("grade");
    try {
      const r = await api.gradeUngraded(project, 10);
      if (r.queued.length === 0) {
        toast("ok", "No ungraded finished runs left");
      } else {
        toast("ok", `Queued grading for ${r.queued.length} run(s)`);
      }
      reloadAll();
    } catch (e) {
      toast("error", (e as Error).message);
    } finally {
      setBusyAction(undefined);
    }
  };

  const runImprover = async (minFailures?: number) => {
    setBusyAction("improver");
    try {
      const r = await api.runImprover({ project: project || undefined, min_failures: minFailures });
      if (r.proposals_created > 0) {
        toast("ok", `Drafted ${r.proposals_created} proposal(s) for review`);
        setTab("proposals");
      } else if (r.clusters_checked === 0) {
        toast("ok", "No eligible agent failure clusters reached the threshold");
      } else {
        toast("ok", `Checked ${r.clusters_checked} cluster(s); no prompt edit warranted`);
      }
      reloadAll();
    } catch (e) {
      toast("error", (e as Error).message);
    } finally {
      setBusyAction(undefined);
    }
  };

  const projects = overview.data?.projects ?? [];
  const d = summary.data;
  const allProps = proposals.data ?? [];
  const pendingProps = allProps.filter((p) => p.status === "pending");
  const pastProps = allProps.filter((p) => p.status !== "pending");

  return (
    <div className="page">
      <div className="page-head">
        <h1>Improve</h1>
        <span className="sub">Automatic run grading, agent vs. environment blame, and human-approved prompt edits.</span>
        <span className="spacer" />
        {projects.length > 1 && (
          <select
            className="input"
            style={{ width: "auto", minWidth: 150 }}
            aria-label="Filter by project"
            value={project}
            onChange={(e) => setProject(e.target.value)}
          >
            <option value="">All projects</option>
            {projects.map((p) => (
              <option key={p.name} value={p.name}>
                {p.name}
              </option>
            ))}
          </select>
        )}
        <button className="btn sm" disabled={!!busyAction} onClick={gradeUngraded} title="Grade up to 10 recent ungraded finished runs">
          {busyAction === "grade" ? <Spinner /> : <ClipboardCheck />}
          Grade ungraded{d?.stats.ungraded_runs ? ` (${d.stats.ungraded_runs})` : ""}
        </button>
        <button className="btn primary sm" disabled={!!busyAction} onClick={() => runImprover()}>
          {busyAction === "improver" ? <Spinner /> : <Sparkles />}
          Run improver
        </button>
        <button className="btn sm" onClick={reloadAll} aria-label="Refresh improvement data">
          <RefreshCw />
        </button>
      </div>

      {summary.error && <div className="error-box mb">{summary.error}</div>}

      <div className="tabs mb" role="tablist">
        <button role="tab" aria-selected={tab === "overview"} className={`tab${tab === "overview" ? " active" : ""}`} onClick={() => setTab("overview")}>
          <TrendingUp size={14} />
          Overview &amp; Trends
        </button>
        <button role="tab" aria-selected={tab === "proposals"} className={`tab${tab === "proposals" ? " active" : ""}`} onClick={() => setTab("proposals")}>
          <Wrench size={14} />
          Proposals
          {pendingProps.length > 0 && <span className="badge">{pendingProps.length}</span>}
        </button>
        <button role="tab" aria-selected={tab === "themes"} className={`tab${tab === "themes" ? " active" : ""}`} onClick={() => setTab("themes")}>
          <AlertTriangle size={14} />
          Failure themes
          {d && d.agent_clusters.length + d.environment_clusters.length > 0 && (
            <span className="badge">{d.agent_clusters.length + d.environment_clusters.length}</span>
          )}
        </button>
        <button role="tab" aria-selected={tab === "grades"} className={`tab${tab === "grades" ? " active" : ""}`} onClick={() => setTab("grades")}>
          <ClipboardCheck size={14} />
          Graded runs
          {d?.stats.total_grades ? <span className="badge">{d.stats.total_grades}</span> : null}
        </button>
      </div>

      {!d ? (
        <div className="center-fill" style={{ height: 240 }}>
          <Spinner lg />
        </div>
      ) : tab === "overview" ? (
        <div className="stack" style={{ gap: 18 }}>
          <div className="kpi-grid">
            <div className="card card-pad">
              <div className="label">Outcome rate</div>
              <div className="kpi-val">{d.stats.finished_runs ? `${d.stats.outcome_rate_pct}%` : "—"}</div>
              <div className="small muted">
                {d.stats.succeeded_runs} succeeded / {d.stats.finished_runs} finished
              </div>
            </div>
            <div className="card card-pad">
              <div className="label">Rubric pass rate</div>
              <div className="kpi-val">{d.stats.total_grades ? `${d.stats.pass_rate_pct}%` : "—"}</div>
              <div className="small muted">
                {d.stats.passed_grades}/{d.stats.total_grades} grades across {d.stats.graded_runs} runs
              </div>
            </div>
            <div className="card card-pad">
              <div className="label">Failure blame split</div>
              <div className="kpi-val">
                {d.stats.agent_failures} <span className="small muted">agent</span> · {d.stats.environment_failures} <span className="small muted">env</span>
              </div>
              <div className="small muted">
                {d.stats.disagreed_grades > 0 ? `${d.stats.disagreed_grades} disagreed (excluded)` : "Env failures excluded from prompts"}
              </div>
            </div>
            <div className="card card-pad">
              <div className="label">Proposals</div>
              <div className="kpi-val">{d.stats.pending_proposals} pending</div>
              <div className="small muted">
                {d.stats.applied_proposals} applied · {d.stats.rejected_proposals} rejected
              </div>
            </div>
            <div className="card card-pad">
              <div className="label">Grader cost</div>
              <div className="kpi-val">{usd(d.stats.grader_cost_usd)}</div>
              <div className="small muted">{tokens(d.stats.grader_tokens)} tokens</div>
            </div>
          </div>

          <div className="card card-pad row wrap small muted" style={{ gap: 8 }}>
            <span className={`chip${d.config.auto_grade ? " outcome-validated" : ""}`}>
              auto-grade: {d.config.auto_grade ? `on (${d.config.sample_pct}%)` : "off"}
            </span>
            {d.config.model && <span className="chip mono">grader: {d.config.model}</span>}
            {d.config.improver_model && d.config.improver_model !== d.config.model && (
              <span className="chip mono">improver: {d.config.improver_model}</span>
            )}
            <span className="chip">min failures: {d.config.min_failures}</span>
            {d.config.schedule_interval && <span className="chip">schedule: every {d.config.schedule_interval}</span>}
            {d.last_improver_run && (
              <span className="chip">
                last improver: {timeAgo(d.last_improver_run.finished_at || d.last_improver_run.started_at)} ({d.last_improver_run.proposals_created} drafted)
              </span>
            )}
            <span className="spacer" />
            <Link className="btn ghost sm" to="/settings">
              Configure in Settings
            </Link>
          </div>

          <section className="card card-pad stack" style={{ gap: 12 }}>
            <div className="row wrap">
              <h2 className="h2-sm">Score &amp; outcome trend</h2>
              <span className="spacer" />
              <div className="row small muted" style={{ gap: 12 }}>
                <span className="row" style={{ gap: 5 }}>
                  <i className="swatch" style={{ background: "var(--accent)" }} /> Outcome rate
                </span>
                <span className="row" style={{ gap: 5 }}>
                  <i className="swatch" style={{ background: "var(--success)" }} /> Rubric pass rate
                </span>
                <span className="row" style={{ gap: 5 }}>
                  <i className="swatch" style={{ background: "var(--gate)" }} /> Prompt edit
                </span>
              </div>
            </div>
            <TrendChart points={d.trend} />
          </section>

          <section className="card card-pad stack" style={{ gap: 12 }}>
            <div className="row">
              <h2 className="h2-sm">Rubrics</h2>
              <span className="small muted">High disagreement flags a rubric that may need tuning.</span>
            </div>
            <div className="improve-rubrics">
              {d.rubrics.map((rb) => (
                <div key={rb.name} className="rubric-card">
                  <div className="row wrap" style={{ gap: 6 }}>
                    <b className="mono">{rb.name}</b>
                    <span className="chip mono small">v:{rb.version}</span>
                    <span className="spacer" />
                    <span className="mono small" style={{ fontWeight: 650 }}>
                      {rb.total ? `${rb.pass_rate_pct}%` : "—"}
                    </span>
                  </div>
                  {rb.description && <div className="small muted">{rb.description}</div>}
                  <div className="rubric-bar" aria-hidden="true">
                    <div className="rubric-bar-fill" style={{ width: `${rb.total ? rb.pass_rate_pct : 0}%` }} />
                  </div>
                  <div className="row wrap small faint" style={{ gap: 8 }}>
                    <span>
                      {rb.passed} pass / {rb.failed} fail
                    </span>
                    {rb.agent_failures > 0 && <span className="blame-text-agent">{rb.agent_failures} agent</span>}
                    {rb.environment_failures > 0 && <span className="blame-text-env">{rb.environment_failures} env</span>}
                    {rb.disagreed > 0 && (
                      <span style={{ color: rb.disagreement_rate_pct >= 20 ? "var(--warn)" : undefined }}>
                        {rb.disagreed} disagreed ({rb.disagreement_rate_pct}%)
                      </span>
                    )}
                    {rb.audited > 0 && (
                      <span title="Grades you confirmed or disagreed with; kappa is the agreement beyond chance (10 audits and 0.6 count as calibrated)">
                        {rb.audited} audited{rb.cohens_kappa !== undefined ? `, kappa ${rb.cohens_kappa}` : ""}
                        {rb.calibrated ? " (calibrated)" : ""}
                      </span>
                    )}
                  </div>
                </div>
              ))}
            </div>
          </section>

          <section className="card card-pad stack" style={{ gap: 12 }}>
            <div className="row wrap">
              <h2 className="h2-sm">Scores by prompt version</h2>
              <span className="small muted">Compare outcome and rubric pass rates before and after prompt edits.</span>
            </div>
            {d.target_versions.length === 0 ? (
              <div className="small muted">Grade finished runs to track scores against pinned prompt versions.</div>
            ) : (
              <div className="table-scroll">
                <table className="improve-table">
                  <thead>
                    <tr>
                      <th>Target</th>
                      <th>Version</th>
                      <th>Runs</th>
                      <th>Outcome rate</th>
                      <th>Rubric pass</th>
                      <th>Change note</th>
                    </tr>
                  </thead>
                  <tbody>
                    {d.target_versions.flatMap((t) =>
                      t.versions.map((v, idx) => (
                        <tr key={`${t.target_kind}:${t.target}:${v.version_hash}`}>
                          <td>
                            {idx === 0 ? (
                              <span className="mono small">
                                <span className="muted">{t.target_kind}:</span>
                                {t.target}
                              </span>
                            ) : null}
                          </td>
                          <td>
                            <span className="chip mono">{v.version_hash}</span>
                            {v.current && (
                              <span className="chip outcome-validated" style={{ marginLeft: 4 }}>
                                live
                              </span>
                            )}
                          </td>
                          <td className="mono small">{v.runs}</td>
                          <td className="mono small">
                            {v.outcome_rate_pct}% <span className="faint">({v.succeeded}/{v.runs})</span>
                          </td>
                          <td className="mono small">
                            {v.grades ? (
                              <>
                                {v.pass_rate_pct}% <span className="faint">({v.passed}/{v.grades})</span>
                              </>
                            ) : (
                              "—"
                            )}
                          </td>
                          <td className="small muted">
                            {v.note || (v.author ? `edited by ${v.author}` : "—")}
                          </td>
                        </tr>
                      )),
                    )}
                  </tbody>
                </table>
              </div>
            )}
          </section>
        </div>
      ) : tab === "proposals" ? (
        <div className="stack" style={{ gap: 18 }}>
          {pendingProps.length === 0 ? (
            <div className="card">
              <Empty icon={CheckCircle2} title="No pending proposals">
                <p>
                  When at least {d.config.min_failures} runs fail for a cause in the same prompt, the improver drafts a targeted prompt edit here
                  for your approval.
                </p>
              </Empty>
            </div>
          ) : (
            <div className="stack" style={{ gap: 12 }}>
              {pendingProps.map((p) => (
                <ProposalCard key={p.id} proposal={p} onDecided={reloadAll} />
              ))}
            </div>
          )}

          {pastProps.length > 0 && (
            <section className="stack" style={{ gap: 10 }}>
              <h2 className="h2-sm muted">Proposal history</h2>
              {pastProps.map((p) => (
                <div key={p.id} className="card card-pad stack" style={{ gap: 6 }}>
                  <div className="row wrap" style={{ gap: 8 }}>
                    <Pill status={p.status === "applied" ? "succeeded" : "canceled"} label={p.status} />
                    <span className="chip mono">
                      {p.target_kind}:{p.target}
                    </span>
                    {p.theme && <span className="chip">{p.theme}</span>}
                    <b>{p.title}</b>
                    <span className="spacer" />
                    <span className="small faint">
                      {p.decided_by ? `${p.decided_by} · ` : ""}
                      {timeAgo(p.decided_at || p.created_at)}
                    </span>
                  </div>
                  {p.decision_note && <div className="small muted">Note: {p.decision_note}</div>}
                  <div className="small faint">
                    Evidence runs:{" "}
                    {p.evidence_runs.map((id, i) => (
                      <span key={id}>
                        {i > 0 ? ", " : ""}
                        <Link className="mono" to={`/runs/${id}`}>
                          {id}
                        </Link>
                      </span>
                    ))}
                  </div>
                </div>
              ))}
            </section>
          )}
        </div>
      ) : tab === "themes" ? (
        <div className="stack" style={{ gap: 20 }}>
          <section className="stack" style={{ gap: 10 }}>
            <div className="row wrap">
              <Wrench size={16} color="var(--accent)" />
              <h2 className="h2-sm">Agent failure clusters</h2>
              <span className="small muted">Unreviewed agent-blamed failures eligible for prompt edits at {d.config.min_failures}+ runs.</span>
            </div>
            {d.agent_clusters.length === 0 ? (
              <div className="card card-pad small muted">No unreviewed agent-blamed failures right now.</div>
            ) : (
              d.agent_clusters.map((cl) => (
                <ClusterCard key={`${cl.target_kind}:${cl.target}:${cl.theme}`} cluster={cl} onDraft={() => runImprover(Math.min(cl.run_ids.length, d.config.min_failures))} busy={busyAction === "improver"} />
              ))
            )}
          </section>

          <section className="stack" style={{ gap: 10 }}>
            <div className="row wrap">
              <ServerCrash size={16} color="var(--warn)" />
              <h2 className="h2-sm">Environment &amp; infrastructure failures</h2>
              <span className="small muted">Excluded from prompt edits — fix cluster, network, credentials, or upstream reliability instead.</span>
            </div>
            {d.environment_clusters.length === 0 ? (
              <div className="card card-pad small muted">No environment-blamed failures recorded.</div>
            ) : (
              d.environment_clusters.map((cl) => <ClusterCard key={`env:${cl.target_kind}:${cl.target}:${cl.theme}`} cluster={cl} />)
            )}
          </section>
        </div>
      ) : (
        <div className="stack" style={{ gap: 12 }}>
          <div className="row wrap" style={{ gap: 8 }}>
            <select className="input" style={{ width: "auto" }} aria-label="Filter by rubric" value={rubricFilter} onChange={(e) => setRubricFilter(e.target.value)}>
              <option value="">All rubrics</option>
              {d.config.rubrics.map((r) => (
                <option key={r.name} value={r.name}>
                  {r.name}
                </option>
              ))}
            </select>
            <select className="input" style={{ width: "auto" }} aria-label="Filter by verdict" value={passFilter} onChange={(e) => setPassFilter(e.target.value)}>
              <option value="">Pass &amp; fail</option>
              <option value="false">Failed only</option>
              <option value="true">Passed only</option>
            </select>
            <select className="input" style={{ width: "auto" }} aria-label="Filter by blame" value={blameFilter} onChange={(e) => setBlameFilter(e.target.value)}>
              <option value="">All blame</option>
              <option value="agent">Agent</option>
              <option value="environment">Environment</option>
            </select>
          </div>
          {!grades.data ? (
            <Spinner />
          ) : grades.data.length === 0 ? (
            <div className="card card-pad small muted">No grades match these filters.</div>
          ) : (
            <div className="stack" style={{ gap: 8 }}>
              {grades.data.map((g) => (
                <GradeRow key={g.id} grade={g} onChanged={reloadAll} />
              ))}
            </div>
          )}
        </div>
      )}
    </div>
  );
}

function TrendChart({ points }: { points: TrendPoint[] }) {
  if (points.length === 0) {
    return <div className="small muted">No finished runs yet to plot.</div>;
  }
  const w = 680;
  const h = 175;
  const padL = 36;
  const padR = 16;
  const padT = 16;
  const padB = 28;
  const plotW = w - padL - padR;
  const plotH = h - padT - padB;
  const xOf = (i: number) => (points.length === 1 ? padL + plotW / 2 : padL + (i / (points.length - 1)) * plotW);
  const yOf = (pct: number) => padT + plotH - (Math.max(0, Math.min(100, pct)) / 100) * plotH;

  const outcomePts = points.filter((p) => p.runs > 0).map((p) => ({ x: xOf(points.indexOf(p)), y: yOf(p.outcome_rate_pct), p }));
  const gradePts = points.filter((p) => p.grades > 0).map((p) => ({ x: xOf(points.indexOf(p)), y: yOf(p.pass_rate_pct), p }));
  const line = (pts: { x: number; y: number }[]) => pts.map((pt, i) => `${i === 0 ? "M" : "L"}${pt.x.toFixed(1)},${pt.y.toFixed(1)}`).join(" ");

  const revMarkers = points.flatMap((p, i) => (p.revisions?.length ? [{ x: xOf(i), date: p.date, revs: p.revisions }] : []));

  return (
    <div className="stack" style={{ gap: 8 }}>
      <svg className="trend-chart" viewBox={`0 0 ${w} ${h}`} role="img" aria-label="Outcome rate and rubric pass rate over time">
        {[0, 50, 100].map((tick) => (
          <g key={tick}>
            <line x1={padL} x2={w - padR} y1={yOf(tick)} y2={yOf(tick)} stroke="var(--border)" strokeDasharray={tick === 0 ? undefined : "3 3"} />
            <text x={padL - 6} y={yOf(tick) + 4} textAnchor="end" fontSize="10" fill="var(--faint)">
              {tick}%
            </text>
          </g>
        ))}
        {revMarkers.map((m) => (
          <g key={m.date}>
            <line x1={m.x} x2={m.x} y1={padT} y2={h - padB} stroke="var(--gate)" strokeWidth="1.5" strokeDasharray="3 3" />
            <title>
              {m.date}: {m.revs.map((r) => `${r.target_kind}:${r.target} -> ${r.version_hash}`).join(", ")}
            </title>
          </g>
        ))}
        {outcomePts.length > 1 && <path d={line(outcomePts)} fill="none" stroke="var(--accent)" strokeWidth="2" />}
        {gradePts.length > 1 && <path d={line(gradePts)} fill="none" stroke="var(--success)" strokeWidth="2" />}
        {outcomePts.map((pt) => (
          <circle key={`o-${pt.p.date}`} cx={pt.x} cy={pt.y} r="3.5" fill="var(--accent)">
            <title>
              {pt.p.date}: outcome {pt.p.outcome_rate_pct}% ({pt.p.succeeded}/{pt.p.runs} runs)
            </title>
          </circle>
        ))}
        {gradePts.map((pt) => (
          <circle key={`g-${pt.p.date}`} cx={pt.x} cy={pt.y} r="3.5" fill="var(--success)">
            <title>
              {pt.p.date}: rubric pass {pt.p.pass_rate_pct}% ({pt.p.passed}/{pt.p.grades} grades)
            </title>
          </circle>
        ))}
        {points.map((p, i) => {
          const show = points.length <= 7 || i === 0 || i === points.length - 1 || i % Math.ceil(points.length / 6) === 0;
          if (!show) return null;
          return (
            <text key={`x-${p.date}`} x={xOf(i)} y={h - 8} textAnchor="middle" fontSize="10" fill="var(--faint)">
              {p.date.slice(5)}
            </text>
          );
        })}
      </svg>
      {revMarkers.length > 0 && (
        <div className="row wrap small muted" style={{ gap: 6 }}>
          <GitCommitHorizontal size={13} color="var(--gate)" />
          <span>Prompt edits:</span>
          {revMarkers.flatMap((m) =>
            m.revs.map((r) => (
              <span key={r.id} className="chip mono">
                {m.date.slice(5)} {r.target_kind}:{r.target} ({r.version_hash})
              </span>
            )),
          )}
        </div>
      )}
    </div>
  );
}

export function ProposalCard({ proposal, onDecided }: { proposal: ImprovementProposal; onDecided: () => void }) {
  const toast = useToast();
  const [editing, setEditing] = useState(false);
  const [afterText, setAfterText] = useState(proposal.after_text);
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState<"apply" | "reject">();
  const rows = useMemo(() => diffRows(proposal.before_text, afterText), [proposal.before_text, afterText]);

  const apply = async () => {
    setBusy("apply");
    try {
      const edited = afterText.trim() !== proposal.after_text.trim() ? afterText : undefined;
      let res: { proposal: ImprovementProposal; warnings: string[] };
      try {
        res = await api.applyProposal(proposal.id, { note, after_text: edited });
      } catch (e) {
        if (e instanceof ApiError && e.status === 409) {
          if (!window.confirm(`${e.message}.\n\nForce apply anyway?`)) return;
          res = await api.applyProposal(proposal.id, { note, after_text: edited, force: true });
        } else {
          throw e;
        }
      }
      toast("ok", `Applied to ${proposal.target_kind}:${proposal.target}`);
      res.warnings.forEach((w) => toast("error", w));
      onDecided();
    } catch (e) {
      toast("error", (e as Error).message);
    } finally {
      setBusy(undefined);
    }
  };

  const reject = async () => {
    setBusy("reject");
    try {
      await api.rejectProposal(proposal.id, note);
      toast("ok", "Proposal rejected and remembered");
      onDecided();
    } catch (e) {
      toast("error", (e as Error).message);
    } finally {
      setBusy(undefined);
    }
  };

  return (
    <article className="card proposal-card">
      <div className="row wrap" style={{ gap: 8 }}>
        <span className="chip mono">
          {proposal.target_kind}:{proposal.target}
        </span>
        {proposal.theme && <span className="chip blame-agent">{proposal.theme}</span>}
        <b className="grow">{proposal.title}</b>
        <span className="small faint">{timeAgo(proposal.created_at)}</span>
      </div>
      <div className="small">{proposal.reason}</div>
      <div className="row wrap small muted" style={{ gap: 6 }}>
        <span>Cited runs ({proposal.evidence_runs.length}):</span>
        {proposal.evidence_runs.map((id) => (
          <Link key={id} className="chip mono" to={`/runs/${id}`}>
            {id}
          </Link>
        ))}
        <span className="spacer" />
        <button className="btn ghost sm" onClick={() => setEditing((v) => !v)}>
          {editing ? "Show diff" : "Edit text"}
        </button>
      </div>
      {editing ? (
        <textarea
          className="input mono"
          rows={10}
          aria-label={`Proposed text for ${proposal.id}`}
          value={afterText}
          onChange={(e) => setAfterText(e.target.value)}
        />
      ) : (
        <div className="proposal-diff-box">
          <DiffView rows={rows} />
        </div>
      )}
      <div className="row wrap" style={{ gap: 8 }}>
        <input
          className="input grow"
          aria-label={`Decision note for ${proposal.id}`}
          placeholder="Optional note (rejection reasons are remembered by future improver runs)…"
          value={note}
          onChange={(e) => setNote(e.target.value)}
        />
        <button className="btn primary sm" disabled={!!busy} onClick={apply}>
          {busy === "apply" ? <Spinner /> : <Check />}
          Apply
        </button>
        <button className="btn sm" disabled={!!busy} onClick={reject}>
          {busy === "reject" ? <Spinner /> : <X />}
          Reject
        </button>
      </div>
    </article>
  );
}

function ClusterCard({ cluster, onDraft, busy }: { cluster: FailureCluster; onDraft?: () => void; busy?: boolean }) {
  const pct = Math.min(100, Math.round((cluster.run_ids.length / Math.max(1, cluster.min_required)) * 100));
  return (
    <div className="card card-pad stack" style={{ gap: 8 }}>
      <div className="row wrap" style={{ gap: 8 }}>
        <span className={`chip ${cluster.blame === "environment" ? "blame-env" : "blame-agent"}`}>{cluster.blame}</span>
        <span className="chip mono">
          {cluster.target_kind}:{cluster.target}
        </span>
        <b className="mono">{cluster.theme}</b>
        {cluster.themes.length > 1 && (
          <span className="chip" title={cluster.themes.join(", ")}>
            +{cluster.themes.length - 1} theme{cluster.themes.length === 2 ? "" : "s"}
          </span>
        )}
        {cluster.rubrics.map((rb) => (
          <span key={rb} className="chip">
            {rb}
          </span>
        ))}
        <span className="spacer" />
        <span className="mono small">
          {cluster.run_ids.length} run{cluster.run_ids.length === 1 ? "" : "s"}
          {cluster.blame === "agent" ? ` / ${cluster.min_required}` : ""}
        </span>
      </div>
      {cluster.blame === "agent" && (
        <div className="rubric-bar" aria-hidden="true">
          <div className="rubric-bar-fill" style={{ width: `${pct}%`, background: cluster.eligible ? "var(--accent)" : "var(--muted)" }} />
        </div>
      )}
      <div className="small muted">{cluster.sample_reason}</div>
      <div className="row wrap small" style={{ gap: 6 }}>
        {cluster.run_ids.slice(0, 6).map((id) => (
          <Link key={id} className="chip mono" to={`/runs/${id}`}>
            {id}
          </Link>
        ))}
        <span className="spacer" />
        {cluster.blame === "agent" && cluster.editable && onDraft && (
          <button className="btn sm" disabled={busy} onClick={onDraft}>
            {busy ? <Spinner /> : <Play />}
            Draft proposal
          </button>
        )}
      </div>
    </div>
  );
}

function GradeRow({ grade, onChanged }: { grade: RunGrade; onChanged: () => void }) {
  const toast = useToast();
  const [open, setOpen] = useState(false);
  const [note, setNote] = useState(grade.disagreed_note ?? "");
  const [busy, setBusy] = useState(false);

  const toggle = async (disagreed: boolean) => {
    setBusy(true);
    try {
      await api.disagreeGrade(grade.id, disagreed, note);
      toast("ok", disagreed ? "Marked disagreed (excluded from improver)" : "Cleared disagreement");
      setOpen(false);
      onChanged();
    } catch (e) {
      toast("error", (e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const confirm = async () => {
    setBusy(true);
    try {
      await api.confirmGrade(grade.id);
      toast("ok", "Grade confirmed");
      onChanged();
    } catch (e) {
      toast("error", (e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="card card-pad stack" style={{ gap: 6 }}>
      <div className="row wrap" style={{ gap: 8 }}>
        <Pill status={grade.pass ? "succeeded" : "failed"} label={grade.pass ? "pass" : "fail"} />
        <span className="chip mono">{grade.rubric}</span>
        {!grade.pass && grade.blame && <span className={`chip ${grade.blame === "environment" ? "blame-env" : "blame-agent"}`}>{grade.blame}</span>}
        {grade.theme && <span className="chip">{grade.theme}</span>}
        {grade.target && (
          <span className="chip mono">
            {grade.target_kind}:{grade.target}
          </span>
        )}
        <Link className="mono small" to={`/runs/${grade.run_id}`}>
          {grade.run_id}
        </Link>
        {grade.flow_name && <span className="small muted">{grade.flow_name}</span>}
        <span className="spacer" />
        {grade.disagreed && (
          <span className="chip outcome-inconclusive" title={grade.disagreed_note || "Human disagreed with this grade"}>
            disagreed
          </span>
        )}
        <span className="small faint">{timeAgo(grade.created_at)}</span>
        {!grade.disagreed && (
          <button className="btn ghost sm" disabled={busy} onClick={confirm}>
            <ThumbsUp size={12} />
            Confirm
          </button>
        )}
        <button className="btn ghost sm" disabled={busy} onClick={() => (grade.disagreed ? toggle(false) : setOpen((v) => !v))}>
          <ThumbsDown size={12} />
          {grade.disagreed ? "Undo" : "Disagree"}
        </button>
      </div>
      <div className="small">{grade.reason}</div>
      {grade.disagreed && grade.disagreed_note && <div className="small muted">Disagreement note: {grade.disagreed_note}</div>}
      {open && (
        <div className="row wrap" style={{ gap: 8 }}>
          <input
            className="input grow"
            aria-label={`Disagreement note for ${grade.id}`}
            placeholder="Why do you disagree with this grade? (optional)"
            value={note}
            onChange={(e) => setNote(e.target.value)}
          />
          <button className="btn sm primary" disabled={busy} onClick={() => toggle(true)}>
            {busy ? <Spinner /> : <XCircle size={13} />}
            Confirm disagree
          </button>
          <button className="btn sm" onClick={() => setOpen(false)}>
            Cancel
          </button>
        </div>
      )}
    </div>
  );
}
