import { useState } from "react";
import { Link } from "react-router-dom";
import { AlertTriangle, Check, CheckCircle2, Coins, FileDiff, Hand, HelpCircle, Play, RefreshCw, Sparkles, Wrench, X, XCircle } from "lucide-react";
import { api, ApiError, type AttentionFailedRun, type AttentionGate, type AttentionProposal } from "../api";
import DiffView, { diffRows } from "../DiffView";
import { useNow, useResource } from "../hooks";
import { timeAgo, usd } from "../format";
import { Empty, PRLink, Pill, Spinner, useToast } from "../ui";

const STRUCTURED_KEYS: { key: string; label: string }[] = [
  { key: "user_impact", label: "User impact" },
  { key: "value_summary", label: "Value summary" },
  { key: "recommendation", label: "Recommendation" },
  { key: "questions", label: "Questions" },
  { key: "alternatives", label: "Alternatives" },
  { key: "assumptions", label: "Assumptions" },
  { key: "gaps", label: "Gaps" },
  { key: "defects", label: "Defects" },
  { key: "comments", label: "Review findings" },
  { key: "caveats", label: "Caveats" },
];

/** Extracts human-scannable fields from a step's outputs for decision cards. */
export function decisionHighlights(outputs?: Record<string, unknown>): { label: string; items: string[] }[] {
  if (!outputs) return [];
  const out: { label: string; items: string[] }[] = [];
  for (const { key, label } of STRUCTURED_KEYS) {
    const val = outputs[key];
    if (typeof val === "string" && val.trim()) {
      out.push({ label, items: [val.trim()] });
    } else if (Array.isArray(val)) {
      const strs = val.map((x) => (typeof x === "string" ? x.trim() : JSON.stringify(x))).filter(Boolean);
      if (strs.length) out.push({ label, items: strs });
    }
  }
  return out;
}

