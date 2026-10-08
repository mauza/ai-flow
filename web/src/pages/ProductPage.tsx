import { useEffect, useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { BookOpen, ExternalLink, FilePlus2, Map as MapIcon, Pencil, Plus, Trash2 } from "lucide-react";
import { api, type ProductView } from "../api";
import { useResource } from "../hooks";
import { Empty, Modal, Spinner, useToast } from "../ui";
import Markdown from "../product/Markdown";
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
  const [params, setParams] = useSearchParams();
  const tab = params.get("tab") === "docs" ? "docs" : "maps";

  if (!view.data)
    return (
      <div className="page">
        {view.error ? <div className="error-box">{view.error}</div> : <div className="row muted"><Spinner /> Checking out product/…</div>}
      </div>
    );
  const d = view.data;
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
      </div>
      <WorkspaceBar project={p} status={d.workspace} onChange={view.reload} defaultMessage={`Update ${d.project.name} product docs`} />
      <div className="tabs" role="tablist">
        <button role="tab" aria-selected={tab === "maps"} className={`tab${tab === "maps" ? " active" : ""}`} onClick={() => setParams({})}>
          <MapIcon size={14} />
          Story maps <span className="badge">{d.maps.length}</span>
        </button>
        <button role="tab" aria-selected={tab === "docs"} className={`tab${tab === "docs" ? " active" : ""}`} onClick={() => setParams({ tab: "docs" })}>
          <BookOpen size={14} />
          Docs <span className="badge">{d.docs.length}</span>
        </button>
      </div>
      {tab === "maps" ? <Maps project={p} view={d} /> : <Docs project={p} view={d} onChange={view.reload} />}
    </div>
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
