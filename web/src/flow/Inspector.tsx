import { useEffect, useMemo, useState } from "react";
import { AlertCircle, AlertTriangle, ArrowRight, Flag, Plus, Trash2, X } from "lucide-react";
import type { Graph, GraphNode, Issue, Overview } from "../api";
import { TypeIcon, typeMeta, useToast } from "../ui";
import { addNode, deleteNode, nodeTemplate, rawFlow, rawNode, renameNode, setFlowField, setNodeField, setStart, setTransition, removeTransition } from "./yamlEdit";

interface Props {
  yaml: string;
  graph: Graph;
  issues: Issue[];
  overview?: Overview;
  selected: string | null;
  onSelect: (id: string | null) => void;
  onChange: (yaml: string) => void;
  readOnly?: boolean;
}

export default function Inspector(p: Props) {
  const node = p.graph.nodes.find((n) => n.id === p.selected);
  if (!node) return <FlowSummary {...p} />;
  return <NodeForm key={node.id} {...p} node={node} />;
}

// ---- editing primitives ----

/** Text input that commits on blur / Enter, so the graph doesn't relayout per keystroke. */
function CommitInput(props: { value: string; placeholder?: string; onCommit: (v: string) => void; mono?: boolean; multiline?: boolean; rows?: number; disabled?: boolean }) {
  const [v, setV] = useState(props.value);
  useEffect(() => setV(props.value), [props.value]);
  const commit = () => v !== props.value && props.onCommit(v);
  if (props.multiline) {
    return (
      <textarea
        className={`input${props.mono ? " mono" : ""}`}
        value={v}
        rows={props.rows ?? 5}
        placeholder={props.placeholder}
        disabled={props.disabled}
        onChange={(e) => setV(e.target.value)}
        onBlur={commit}
        onKeyDown={(e) => (e.metaKey || e.ctrlKey) && e.key === "Enter" && commit()}
      />
    );
  }
  return (
    <input
      className={`input${props.mono ? " mono" : ""}`}
      value={v}
      placeholder={props.placeholder}
      disabled={props.disabled}
      onChange={(e) => setV(e.target.value)}
      onBlur={commit}
      onKeyDown={(e) => e.key === "Enter" && commit()}
    />
  );
}

function IssueList({ issues, onSelect }: { issues: Issue[]; onSelect?: (id: string | null) => void }) {
  if (!issues.length) return null;
  return (
    <div className="issue-list">
      {issues.map((i, n) => (
        <div key={n} className={`issue ${i.severity}`} onClick={() => i.node && onSelect?.(i.node)}>
          {i.severity === "error" ? <AlertCircle /> : <AlertTriangle />}
          <div>
            {(i.node || i.field) && (
              <div className="where">
                {i.node ?? "flow"}
                {i.field ? `.${i.field}` : ""}
              </div>
            )}
            {i.message}
          </div>
        </div>
      ))}
    </div>
  );
}

// ---- flow summary (nothing selected) ----

function FlowSummary({ yaml, graph, issues, overview, onSelect, onChange, readOnly }: Props) {
  const raw = useMemo(() => rawFlow(yaml), [yaml]);
  const spec = (raw?.spec ?? {}) as Record<string, unknown>;
  const errors = issues.filter((i) => i.severity === "error");
  const warnings = issues.filter((i) => i.severity === "warning");
  const toast = useToast();

  const apply = (fn: () => string) => {
    try {
      onChange(fn());
    } catch (e) {
      toast("error", (e as Error).message);
    }
  };

  return (
    <div className="side-pad">
      <div className="section-title" style={{ marginTop: 0 }}>
        Flow
      </div>
      <div className="field">
        <label>Goal</label>
        <CommitInput
          value={String(spec.description ?? "")}
          placeholder="One line: what this flow achieves"
          disabled={readOnly}
          onCommit={(v) => apply(() => setFlowField(yaml, ["spec", "description"], v))}
        />
      </div>
      <dl className="kv">
        <dt>Start</dt>
        <dd className="mono">{graph.start || "—"}</dd>
        <dt>Nodes</dt>
        <dd>{graph.nodes.length}</dd>
        <dt>Repo</dt>
        <dd className="mono">{String((spec.repo as { grant?: string })?.grant ?? "project default")}</dd>
      </dl>

      <div className="section-title">Validation</div>
      {errors.length === 0 && warnings.length === 0 && <div className="pill valid">No problems</div>}
      <IssueList issues={[...errors, ...warnings]} onSelect={onSelect} />

      <div className="section-title">Nodes</div>
      <div className="stack" style={{ gap: 6 }}>
        {graph.nodes.map((n) => (
          <button key={n.id} className="btn ghost" style={{ justifyContent: "flex-start", height: 36 }} onClick={() => onSelect(n.id)}>
            <TypeIcon type={n.type} size={22} />
            <span className="mono">{n.id}</span>
            <span className="spacer" />
            <span className="faint small">{typeMeta[n.type]?.label ?? n.type}</span>
          </button>
        ))}
      </div>
      {!readOnly && <AddNode yaml={yaml} overview={overview} onAdd={(y, id) => (onChange(y), onSelect(id))} />}
    </div>
  );
}

