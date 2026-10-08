import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { CheckCircle2, Send, Trash2, X } from "lucide-react";
import type { MapWork, StoryMap, UserTask } from "../api";
import { timeAgo } from "../format";
import { Pill } from "../ui";
import { workState } from "./model";

/** The side panel for one user task: edit it, send it to a flow, see its work. */
export default function TaskPanel({
  map,
  task,
  isNew,
  works,
  statuses,
  dirty,
  onSave,
  onDelete,
  onSend,
  onClose,
}: {
  map: StoryMap;
  task: UserTask;
  isNew: boolean;
  works: MapWork[];
  statuses: string[];
  dirty: boolean;
  onSave: (t: UserTask) => Promise<boolean>;
  onDelete: () => void;
  onSend: () => void;
  onClose: () => void;
}) {
  const [t, setT] = useState(task);
  const [acceptance, setAcceptance] = useState((task.acceptance ?? []).join("\n"));
  const [saving, setSaving] = useState(false);
  useEffect(() => {
    setT(task);
    setAcceptance((task.acceptance ?? []).join("\n"));
  }, [task]);

  const set = <K extends keyof UserTask>(k: K, v: UserTask[K]) => setT((x) => ({ ...x, [k]: v }));
  const value = (): UserTask => ({
    ...t,
    title: t.title.trim(),
    acceptance: acceptance
      .split("\n")
      .map((s) => s.replace(/^\s*[-*]\s*/, "").trim())
      .filter(Boolean),
  });
  const changed = isNew || JSON.stringify(value()) !== JSON.stringify({ ...task, acceptance: task.acceptance ?? [] });
  const mine = works.filter((w) => w.scope.tasks.includes(task.id));
  const succeeded = mine[0]?.run?.status === "succeeded";

  const save = async (extra: Partial<UserTask> = {}) => {
    setSaving(true);
    const ok = await onSave({ ...value(), ...extra });
    setSaving(false);
    return ok;
  };

  return (
    <aside className="map-panel" aria-label={isNew ? "New user task" : `User task ${task.title}`}>
      <div className="map-panel-head">
        <h2 className="grow">{isNew ? "New user task" : "User task"}</h2>
        {dirty && <span className="chip" title="Edited since the last commit">uncommitted</span>}
        <button className="btn ghost icon sm" onClick={onClose} aria-label="Close">
          <X />
        </button>
      </div>
      <div className="map-panel-body">
        <div className="field">
          <label htmlFor="t-title">Title</label>
          <input id="t-title" className="input" value={t.title} onChange={(e) => set("title", e.target.value)} autoFocus={isNew} />
          <span className="hint mono">tasks/{t.id}.yaml</span>
        </div>
        <div className="field-row">
          <div className="field">
            <label htmlFor="t-activity">Activity</label>
            <select id="t-activity" className="input" value={t.activity} onChange={(e) => set("activity", e.target.value)}>
              {map.journey.map((ph) => (
                <optgroup key={ph.id} label={ph.title}>
                  {ph.activities.map((a) => (
                    <option key={a.id} value={a.id}>
                      {a.title}
                    </option>
                  ))}
                </optgroup>
              ))}
            </select>
          </div>
          <div className="field">
            <label htmlFor="t-release">Release</label>
            <select id="t-release" className="input" value={t.release ?? ""} onChange={(e) => set("release", e.target.value || undefined)}>
              <option value="">Unscheduled</option>
              {map.releases.map((r) => (
                <option key={r.id} value={r.id}>
                  {r.title}
                </option>
              ))}
            </select>
          </div>
        </div>
        <div className="field-row">
          <div className="field">
            <label htmlFor="t-status">Status</label>
            <select id="t-status" className="input" value={t.status ?? "todo"} onChange={(e) => set("status", e.target.value)}>
              {statuses.map((s) => (
                <option key={s} value={s}>
                  {s.replace(/_/g, " ")}
                </option>
              ))}
            </select>
          </div>
          <div className="field">
            <label htmlFor="t-persona">Persona</label>
            <select id="t-persona" className="input" value={t.persona ?? ""} onChange={(e) => set("persona", e.target.value || undefined)}>
              <option value="">From the activity</option>
              {map.personas.map((p) => (
                <option key={p.id} value={p.id}>
                  {p.name}
                </option>
              ))}
            </select>
          </div>
        </div>
        <div className="field">
          <label htmlFor="t-story">Story</label>
          <textarea id="t-story" className="input" rows={2} value={t.story ?? ""} onChange={(e) => set("story", e.target.value || undefined)} placeholder="As a … I want … so that …" />
        </div>
        <div className="field">
          <label htmlFor="t-acceptance">Acceptance criteria</label>
          <textarea id="t-acceptance" className="input" rows={3} value={acceptance} onChange={(e) => setAcceptance(e.target.value)} placeholder="One per line" />
        </div>
        <div className="field">
          <label htmlFor="t-desc">Notes</label>
          <textarea id="t-desc" className="input" rows={3} value={t.description ?? ""} onChange={(e) => set("description", e.target.value || undefined)} />
        </div>
        {map.metrics.length > 0 && (
          <fieldset className="field metric-picks">
            <legend className="label">Moves metrics</legend>
            {map.metrics.map((m) => (
              <label key={m.id} className="row small">
                <input
                  type="checkbox"
                  checked={t.metrics?.includes(m.id) ?? false}
                  onChange={(e) => set("metrics", e.target.checked ? [...(t.metrics ?? []), m.id] : (t.metrics ?? []).filter((x) => x !== m.id))}
                />
                {m.title}
              </label>
            ))}
          </fieldset>
        )}

        {!isNew && (
          <div className="stack">
            <div className="label">Work</div>
            {mine.length === 0 && <div className="small muted">Not sent to a flow yet.</div>}
            {mine.map((w) => {
              const s = workState(w);
              return (
                <Link key={w.task.id} to={s.href} className="work-row">
                  <Pill status={s.status} label={s.label} />
                  <span className="small grow clamp">{w.scope.kind === "task" ? "this task" : `with ${w.scope.kind} ${w.scope.id}`}</span>
                  <span className="small faint">{timeAgo(w.task.created_at)}</span>
                </Link>
              );
            })}
            {succeeded && t.status !== "done" && (
              <button className="btn sm success" onClick={() => save({ status: "done" })}>
                <CheckCircle2 />
                The flow succeeded: mark done
              </button>
            )}
          </div>
        )}
      </div>
      <div className="map-panel-foot">
        {!isNew && (
          <button className="btn ghost icon sm" onClick={onDelete} aria-label="Delete user task" title="Delete">
            <Trash2 />
          </button>
        )}
        <span className="spacer" />
        {!isNew && (
          <button className="btn sm" onClick={onSend} disabled={changed} title={changed ? "Save first" : "Plan a flow for this user task"}>
            <Send />
            Send to flow
          </button>
        )}
        <button className="btn primary sm" onClick={() => save()} disabled={saving || !changed || !t.title.trim()}>
          Save
        </button>
      </div>
    </aside>
  );
}
