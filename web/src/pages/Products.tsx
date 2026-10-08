import { useMemo, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { FolderGit2, Lock, Plus, Search } from "lucide-react";
import { api, type RepoRow } from "../api";
import { useResource } from "../hooks";
import { timeAgo } from "../format";
import { Empty, Modal, Spinner, useToast } from "../ui";

export default function Products() {
  const overview = useResource(() => api.overview(), [], (e) => e.type === "config");
  const [linking, setLinking] = useState(false);
  if (!overview.data) return <div className="page">{overview.error ? <div className="error-box">{overview.error}</div> : <Spinner lg />}</div>;
  const projects = overview.data.projects ?? [];
  return (
    <div className="page">
      <div className="page-head">
        <h1>Products</h1>
        <span className="sub">Linked repositories. Each keeps its product docs and user story maps in product/.</span>
        <span className="spacer" />
        <button className="btn primary" onClick={() => setLinking(true)}>
          <Plus />
          Link a repository
        </button>
      </div>
      {projects.length === 0 ? (
        <Empty icon={FolderGit2} title="No repositories linked">
          <p>Link a GitHub repository to write its product docs and story maps here.</p>
        </Empty>
      ) : (
        <div className="grid-cards">
          {projects.map((p) => (
            <Link key={p.name} to={`/products/${p.name}`} className="card card-pad product-card">
              <h3>
                <FolderGit2 size={16} />
                {p.name}
              </h3>
              {p.description && <p className="small muted clamp">{p.description}</p>}
              <div className="chips">
                <span className="chip mono">{p.repo}</span>
                <span className="chip">base {p.base}</span>
              </div>
            </Link>
          ))}
        </div>
      )}
      {linking && <LinkRepo onClose={() => setLinking(false)} />}
    </div>
  );
}

function LinkRepo({ onClose }: { onClose: () => void }) {
  const repos = useResource(() => api.repos(), []);
  const [q, setQ] = useState("");
  const [picked, setPicked] = useState<RepoRow | null>(null);
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const toast = useToast();
  const navigate = useNavigate();
  const shown = useMemo(
    () => (repos.data ?? []).filter((r) => !r.archived && r.full_name.toLowerCase().includes(q.toLowerCase())).slice(0, 100),
    [repos.data, q],
  );

  const pick = (r: RepoRow) => {
    setPicked(r);
    setName(r.full_name.split("/")[1].toLowerCase().replace(/[^a-z0-9-]/g, "-"));
    setError("");
  };
  const link = async () => {
    if (!picked) return;
    setBusy(true);
    try {
      const r = await api.linkRepo(picked.full_name, name, "");
      toast("ok", `Linked ${picked.full_name}`);
      navigate(`/products/${r.project}`);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal
      title="Link a repository"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" disabled={!picked || !name || busy} onClick={link}>
            {busy && <Spinner />}
            Link {picked?.full_name ?? ""}
          </button>
        </>
      }
    >
      <p className="small muted" style={{ marginTop: 0 }}>
        Linking adds a project with read and write access to the repository. Flows can then work in it, and you can keep product docs and story maps in its
        product/ directory.
      </p>
      <div className="field">
        <div className="search-input">
          <Search size={14} />
          <input className="input" placeholder="Filter repositories" value={q} onChange={(e) => setQ(e.target.value)} autoFocus aria-label="Filter repositories" />
        </div>
      </div>
      <div className="repo-list" role="listbox" aria-label="Repositories">
        {repos.error && <div className="error-box">{repos.error}</div>}
        {!repos.data && !repos.error && <Spinner />}
        {shown.map((r) => (
          <button
            key={r.full_name}
            role="option"
            aria-selected={picked?.full_name === r.full_name}
            className={`repo-row${picked?.full_name === r.full_name ? " selected" : ""}`}
            disabled={!!r.project}
            onClick={() => pick(r)}
          >
            <span className="mono grow">
              {r.private && <Lock size={12} />} {r.full_name}
            </span>
            {r.project ? <span className="chip accent">linked as {r.project}</span> : r.pushed_at && <span className="small faint">{timeAgo(Date.parse(r.pushed_at))}</span>}
            {r.description && <span className="small muted repo-desc">{r.description}</span>}
          </button>
        ))}
      </div>
      {picked && (
        <div className="field" style={{ marginTop: 12 }}>
          <label htmlFor="project-name">Project name</label>
          <input id="project-name" className="input mono" value={name} onChange={(e) => setName(e.target.value)} />
          <span className="hint">Base branch {picked.default_branch}. Change access, models and deploy settings later in Settings → Projects.</span>
        </div>
      )}
      {error && <div className="error-box">{error}</div>}
    </Modal>
  );
}
