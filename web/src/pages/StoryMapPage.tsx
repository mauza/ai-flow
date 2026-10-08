import { useMemo, useState, type DragEvent } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { AlertTriangle, BarChart3, FileCode2, Plus, Send, Sparkles, Trash2, User } from "lucide-react";
import { api, type MapView, type StoryMap, type UserTask } from "../api";
import { useResource } from "../hooks";
import { Modal, Pill, Spinner, useToast } from "../ui";
import { slug } from "./ProductPage";
import WorkspaceBar from "../product/WorkspaceBar";
import TaskPanel from "../storymap/TaskPanel";
import SendDialog from "../storymap/SendDialog";
import MetricsPanel from "../storymap/MetricsPanel";
import Assistant from "../storymap/Assistant";
import MapEditor from "../storymap/MapEditor";
import { cellTasks, latestWork, releaseRows, UNSCHEDULED, uniqueID, workState } from "../storymap/model";

type Sending = { kind: "task" | "activity" | "phase"; id: string; title: string };
type Adding = { kind: "phase" | "activity" | "release"; phase?: string };

export default function StoryMapPage() {
  const { p = "", map: mapID = "" } = useParams();
  const view = useResource(() => api.map(p, mapID), [p, mapID], (e) => e.type === "task" || e.type === "run");
  const product = useResource(() => api.product(p), [p]);
  const [open, setOpen] = useState<{ task: UserTask; isNew: boolean } | null>(null);
  const [sending, setSending] = useState<Sending | null>(null);
  const [adding, setAdding] = useState<Adding | null>(null);
  const [editing, setEditing] = useState(false);
  const [panel, setPanel] = useState<"assistant" | null>(null);
  const [showMetrics, setShowMetrics] = useState(true);
  const toast = useToast();
  const navigate = useNavigate();

  const reload = () => {
    view.reload();
    product.reload();
  };

  if (!view.data)
    return (
      <div className="page">
        {view.error ? <div className="error-box">{view.error}</div> : <div className="row muted"><Spinner /> Loading the map…</div>}
      </div>
    );
  const d = view.data;
  const map = d.map.map;
  const tasks = d.map.tasks;

  const saveTasks = async (changed: UserTask[], del: string[] = []) => {
    try {
      await api.saveTasks(p, mapID, changed, del);
      reload();
      return true;
    } catch (e) {
      toast("error", (e as Error).message);
      return false;
    }
  };
  const saveMap = async (m: StoryMap) => {
    try {
      await api.saveMap(p, mapID, m);
      reload();
      return true;
    } catch (e) {
      toast("error", (e as Error).message);
      return false;
    }
  };
  const newTask = (activity: string, release: string) => {
    const cell = cellTasks(map, tasks, activity, release);
    setOpen({
      isNew: true,
      task: { id: "", title: "", activity, release: release || undefined, order: (cell.at(-1)?.order ?? 0) + 1, status: "todo" },
    });
  };
  const deleteMap = async () => {
    if (!window.confirm(`Delete the whole ${map.title} map? (Nothing is committed until you commit.)`)) return;
    await api.deleteMap(p, mapID);
    navigate(`/products/${p}`);
  };

  return (
    <div className="page map-page">
      <div className="page-head">
        <div className="crumb">
          <Link to="/products">Products</Link>/<Link to={`/products/${p}`}>{p}</Link>/
        </div>
        <h1>{map.title}</h1>
        {map.description && <span className="sub clamp">{map.description}</span>}
        <span className="spacer" />
        <div className="row wrap map-tools">
          <button className={`btn sm${showMetrics ? " active" : ""}`} onClick={() => setShowMetrics((v) => !v)} aria-pressed={showMetrics}>
            <BarChart3 />
            Metrics
          </button>
          <button className="btn sm" onClick={() => setEditing(true)}>
            <FileCode2 />
            Edit map
          </button>
          <button className={`btn sm${panel === "assistant" ? " primary" : ""}`} onClick={() => setPanel(panel === "assistant" ? null : "assistant")}>
            <Sparkles />
            Assistant
          </button>
          <button className="btn ghost icon sm" onClick={deleteMap} aria-label="Delete map" title="Delete map">
            <Trash2 />
          </button>
        </div>
      </div>
      {product.data && <WorkspaceBar project={p} status={product.data.workspace} onChange={reload} defaultMessage={`Update the ${map.title} story map`} />}
      {d.map.problems.length > 0 && (
        <div className="warn-box">
          <AlertTriangle size={14} />
          <div>
            {d.map.problems.map((x) => (
              <div key={x}>{x}</div>
            ))}
          </div>
        </div>
      )}
      {showMetrics && <MetricsPanel project={p} mapID={mapID} map={map} />}

      <div className={`map-layout${open || panel ? " with-panel" : ""}`}>
        <MapGrid
          view={d}
          onOpen={(t) => setOpen({ task: t, isNew: false })}
          onNew={newTask}
          onSend={setSending}
          onAdd={setAdding}
          onMove={(changed) => saveTasks(changed)}
        />
        {open && (
          <TaskPanel
            map={map}
            task={open.task}
            isNew={open.isNew}
            works={d.works}
            statuses={d.statuses}
            dirty={d.dirty.some((x) => x.endsWith(`/tasks/${open.task.id}.yaml`))}
            onClose={() => setOpen(null)}
            onSave={async (t) => {
              const task = open.isNew ? { ...t, id: uniqueID(slug(t.title), tasks.map((x) => x.id)) } : t;
              const ok = await saveTasks([task]);
              if (ok) setOpen({ task, isNew: false });
              return ok;
            }}
            onDelete={async () => {
              if (!window.confirm(`Delete "${open.task.title}"?`)) return;
              if (await saveTasks([], [open.task.id])) setOpen(null);
            }}
            onSend={() => setSending({ kind: "task", id: open.task.id, title: open.task.title })}
          />
        )}
        {!open && panel === "assistant" && <Assistant project={p} mapID={mapID} onApplied={reload} onClose={() => setPanel(null)} />}
      </div>

      {sending && <SendDialog project={p} mapID={mapID} map={map} tasks={tasks} {...sending} onClose={() => setSending(null)} />}
      {adding && <AddItem map={map} adding={adding} onClose={() => setAdding(null)} onSave={async (m) => (await saveMap(m)) && setAdding(null)} />}
      {editing && (
        <MapEditor
          project={p}
          mapID={mapID}
          map={map}
          onClose={() => setEditing(false)}
          onSaved={() => {
            setEditing(false);
            reload();
          }}
        />
      )}
    </div>
  );
}

