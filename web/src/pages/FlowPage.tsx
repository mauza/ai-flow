import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { AlertCircle, Code2, History, ListTree, MessageSquare, Play, RotateCcw, Save, Sparkles, Trash2 } from "lucide-react";
import { api, type FlowView, type Graph, type Issue } from "../api";
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
  return <FlowEditor key={name} name={name} />;
}

function FlowEditor({ name }: { name: string }) {
  const [params, setParams] = useSearchParams();
  const version = Number(params.get("v")) || undefined;
  const navigate = useNavigate();
  const toast = useToast();

  // `flow` events also cover a flow being created, so a 404 page recovers on its own.
  const view = useResource(() => api.flow(name, version), [name, version], (e) => (e.type === "flow" && e.id === name) || e.type === "run" || e.type === "task");
  const overview = useResource(() => api.overview(), []);

  const [yaml, setYaml] = useState("");
  const [saved, setSaved] = useState("");
  const [analysis, setAnalysis] = useState<{ yaml: string; graph: Graph; issues: Issue[] } | null>(null);
  const [baseVersion, setBaseVersion] = useState(0);
  const [remote, setRemote] = useState<FlowView | null>(null);
  const [validationError, setValidationError] = useState<{ yaml: string; message: string } | null>(null);
  const [validationRetry, setValidationRetry] = useState(0);
  const [selected, setSelected] = useState<string | null>(null);
  const [tab, setTab] = useState<Tab>("node");
  const [saving, setSaving] = useState(false);
  const [starting, setStarting] = useState(false);
  const loadedKey = useRef("");
  const mounted = useRef(false);
  useEffect(() => {
    mounted.current = true;
    return () => { mounted.current = false; };
  }, []);

  const latest = view.data?.versions[0]?.version ?? 0;
  const current = baseVersion;
  const isOld = !!version && version !== latest;
  const dirty = yaml !== saved;
  const validationFailed = validationError?.yaml === yaml;
  const validating = analysis?.yaml !== yaml && !validationFailed;
  const validated = analysis?.yaml === yaml && !validationFailed;

  const adopt = (data: FlowView) => {
    loadedKey.current = `${data.flow.name}@${data.flow.version}`;
    setBaseVersion(data.flow.version);
    setYaml(data.flow.yaml);
    setSaved(data.flow.yaml);
    setAnalysis({ yaml: data.flow.yaml, graph: data.graph, issues: data.issues });
    setValidationError(null);
    setRemote(null);
  };

  // Adopt server content when it changes, unless the user has unsaved edits.
  useEffect(() => {
    const f = view.data?.flow;
    if (!f) return;
    const key = `${f.name}@${f.version}`;
    if (key === loadedKey.current) {
      // The same YAML can resolve differently after a configuration change.
      if (yaml === f.yaml) {
        setAnalysis({ yaml: f.yaml, graph: view.data!.graph, issues: view.data!.issues });
        setValidationError(null);
      }
      return;
    }
    // A refresh that started before our save must not roll the editor backward.
    if (!version && loadedKey.current && f.version < baseVersion) return;
    if (loadedKey.current.startsWith(f.name + "@") && yaml !== saved) {
      setRemote(view.data!);
      return;
    }
    adopt(view.data!);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [view.data]);

  // Re-validate as the YAML changes. Before the flow has loaded, the editor's
  // empty YAML is a placeholder, not a document to validate.
  useEffect(() => {
    if (!loadedKey.current) return;
    if (analysis?.yaml === yaml && !validationRetry) return;
    let canceled = false;
    setValidationError(null);
    const t = window.setTimeout(() => {
      api
        .validate(yaml)
        .then((result) => {
          if (!canceled) setAnalysis({ ...result, yaml });
        })
        .catch((e: Error) => {
          if (!canceled) setValidationError({ yaml, message: e.message });
        });
    }, 250);
    return () => {
      canceled = true;
      window.clearTimeout(t);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [yaml, validationRetry]);

  const save = useCallback(async () => {
    if (!dirty || saving || starting) return;
    setSaving(true);
    try {
      const res = await api.saveFlow(name, yaml, isOld ? `Restored from v${version}` : "Edited in the UI");
      if (!mounted.current) return;
      setSaved(yaml);
      setAnalysis((previous) => previous?.yaml === yaml ? { ...previous, issues: res.issues } : previous);
      loadedKey.current = `${name}@${res.flow.version}`;
      setBaseVersion(res.flow.version);
      setRemote(null);
      toast("ok", `Saved v${res.flow.version}${res.issues.some((i) => i.severity === "error") ? " (with errors)" : ""}`);
      if (version) setParams({});
      view.reload();
    } catch (e) {
      if (mounted.current) toast("error", (e as Error).message);
    } finally {
      setSaving(false);
    }
  }, [dirty, saving, starting, name, yaml, isOld, version, toast, setParams, view]);

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
  const displayedIssues: Issue[] = validated ? analysis.issues : [{ severity: "warning", message: validationFailed ? "Validation unavailable; retry before running." : "Validating current edits…" }];

  const run = async () => {
    if (!validated || errors.length || starting || saving || !baseVersion) return;
    setStarting(true);
    try {
      let runVersion = baseVersion;
      if (dirty) {
        const res = await api.saveFlow(name, yaml, "Saved before running");
        if (!mounted.current) return;
        runVersion = res.flow.version;
        loadedKey.current = `${name}@${runVersion}`;
        setBaseVersion(runVersion);
        setSaved(yaml);
        setRemote(null);
        if (res.issues.some((i) => i.severity === "error")) throw new Error("Saved flow has validation errors; fix them before running.");
      }
      const r = await api.startRun(name, runVersion);
      if (mounted.current) navigate(`/runs/${r.id}`);
    } catch (e) {
      if (mounted.current) toast("error", (e as Error).message);
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
          value={current}
          disabled={saving || starting}
          onChange={(e) => {
            const v = Number(e.target.value);
            if (dirty && !window.confirm("Discard unsaved changes?")) return;
            if (remote && v === remote.flow.version) {
              adopt(remote);
              setParams(v === latest ? {} : { v: String(v) });
              return;
            }
            loadedKey.current = "";
            setRemote(null);
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
        {validationFailed ? <Pill status="invalid" label="validation unavailable" /> : validating ? <Pill status="pending" label="validating" /> : errors.length ? <Pill status="invalid" label={`${errors.length} error${errors.length > 1 ? "s" : ""}`} /> : <Pill status="valid" label="valid" />}
        {(dirty || remote) && (
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
            disabled={saving || starting}
            onClick={() => {
              if (remote) adopt(remote);
              else setYaml(saved);
            }}
            title="Discard unsaved changes"
          >
            <RotateCcw />
            Revert
          </button>
        )}
        <button className="btn" disabled={!dirty || saving || starting} onClick={save} title="Save a new version (Ctrl+S)">
          {saving ? <Spinner /> : <Save />}
          {isOld ? "Restore" : "Save"}
        </button>
        <button className="btn primary" disabled={!validated || errors.length > 0 || starting || saving || isOld} onClick={run} title={errors.length ? "Fix validation errors first" : dirty ? "Save and run" : "Run this flow"}>
          {starting ? <Spinner /> : <Play />}
          {dirty ? "Save & run" : "Run"}
        </button>
        <button
          className="btn ghost icon"
          aria-label="Delete flow"
          disabled={!!activeRun || saving || starting}
          title={activeRun ? "A run of this flow is active; cancel or finish it first" : "Delete this flow, all its versions and runs"}
          onClick={async () => {
            const runs = view.data?.runs.length ?? 0;
            if (!window.confirm(`Delete ${name} with all ${view.data?.versions.length ?? 0} version(s) and ${runs} run(s)? This cannot be undone.`)) return;
            try {
              await api.deleteFlow(name);
              toast("ok", `Deleted ${name}`);
              navigate("/flows");
            } catch (e) {
              toast("error", (e as Error).message);
            }
          }}
        >
          <Trash2 />
        </button>
        </div>
      </div>

      {remote && <div className="warn-box" role="status">v{remote.flow.version} was saved elsewhere. You are editing v{baseVersion}. Revert loads v{remote.flow.version}; Save keeps your edits as a new version.</div>}
      {validationFailed && <div className="warn-box" role="status">Validation unavailable: {validationError.message} <button className="btn sm" onClick={() => setValidationRetry((n) => n + 1)}>Retry validation</button></div>}

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
          <button className="btn sm" disabled={saving || starting} onClick={() => setParams({})}>
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
              issues={displayedIssues}
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
            {tab === "node" && <Inspector yaml={yaml} graph={graph} issues={displayedIssues} overview={overview.data} selected={selected} onSelect={onSelect} onChange={setYaml} />}
            {tab === "yaml" && <YamlEditor value={yaml} onChange={setYaml} issues={displayedIssues} fileName={name} />}
            <div hidden={tab !== "chat"} style={{ height: "100%" }}><Chat name={name} yaml={yaml} onApply={setYaml} /></div>
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