function AddNode({ yaml, overview, onAdd }: { yaml: string; overview?: Overview; onAdd: (yaml: string, id: string) => void }) {
  const [open, setOpen] = useState(false);
  const [id, setId] = useState("");
  const [type, setType] = useState("agent");
  const toast = useToast();
  if (!open) {
    return (
      <button className="btn mt" onClick={() => setOpen(true)}>
        <Plus />
        Add node
      </button>
    );
  }
  const valid = /^[a-z][a-z0-9_]{0,40}$/.test(id);
  return (
    <div className="card card-pad mt">
      <div className="field">
        <label>Type</label>
        <div className="row wrap" style={{ gap: 6 }}>
          {Object.entries(typeMeta).map(([t, m]) => (
            <button key={t} className={`btn sm${type === t ? " primary" : ""}`} onClick={() => setType(t)} title={m.blurb}>
              {m.label}
            </button>
          ))}
        </div>
        <span className="hint">{typeMeta[type]?.blurb}</span>
      </div>
      <div className="field">
        <label>Node id</label>
        <input className="input mono" autoFocus value={id} placeholder="e.g. run_linter" onChange={(e) => setId(e.target.value.toLowerCase().replace(/[^a-z0-9_]/g, "_"))} />
      </div>
      <div className="row">
        <button
          className="btn primary"
          disabled={!valid}
          onClick={() => {
            try {
              onAdd(addNode(yaml, id, nodeTemplate(type, overview?.models.map((m) => m.name) ?? [])), id);
              setOpen(false);
              setId("");
            } catch (e) {
              toast("error", (e as Error).message);
            }
          }}
        >
          Add
        </button>
        <button className="btn ghost" onClick={() => setOpen(false)}>
          Cancel
        </button>
      </div>
    </div>
  );
}

// ---- node form ----