function MapGrid({
  view,
  onOpen,
  onNew,
  onSend,
  onAdd,
  onMove,
}: {
  view: MapView;
  onOpen: (t: UserTask) => void;
  onNew: (activity: string, release: string) => void;
  onSend: (s: Sending) => void;
  onAdd: (a: Adding) => void;
  onMove: (changed: UserTask[]) => void;
}) {
  const map = view.map.map;
  const tasks = view.map.tasks;
  const work = useMemo(() => latestWork(view.works), [view.works]);
  const dirty = useMemo(() => new Set(view.dirty), [view.dirty]);
  const [over, setOver] = useState("");
  // A phase without activities still gets a column, so the headers line up.
  const activities = map.journey.flatMap((ph) =>
    ph.activities.length
      ? ph.activities.map((a) => ({ ...a, phase: ph.id, empty: false }))
      : [{ id: `-${ph.id}`, title: "No activities yet", phase: ph.id, empty: true, description: undefined, persona: undefined }],
  );
  const persona = (id?: string) => map.personas.find((x) => x.id === id)?.name;

  // Dropping a card into a cell (before a card, or at the end) renumbers that cell.
  const drop = (e: DragEvent, activity: string, release: string, before?: string) => {
    e.preventDefault();
    e.stopPropagation();
    setOver("");
    const id = e.dataTransfer.getData("text/x-user-task");
    const moving = tasks.find((t) => t.id === id);
    if (!moving || id === before) return;
    const cell = cellTasks(map, tasks, activity, release).filter((t) => t.id !== id);
    const at = before ? cell.findIndex((t) => t.id === before) : cell.length;
    cell.splice(at < 0 ? cell.length : at, 0, { ...moving, activity, release: release || undefined });
    const changed = cell
      .map((t, i) => ({ ...t, order: i + 1 }))
      .filter((t) => {
        const old = tasks.find((x) => x.id === t.id)!;
        return old.order !== t.order || old.activity !== t.activity || (old.release ?? "") !== (t.release ?? "");
      });
    if (changed.length) onMove(changed);
  };

  if (map.journey.length === 0 || activities.length === 0)
    return (
      <div className="map-empty">
        <p className="muted">The journey is empty. Add a phase, then the activities users do in it.</p>
        <button className="btn primary" onClick={() => onAdd({ kind: "phase" })}>
          <Plus />
          Add a phase
        </button>
      </div>
    );

  const cols = `var(--map-label) repeat(${activities.length}, var(--map-col)) var(--map-add)`;
  return (
    <div className="map-scroll" role="region" aria-label="Story map">
      <div className="map-grid" style={{ gridTemplateColumns: cols }}>
        <div className="map-corner small faint">Journey →</div>
        {map.journey.map((ph) => (
          <div key={ph.id} className="map-phase" style={{ gridColumn: `span ${Math.max(ph.activities.length, 1)}` }} title={ph.description}>
            <span className="grow clamp">{ph.title}</span>
            <button className="btn ghost icon xs" onClick={() => onAdd({ kind: "activity", phase: ph.id })} aria-label={`Add activity to ${ph.title}`} title="Add activity">
              <Plus />
            </button>
            <button className="btn ghost icon xs" onClick={() => onSend({ kind: "phase", id: ph.id, title: `${ph.title} phase` })} aria-label={`Send phase ${ph.title} to a flow`} title="Send phase to a flow">
              <Send />
            </button>
          </div>
        ))}
        <button className="map-add-col btn ghost sm" onClick={() => onAdd({ kind: "phase" })} title="Add a phase">
          <Plus />
          Phase
        </button>

        <div className="map-corner small faint">Activities</div>
        {activities.map((a) =>
          a.empty ? (
            <div key={a.id} className="map-activity empty small faint">
              {a.title}
            </div>
          ) : (
          <div key={a.id} className="map-activity" title={a.description}>
            <div className="row">
              <b className="grow clamp">{a.title}</b>
              <button className="btn ghost icon xs" onClick={() => onSend({ kind: "activity", id: a.id, title: a.title })} aria-label={`Send activity ${a.title} to a flow`} title="Send activity to a flow">
                <Send />
              </button>
            </div>
            {persona(a.persona) && (
              <span className="small muted row">
                <User size={11} />
                {persona(a.persona)}
              </span>
            )}
          </div>
          ),
        )}
        <div />

        {releaseRows(map).map((r) => (
          <ReleaseRow key={r.id || "unscheduled"} release={r}>
            {activities.map((a) => {
              const key = `${a.id}|${r.id}`;
              if (a.empty) return <div key={key} className="map-cell empty" />;
              return (
                <div
                  key={key}
                  className={`map-cell${over === key ? " over" : ""}`}
                  onDragOver={(e) => (e.preventDefault(), setOver(key))}
                  onDragLeave={() => setOver((o) => (o === key ? "" : o))}
                  onDrop={(e) => drop(e, a.id, r.id)}
                >
                  {cellTasks(map, tasks, a.id, r.id).map((t) => {
                    const w = work.get(t.id);
                    const s = w && workState(w);
                    return (
                      <button
                        key={t.id}
                        className={`task-card${t.status === "done" ? " done" : ""}`}
                        draggable
                        onDragStart={(e) => e.dataTransfer.setData("text/x-user-task", t.id)}
                        onDrop={(e) => drop(e, a.id, r.id, t.id)}
                        onClick={() => onOpen(t)}
                        title={t.story}
                      >
                        <span className="task-title">{t.title}</span>
                        <span className="task-meta">
                          {t.status && t.status !== "todo" && <span className={`chip status-${t.status}`}>{t.status.replace(/_/g, " ")}</span>}
                          {s && <Pill status={s.status} label={s.label} />}
                          {dirty.has(`product/user-story-maps/${view.map.id}/tasks/${t.id}.yaml`) && <span className="dot-changed" title="Uncommitted edits" />}
                        </span>
                      </button>
                    );
                  })}
                  <button className="cell-add" onClick={() => onNew(a.id, r.id)} aria-label={`Add user task to ${a.title}, ${r.title}`}>
                    <Plus size={13} />
                  </button>
                </div>
              );
            })}
            <div />
          </ReleaseRow>
        ))}
        <button className="map-add-row btn ghost sm" onClick={() => onAdd({ kind: "release" })}>
          <Plus />
          Release
        </button>
      </div>
    </div>
  );
}