export default function Inbox() {
  const [project, setProject] = useState("");
  const overview = useResource(() => api.overview(), []);
  const view = useResource(() => api.attention(project), [project], (e) => e.type === "run" || e.type === "task" || e.type === "visit" || e.type === "improvement");
  const now = useNow(15000);
  const d = view.data;
  const projects = overview.data?.projects ?? [];

  return (
    <div className="page">
      <div className="page-head">
        <h1>Inbox</h1>
        <span className="sub">Decisions, stalled runs, unverified shipments, and open product questions.</span>
        <span className="spacer" />
        {projects.length > 1 && (
          <select className="input" style={{ width: "auto", minWidth: 160 }} aria-label="Filter by project" value={project} onChange={(e) => setProject(e.target.value)}>
            <option value="">All projects</option>
            {projects.map((p) => (
              <option key={p.name} value={p.name}>
                {p.name}
              </option>
            ))}
          </select>
        )}
        <button className="btn sm" onClick={view.reload} aria-label="Refresh inbox">
          <RefreshCw />
          Refresh
        </button>
      </div>

      {view.error && <div className="error-box mb">{view.error}</div>}
      {!d ? (
        <div className="center-fill" style={{ height: 240 }}>
          <Spinner lg />
        </div>
      ) : d.counts.total === 0 ? (
        <div className="card">
          <Empty icon={CheckCircle2} title="All clear">
            <p>Nothing is waiting for your decision or review right now. Active flows will surface here whenever they need steering.</p>
          </Empty>
        </div>
      ) : (
        <div className="inbox-sections">
          {d.gates.length > 0 && (
            <section aria-label="Decisions needed" className="inbox-section">
              <div className="section-head">
                <Hand size={16} color="var(--gate)" />
                <h2>Decisions needed</h2>
                <span className="badge">{d.gates.length}</span>
              </div>
              <div className="stack" style={{ gap: 12 }}>
                {d.gates.map((g) => (
                  <GateCard key={`${g.run_id}-${g.seq}`} gate={g} now={now} onDecided={view.reload} />
                ))}
              </div>
            </section>
          )}

          {d.failed_runs.length > 0 && (
            <section aria-label="Failed runs" className="inbox-section">
              <div className="section-head">
                <XCircle size={16} color="var(--danger)" />
                <h2>Failed runs</h2>
                <span className="badge">{d.failed_runs.length}</span>
              </div>
              <div className="stack" style={{ gap: 10 }}>
                {d.failed_runs.map((f) => (
                  <FailedCard key={f.run_id} item={f} now={now} onResumed={view.reload} />
                ))}
              </div>
            </section>
          )}

          {d.proposals && d.proposals.length > 0 && (
            <section aria-label="Prompt improvement proposals" className="inbox-section">
              <div className="section-head">
                <Wrench size={16} color="var(--accent)" />
                <h2>Prompt improvement proposals</h2>
                <span className="badge">{d.proposals.length}</span>
                <span className="spacer" />
                <Link className="btn ghost sm" to="/improve">
                  Open Improve
                </Link>
              </div>
              <div className="stack" style={{ gap: 10 }}>
                {d.proposals.map((p) => (
                  <InboxProposalCard key={p.id} proposal={p} now={now} onDecided={view.reload} />
                ))}
              </div>
            </section>
          )}

          {d.shipped_unmarked.length > 0 && (
            <section aria-label="Shipped tasks to verify" className="inbox-section">
              <div className="section-head">
                <Sparkles size={16} color="var(--success)" />
                <h2>Shipped — verify outcome</h2>
                <span className="badge">{d.shipped_unmarked.length}</span>
              </div>
              <div className="stack" style={{ gap: 8 }}>
                {d.shipped_unmarked.map((s) => (
                  <div key={`${s.project}-${s.map_id}-${s.user_task_id}`} className="card card-pad row wrap">
                    <div className="grow" style={{ minWidth: 0 }}>
                      <div className="row wrap" style={{ gap: 6 }}>
                        <span className="chip">{s.project}</span>
                        <span className="small muted">{s.map_title}</span>
                        {s.release && <span className="chip">{s.release}</span>}
                      </div>
                      <div style={{ fontWeight: 600, marginTop: 4 }}>{s.title}</div>
                      {s.metrics && s.metrics.length > 0 && <div className="small muted">Targets metrics: {s.metrics.join(", ")}</div>}
                    </div>
                    <PRLink url={s.pr_url} />
                    <Link className="btn sm" to={`/runs/${s.run_id}`}>
                      Run
                    </Link>
                    <Link className="btn primary sm" to={`/products/${s.project}/maps/${s.map_id}`}>
                      Review on map
                    </Link>
                  </div>
                ))}
              </div>
            </section>
          )}

          {d.open_questions.length > 0 && (
            <section aria-label="Open product questions" className="inbox-section">
              <div className="section-head">
                <HelpCircle size={16} color="var(--warn)" />
                <h2>Open product questions</h2>
                <span className="badge">{d.open_questions.length}</span>
              </div>
              <div className="stack" style={{ gap: 8 }}>
                {d.open_questions.map((q) => (
                  <div key={`${q.project}-${q.map_id}-${q.kind}-${q.id}`} className="card card-pad">
                    <div className="row wrap" style={{ gap: 8, marginBottom: 6 }}>
                      <span className="chip">{q.project}</span>
                      <span className="chip">{q.kind}</span>
                      <b>{q.title}</b>
                      <span className="spacer" />
                      <Link className="btn sm" to={`/products/${q.project}/maps/${q.map_id}`}>
                        Open map
                      </Link>
                    </div>
                    <ul className="decision-list">
                      {q.questions.map((text) => (
                        <li key={text}>{text}</li>
                      ))}
                    </ul>
                  </div>
                ))}
              </div>
            </section>
          )}

          {d.plan_issues.length > 0 && (
            <section aria-label="Planning issues" className="inbox-section">
              <div className="section-head">
                <AlertTriangle size={16} color="var(--warn)" />
                <h2>Planning needs attention</h2>
                <span className="badge">{d.plan_issues.length}</span>
              </div>
              <div className="stack" style={{ gap: 8 }}>
                {d.plan_issues.map((p) => (
                  <div key={p.task_id} className="card card-pad row wrap">
                    <div className="grow" style={{ minWidth: 0 }}>
                      <div className="row wrap" style={{ gap: 6 }}>
                        <Pill status="plan_failed" label={p.status.replace(/_/g, " ")} />
                        <span className="chip">{p.project}</span>
                        <b>{p.title}</b>
                      </div>
                      {p.error && <div className="small muted clamp" style={{ marginTop: 4 }}>{p.error}</div>}
                    </div>
                    <Link className="btn sm" to={p.flow_name ? `/flows/${p.flow_name}` : `/?task=${p.task_id}`}>
                      {p.flow_name ? "Open flow" : "Open task"}
                    </Link>
                  </div>
                ))}
              </div>
            </section>
          )}
        </div>
      )}
    </div>
  );
}

