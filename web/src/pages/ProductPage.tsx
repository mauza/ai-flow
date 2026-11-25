import { useEffect, useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { Activity, BookOpen, Copy, Download, ExternalLink, FilePlus2, HelpCircle, Map as MapIcon, Pencil, Plus, Share2, Trash2 } from "lucide-react";
import { api, type ProductView, type ProjectSummary } from "../api";
import { useResource } from "../hooks";
import { timeAgo, usd } from "../format";
import { Empty, Modal, PRLink, Pill, Spinner, useToast } from "../ui";
import Markdown from "../product/Markdown";
import Schedules from "../product/Schedules";
import WorkspaceBar from "../product/WorkspaceBar";

const README_TEMPLATE = `# Product name

One paragraph: what this product is and who it is for.

## Problem

What hurts today, for whom.

## Vision

What good looks like once this exists.

## Users

- Persona: what they need

## Principles

- A rule that guides trade-offs
`;

export function slug(s: string): string {
  return s
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 48);
}

export default function ProductPage() {
  const { p = "" } = useParams();
  const view = useResource(() => api.product(p), [p], (e) => e.type === "config");
  const summary = useResource(() => api.projectSummary(p).catch(() => null), [p], (e) => e.type === "run" || e.type === "task");
  const [params, setParams] = useSearchParams();
  const [sharing, setSharing] = useState(false);
  const rawTab = params.get("tab");
  const tab = rawTab === "docs" ? "docs" : rawTab === "overview" ? "overview" : "maps";

  if (!view.data)
    return (
      <div className="page">
        {view.error ? <div className="error-box">{view.error}</div> : <div className="row muted"><Spinner /> Checking out product/…</div>}
      </div>
    );
  const d = view.data;
  const attn = summary.data?.attention?.counts?.total ?? 0;
  return (
    <div className="page product-page">
      <div className="page-head">
        <div className="crumb">
          <Link to="/products">Products</Link>/
        </div>
        <h1>{d.project.name}</h1>
        <a className="chip mono" href={`https://github.com/${d.project.repo}`} target="_blank" rel="noreferrer">
          {d.project.repo}
          <ExternalLink />
        </a>
        {d.project.description && <span className="sub">{d.project.description}</span>}
        <span className="spacer" />
        <button className="btn sm" onClick={() => setSharing(true)}>
          <Share2 />
          Share digest
        </button>
      </div>
      <WorkspaceBar project={p} status={d.workspace} onChange={() => (view.reload(), summary.reload())} defaultMessage={`Update ${d.project.name} product docs`} />
      <div className="tabs" role="tablist">
        <button role="tab" aria-selected={tab === "maps"} className={`tab${tab === "maps" ? " active" : ""}`} onClick={() => setParams({})}>
          <MapIcon size={14} />
          Story maps <span className="badge">{d.maps.length}</span>
        </button>
        <button role="tab" aria-selected={tab === "overview"} className={`tab${tab === "overview" ? " active" : ""}`} onClick={() => setParams({ tab: "overview" })}>
          <Activity size={14} />
          Health &amp; value {attn > 0 && <span className="badge">{attn}</span>}
        </button>
        <button role="tab" aria-selected={tab === "docs"} className={`tab${tab === "docs" ? " active" : ""}`} onClick={() => setParams({ tab: "docs" })}>
          <BookOpen size={14} />
          Docs <span className="badge">{d.docs.length}</span>
        </button>
      </div>
      {tab === "maps" ? (
        <Maps project={p} view={d} />
      ) : tab === "overview" ? (
        <ProjectOverview project={p} summary={summary.data} loading={summary.loading} />
      ) : (
        <Docs project={p} view={d} onChange={view.reload} />
      )}
      {sharing && <ShareDigestModal project={p} onClose={() => setSharing(false)} />}
    </div>
  );
}

