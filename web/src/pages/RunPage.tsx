import { useEffect, useMemo, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { Clock, Coins, Cpu, FileDiff, GitCommit, Hand, History, ListOrdered, RotateCcw, ScrollText, Square, X } from "lucide-react";
import { api, type Graph, type Visit } from "../api";
import { useEvents, useNow, useResource } from "../hooks";
import { clock, duration, timeAgo, tokens, usd } from "../format";
import FlowGraph, { type RunOverlay } from "../graph/FlowGraph";
import { edgeId } from "../graph/layout";
import Transcript from "../run/Transcript";
import { BranchChip, PRLink, Pill, SourceChip, Spinner, StatusIcon, useToast } from "../ui";

export default function RunPage() {
  const { id = "" } = useParams();
  const view = useResource(() => api.run(id), [id], (e) => (e.type === "run" || e.type === "visit") && e.id === id);
  const [progress, setProgress] = useState<Record<number, string>>({});
  const [selectedSeq, setSelectedSeq] = useState<number | null>(null);
  const [tab, setTab] = useState<"steps" | "events">("steps");
  const toast = useToast();
  const navigate = useNavigate();
  const now = useNow(1000);

  useEvents((e) => {
    if (e.type === "progress" && e.id === id && e.seq) setProgress((p) => ({ ...p, [e.seq!]: e.text ?? "" }));
  });

  const data = view.data;
  const visits = useMemo(() => (data?.visits ?? []).map((v) => ({ ...v, progress: progress[v.seq] ?? v.progress })), [data?.visits, progress]);
  const overlay = useMemo(() => (data?.graph ? buildOverlay(data.graph, visits, data.run.status) : undefined), [data?.graph, visits, data?.run.status]);
  const waiting = visits.find((v) => v.status === "waiting");
  const selected = visits.find((v) => v.seq === selectedSeq) ?? null;

  // Follow the active step unless the user picked one.
  const [pinned, setPinned] = useState(false);
  useEffect(() => {
    setPinned(false);
    setSelectedSeq(null);
    setProgress({});
  }, [id]);
  useEffect(() => {
    if (pinned || !visits.length) return;
    setSelectedSeq(visits[visits.length - 1].seq);
  }, [visits, pinned]);

  if (view.error && !data) {
    return (
      <div className="page">
        <div className="error-box">{view.error}</div>
      </div>
    );
  }
  if (!data) {
    return (
      <div className="center-fill">
        <Spinner lg />
      </div>
    );
  }
  const { run, task } = data;
  const active = run.status === "running" || run.status === "waiting" || run.status === "queued";

  return (
    <div className="workspace">
      <div className="workbar">
        <div className="crumb">
          <Link to="/runs">Runs</Link>/
        </div>
        <h1 className="mono">{run.id}</h1>
        <Pill status={run.status} />
        <Link className="chip mono" to={`/flows/${run.flow_name}?v=${run.flow_version}`} title="Flow version this run pinned">
          {run.flow_name} v{run.flow_version}
        </Link>
        {task && (
          <span className="row small" style={{ gap: 6, minWidth: 0 }}>
            <SourceChip source={task.source} identifier={task.identifier} url={task.url} />
          </span>
        )}
        <PRLink url={run.pr_url} />
        <span className="spacer" />
        <div className="stats">
          <span title="Duration">
            <Clock />
            <b>{duration(run.started_at || run.created_at, run.finished_at, now)}</b>
          </span>
          <span title="Tokens">
            <Cpu />
            <b>{tokens(run.tokens)}</b> tokens
          </span>
          <span title="Cost">
            <Coins />
            <b>{usd(run.cost_usd)}</b>
          </span>
          {run.diff.files_changed ? (
            <span title="Diff against base">
              <FileDiff />
              <b>{run.diff.files_changed}</b> files, <b style={{ color: "var(--success)" }}>+{run.diff.lines_added}</b>/<b style={{ color: "var(--danger)" }}>-{run.diff.lines_removed}</b>
            </span>
          ) : null}
        </div>
        {!active && (
          <button
            className="btn sm"
            title={`Start a new run of ${run.flow_name} v${run.flow_version}`}
            onClick={async () => {
              try {
                const r = await api.startRun(run.flow_name, run.flow_version);
                navigate(`/runs/${r.id}`);
              } catch (e) {
                toast("error", (e as Error).message);
              }
            }}
          >
            <RotateCcw />
            Run again
          </button>
        )}
        {active && (
          <button
            className="btn danger sm"
            onClick={async () => {
              if (!window.confirm("Cancel this run?")) return;
              try {
                await api.cancelRun(run.id);
              } catch (e) {
                toast("error", (e as Error).message);
              }
            }}
          >
            <Square />
            Cancel
          </button>
        )}
      </div>

      {waiting && <GateBanner runId={run.id} visit={waiting} graph={data.graph} />}
      {run.status === "failed" && run.error && (
        <div className="gate-banner" style={{ borderColor: "color-mix(in srgb, var(--danger) 45%, transparent)", background: "var(--danger-bg)" }}>
          <X size={16} color="var(--danger)" />
          <div className="q" style={{ whiteSpace: "pre-wrap" }}>
            <b>Run failed</b>
            <span className="small">{run.error}</span>
          </div>
        </div>
      )}

      <div className="split">
        <div className="canvas">
          {data.graph && (
            <FlowGraph
              graph={data.graph}
              run={overlay}
              selected={selected?.node ?? null}
              onSelect={(node) => {
                if (!node) return;
                const last = [...visits].reverse().find((v) => v.node === node);
                if (last) {
                  setPinned(true);
                  setSelectedSeq(last.seq);
                }
              }}
            />
          )}
          <div className="canvas-overlay bl">
            <div className="legend">
              <span>
                <i style={{ background: "var(--info)" }} />
                running
              </span>
              <span>
                <i style={{ background: "var(--success)" }} />
                done
              </span>
              <span>
                <i style={{ background: "var(--gate)" }} />
                waiting
              </span>
              <span>
                <i style={{ background: "var(--danger)" }} />
                failed
              </span>
              <span>
                <i style={{ background: "var(--faint)" }} />
                not reached
              </span>
            </div>
          </div>
        </div>
        <aside className="side">
          <div className="tabs">
            <button className={`tab${tab === "steps" ? " active" : ""}`} onClick={() => setTab("steps")}>
              <ListOrdered size={14} />
              Steps <span className="badge">{visits.length}</span>
            </button>
            <button className={`tab${tab === "events" ? " active" : ""}`} onClick={() => setTab("events")}>
              <History size={14} />
              Events
            </button>
            <span className="spacer" />
            <span style={{ maxWidth: 200, minWidth: 0, display: "flex", alignSelf: "center" }}>
              <BranchChip branch={run.branch} />
            </span>
          </div>
          <div className="side-body">
            {tab === "events" ? (
              <div className="timeline">
                {data.events.map((e) => (
                  <div key={e.id} className="tl-item" style={{ cursor: "default" }}>
                    <span className="tl-icon canceled">
                      <History />
                    </span>
                    <div>
                      <div className="small">{e.message}</div>
                    </div>
                    <div className="tl-time">{clock(e.created_at)}</div>
                  </div>
                ))}
              </div>
            ) : (
              <>
                {run.status === "queued" && <div className="side-pad muted small">Waiting for a free slot…</div>}
                <div className="timeline">
                  {visits.map((v) => (
                    <div
                      key={v.seq}
                      className={`tl-item${selected?.seq === v.seq ? " selected" : ""}`}
                      onClick={() => {
                        setPinned(true);
                        setSelectedSeq(v.seq);
                      }}
                    >
                      <span className={`tl-icon ${v.status}`}>
                        <StatusIcon status={v.status} />
                      </span>
                      <div style={{ minWidth: 0 }}>
                        <div className="tl-title">
                          <span className="node">
                            {v.node}
                            {v.visit > 1 ? <span className="faint">#{v.visit}</span> : null}
                          </span>
                          {v.outcome && <span className="chip mono">→ {v.outcome}</span>}
                        </div>
                        <div className="tl-sub">{v.status === "running" || v.status === "pending" ? v.progress || "starting…" : v.error || v.summary}</div>
                      </div>
                      <div className="tl-time">
                        {v.started_at ? duration(v.started_at, v.finished_at, now) : ""}
                        {v.tokens_in + v.tokens_out > 0 && <div>{tokens(v.tokens_in + v.tokens_out)} tok</div>}
                      </div>
                    </div>
                  ))}
                </div>
                {selected && <VisitDetail key={selected.seq} runId={run.id} v={selected} />}
              </>
            )}
          </div>
        </aside>
      </div>
    </div>
  );
}

function buildOverlay(graph: Graph, visits: Visit[], runStatus: string): RunOverlay {
  const nodes: RunOverlay["nodes"] = {};
  const taken = new Set<string>();
  const next = new Map(graph.edges.filter((e) => e.kind === "next").map((e) => [edgeId(e.from, e.outcome), e.to]));
  let lastEdge: string | undefined;
  visits.forEach((v, i) => {
    const s = (nodes[v.node] ??= { status: v.status, visits: 0, taken: [] });
    s.visits++;
    s.status = v.status;
    s.progress = v.progress;
    s.outcome = v.outcome;
    if (v.status === "succeeded" && v.outcome) {
      s.taken.push(v.outcome);
      const id = edgeId(v.node, v.outcome);
      taken.add(id);
      lastEdge = id;
      // on_exhausted jump: the outcome's target was skipped
      const target = next.get(id);
      const after = visits[i + 1];
      if (target && after && target !== after.node && !target.startsWith("$")) {
        taken.add(edgeId(target, "exhausted"));
      }
    }
  });
  let terminal: RunOverlay["terminal"];
  if (runStatus === "succeeded") terminal = "$success";
  if (runStatus === "failed" && lastEdge && next.get(lastEdge) === "$fail") terminal = "$fail";
  const live = runStatus === "running";
  return { nodes, takenEdges: taken, lastEdge: live ? lastEdge : undefined, terminal };
}

function GateBanner({ runId, visit, graph }: { runId: string; visit: Visit; graph?: Graph }) {
  const toast = useToast();
  const [busy, setBusy] = useState<string>();
  const node = graph?.nodes.find((n) => n.id === visit.node);
  const outcomes = (node?.outcomes ?? []).filter((o) => o !== "timeout");
  return (
    <div className="gate-banner">
      <Hand size={18} color="var(--gate)" />
      <div className="q">
        <b>
          <span className="mono">{visit.node}</span> needs your decision
        </b>
        <span className="small" style={{ whiteSpace: "pre-wrap" }}>
          {visit.prompt || node?.prompt}
        </span>
        {visit.deadline ? <div className="small muted">Times out {timeAgo(visit.deadline).replace(" ago", "") === "just now" ? "now" : new Date(visit.deadline).toLocaleString()}</div> : null}
      </div>
      <div className="row wrap">
        {outcomes.map((o, i) => (
          <button
            key={o}
            className={`btn${i === 0 ? " primary" : ""}`}
            disabled={!!busy}
            onClick={async () => {
              setBusy(o);
              try {
                await api.decide(runId, visit.seq, o);
                toast("ok", `Decided: ${o}`);
              } catch (e) {
                toast("error", (e as Error).message);
              } finally {
                setBusy(undefined);
              }
            }}
          >
            {busy === o && <Spinner />}
            {o}
          </button>
        ))}
      </div>
    </div>
  );
}

function VisitDetail({ runId, v }: { runId: string; v: Visit }) {
  const [tab, setTab] = useState<"result" | "prompt" | "transcript">("result");
  const outputs = Object.entries(v.outputs ?? {}).filter(([k]) => k !== "log_tail");
  const logTail = (v.outputs?.log_tail as string) || v.log_tail;
  const isPod = v.type === "agent" || v.type === "llm" || v.type === "check";
  return (
    <div style={{ borderTop: "1px solid var(--border)" }}>
      <div className="detail-head">
        <h2>
          <span className="node">
            {v.node}
            {v.visit > 1 && <span className="faint">#{v.visit}</span>}
          </span>
          <Pill status={v.status} />
          {v.outcome && <span className="chip mono">→ {v.outcome}</span>}
        </h2>
        <div className="stats small">
          <span>{v.type}</span>
          {v.model && <span className="mono">{v.model}</span>}
          {v.started_at ? <span>{duration(v.started_at, v.finished_at)}</span> : null}
          {v.llm_calls > 0 && (
            <span>
              {v.llm_calls} calls · {tokens(v.tokens_in)} in / {tokens(v.tokens_out)} out
            </span>
          )}
          {v.commit_sha && (
            <span className="mono">
              <GitCommit />
              {v.commit_sha.slice(0, 8)}
            </span>
          )}
        </div>
      </div>
      {isPod && (
        <div className="tabs" style={{ paddingTop: 4 }}>
          <button className={`tab${tab === "result" ? " active" : ""}`} onClick={() => setTab("result")}>
            Result
          </button>
          <button className={`tab${tab === "prompt" ? " active" : ""}`} onClick={() => setTab("prompt")}>
            Prompt
          </button>
          <button className={`tab${tab === "transcript" ? " active" : ""}`} onClick={() => setTab("transcript")}>
            <ScrollText size={13} />
            Transcript
          </button>
        </div>
      )}
      <div className="side-pad">
        {(tab === "result" || !isPod) && (
          <div className="stack">
            {v.type === "gate" && v.prompt && (
              <>
                <div className="label">Question</div>
                <div className="prose">{v.prompt}</div>
              </>
            )}
            {v.status === "waiting" && <div className="small" style={{ color: "var(--gate)" }}>Waiting for a decision — use the buttons above.</div>}
            {v.error && <div className="error-box">{v.error}</div>}
            {v.summary && <div className="prose">{v.summary}</div>}
            {(v.status === "running" || v.status === "pending") && (
              <div className="row small muted">
                <Spinner /> {v.progress || "Starting…"}
              </div>
            )}
            {v.decided_by && <div className="small muted">Decided by {v.decided_by}</div>}
            {outputs.length > 0 && (
              <>
                <div className="label">Outputs</div>
                <pre className="code">{JSON.stringify(Object.fromEntries(outputs), null, 2)}</pre>
              </>
            )}
            {logTail && (
              <>
                <div className="label">Output</div>
                <pre className="code">{logTail}</pre>
              </>
            )}
          </div>
        )}
        {tab === "prompt" && isPod && (v.prompt ? <pre className="code">{v.prompt}</pre> : <div className="muted small">Not started yet.</div>)}
        {tab === "transcript" && isPod && <Transcript runId={runId} seq={v.seq} live={v.status === "running" || v.status === "pending"} />}
      </div>
    </div>
  );
}