function ReleaseRow({ release, children }: { release: { id: string; title: string; goal?: string; status?: string }; children: React.ReactNode }) {
  return (
    <>
      <div className={`map-release${release.id === UNSCHEDULED ? " unscheduled" : ""}`}>
        <b>{release.title}</b>
        {release.status && <span className="small faint">{release.status.replace(/_/g, " ")}</span>}
        {release.goal && <span className="small muted clamp">{release.goal}</span>}
      </div>
      {children}
    </>
  );
}

function AddItem({ map, adding, onClose, onSave }: { map: StoryMap; adding: Adding; onClose: () => void; onSave: (m: StoryMap) => void }) {
  const [title, setTitle] = useState("");
  const [extra, setExtra] = useState("");
  const noun = adding.kind;
  const save = () => {
    const m: StoryMap = structuredClone(map);
    const taken = [...m.journey.map((p) => p.id), ...m.journey.flatMap((p) => p.activities.map((a) => a.id)), ...m.releases.map((r) => r.id)];
    const id = uniqueID(slug(title), taken);
    if (noun === "phase") m.journey.push({ id, title, activities: [{ id: uniqueID(`${id}-start`, taken), title: "New activity" }] });
    if (noun === "activity") m.journey.find((p) => p.id === adding.phase)!.activities.push({ id, title, persona: extra || undefined });
    if (noun === "release") m.releases.push({ id, title, goal: extra || undefined, status: "planned" });
    onSave(m);
  };
  return (
    <Modal
      title={`Add ${noun}`}
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" disabled={!title.trim()} onClick={save}>
            Add
          </button>
        </>
      }
    >
      <form onSubmit={(e) => (e.preventDefault(), title.trim() && save())}>
        <div className="field">
          <label htmlFor="add-title">Title</label>
          <input id="add-title" className="input" value={title} onChange={(e) => setTitle(e.target.value)} autoFocus />
        </div>
        {noun === "activity" && map.personas.length > 0 && (
          <div className="field">
            <label htmlFor="add-persona">Persona</label>
            <select id="add-persona" className="input" value={extra} onChange={(e) => setExtra(e.target.value)}>
              <option value="">None</option>
              {map.personas.map((x) => (
                <option key={x.id} value={x.id}>
                  {x.name}
                </option>
              ))}
            </select>
          </div>
        )}
        {noun === "release" && (
          <div className="field">
            <label htmlFor="add-goal">Goal</label>
            <input id="add-goal" className="input" value={extra} onChange={(e) => setExtra(e.target.value)} placeholder="What this release lets users do" />
          </div>
        )}
      </form>
    </Modal>
  );
}