function NodeForm({ yaml, graph, issues, overview, node, onSelect, onChange, readOnly }: Props & { node: GraphNode }) {
  const raw = useMemo(() => rawNode(yaml, node.id) ?? {}, [yaml, node.id]);
  const toast = useToast();
  const myIssues = issues.filter((i) => i.node === node.id);
  const meta = typeMeta[node.type];

  const apply = (fn: () => string) => {
    try {
      onChange(fn());
    } catch (e) {
      toast("error", (e as Error).message);
    }
  };
  const set = (path: (string | number)[], v: unknown) => apply(() => setNodeField(yaml, node.id, path, v));
  const str = (k: string) => (typeof raw[k] === "string" ? (raw[k] as string) : "");

  const targets = ["$success", "$fail", ...graph.nodes.map((n) => n.id).filter((id) => id !== node.id), node.id];
  const next = (raw.next ?? {}) as Record<string, string>;
  const effectiveNext = Object.fromEntries(graph.edges.filter((e) => e.from === node.id && e.kind === "next").map((e) => [e.outcome, e.to]));
  const rawOutcomes = Array.isArray(raw.outcomes) ? (raw.outcomes as string[]) : undefined;
  const models = overview?.models ?? [];
  const isModel = node.type === "llm" || node.type === "agent";
  const isPod = isModel || node.type === "check";
  const repoGrants = overview?.grants.filter((g) => g.kind === "git") ?? [];
  const otherGrants = overview?.grants.filter((g) => g.kind !== "git") ?? [];
  const grants = (Array.isArray(raw.grants) ? (raw.grants as string[]) : node.grants) ?? [];

  const toggleGrant = (g: string, on: boolean) => {
    const cur = grants.filter((x) => x !== g && !(g.includes(":") && x === g.split(":")[0]));
    set(["grants"], on ? [...cur, g] : cur);
  };

  return (
    <div className="side-pad">
      <div className="insp-head">
        <TypeIcon type={node.type} />
        <div className="grow">
          <h2>{node.id}</h2>
          <div className="small muted">
            {meta?.label ?? node.type}
            {node.preset ? ` · preset/${node.preset}` : ""} — {meta?.blurb}
          </div>
        </div>
        <button className="btn ghost icon sm" title="Close" onClick={() => onSelect(null)}>
          <X />
        </button>
      </div>

      <IssueList issues={myIssues} />

      <div className="field mt">
        <label>Description</label>
        <CommitInput value={str("description")} placeholder={node.description || "Why this step exists"} disabled={readOnly} onCommit={(v) => set(["description"], v)} />
      </div>

      {isModel && (
        <div className="field">
          <label>Model</label>
          <select className="input" value={str("model") || (raw.llm as { model?: string })?.model || ""} disabled={readOnly} onChange={(e) => set(["model"], e.target.value)}>
            <option value="">{node.model ? `${node.model} (default)` : "— pick a model —"}</option>
            {models.map((m) => (
              <option key={m.name} value={m.name}>
                {m.name} — {m.size ?? ""} {m.cost ? `· ${m.cost}` : ""}
              </option>
            ))}
          </select>
          {node.fallbacks?.length ? <span className="hint">Falls back to {node.fallbacks.join(", ")} (on_limit)</span> : null}
        </div>
      )}

      {(isModel || node.type === "gate") && (
        <div className="field">
          <label>{node.type === "gate" ? "Question for the human" : "Prompt"}</label>
          <CommitInput multiline rows={node.type === "gate" ? 3 : 7} mono value={str("prompt")} placeholder={node.prompt || ""} disabled={readOnly} onCommit={(v) => set(["prompt"], v)} />
          {!str("prompt") && node.prompt && <span className="hint">Using the preset's prompt. Type to override.</span>}
        </div>
      )}

      {node.type === "check" && (
        <div className="field">
          <label>Command</label>
          <CommitInput mono value={str("run")} placeholder={node.run || "make test"} disabled={readOnly} onCommit={(v) => set(["run"], v)} />
          <span className="hint">Exit 0 → pass, anything else → fail.</span>
        </div>
      )}

      {node.type === "action" && (
        <>
          <div className="field">
            <label>Action</label>
            <select className="input" value={str("action") || node.action} disabled={readOnly} onChange={(e) => set(["action"], e.target.value)}>
              {(overview?.actions ?? ["open_pull_request", "comment_task"]).map((a) => (
                <option key={a}>{a}</option>
              ))}
            </select>
          </div>
          <div className="field">
            <label>{node.action === "comment_task" ? "Comment" : "PR title"}</label>
            <CommitInput
              value={String(((raw.with ?? {}) as Record<string, unknown>)[node.action === "comment_task" ? "body" : "title"] ?? "")}
              placeholder="${{ task.title }}"
              disabled={readOnly}
              onCommit={(v) => set(["with", node.action === "comment_task" ? "body" : "title"], v)}
            />
          </div>
        </>
      )}

      {node.type === "switch" && (
        <>
          <div className="section-title">Cases</div>
          {(node.cases ?? []).map((c, i) => (
            <div key={i} className="row mb" style={{ gap: 6 }}>
              <code className="chip mono grow" title={c.when} style={{ height: "auto", padding: "4px 7px", whiteSpace: "normal" }}>
                {c.when}
              </code>
              <ArrowRight size={14} className="faint" />
              <span className="chip mono">{c.outcome}</span>
            </div>
          ))}
          <div className="small muted">
            Otherwise → <span className="mono">{node.default}</span>. Edit cases in the YAML tab.
          </div>
        </>
      )}

      {node.type === "gate" && (
        <div className="field">
          <label>Timeout</label>
          <CommitInput mono value={String(raw.timeout ?? "")} placeholder="none (wait forever)" disabled={readOnly} onCommit={(v) => set(["timeout"], v)} />
          <span className="hint">e.g. 48h. Adds a <span className="mono">timeout</span> outcome.</span>
        </div>
      )}

      {isPod && (
        <>
          <div className="section-title">Access</div>
          {repoGrants.map((g) => {
            const write = grants.includes(`${g.name}:write`);
            return (
              <label key={g.name} className="row small mb" style={{ cursor: "pointer" }}>
                <input type="checkbox" checked={write} disabled={readOnly} onChange={(e) => toggleGrant(`${g.name}:write`, e.target.checked)} />
                <span>
                  Commit changes to <span className="mono">{g.name}</span>
                  <span className="faint"> (every pod node can read the repo)</span>
                </span>
              </label>
            );
          })}
          {otherGrants.map((g) => (
            <label key={g.name} className="row small mb" style={{ cursor: "pointer" }}>
              <input type="checkbox" checked={grants.includes(g.name)} disabled={readOnly} onChange={(e) => toggleGrant(g.name, e.target.checked)} />
              <span>
                <span className="mono">{g.name}</span> <span className="faint">({g.kind})</span>
                {g.description ? <span className="faint"> — {g.description}</span> : null}
              </span>
            </label>
          ))}
          {node.type === "agent" && overview?.skills.length ? (
            <>
              <div className="label mt mb">Skills</div>
              {overview.skills.map((s) => {
                const skills = (Array.isArray(raw.skills) ? (raw.skills as string[]) : node.skills) ?? [];
                return (
                  <label key={s.name} className="row small mb" style={{ cursor: "pointer" }}>
                    <input
                      type="checkbox"
                      checked={skills.includes(s.name)}
                      disabled={readOnly}
                      onChange={(e) => set(["skills"], e.target.checked ? [...skills, s.name] : skills.filter((x) => x !== s.name))}
                    />
                    <span>
                      <span className="mono">{s.name}</span>
                      {s.description ? <span className="faint"> — {s.description}</span> : null}
                    </span>
                  </label>
                );
              })}
            </>
          ) : null}
        </>
      )}

      <div className="section-title">Outcomes → next</div>
      {node.outcomes.map((o) => {
        const derived = rawOutcomes === undefined || !rawOutcomes.includes(o);
        return (
          <div key={o} className="transition-row">
            <span className="chip mono" title={derived ? "from preset or node type" : undefined}>
              {o}
            </span>
            <ArrowRight size={14} className="arrow" />
            <select
              className="input"
              value={next[o] ?? effectiveNext[o] ?? ""}
              disabled={readOnly}
              onChange={(e) => apply(() => (e.target.value ? setTransition(yaml, node.id, o, e.target.value) : removeTransition(yaml, node.id, o)))}
            >
              <option value="">— not routed —</option>
              {targets.map((t) => (
                <option key={t} value={t}>
                  {t === node.id ? `${t} (itself)` : t}
                </option>
              ))}
            </select>
            {!derived && !readOnly ? (
              <button
                className="btn ghost icon sm"
                title={`Remove outcome ${o}`}
                onClick={() => apply(() => removeTransition(setNodeField(yaml, node.id, ["outcomes"], rawOutcomes!.filter((x) => x !== o)), node.id, o))}
              >
                <X />
              </button>
            ) : (
              <span />
            )}
          </div>
        );
      })}
      {!readOnly && node.type !== "check" && node.type !== "switch" && (
        <AddOutcome
          onAdd={(o) => {
            const base = rawOutcomes ?? node.outcomes.filter((x) => x !== "limit" && x !== "timeout");
            if (base.includes(o)) return;
            set(["outcomes"], [...base, o]);
          }}
        />
      )}

      <div className="section-title">Loops</div>
      <div className="row">
        <div className="field grow" style={{ marginBottom: 0 }}>
          <label>Max visits</label>
          <CommitInput
            value={raw.max_visits ? String(raw.max_visits) : ""}
            placeholder={node.max_visits ? String(node.max_visits) : "unlimited"}
            disabled={readOnly}
            onCommit={(v) => set(["max_visits"], v ? Number(v) || undefined : undefined)}
          />
        </div>
        <div className="field grow" style={{ marginBottom: 0 }}>
          <label>Then go to</label>
          <select className="input" value={str("on_exhausted") || node.on_exhausted || ""} disabled={readOnly || !node.max_visits} onChange={(e) => set(["on_exhausted"], e.target.value)}>
            <option value="">$fail (default)</option>
            {targets.filter((t) => t !== node.id).map((t) => (
              <option key={t}>{t}</option>
            ))}
          </select>
        </div>
      </div>
      <div className="hint small faint mt">Every loop needs max visits on one of its nodes.</div>

      {isPod && (
        <>
          <div className="section-title">Effective settings</div>
          <dl className="kv">
            <dt>Runtime</dt>
            <dd className="mono">{node.runtime}</dd>
            {node.harness && (
              <>
                <dt>Harness</dt>
                <dd className="mono">{node.harness}</dd>
              </>
            )}
            <dt>Timeout</dt>
            <dd>{node.timeout}</dd>
            {node.limits && (
              <>
                <dt>Limits</dt>
                <dd>
                  {[node.limits.tokens && `${node.limits.tokens.toLocaleString()} tokens`, node.limits.usd && `$${node.limits.usd}`, node.limits.turns && `${node.limits.turns} turns`]
                    .filter(Boolean)
                    .join(", ")}
                </dd>
              </>
            )}
            {node.on_limit &&
              Object.entries(node.on_limit).map(([k, v]) => (
                <span key={k} style={{ display: "contents" }}>
                  <dt className="mono small">{k}</dt>
                  <dd className="mono small">{v}</dd>
                </span>
              ))}
            {node.outputs && (
              <>
                <dt>Outputs</dt>
                <dd className="mono small">{Object.keys(node.outputs).join(", ")}</dd>
              </>
            )}
          </dl>
        </>
      )}

      {!readOnly && (
        <>
          <div className="section-title">Node</div>
          <RenameNode id={node.id} onRename={(to) => apply(() => renameNode(yaml, node.id, to))} onDone={(to) => onSelect(to)} />
          <div className="row mt">
            {graph.start !== node.id && (
              <button className="btn sm" onClick={() => apply(() => setStart(yaml, node.id))}>
                <Flag />
                Make start
              </button>
            )}
            <span className="spacer" />
            <button
              className="btn sm danger"
              onClick={() => {
                apply(() => deleteNode(yaml, node.id));
                onSelect(null);
              }}
            >
              <Trash2 />
              Delete node
            </button>
          </div>
        </>
      )}
    </div>
  );
}

function AddOutcome({ onAdd }: { onAdd: (o: string) => void }) {
  const [v, setV] = useState("");
  const ok = /^[a-z][a-z0-9_]{0,40}$/.test(v);
  return (
    <div className="row mt" style={{ gap: 6 }}>
      <input
        className="input mono"
        value={v}
        placeholder="new outcome"
        onChange={(e) => setV(e.target.value.toLowerCase().replace(/[^a-z0-9_]/g, "_"))}
        onKeyDown={(e) => e.key === "Enter" && ok && (onAdd(v), setV(""))}
      />
      <button className="btn sm" disabled={!ok} onClick={() => (onAdd(v), setV(""))}>
        <Plus />
        Add
      </button>
    </div>
  );
}

function RenameNode({ id, onRename, onDone }: { id: string; onRename: (to: string) => void; onDone: (to: string) => void }) {
  const [v, setV] = useState(id);
  useEffect(() => setV(id), [id]);
  const ok = /^[a-z][a-z0-9_]{0,40}$/.test(v) && v !== id;
  return (
    <div className="row" style={{ gap: 6 }}>
      <input className="input mono" value={v} onChange={(e) => setV(e.target.value.toLowerCase().replace(/[^a-z0-9_]/g, "_"))} />
      <button
        className="btn sm"
        disabled={!ok}
        onClick={() => {
          onRename(v);
          onDone(v);
        }}
      >
        Rename
      </button>
    </div>
  );
}
