import { useState } from "react";
import { ClipboardCheck, Lightbulb, RefreshCw, Sparkles, ThumbsDown, ThumbsUp, TriangleAlert } from "lucide-react";
import { api, type RunGrade } from "../api";
import { useResource } from "../hooks";
import { duration, timeAgo } from "../format";
import { Pill, Spinner, useToast } from "../ui";

const kindLabels: Record<string, string> = {
  flow: "This flow",
  preset: "Catalog node",
  primitive: "ai-flow primitive",
  prompt: "Prompt",
  model: "Model",
  config: "Config",
};

/** Automatic rubric grades + free-form LLM review for one run. */
export default function Retro({ runId, active }: { runId: string; active: boolean }) {
  const view = useResource(() => api.retro(runId), [runId], (e) => e.type === "run" && e.id === runId);
  const gradesView = useResource(() => api.runGrades(runId), [runId], (e) => (e.type === "improvement" || e.type === "run") && e.id === runId);
  const [note, setNote] = useState("");
  const [starting, setStarting] = useState(false);
  const [grading, setGrading] = useState(false);
  const toast = useToast();
  const retro = view.data?.retro;

  const startGrade = async () => {
    setGrading(true);
    try {
      await api.gradeRun(runId);
      toast("ok", "Grading started");
      gradesView.reload();
    } catch (e) {
      toast("error", (e as Error).message);
    } finally {
      setGrading(false);
    }
  };

  const toggleDisagree = async (g: RunGrade) => {
    try {
      await api.disagreeGrade(g.id, !g.disagreed, !g.disagreed ? "Flagged from run view" : "");
      toast("ok", !g.disagreed ? "Marked disagreed" : "Cleared disagreement");
      gradesView.reload();
    } catch (e) {
      toast("error", (e as Error).message);
    }
  };

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

  const gv = gradesView.data;
  const gradesSection = (
    <section className="stack" style={{ gap: 8, paddingBottom: 14, borderBottom: "1px solid var(--border)" }}>
      <div className="row wrap" style={{ gap: 6 }}>
        <span className="label row">
          <ClipboardCheck size={13} /> Automatic grades
        </span>
        <span className="spacer" />
        <button className="btn sm" disabled={grading || gv?.grading} onClick={startGrade}>
          {grading || gv?.grading ? <Spinner /> : <RefreshCw />}
          {gv?.grades.length ? "Re-grade" : "Grade run"}
        </button>
      </div>
      {gv?.facts && (
        <div className="row wrap small faint" style={{ gap: 6 }}>
          <span className="chip">{gv.facts.visits} steps</span>
          {gv.facts.loop_visits > 0 && <span className="chip">{gv.facts.loop_visits} loop retries</span>}
          {gv.facts.step_errors > 0 && <span className="chip">{gv.facts.step_errors} errors</span>}
          {gv.facts.resumes > 0 && <span className="chip">{gv.facts.resumes} resumes</span>}
          {gv.facts.steers > 0 && <span className="chip">{gv.facts.steers} steers</span>}
          {gv.facts.prompt_versions &&
            Object.entries(gv.facts.prompt_versions).map(([k, h]) => (
              <span key={k} className="chip mono" title={`Pinned prompt version for ${k}`}>
                {k.split(":")[1] || k}@{h.slice(0, 6)}
              </span>
            ))}
        </div>
      )}
      {gv?.grading && (
        <div className="row small muted">
          <Spinner /> Grading rubrics…
        </div>
      )}
      {gv && gv.grades.length > 0 ? (
        <div className="stack" style={{ gap: 6 }}>
          {gv.grades.map((g) => (
            <div key={g.id} className="retro-item stack" style={{ gap: 4 }}>
              <div className="row wrap small" style={{ gap: 6 }}>
                <Pill status={g.pass ? "succeeded" : "failed"} label={g.pass ? "pass" : "fail"} />
                <span className="mono" style={{ fontWeight: 600 }}>
                  {g.rubric}
                </span>
                {!g.pass && g.blame && <span className={`chip ${g.blame === "environment" ? "blame-env" : "blame-agent"}`}>{g.blame}</span>}
                {g.theme && <span className="chip">{g.theme}</span>}
                <span className="spacer" />
                <button className="btn ghost sm" onClick={() => toggleDisagree(g)} title={g.disagreed ? "Clear disagreement" : "Disagree with this grade"}>
                  <ThumbsDown size={11} />
                  {g.disagreed ? "Disagreed" : "Disagree"}
                </button>
              </div>
              <div className="small muted">{g.reason}</div>
            </div>
          ))}
        </div>
      ) : (
        !gv?.grading && <div className="small muted">Not graded yet. Finished runs are graded automatically when auto-grade is enabled.</div>
      )}
    </section>
  );

  if (!view.data) return <div className="side-pad">{view.error ? <div className="error-box">{view.error}</div> : <Spinner />}</div>;
  if (!retro)
    return (
      <div className="side-pad stack">
        {gradesSection}
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
      {gradesSection}
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
