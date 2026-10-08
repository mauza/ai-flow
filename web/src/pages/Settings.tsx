import { lazy, Suspense, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { Box, Cpu, FolderGit2, KeyRound, Plug, Plus, Puzzle, Settings2, Sparkles, Trash2, Wand2, Wrench } from "lucide-react";
import { parse } from "yaml";
import { api, type ConfigEdit, type ConfigView } from "../api";
import { useResource } from "../hooks";
import { Modal, Spinner, useToast } from "../ui";

const YamlEditor = lazy(() => import("../flow/YamlEditor"));

type TabID = "models" | "presets" | "runtimes" | "harnesses" | "grants" | "skills" | "planner" | "projects" | "connections";

const tabs: { id: TabID; label: string; icon: typeof Cpu; noun: string; blurb: string; template: string }[] = [
  {
    id: "models", label: "Models", icon: Cpu, noun: "model",
    blurb: "Models nodes and the planner can use. The upstream is an LLM endpoint from the environment.",
    template: "upstream: home\nmodel: \nsize: medium # small | medium | large | frontier\ncontext_tokens: 32000\ntool_use: good\ncost: free\nnotes: \n",
  },
  {
    id: "presets", label: "Nodes", icon: Wand2, noun: "node preset",
    blurb: "Reusable steps the planner and the flow editor offer (uses: preset/<name>).",
    template: "type: agent\ncategory: Implementation\ndescription: \nwhen_to_use: \nprompt: |\n  \noutcomes: [done, stuck]\n",
  },
  {
    id: "runtimes", label: "Runtimes", icon: Box, noun: "runtime",
    blurb: "Container images node pods run in.",
    template: "image: \ndescription: \n",
  },
  {
    id: "harnesses", label: "Harnesses", icon: Wrench, noun: "harness",
    blurb: "Agent harnesses a runtime image provides.",
    template: "description: \n",
  },
  {
    id: "grants", label: "Access", icon: KeyRound, noun: "grant",
    blurb: "What a node may reach: git repositories, MCP tools, secrets, the internet.",
    template: "kind: git # git | mcp | secret | egress\nurl: https://github.com/owner/repo.git\nmodes: [read, write]\ndescription: \n",
  },
  {
    id: "skills", label: "Skills", icon: Puzzle, noun: "skill",
    blurb: "Instruction bundles mounted into agent nodes.",
    template: "description: \nfiles:\n  SKILL.md: |\n    \n",
  },
  { id: "planner", label: "Planner", icon: Sparkles, noun: "setting", blurb: "The planner's model and guidance, and node defaults for every flow.", template: "" },
  {
    id: "projects", label: "Projects", icon: FolderGit2, noun: "project",
    blurb: "Repositories ai-flow works on, what flows in them may use, and how they deploy.",
    template: "spec:\n  description: \n  repo: repo/\n  base: main\n  start: manual\n  allow:\n    grants: []\n    models: []\n",
  },
  { id: "connections", label: "Connections", icon: Plug, noun: "", blurb: "", template: "" },
];

// summary picks the most telling field of an entry for its card.
function summary(src: string): string {
  try {
    const v = parse(src) as Record<string, unknown>;
    const spec = (v?.spec ?? v) as Record<string, unknown>;
    for (const k of ["description", "notes", "when_to_use", "url", "image", "model", "repo"]) {
      if (typeof spec?.[k] === "string" && spec[k]) return spec[k] as string;
    }
  } catch {
    /* shown as-is */
  }
  return "";
}

function chips(tab: TabID, src: string): string[] {
  try {
    const v = parse(src) as Record<string, any>;
    switch (tab) {
      case "models":
        return [v.upstream && `upstream ${v.upstream}`, v.size, v.context_tokens && `${Math.round(v.context_tokens / 1000)}k ctx`, v.cost].filter(Boolean);
      case "presets":
        return [v.type ?? (v.uses ? "uses " + v.uses : ""), v.category, ...(v.outcomes ?? [])].filter(Boolean);
      case "grants":
        return [v.kind, ...(v.modes ?? [])].filter(Boolean);
      case "projects":
        return [v.spec?.repo, v.spec?.base && `base ${v.spec.base}`, v.spec?.start, v.spec?.deploy && "deploys"].filter(Boolean);
    }
  } catch {
    /* none */
  }
  return [];
}

interface Editing {
  section: string;
  name: string;
  yaml: string;
  isNew: boolean;
  noun: string;
}

export default function Settings() {
  const cfg = useResource(() => api.config(), [], (e) => e.type === "config");
  const [tab, setTab] = useState<TabID>("models");
  const [editing, setEditing] = useState<Editing | null>(null);
  const toast = useToast();
  const meta = tabs.find((t) => t.id === tab)!;

  const remove = async (section: string, name: string, noun: string) => {
    if (!window.confirm(`Delete ${noun} ${name}?`)) return;
    try {
      const r = await api.editConfig([{ section, name, yaml: "" }]);
      toast("ok", `Deleted ${name}`);
      r.warnings.forEach((w) => toast("error", w));
      cfg.reload();
    } catch (e) {
      toast("error", (e as Error).message);
    }
  };

  if (!cfg.data) return <div className="page">{cfg.error ? <div className="error-box">{cfg.error}</div> : <Spinner lg />}</div>;
  const d = cfg.data;
  const section = tab === "projects" ? d.projects : d.sections[tab] ?? {};
  const names = Object.keys(section).sort();

  return (
    <div className="page settings-page">
      <div className="page-head">
        <h1>Settings</h1>
        <span className="sub">The catalog and projects. Changes are checked first, then apply right away.</span>
      </div>
      <div className="tabs settings-tabs" role="tablist">
        {tabs.map((t) => (
          <button key={t.id} role="tab" aria-selected={tab === t.id} className={`tab${tab === t.id ? " active" : ""}`} onClick={() => setTab(t.id)}>
            <t.icon size={14} />
            {t.label}
            {t.id !== "planner" && t.id !== "connections" && (
              <span className="badge">{Object.keys(t.id === "projects" ? d.projects : d.sections[t.id] ?? {}).length}</span>
            )}
          </button>
        ))}
      </div>

      {tab === "connections" ? (
        <Connections cfg={d} />
      ) : tab === "planner" ? (
        <div className="settings-body">
          <p className="muted small">{meta.blurb}</p>
          <div className="grid-cards">
            {["planner", "defaults"].map((name) => (
              <div key={name} className="card card-pad setting-card">
                <div className="row">
                  <h3 className="mono">{name}</h3>
                  <span className="spacer" />
                  <button className="btn sm" onClick={() => setEditing({ section: "settings", name, yaml: d.settings[name] ?? "", isNew: false, noun: name })}>
                    Edit
                  </button>
                </div>
                <pre className="code">{d.settings[name] || "(not set)"}</pre>
              </div>
            ))}
          </div>
        </div>
      ) : (
        <div className="settings-body">
          <div className="row wrap">
            <p className="muted small grow">{meta.blurb}</p>
            {tab === "projects" && (
              <Link className="btn sm" to="/products">
                <FolderGit2 />
                Link a repository
              </Link>
            )}
            <button className="btn primary sm" onClick={() => setEditing({ section: tab, name: "", yaml: meta.template, isNew: true, noun: meta.noun })}>
              <Plus />
              New {meta.noun}
            </button>
          </div>
          <div className="grid-cards">
            {names.map((name) => (
              <div key={name} className="card card-pad setting-card">
                <div className="row">
                  <h3 className="mono grow" title={name}>
                    {name}
                  </h3>
                  <button className="btn sm" onClick={() => setEditing({ section: tab, name, yaml: section[name], isNew: false, noun: meta.noun })} aria-label={`Edit ${name}`}>
                    Edit
                  </button>
                  <button className="btn ghost icon sm" onClick={() => remove(tab, name, meta.noun)} aria-label={`Delete ${name}`} title="Delete">
                    <Trash2 />
                  </button>
                </div>
                {summary(section[name]) && <p className="small muted clamp">{summary(section[name])}</p>}
                <div className="chips">
                  {chips(tab, section[name]).map((c) => (
                    <span key={c} className="chip">
                      {c}
                    </span>
                  ))}
                </div>
              </div>
            ))}
            {names.length === 0 && <div className="muted small">None yet.</div>}
          </div>
        </div>
      )}
      {editing && (
        <EntryEditor
          editing={editing}
          onClose={() => setEditing(null)}
          onSaved={(warnings) => {
            setEditing(null);
            toast("ok", "Saved; it applies now");
            warnings.forEach((w) => toast("error", w));
            cfg.reload();
          }}
        />
      )}
    </div>
  );
}

function EntryEditor({ editing, onClose, onSaved }: { editing: Editing; onClose: () => void; onSaved: (warnings: string[]) => void }) {
  const [name, setName] = useState(editing.name);
  const [yaml, setYaml] = useState(editing.yaml);
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  const parseError = useMemo(() => {
    try {
      parse(yaml);
      return "";
    } catch (e) {
      return (e as Error).message.split("\n")[0];
    }
  }, [yaml]);

  const save = async () => {
    setSaving(true);
    setError("");
    try {
      const edit: ConfigEdit = { section: editing.section, name: name.trim(), yaml };
      const r = await api.editConfig([edit]);
      onSaved(r.warnings);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setSaving(false);
    }
  };

  return (
    <Modal
      title={editing.isNew ? `New ${editing.noun}` : `Edit ${editing.noun} ${editing.name}`}
      onClose={onClose}
      footer={
        <>
          <span className="small faint grow">Checked against the whole config before it applies.</span>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" disabled={saving || !name.trim() || !!parseError} onClick={save}>
            {saving ? <Spinner /> : null}
            Save
          </button>
        </>
      }
    >
      {editing.isNew && (
        <div className="field">
          <label htmlFor="entry-name">Name</label>
          <input id="entry-name" className="input mono" value={name} onChange={(e) => setName(e.target.value)} autoFocus placeholder={editing.section === "grants" ? "repo/my-app" : "name"} />
        </div>
      )}
      <div className="entry-yaml">
        <Suspense fallback={<Spinner />}>
          <YamlEditor value={yaml} onChange={setYaml} issues={[]} fileName={name || "entry"} hint="YAML for this entry only." />
        </Suspense>
      </div>
      {(parseError || error) && <div className="error-box" style={{ marginTop: 10 }}>{parseError || error}</div>}
    </Modal>
  );
}

function Connections({ cfg }: { cfg: ConfigView }) {
  const env = cfg.env;
  const ok = (set: boolean) => <span className={`pill ${set ? "succeeded" : "failed"}`}><span className="dot" />{set ? "token set" : "token missing"}</span>;
  return (
    <div className="settings-body stack">
      <p className="muted small">
        Connections come from the environment file and sealed secrets in the cluster; ai-flow reads them but never shows or stores tokens. To change one, update the
        environment config and its secret.
      </p>
      <div className="grid-cards">
        <div className="card card-pad setting-card">
          <h3>
            <Settings2 size={15} /> GitHub
          </h3>
          <div className="row wrap small">
            {ok(env.github.token_set)}
            <span className="chip mono">{env.github.token_env}</span>
            <span className="chip mono">{env.github.api_url}</span>
          </div>
          <p className="small muted">Lists your repositories, reads and commits product files, opens and merges pull requests.</p>
        </div>
        <div className="card card-pad setting-card">
          <h3>Git hosts</h3>
          {env.git_hosts.map((h) => (
            <div key={h.host} className="row wrap small">
              <b className="mono">{h.host}</b>
              {ok(h.token_set)}
              <span className="chip mono">{h.token_env}</span>
            </div>
          ))}
          <p className="small muted">Commits by nodes are authored as {env.git_author}.</p>
        </div>
        <div className="card card-pad setting-card">
          <h3>LLM endpoints</h3>
          {env.upstreams.map((u) => (
            <div key={u.name} className="row small">
              <b className="mono">{u.name}</b>
              <span className="mono muted clamp">{u.base_url}</span>
            </div>
          ))}
        </div>
        <div className="card card-pad setting-card">
          <h3>Metrics and tools</h3>
          <div className="small">
            Metrics: <span className="mono">{env.metrics_url || "not configured"}</span>
          </div>
          <div className="small">MCP servers: {env.mcp_servers.length ? env.mcp_servers.join(", ") : "none"}</div>
        </div>
      </div>
      {cfg.seeded_at > 0 && (
        <p className="small faint">
          The catalog and projects have been stored in ai-flow's database since {new Date(cfg.seeded_at).toLocaleString()}; edits to the config files no longer
          change them.
        </p>
      )}
    </div>
  );
}