function ProjectOverview({ project, summary, loading }: { project: string; summary?: ProjectSummary | null; loading: boolean }) {
  if (!summary) {
    return loading ? <Spinner /> : <div className="muted small">Project summary unavailable.</div>;
  }
  const { delivery, attention, maps, recent_runs, retros } = summary;
  return (
    <div className="product-body stack" style={{ gap: 18 }}>
      <div className="kpi-grid">
        <div className="card card-pad">
          <div className="label">Runs</div>
          <div className="kpi-val">{delivery.total_runs}</div>
          <div className="small muted">
            {delivery.succeeded_runs} ok · {delivery.failed_runs} failed · {delivery.waiting_runs} waiting
          </div>
        </div>
        <div className="card card-pad">
          <div className="label">Success rate</div>
          <div className="kpi-val">{delivery.success_rate_pct !== undefined ? `${delivery.success_rate_pct}%` : "—"}</div>
          <div className="small muted">Median cycle: {delivery.median_cycle_hours !== undefined ? `${delivery.median_cycle_hours}h` : "—"}</div>
        </div>
        <div className="card card-pad">
          <div className="label">Delivery spend</div>
          <div className="kpi-val">{usd(delivery.total_cost_usd)}</div>
          <div className="small muted">{delivery.total_tokens.toLocaleString()} tokens</div>
        </div>
        <div className="card card-pad">
          <div className="label">Needs human input</div>
          <div className="kpi-val" style={{ color: attention?.counts.total ? "var(--gate)" : "var(--success)" }}>
            {attention?.counts.total ?? 0}
          </div>
          <div className="small muted">
            {attention?.counts.gates ?? 0} gates · {attention?.counts.open_questions ?? 0} questions
          </div>
        </div>
      </div>

      {attention && attention.counts.total > 0 && (
        <div className="card card-pad row wrap">
          <span className="small">
            <b>{attention.counts.total} item(s)</b> need your attention on this project ({attention.counts.gates} decision(s), {attention.counts.failed_runs} failed run(s),{" "}
            {attention.counts.shipped_unmarked} unverified shipment(s), {attention.counts.open_questions} open question(s)).
          </span>
          <span className="spacer" />
          <Link className="btn primary sm" to="/inbox">
            Open Inbox
          </Link>
        </div>
      )}

      <Schedules project={project} />

      {maps.map((m) => (
        <section key={m.id} className="card card-pad stack" style={{ gap: 12 }}>
          <div className="row wrap">
            <h3 style={{ margin: 0 }}>{m.title}</h3>
            <span className="chip">
              {m.tasks_done}/{m.tasks_total} done
            </span>
            <span className="spacer" />
            <Link className="btn sm" to={`/products/${project}/maps/${m.id}`}>
              Open story map
            </Link>
          </div>
          {m.metrics.length > 0 && (
            <div className="kpi-grid">
              {m.metrics.map((mt) => (
                <div key={mt.id} className="metric card">
                  <div className="row small">
                    <span className="grow clamp">{mt.title}</span>
                    <span className="chip">{mt.kind}</span>
                  </div>
                  <div className={`metric-value${mt.on_target === true ? " good" : mt.on_target === false ? " bad" : ""}`}>
                    {mt.error ? "err" : mt.value !== undefined ? `${mt.value}${mt.unit === "%" ? "%" : mt.unit ? ` ${mt.unit}` : ""}` : "—"}
                  </div>
                  {mt.detail && <div className="small faint clamp">{mt.detail}</div>}
                </div>
              ))}
            </div>
          )}
          {m.releases.length > 0 && (
            <div style={{ overflowX: "auto" }}>
              <table className="table">
                <thead>
                  <tr>
                    <th>Release</th>
                    <th>Status</th>
                    <th>Progress</th>
                    <th>Goal</th>
                  </tr>
                </thead>
                <tbody>
                  {m.releases.map((r) => (
                    <tr key={r.id}>
                      <td>
                        <b>{r.title}</b>
                      </td>
                      <td>
                        <span className="chip">{(r.status || "planned").replace(/_/g, " ")}</span>
                      </td>
                      <td className="mono small">
                        {r.done}/{r.total}
                      </td>
                      <td className="small muted">{r.goal || "—"}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          {m.risky_bets.length > 0 && (
            <div>
              <div className="label" style={{ marginBottom: 6 }}>
                High-uncertainty bets (validate before over-investing)
              </div>
              <div className="stack" style={{ gap: 6 }}>
                {m.risky_bets.map((b) => (
                  <div key={b.id} className="small">
                    <b>{b.title}</b> <span className="chip">value: {b.value || "—"}</span> <span className="chip">confidence: {b.confidence || "—"}</span>
                    {b.hypothesis && <div className="muted">{b.hypothesis}</div>}
                  </div>
                ))}
              </div>
            </div>
          )}
          {m.open_questions.length > 0 && (
            <div>
              <div className="label row" style={{ gap: 5, marginBottom: 6 }}>
                <HelpCircle size={12} /> Open product questions
              </div>
              <ul className="decision-list">
                {m.open_questions.map((q) => (
                  <li key={q}>{q}</li>
                ))}
              </ul>
            </div>
          )}
          {m.outcomes.length > 0 && (
            <div>
              <div className="label" style={{ marginBottom: 6 }}>
                Post-ship outcomes
              </div>
              <div className="stack" style={{ gap: 4 }}>
                {m.outcomes.map((o) => (
                  <div key={o.id} className="small row wrap" style={{ gap: 6 }}>
                    <span className={`chip outcome-${o.outcome}`}>{o.outcome}</span>
                    <b>{o.title}</b>
                    {o.outcome_note && <span className="muted">{o.outcome_note}</span>}
                  </div>
                ))}
              </div>
            </div>
          )}
        </section>
      ))}

      {recent_runs.length > 0 && (
        <section className="card card-pad stack" style={{ gap: 10 }}>
          <div className="label">Recent runs</div>
          <div style={{ overflowX: "auto" }}>
            <table className="table">
              <thead>
                <tr>
                  <th>Status</th>
                  <th>Task / flow</th>
                  <th>Cost</th>
                  <th>PR</th>
                  <th>When</th>
                </tr>
              </thead>
              <tbody>
                {recent_runs.map((r) => (
                  <tr key={r.run_id}>
                    <td>
                      <Pill status={r.status} />
                    </td>
                    <td>
                      <Link to={`/runs/${r.run_id}`}>{r.task_title || r.flow_name}</Link>
                    </td>
                    <td className="num small">{usd(r.cost_usd)}</td>
                    <td>
                      <PRLink url={r.pr_url} />
                    </td>
                    <td className="small muted">{timeAgo(r.created_at)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>
      )}

      {retros.length > 0 && (
        <section className="card card-pad stack" style={{ gap: 8 }}>
          <div className="label">Recent retro takeaways</div>
          {retros.map((rt) => (
            <div key={rt.run_id} className="retro-item">
              <div className="row wrap">
                <Link className="mono small" to={`/runs/${rt.run_id}`}>
                  {rt.flow_name} ({rt.run_id})
                </Link>
              </div>
              <div className="small">{rt.summary}</div>
              {rt.suggestions && rt.suggestions.length > 0 && (
                <ul>
                  {rt.suggestions.map((sg, i) => (
                    <li key={i} className="small muted">
                      <b>[{sg.kind}]</b> {sg.change}
                    </li>
                  ))}
                </ul>
              )}
            </div>
          ))}
        </section>
      )}
    </div>
  );
}

function ShareDigestModal({ project, onClose }: { project: string; onClose: () => void }) {
  const [md, setMd] = useState<string | null>(null);
  const [error, setError] = useState("");
  const toast = useToast();

  useEffect(() => {
    api.projectDigest(project, "md").then(setMd, (e) => setError((e as Error).message));
  }, [project]);

  const copy = async () => {
    if (!md) return;
    try {
      await navigator.clipboard.writeText(md);
      toast("ok", "Copied Markdown digest");
    } catch {
      toast("error", "Clipboard write blocked");
    }
  };

  const download = async (format: "md" | "html") => {
    try {
      const text = format === "md" && md ? md : await api.projectDigest(project, format);
      const blob = new Blob([text], { type: format === "html" ? "text/html;charset=utf-8" : "text/markdown;charset=utf-8" });
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = `${project}-digest.${format}`;
      a.click();
      URL.revokeObjectURL(url);
      toast("ok", `Downloaded ${project}-digest.${format}`);
    } catch (e) {
      toast("error", (e as Error).message);
    }
  };

  return (
    <Modal
      title={`Share ${project} digest`}
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Close
          </button>
          <button className="btn" disabled={!md} onClick={() => download("md")}>
            <Download />
            Download .md
          </button>
          <button className="btn" disabled={!md} onClick={() => download("html")}>
            <Download />
            Download .html
          </button>
          <button className="btn primary" disabled={!md} onClick={copy}>
            <Copy />
            Copy Markdown
          </button>
        </>
      }
    >
      <p className="small muted" style={{ marginTop: 0 }}>
        Self-contained status &amp; value digest you can paste into Slack, Linear, or GitHub, or export as a standalone HTML file.
      </p>
      {error ? <div className="error-box">{error}</div> : md === null ? <Spinner /> : <pre className="code" style={{ maxHeight: 360, overflow: "auto" }}>{md}</pre>}
    </Modal>
  );
}

function Maps({ project, view }: { project: string; view: ProductView }) {
  const [creating, setCreating] = useState(false);
  return (
    <div className="product-body">
      <div className="row wrap">
        <p className="small muted grow">
          Each map lives in <code>product/user-story-maps/&lt;map&gt;/</code>: a <code>map.yaml</code> and one YAML file per user task.
        </p>
        <button className="btn primary sm" onClick={() => setCreating(true)}>
          <Plus />
          New story map
        </button>
      </div>
      {view.maps.length === 0 ? (
        <Empty icon={MapIcon} title="No story maps yet">
          <p>A story map lays out the user journey, the activities in it, and the user tasks that deliver each one, release by release.</p>
        </Empty>
      ) : (
        <div className="grid-cards">
          {view.maps.map((m) => (
            <Link key={m.id} to={`/products/${project}/maps/${m.id}`} className="card card-pad product-card">
              <h3>
                <MapIcon size={16} />
                {m.title}
              </h3>
              {m.error ? <div className="error-box small">{m.error}</div> : m.description && <p className="small muted clamp">{m.description}</p>}
              <div className="progress" title={`${m.done} of ${m.tasks} user tasks done`}>
                <span style={{ width: `${m.tasks ? (100 * m.done) / m.tasks : 0}%` }} />
              </div>
              <span className="small faint">
                {m.done} of {m.tasks} user tasks done
              </span>
            </Link>
          ))}
        </div>
      )}
      {creating && <NewMap project={project} onClose={() => setCreating(false)} />}
    </div>
  );
}

function NewMap({ project, onClose }: { project: string; onClose: () => void }) {
  const [title, setTitle] = useState("");
  const [id, setId] = useState("");
  const [touched, setTouched] = useState(false);
  const [description, setDescription] = useState("");
  const [error, setError] = useState("");
  const navigate = useNavigate();
  const mapID = touched ? id : slug(title);
  const create = async () => {
    try {
      await api.createMap(project, mapID, title, description);
      navigate(`/products/${project}/maps/${mapID}`);
    } catch (e) {
      setError((e as Error).message);
    }
  };
  return (
    <Modal
      title="New story map"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" disabled={!title.trim() || !mapID} onClick={create}>
            Create
          </button>
        </>
      }
    >
      <div className="field">
        <label htmlFor="map-title">Title</label>
        <input id="map-title" className="input" value={title} onChange={(e) => setTitle(e.target.value)} autoFocus placeholder="e.g. Arcade" />
      </div>
      <div className="field">
        <label htmlFor="map-id">Directory</label>
        <input id="map-id" className="input mono" value={mapID} onChange={(e) => (setTouched(true), setId(e.target.value))} />
        <span className="hint">product/user-story-maps/{mapID || "…"}/</span>
      </div>
      <div className="field">
        <label htmlFor="map-desc">Description</label>
        <textarea id="map-desc" className="input" value={description} onChange={(e) => setDescription(e.target.value)} placeholder="Whose journey this maps, and why" />
      </div>
      {error && <div className="error-box">{error}</div>}
      <p className="small faint">It starts with one phase, one activity and an MVP release; nothing is committed until you commit.</p>
    </Modal>
  );
}

function Docs({ project, view, onChange }: { project: string; view: ProductView; onChange: () => void }) {
  const [selected, setSelected] = useState(view.docs[0] ?? "");
  const [content, setContent] = useState<string | null>(null);
  const [draft, setDraft] = useState<string | null>(null);
  const [adding, setAdding] = useState(false);
  const [newName, setNewName] = useState("");
  const toast = useToast();
  const changed = new Set(view.workspace.changes.map((c) => c.path));

  useEffect(() => {
    if (!selected) return;
    setContent(null);
    setDraft(null);
    api.readFile(project, selected).then(
      (f) => setContent(f.content),
      (e) => toast("error", (e as Error).message),
    );
  }, [project, selected, toast]);

  const save = async () => {
    if (draft === null) return;
    try {
      await api.writeFile(project, selected, draft);
      setContent(draft);
      setDraft(null);
      onChange();
    } catch (e) {
      toast("error", (e as Error).message);
    }
  };
  const create = async (path: string, body: string) => {
    try {
      await api.writeFile(project, path, body);
      setAdding(false);
      setNewName("");
      setSelected(path);
      onChange();
    } catch (e) {
      toast("error", (e as Error).message);
    }
  };
  const remove = async () => {
    if (!window.confirm(`Delete ${selected}? (Nothing is committed until you commit.)`)) return;
    try {
      await api.removeFile(project, selected);
      setSelected(view.docs.find((d) => d !== selected) ?? "");
      onChange();
    } catch (e) {
      toast("error", (e as Error).message);
    }
  };

  if (view.docs.length === 0 && !adding)
    return (
      <Empty icon={BookOpen} title="No product docs yet">
        <p>Start with a README that says what the product is, who it is for and what good looks like. Flows read it for context.</p>
        <button className="btn primary" onClick={() => create("product/README.md", README_TEMPLATE)}>
          <FilePlus2 />
          Create product/README.md
        </button>
      </Empty>
    );

  return (
    <div className="docs">
      <nav className="docs-list" aria-label="Product docs">
        {view.docs.map((d) => (
          <button key={d} className={`docs-item${d === selected ? " active" : ""}`} onClick={() => setSelected(d)}>
            <span className="mono clamp">{d.replace(/^product\//, "")}</span>
            {changed.has(d) && <span className="dot-changed" title="Uncommitted edits" />}
          </button>
        ))}
        {adding ? (
          <form className="row" onSubmit={(e) => (e.preventDefault(), create(`product/${slug(newName.replace(/\.md$/, "")) || "doc"}.md`, `# ${newName.replace(/\.md$/, "")}\n`))}>
            <input className="input" value={newName} onChange={(e) => setNewName(e.target.value)} placeholder="Doc name" autoFocus aria-label="Doc name" />
            <button className="btn sm" disabled={!newName.trim()}>
              Add
            </button>
          </form>
        ) : (
          <button className="btn ghost sm" onClick={() => setAdding(true)}>
            <FilePlus2 />
            New doc
          </button>
        )}
      </nav>
      <section className="docs-view card">
        {selected && (
          <div className="row docs-head">
            <span className="mono small grow clamp">{selected}</span>
            {draft === null ? (
              <>
                <button className="btn sm" onClick={() => setDraft(content ?? "")} disabled={content === null}>
                  <Pencil />
                  Edit
                </button>
                <button className="btn ghost icon sm" onClick={remove} aria-label="Delete doc" title="Delete">
                  <Trash2 />
                </button>
              </>
            ) : (
              <>
                <button className="btn sm" onClick={() => setDraft(null)}>
                  Cancel
                </button>
                <button className="btn primary sm" onClick={save}>
                  Save
                </button>
              </>
            )}
          </div>
        )}
        <div className="docs-body">
          {content === null ? (
            <Spinner />
          ) : draft !== null ? (
            <textarea className="input mono docs-editor" value={draft} onChange={(e) => setDraft(e.target.value)} aria-label="Markdown" />
          ) : (
            <Markdown source={content} />
          )}
        </div>
      </section>
    </div>
  );
}