function GateCard({ gate, now, onDecided }: { gate: AttentionGate; now: number; onDecided: () => void }) {
  const toast = useToast();
  const [note, setNote] = useState("");
  const [extend, setExtend] = useState(false);
  const [busy, setBusy] = useState<string>();
  const highlights = decisionHighlights(gate.prev?.outputs);
  const hasLoops = (gate.loops?.length ?? 0) > 0;
  const nearLimit = gate.loops?.some((l) => l.used >= l.max_visits) ?? false;

  const submit = async (outcome: string) => {
    setBusy(outcome);
    try {
      await api.decide(gate.run_id, gate.seq, outcome, note, extend);
      toast("ok", `Decided: ${outcome}`);
      onDecided();
    } catch (e) {
      toast("error", (e as Error).message);
    } finally {
      setBusy(undefined);
    }
  };

  return (
    <article className="card decision-card">
      <div className="row wrap" style={{ gap: 8 }}>
        <Pill status="waiting" label={gate.is_pause ? `paused before ${gate.next_node}` : gate.node} />
        {gate.project && <span className="chip">{gate.project}</span>}
        <Link className="mono small" to={`/runs/${gate.run_id}`}>
          {gate.run_id}
        </Link>
        <span className="small muted">{gate.task_title || gate.flow_name}</span>
        <span className="spacer" />
        <PRLink url={gate.pr_url} />
        <span className="small faint row" style={{ gap: 4 }}>
          <Coins size={12} />
          {usd(gate.cost_usd)}
        </span>
        {gate.diff?.files_changed ? (
          <span className="small faint row" style={{ gap: 4 }}>
            <FileDiff size={12} />
            {gate.diff.files_changed}f +{gate.diff.lines_added ?? 0}/-{gate.diff.lines_removed ?? 0}
          </span>
        ) : null}
        {gate.started_at ? <span className="small faint">waiting {timeAgo(gate.started_at, now)}</span> : null}
      </div>

      <div className="decision-prompt">{gate.prompt}</div>

      {gate.prev && (gate.prev.summary || highlights.length > 0) && (
        <div className="decision-context">
          <div className="small muted" style={{ marginBottom: 4 }}>
            From <span className="mono">{gate.prev.node}</span>
            {gate.prev.outcome ? ` → ${gate.prev.outcome}` : ""}
          </div>
          {gate.prev.summary && <div className="small">{gate.prev.summary}</div>}
          {highlights.map((h) => (
            <div key={h.label} className="decision-block">
              <div className="label">{h.label}</div>
              {h.items.length === 1 ? (
                <div className="small">{h.items[0]}</div>
              ) : (
                <ul className="decision-list">
                  {h.items.map((item, idx) => (
                    <li key={idx}>{item}</li>
                  ))}
                </ul>
              )}
            </div>
          ))}
        </div>
      )}

      {hasLoops && (
        <div className="row wrap small muted" style={{ gap: 10 }}>
          {gate.loops!.map((l) => (
            <span key={l.outcome} className="chip mono">
              {l.outcome} → {l.target}: {l.used}/{l.max_visits} visits
            </span>
          ))}
          <label className="row small" style={{ gap: 6, cursor: "pointer", color: nearLimit ? "var(--warn)" : undefined }}>
            <input type="checkbox" checked={extend} onChange={(e) => setExtend(e.target.checked)} />
            Grant +1 loop round {nearLimit ? "(limit reached)" : ""}
          </label>
        </div>
      )}

      <textarea
        className="input"
        rows={2}
        aria-label={`Direction note for ${gate.run_id}`}
        placeholder="Optional note or direction for the next step…"
        value={note}
        onChange={(e) => setNote(e.target.value)}
      />

      <div className="row wrap">
        {gate.outcomes.map((o, i) => (
          <button key={o} className={`btn sm${i === 0 ? " primary" : ""}`} disabled={!!busy} onClick={() => submit(o)}>
            {busy === o && <Spinner />}
            {o}
          </button>
        ))}
        <span className="spacer" />
        <Link className="btn ghost sm" to={`/runs/${gate.run_id}`}>
          Inspect run
        </Link>
      </div>
    </article>
  );
}

