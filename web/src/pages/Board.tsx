import { useEffect, useMemo, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { AlertTriangle, ExternalLink, Inbox, Plus, RefreshCw, Sparkles, Trash2, Workflow } from "lucide-react";
import { api, type Task } from "../api";
import { useNow, useResource } from "../hooks";
import { taskStatusLabel, timeAgo } from "../format";
import { Empty, LinearMark, Modal, PRLink, Pill, SourceChip, Spinner, useToast } from "../ui";

const columns: { id: string; title: string; color: string; statuses: string[] }[] = [
  { id: "planning", title: "Planning", color: "var(--info)", statuses: ["new", "planning"] },
  { id: "ready", title: "Ready to run", color: "var(--accent)", statuses: ["flow_ready", "plan_failed"] },
  { id: "running", title: "Running", color: "var(--gate)", statuses: ["running"] },
  { id: "done", title: "Done", color: "var(--success)", statuses: ["succeeded"] },
  { id: "failed", title: "Failed", color: "var(--danger)", statuses: ["failed"] },
];

export default function Board() {
  const tasks = useResource(() => api.tasks(), [], (e) => e.type === "task" || e.type === "run");
  const overview = useResource(() => api.overview(), []);
  const [creating, setCreating] = useState(false);
  const [params, setParams] = useSearchParams();
  const openTask = params.get("task");
  const now = useNow(15000);

  const grouped = useMemo(() => {
    const g: Record<string, Task[]> = {};
    for (const c of columns) g[c.id] = [];
    for (const t of tasks.data ?? []) {
      const col = columns.find((c) => c.statuses.includes(t.status)) ?? columns[0];
      g[col.id].push(t);
    }
    return g;
  }, [tasks.data]);

  const linear = overview.data?.projects.find((p) => p.linear)?.linear;

  return (
    <div className="page">
      <div className="page-head">
        <h1>Board</h1>
        <span className="sub">
          {linear ? (
            <span className="row" style={{ gap: 6 }}>
              <LinearMark /> Issues in {linear.team} labelled <b>{linear.label}</b> in {linear.states.join(" / ")} become tasks automatically.
            </span>
          ) : (
            "Every task gets its own flow."
          )}
        </span>
        <span className="spacer" />
        <button className="btn primary" onClick={() => setCreating(true)}>
          <Plus />
          New task
        </button>
      </div>

      {tasks.error && <div className="error-box mb">{tasks.error}</div>}
      {!tasks.data && tasks.loading ? (
        <div className="center-fill" style={{ height: 300 }}>
          <Spinner lg />
        </div>
      ) : tasks.data?.length === 0 ? (
        <div className="card">
          <Empty icon={Inbox} title="No tasks yet">
            <p>Create a task here{linear ? " or add the label to a Linear issue" : ""}. The planner designs a flow for it; you review it and press run.</p>
            <button className="btn primary" onClick={() => setCreating(true)}>
              <Plus />
              New task
            </button>
          </Empty>
        </div>
      ) : (
        <div style={{ overflowX: "auto" }}>
          <div className="board">
            {columns.map((c) => (
              <section key={c.id} className="column" aria-label={c.title}>
                <div className="column-head">
                  <span className="bar" style={{ background: c.color }} />
                  {c.title}
                  <span className="n">{grouped[c.id].length}</span>
                </div>
                <div className="column-body">
                  {grouped[c.id].length === 0 && <div className="column-empty">Nothing here</div>}
                  {grouped[c.id].map((t) => (
                    <TaskCard key={t.id} t={t} now={now} onOpen={() => setParams({ task: t.id })} />
                  ))}
                </div>
              </section>
            ))}
          </div>
        </div>
      )}

      {creating && overview.data && <NewTask projects={overview.data.projects.map((p) => p.name)} onClose={() => setCreating(false)} onCreated={(t) => (setCreating(false), setParams({ task: t.id }))} />}
      {openTask && <TaskDrawer id={openTask} onClose={() => setParams({})} />}
    </div>
  );
}

function TaskCard({ t, now, onOpen }: { t: Task; now: number; onOpen: () => void }) {
  const lastRun = t.runs?.[0];
  return (
    <div className="task-card" onClick={onOpen} role="button" tabIndex={0} onKeyDown={(e) => e.key === "Enter" && onOpen()}>
      <div className="meta">
        <SourceChip source={t.source} identifier={t.identifier} url={t.url} />
        <span className="spacer" />
        <span title={new Date(t.updated_at).toLocaleString()}>{timeAgo(t.updated_at, now)}</span>
      </div>
      <div className="title">{t.title}</div>
      <div className="foot">
        {t.planning || t.status === "planning" ? (
          <Pill status="planning" label="planning" />
        ) : t.status === "plan_failed" ? (
          <Pill status="plan_failed" />
        ) : lastRun ? (
          <Pill status={lastRun.status} label={`run ${lastRun.status}`} />
        ) : t.status === "flow_ready" ? (
          <Pill status="flow_ready" label="ready" />
        ) : null}
        {t.flow_name && (
          <span className="chip mono" title={t.flow_name}>
            <Workflow />
            {t.flow_name.length > 22 ? t.flow_name.slice(0, 21) + "…" : t.flow_name}
          </span>
        )}
        <PRLink url={lastRun?.pr_url} />
      </div>
      {t.status === "plan_failed" && t.error && (
        <div className="err">
          <AlertTriangle size={12} /> {t.error}
        </div>
      )}
    </div>
  );
}

function NewTask({ projects, onClose, onCreated }: { projects: string[]; onClose: () => void; onCreated: (t: Task) => void }) {
  const [title, setTitle] = useState("");
  const [body, setBody] = useState("");
  const [project, setProject] = useState(projects[0] ?? "");
  const [plan, setPlan] = useState(true);
  const [busy, setBusy] = useState(false);
  const toast = useToast();
  const submit = async () => {
    if (!title.trim()) return;
    setBusy(true);
    try {
      onCreated(await api.createTask({ title, body, project, plan }));
      toast("ok", plan ? "Task created — the planner is on it" : "Task created");
    } catch (e) {
      toast("error", (e as Error).message);
    } finally {
      setBusy(false);
    }
  };
  return (
    <Modal
      title="New task"
      onClose={onClose}
      footer={
        <>
          <button className="btn ghost" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" disabled={!title.trim() || busy} onClick={submit}>
            {busy ? <Spinner /> : <Sparkles />}
            {plan ? "Create & plan" : "Create"}
          </button>
        </>
      }
    >
      <div className="field">
        <label>Title</label>
        <input className="input" autoFocus value={title} placeholder="What should change?" onChange={(e) => setTitle(e.target.value)} onKeyDown={(e) => e.key === "Enter" && submit()} />
      </div>
      <div className="field">
        <label>Details</label>
        <textarea className="input" rows={6} value={body} placeholder="Context, expected behaviour, acceptance criteria…" onChange={(e) => setBody(e.target.value)} />
      </div>
      <div className="row">
        <div className="field grow">
          <label>Project</label>
          <select className="input" value={project} onChange={(e) => setProject(e.target.value)}>
            {projects.map((p) => (
              <option key={p}>{p}</option>
            ))}
          </select>
        </div>
        <label className="row small" style={{ cursor: "pointer", marginTop: 8 }}>
          <input type="checkbox" checked={plan} onChange={(e) => setPlan(e.target.checked)} />
          Plan a flow now
        </label>
      </div>
    </Modal>
  );
}

function TaskDrawer({ id, onClose }: { id: string; onClose: () => void }) {
  const data = useResource(() => api.task(id), [id], (e) => (e.type === "task" && e.id === id) || e.type === "run" || e.type === "flow");
  const navigate = useNavigate();
  const toast = useToast();
  const [starting, setStarting] = useState(false);
  const t = data.data?.task;

  useEffect(() => {
    if (data.error) toast("error", data.error);
  }, [data.error, toast]);

  return (
    <Modal title={t ? t.title : "Task"} onClose={onClose}>
      {!t ? (
        <Spinner />
      ) : (
        <div className="stack">
          <div className="row wrap">
            <Pill status={t.planning ? "planning" : t.status} label={t.planning ? "planning" : taskStatusLabel[t.status]} />
            <SourceChip source={t.source} identifier={t.identifier} url={t.url} />
            <span className="chip">{t.project}</span>
            {t.url && (
              <a className="small row" href={t.url} target="_blank" rel="noreferrer" style={{ gap: 4 }}>
                Open in Linear <ExternalLink size={12} />
              </a>
            )}
          </div>
          {t.body ? <div className="prose card card-pad" style={{ maxHeight: 220, overflow: "auto" }}>{t.body}</div> : <div className="muted small">No description.</div>}
          {t.error && t.status === "plan_failed" && <div className="warn-box" style={{ whiteSpace: "pre-wrap" }}>{t.error}</div>}

          <div className="row wrap">
            {(t.planning || t.status === "planning") && (
              <span className="row small muted">
                <Spinner /> The planner is drafting a flow…
              </span>
            )}
            {t.flow_name && !t.planning && t.status !== "planning" && (
              <Link className="btn primary" to={`/flows/${t.flow_name}`}>
                <Workflow />
                Open flow
              </Link>
            )}
            {t.flow_name && t.status !== "running" && !t.planning && t.status !== "plan_failed" && (
              <button
                className="btn"
                disabled={starting}
                onClick={async () => {
                  setStarting(true);
                  try {
                    const r = await api.startRun(t.flow_name!);
                    navigate(`/runs/${r.id}`);
                  } catch (e) {
                    toast("error", (e as Error).message);
                  } finally {
                    setStarting(false);
                  }
                }}
              >
                {starting ? <Spinner /> : null}Run flow
              </button>
            )}
            <button
              className="btn ghost"
              disabled={t.planning || t.status === "planning" || t.status === "running"}
              onClick={async () => {
                await api.planTask(t.id);
                toast("ok", "Re-planning — a new flow version will appear");
                data.reload();
              }}
            >
              <RefreshCw />
              {t.flow_name ? "Re-plan" : "Plan"}
            </button>
            <span className="spacer" />
            <button
              className="btn ghost danger"
              onClick={async () => {
                if (!window.confirm("Delete this task? Its flow and runs are kept.")) return;
                await api.deleteTask(t.id);
                onClose();
              }}
            >
              <Trash2 />
            </button>
          </div>

          {data.data!.runs.length > 0 && (
            <>
              <div className="section-title">Runs</div>
              {data.data!.runs.map((r) => (
                <Link key={r.id} to={`/runs/${r.id}`} className="row small" style={{ color: "var(--text)" }}>
                  <Pill status={r.status} />
                  <span className="mono">{r.id}</span>
                  <span className="muted">v{r.flow_version}</span>
                  <span className="spacer" />
                  <PRLink url={r.pr_url} />
                  <span className="faint">{timeAgo(r.created_at)}</span>
                </Link>
              ))}
            </>
          )}
          {data.data!.events.length > 0 && (
            <>
              <div className="section-title">Activity</div>
              <div className="stack" style={{ gap: 4 }}>
                {data.data!.events.slice(0, 8).map((e) => (
                  <div key={e.id} className="row small">
                    <span className="faint nowrap" style={{ width: 70 }}>
                      {timeAgo(e.created_at)}
                    </span>
                    <span className="muted">{e.message}</span>
                  </div>
                ))}
              </div>
            </>
          )}
        </div>
      )}
    </Modal>
  );
}
