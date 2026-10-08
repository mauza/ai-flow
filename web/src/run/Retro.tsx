import { useState } from "react";
import { Lightbulb, RefreshCw, Sparkles, ThumbsUp, TriangleAlert } from "lucide-react";
import { api } from "../api";
import { useResource } from "../hooks";
import { duration, timeAgo } from "../format";
import { Spinner, useToast } from "../ui";

const kindLabels: Record<string, string> = {
  flow: "This flow",
  preset: "Catalog node",
  primitive: "ai-flow primitive",
  prompt: "Prompt",
  model: "Model",
  config: "Config",
};

/** An LLM's review of a run: what to improve, and which primitives to add or tweak. */
export default function Retro({ runId, active }: { runId: string; active: boolean }) {
  const view = useResource(() => api.retro(runId), [runId], (e) => e.type === "run" && e.id === runId);
  const [note, setNote] = useState("");
  const [starting, setStarting] = useState(false);
  const toast = useToast();
  const retro = view.data?.retro;

  const start = async () => {
    setStarting(true);
    try {
      await api.startRetro(runId, note);
      view.reload();
    } catch (e) {
      toast("error", (e as Error).message);
    } finally {
      setStarting(false);
    }
  };

  const form = (
    <form className="stack" onSubmit={(e) => (e.preventDefault(), start())}>
      <textarea className="input" rows={2} value={note} onChange={(e) => setNote(e.target.value)} placeholder="Optional: what should the review focus on?" aria-label="Focus" />
      <button className="btn primary sm" disabled={starting || retro?.status === "pending"}>
        {starting ? <Spinner /> : retro ? <RefreshCw /> : <Sparkles />}
        {retro ? "Review again" : "Review this run"}
      </button>
    </form>
  );

  if (!view.data) return <div className="side-pad">{view.error ? <div className="error-box">{view.error}</div> : <Spinner />}</div>;
  if (!retro)
    return (
      <div className="side-pad stack">
        <p className="small muted">
          An LLM reads this run's record (flow, steps, outcomes, errors, costs and log tails) and says where it went wrong, where it wasted time or money, and what to
          change: the flow, catalog nodes, prompts, models, or ai-flow's own primitives.
          {active && " The run is still going; a review now covers what has happened so far."}
        </p>
        {form}
      </div>
    );

  const r = retro.report;
  return (
    <div className="side-pad stack retro">
      <div className="small faint">
        {retro.status === "pending" ? "Reviewing" : "Reviewed"} by <span className="mono">{retro.model}</span> {timeAgo(retro.started_at)}
        {retro.finished_at ? ` in ${duration(retro.started_at, retro.finished_at)}` : ""}
        {retro.note && <> · focus: {retro.note}</>}
      </div>
      {retro.status === "pending" && (
        <div className="row small muted">
          <Spinner /> Reading the run…
        </div>
      )}
      {retro.status === "failed" && <div className="error-box">{retro.error}</div>}
      {r && (
        <>
          <p className="prose">{r.summary}</p>
          {r.suggestions.length > 0 && (
            <section>
              <div className="label row">
                <Lightbulb size={13} /> Suggestions
              </div>
              {r.suggestions.map((s, i) => (
                <div key={i} className={`retro-item retro-sugg prio-${s.priority ?? "medium"}`}>
                  <div className="row small">
                    <span className="chip">{kindLabels[s.kind] ?? s.kind}</span>
                    {s.target && <span className="mono clamp grow">{s.target}</span>}
                    {s.priority && <span className={`chip prio prio-${s.priority}`}>{s.priority}</span>}
                  </div>
                  <div>{s.change}</div>
                  <div className="small muted">{s.why}</div>
                </div>
              ))}
            </section>
          )}
          {r.problems.length > 0 && (
            <section>
              <div className="label row">
                <TriangleAlert size={13} /> Problems
              </div>
              {r.problems.map((p, i) => (
                <div key={i} className="retro-item">
                  {p.node && <span className="chip mono">{p.node}</span>} {p.evidence}
                  {p.impact && <div className="small muted">Impact: {p.impact}</div>}
                </div>
              ))}
            </section>
          )}
          {r.went_well.length > 0 && (
            <section>
              <div className="label row">
                <ThumbsUp size={13} /> Went well
              </div>
              <ul className="small">
                {r.went_well.map((w, i) => (
                  <li key={i}>{w}</li>
                ))}
              </ul>
            </section>
          )}
        </>
      )}
      {retro.status !== "pending" && form}
    </div>
  );
}