function FailedCard({ item, now, onResumed }: { item: AttentionFailedRun; now: number; onResumed: () => void }) {
  const toast = useToast();
  const [open, setOpen] = useState(false);
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState(false);

  const resume = async () => {
    setBusy(true);
    try {
      await api.resumeRun(item.run_id, item.last_node || "", note);
      toast("ok", `Resumed ${item.run_id}`);
      setOpen(false);
      setNote("");
      onResumed();
    } catch (e) {
      toast("error", (e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="card card-pad stack" style={{ gap: 8 }}>
      <div className="row wrap" style={{ gap: 8 }}>
        <Pill status="failed" />
        {item.project && <span className="chip">{item.project}</span>}
        <Link className="mono small" to={`/runs/${item.run_id}`}>
          {item.run_id}
        </Link>
        <b>{item.task_title || item.flow_name}</b>
        {item.last_node && <span className="chip mono">at {item.last_node}</span>}
        <span className="spacer" />
        <span className="small faint">{usd(item.cost_usd)}</span>
        {item.finished_at ? <span className="small faint">{timeAgo(item.finished_at, now)}</span> : null}
      </div>
      {item.error && <div className="small muted">{item.error}</div>}
      {!open ? (
        <div className="row wrap">
          <button className="btn sm" onClick={() => setOpen(true)}>
            <Play />
            Quick resume
          </button>
          <Link className="btn ghost sm" to={`/runs/${item.run_id}`}>
            Inspect run
          </Link>
        </div>
      ) : (
        <div className="stack" style={{ gap: 8 }}>
          <textarea
            className="input"
            rows={2}
            aria-label={`Resume note for ${item.run_id}`}
            placeholder="Direction for the resumed step (optional)…"
            value={note}
            onChange={(e) => setNote(e.target.value)}
          />
          <div className="row wrap">
            <button className="btn primary sm" disabled={busy} onClick={resume}>
              {busy ? <Spinner /> : <Play />}
              Resume at {item.last_node || "last step"}
            </button>
            <button className="btn sm" onClick={() => setOpen(false)}>
              Cancel
            </button>
          </div>
        </div>
      )}
    </div>
  );
}

function InboxProposalCard({ proposal, now, onDecided }: { proposal: AttentionProposal; now: number; onDecided: () => void }) {
  const toast = useToast();
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState<"apply" | "reject">();
  const rows = diffRows(proposal.before_text, proposal.after_text);

  const apply = async () => {
    setBusy("apply");
    try {
      try {
        await api.applyProposal(proposal.id, { note });
      } catch (e) {
        if (e instanceof ApiError && e.status === 409) {
          if (!window.confirm(`${e.message}.\n\nForce apply anyway?`)) return;
          await api.applyProposal(proposal.id, { note, force: true });
        } else {
          throw e;
        }
      }
      toast("ok", `Applied edit to ${proposal.target_kind}:${proposal.target}`);
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
      toast("ok", "Proposal rejected");
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
        <b>{proposal.title}</b>
        <span className="spacer" />
        <span className="small faint">{timeAgo(proposal.created_at, now)}</span>
      </div>
      <div className="small muted">{proposal.reason}</div>
      <div className="proposal-diff-box">
        <DiffView rows={rows} />
      </div>
      <div className="row wrap" style={{ gap: 8 }}>
        <input
          className="input grow"
          aria-label={`Proposal note for ${proposal.id}`}
          placeholder="Optional note…"
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
