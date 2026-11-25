import { Pause, Play, Zap } from "lucide-react";
import { useState } from "react";
import { Link } from "react-router-dom";
import { api, type Schedule } from "../api";
import { timeAgo } from "../format";
import { useResource } from "../hooks";
import { useToast } from "../ui";

function target(s: Schedule): string {
  if (s.storymap) return `Next card from ${s.storymap.map || "every story map"}`;
  return s.flow ? `Flow ${s.flow}` : s.task_title || "Task";
}

function due(s: Schedule): string {
  if (!s.enabled) return "disabled in config";
  if (s.paused) return "paused";
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
            <span className="chip">every {s.every.replace(/(\D)0s$/, "$1").replace(/h0m$/, "h")}</span>
            <span className={`chip${s.paused || !s.enabled ? "" : " outcome-validated"}`}>{due(s)}</span>
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
            {target(s)} · at most {s.max_active_runs} active run(s), {s.max_open_prs} open PR(s), {s.max_open_tasks} open task(s)
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
