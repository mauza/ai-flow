import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { AlertCircle, Code2, History, ListTree, MessageSquare, Play, RotateCcw, Save, Sparkles } from "lucide-react";
import { api, type Graph, type Issue } from "../api";
import { useResource } from "../hooks";
import { timeAgo } from "../format";
import FlowGraph from "../graph/FlowGraph";
import Inspector from "../flow/Inspector";
import YamlEditor from "../flow/YamlEditor";
import Chat from "../flow/Chat";
import { setTransition } from "../flow/yamlEdit";
import { Pill, SourceChip, Spinner, useToast } from "../ui";
import { RunsTable } from "./Runs";

type Tab = "node" | "yaml" | "chat" | "runs";

export default function FlowPage() {
  const { name = "" } = useParams();
  const [params, setParams] = useSearchParams();
  const version = Number(params.get("v")) || undefined;
  const navigate = useNavigate();
  const toast = useToast();

  // `flow` events also cover a flow being created, so a 404 page recovers on its own.
  const view = useResource(() => api.flow(name, version), [name, version], (e) => (e.type === "flow" && e.id === name) || e.type === "run" || e.type === "task");
  const overview = useResource(() => api.overview(), []);

  const [yaml, setYaml] = useState("");
  const [saved, setSaved] = useState("");
  const [analysis, setAnalysis] = useState<{ graph: Graph; issues: Issue[] } | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [tab, setTab] = useState<Tab>("node");
  const [saving, setSaving] = useState(false);
  const [starting, setStarting] = useState(false);
  const loadedKey = useRef("");

  const latest = view.data?.versions[0]?.version ?? 0;
  const current = view.data?.flow.version ?? 0;
  const isOld = !!version && version !== latest;
  const dirty = yaml !== saved;

  // Adopt server content when it changes, unless the user has unsaved edits.
  useEffect(() => {
    const f = view.data?.flow;
    if (!f) return;
    const key = `${f.name}@${f.version}`;
    if (key === loadedKey.current) return;
    if (loadedKey.current.startsWith(f.name + "@") && yaml !== saved) {
      toast("error", `v${f.version} was saved elsewhere; your unsaved edits are kept. Revert to load it.`);
      loadedKey.current = key;
      return;
    }
    loadedKey.current = key;
    setYaml(f.yaml);
    setSaved(f.yaml);
    setAnalysis({ graph: view.data!.graph, issues: view.data!.issues });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [view.data]);

  // Re-validate as the YAML changes.
  useEffect(() => {
    if (!yaml || (view.data && yaml === view.data.flow.yaml && analysis)) return;
    const t = window.setTimeout(() => {
      api
        .validate(yaml)
        .then(setAnalysis)
        .catch(() => {});
    }, 250);
    return () => window.clearTimeout(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [yaml]);

  const save = useCallback(async () => {
    if (!dirty || saving) return;
    setSaving(true);
    try {
      const res = await api.saveFlow(name, yaml, isOld ? `Restored from v${version}` : "Edited in the UI");
      setSaved(yaml);
      loadedKey.current = `${name}@${res.flow.version}`;
      toast("ok", `Saved v${res.flow.version}${res.issues.some((i) => i.severity === "error") ? " (with errors)" : ""}`);
      if (version) setParams({});
      view.reload();
    } catch (e) {
      toast("error", (e as Error).message);
    } finally {
      setSaving(false);
    }
  }, [dirty, saving, name, yaml, isOld, version, toast, setParams, view]);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key === "s") {
        e.preventDefault();
        save();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [save]);

  useEffect(() => {
    const onUnload = (e: BeforeUnloadEvent) => {
      if (dirty) e.preventDefault();
    };
    window.addEventListener("beforeunload", onUnload);
    return () => window.removeEventListener("beforeunload", onUnload);
  }, [dirty]);

  const errors = analysis?.issues.filter((i) => i.severity === "error") ?? [];
  const graph = analysis?.graph;

  const run = async () => {
    setStarting(true);
    try {
      if (dirty) {
        await api.saveFlow(name, yaml, "Saved before running");
        setSaved(yaml);
      }
      const r = await api.startRun(name);
      navigate(`/runs/${r.id}`);
    } catch (e) {
      toast("error", (e as Error).message);
    } finally {
      setStarting(false);
    }
  };

  const onSelect = useCallback((id: string | null) => {
    setSelected(id);
    if (id) setTab("node");
  }, []);

  const task = view.data?.task;
  const runsCount = view.data?.runs.length ?? 0;
  const activeRun = view.data?.runs.find((r) => r.status === "running" || r.status === "waiting" || r.status === "queued");

  const tabs = useMemo(
    () =>
      [
        { id: "node", label: selected ? "Node" : "Flow", icon: ListTree, badge: errors.length ? String(errors.length) : undefined, err: true },
        { id: "yaml", label: "YAML", icon: Code2 },
        { id: "chat", label: "Planner", icon: MessageSquare },
        { id: "runs", label: "Runs", icon: History, badge: runsCount ? String(runsCount) : undefined },
      ] as { id: Tab; label: string; icon: typeof Code2; badge?: string; err?: boolean }[],
    [selected, errors.length, runsCount],
  );

  if (view.error && !view.data) {
    return (
      <div className="page">
        {view.error === "not found" ? (
          <div className="empty">
            <Spinner lg />
            <h3>No flow named {name} yet</h3>
            <p>If the planner is still drafting it, it opens here as soon as it is saved.</p>
          </div>
        ) : (
          <div className="error-box">{view.error}</div>
        )}
      </div>
    );
  }
  if (!view.data || !graph) {
    return (
      <div className="center-fill">
        <Spinner lg />
      </div>
    );
  }

  return (
    <div className="workspace">
      <div className="workbar">
        <div className="title-group">
        <div className="crumb">
          <Link to="/flows">Flows</Link>/
        </div>
        <h1 className="mono" title={name}>
          {name}
        </h1>
        <select
          className="input"
          style={{ width: "auto", maxWidth: 190, height: 30, padding: "0 8px", flex: "none" }}
          value={version ?? latest}
          onChange={(e) => {
            const v = Number(e.target.value);
            if (dirty && !window.confirm("Discard unsaved changes?")) return;
            loadedKey.current = "";
            setParams(v === latest ? {} : { v: String(v) });
          }}
          title="Version"
        >
          {view.data.versions.map((v) => (
            <option key={v.version} value={v.version}>
              v{v.version}
              {v.version === latest ? " (latest)" : ""} · {v.created_by || ""} · {timeAgo(v.created_at)}
            </option>
          ))}
        </select>
        {errors.length ? <Pill status="invalid" label={`${errors.length} error${errors.length > 1 ? "s" : ""}`} /> : <Pill status="valid" label="valid" />}
        {dirty && (
          <span className="small muted row" style={{ gap: 6 }}>
            <span className="dirty-dot" /> unsaved
          </span>
        )}
        {task && (
          <Link to={`/?task=${task.id}`} className="small muted row task-link" style={{ gap: 6 }} title={task.title}>
            <SourceChip source={task.source} identifier={task.identifier} />
            <span className="ellipsis">{task.title}</span>
          </Link>
        )}
        </div>
        <div className="actions">
        {activeRun && (
          <Link className="btn sm" to={`/runs/${activeRun.id}`}>
            <Pill status={activeRun.status} /> view run
          </Link>
        )}
        {dirty && (
          <button
            className="btn ghost"
            onClick={() => {
              setYaml(saved);
            }}
            title="Discard unsaved changes"
          >
            <RotateCcw />
            Revert
          </button>
        )}
        <button className="btn" disabled={!dirty || saving} onClick={save} title="Save a new version (Ctrl+S)">
          {saving ? <Spinner /> : <Save />}
          {isOld ? "Restore" : "Save"}
        </button>
        <button className="btn primary" disabled={errors.length > 0 || starting || isOld} onClick={run} title={errors.length ? "Fix validation errors first" : dirty ? "Save and run" : "Run this flow"}>
          {starting ? <Spinner /> : <Play />}
          {dirty ? "Save & run" : "Run"}
        </button>
        </div>
      </div>

      {view.data.planning && (
        <div className="gate-banner" style={{ borderColor: "var(--accent)", background: "var(--accent-bg)" }}>
          <Spinner />
          <div className="q">
            <b>The planner is working on this flow</b>
            <span className="small muted">A new version will appear here when it's done.</span>
          </div>
        </div>
      )}
      {isOld && (
        <div className="gate-banner" style={{ borderColor: "var(--warn)", background: "var(--warn-bg)" }}>
          <AlertCircle size={16} />
          <div className="q">
            Viewing v{current}. Edits here are saved as a new version (Restore).
          </div>
          <button className="btn sm" onClick={() => setParams({})}>
            Back to latest
          </button>
        </div>
      )}

      <div className="split">
        <div className="canvas">
          {graph.nodes.length === 0 ? (
            <div className="center-fill" style={{ height: "100%" }}>
              <div className="empty">
                <Sparkles />
                <h3>No nodes yet</h3>
                <p>Add a node from the panel, write YAML, or ask the planner.</p>
              </div>
            </div>
          ) : (
            <FlowGraph
              graph={graph}
              issues={analysis?.issues}
              selected={selected}
              onSelect={onSelect}
              editable={!isOld}
              onConnect={(from, outcome, to) => {
                try {
                  setYaml(setTransition(yaml, from, outcome, to));
                  toast("ok", `${from}: ${outcome} → ${to}`);
                } catch (e) {
                  toast("error", (e as Error).message);
                }
              }}
            />
          )}
          <div className="canvas-overlay bl">
            <div className="legend">
              <span>Drag from an outcome to a node to route it</span>
            </div>
          </div>
        </div>
        <aside className="side">
          <div className="tabs" role="tablist">
            {tabs.map((t) => (
              <button key={t.id} role="tab" aria-selected={tab === t.id} className={`tab${tab === t.id ? " active" : ""}`} onClick={() => setTab(t.id)}>
                <t.icon size={14} />
                {t.label}
                {t.badge && <span className={`badge${t.err ? " err" : ""}`}>{t.badge}</span>}
              </button>
            ))}
          </div>
          <div className="side-body">
            {tab === "node" && <Inspector yaml={yaml} graph={graph} issues={analysis?.issues ?? []} overview={overview.data} selected={selected} onSelect={onSelect} onChange={setYaml} />}
            {tab === "yaml" && <YamlEditor value={yaml} onChange={setYaml} issues={analysis?.issues ?? []} fileName={name} />}
            {tab === "chat" && <Chat name={name} yaml={yaml} onApply={setYaml} />}
            {tab === "runs" && (
              <div className="side-pad">
                <RunsTable runs={view.data.runs} compact />
              </div>
            )}
          </div>
        </aside>
      </div>
    </div>
  );
}
