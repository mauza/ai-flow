import { Pause, Play, Zap } from "lucide-react";
import { useState } from "react";
import { Link } from "react-router-dom";
import { api, type Schedule } from "../api";
import { timeAgo } from "../format";
import { useResource } from "../hooks";
import { useToast } from "../ui";

function target(s: Schedule): string {
  if (s.storymap) return `Next card from ${s.storymap.map || "every story map"}`;
  if (s.hone_maps) return `Hone ${s.hone_maps.active_maps || 2} story maps`;
  return s.flow ? `Flow ${s.flow}` : s.task_title || "Task";
}

function fmtEvery(s: string): string {
  return s.replace(/(\D)0s$/, "$1").replace(/h0m$/, "h");
}

function due(s: Schedule): string {
  if (!s.enabled) return "disabled in config";
  if (s.paused) return "paused";
  if (s.pacing_mode === "hold") return "held by quota";
  const min = Math.round(((s.next_due_at ?? 0) - Date.now()) / 60_000);
  if (min < 1) return "due now";
  return min < 90 ? `due in ${min}m` : `due in ${Math.round(min / 60)}h`;
}

/** A project's recurring work, with a pause that needs no config change. */
export default function Schedules({ project }: { project: string }) {
  const list = useResource(() => api.schedules(project), [project], (e) => e.type === "schedule" || e.type === "config");
  const [busy, setBusy] = useState("");
  const toast = useToast();
  if (list.error) return <div className="error-box">Schedules: {list.error}</div>;
  if (!list.data?.length) return null;

  const act = async (s: Schedule, fn: () => Promise<string | void>) => {
    setBusy(s.name);
    try {
      const msg = await fn();
      if (msg) toast("ok", msg);
    } catch (e) {
      toast("error", (e as Error).message);
    } finally {
      setBusy("");
      list.reload();
    }
  };

  return (
    <section className="card card-pad stack" style={{ gap: 12 }} aria-label="Schedules">
      <div className="label">Schedules</div>
      {list.data.map((s) => (
        <div key={s.name} className="stack" style={{ gap: 4 }}>
          <div className="row wrap">
            <b>{s.name}</b>
            <span className="chip" title={s.pacing_reason || undefined}>
              every {fmtEvery(s.every)}
              {s.effective_every && s.effective_every !== s.every && s.pacing_mode !== "hold" ? ` → ${fmtEvery(s.effective_every)}` : ""}
            </span>
            {s.pacing_mode && s.pacing_mode !== "normal" && (
              <span className="chip" title={s.pacing_reason || undefined}>
                quota: {s.pacing_mode}
              </span>
            )}
            <span className={`chip${s.paused || !s.enabled || s.pacing_mode === "hold" ? "" : " outcome-validated"}`}>{due(s)}</span>
            <span className="spacer" />
            <button
              className="btn sm"
              disabled={busy !== "" || !s.enabled || s.paused}
              onClick={() => act(s, async () => (await api.runSchedule(project, s.name)).last_reason)}
            >
              <Zap />
              Run now
            </button>
            <button className="btn sm" disabled={busy !== "" || !s.enabled} onClick={() => act(s, () => api.pauseSchedule(project, s.name, !s.paused).then(() => undefined))}>
              {s.paused ? <Play /> : <Pause />}
              {s.paused ? "Resume" : "Pause"}
            </button>
          </div>
          <div className="small muted">
            {target(s)} · at most{" "}
            {s.effective_max_active_runs !== undefined && s.effective_max_active_runs !== s.max_active_runs
              ? `${s.max_active_runs} → ${s.effective_max_active_runs}`
              : s.max_active_runs}{" "}
            active run(s), {s.max_open_prs} open PR(s), {s.max_open_tasks} open task(s)
            {!s.auto_start && " · waits for you to start runs"}
          </div>
          {s.state.last_status && (
            <div className="small">
              {s.state.last_status}
              {s.state.last_run_at ? ` · last ran ${timeAgo(s.state.last_run_at)}` : ""}: {s.state.last_reason}
              {s.state.last_run_id && (
                <>
                  {" "}
                  <Link to={`/runs/${s.state.last_run_id}`}>open run</Link>
                </>
              )}
            </div>
          )}
        </div>
      ))}
    </section>
  );
}
