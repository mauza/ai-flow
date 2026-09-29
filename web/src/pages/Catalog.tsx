import { useState } from "react";
import { Box, Cpu, FolderGit2, KeyRound, Puzzle, Sparkles, Wand2 } from "lucide-react";
import { api, type Overview } from "../api";
import { useResource } from "../hooks";
import { LinearMark, Spinner, TypeIcon, typeMeta } from "../ui";

export default function Catalog() {
  const o = useResource(() => api.overview(), []);
  if (!o.data) return <div className="page">{o.error ? <div className="error-box">{o.error}</div> : <Spinner lg />}</div>;
  const d = o.data;
  return (
    <div className="page catalog-page">
      <div className="page-head">
        <h1>Catalog</h1>
        <span className="sub">Choose reusable steps for your workflow, then add them in the flow editor.</span>
      </div>

      <Section icon={Wand2} title="Node library">
        <PresetLibrary presets={d.presets} />
      </Section>

      <details className="library-reference">
        <summary>Configuration reference · node types, models, access and projects</summary>
      <Section icon={Sparkles} title="Node types">
        <div className="grid-cards">
          {d.node_types.map((t) => (
            <div key={t} className="card cat-card">
              <div className="name">
                <TypeIcon type={t} size={24} />
                {t}
              </div>
              <p>{typeMeta[t]?.blurb}</p>
            </div>
          ))}
        </div>
      </Section>

      <Section icon={Cpu} title="Models">
        <div className="grid-cards">
          {d.models.map((m) => (
            <div key={m.name} className="card cat-card">
              <div className="name">{m.name}</div>
              <div className="chips">
                <span className="chip mono">
                  {m.upstream}/{m.model}
                </span>
                {m.size && <span className="chip">{m.size}</span>}
                {m.context_tokens ? <span className="chip">{Math.round(m.context_tokens / 1000)}k ctx</span> : null}
                {m.tool_use && <span className="chip">tools: {m.tool_use}</span>}
                {m.cost && <span className="chip">{m.cost}</span>}
                {d.planner.model === m.name && <span className="chip accent">planner</span>}
              </div>
              {m.notes && <p>{m.notes}</p>}
            </div>
          ))}
        </div>
      </Section>

      <Section icon={KeyRound} title="Grants">
        <div className="grid-cards">
          {d.grants.map((g) => (
            <div key={g.name} className="card cat-card">
              <div className="name">{g.name}</div>
              <div className="chips">
                <span className="chip">{g.kind}</span>
              </div>
              {g.description && <p>{g.description}</p>}
            </div>
          ))}
        </div>
      </Section>

      <div className="row" style={{ alignItems: "flex-start", gap: 12, flexWrap: "wrap" }}>
        <div className="grow" style={{ minWidth: 300 }}>
          <Section icon={Puzzle} title="Skills">
            <div className="stack">
              {d.skills.map((s) => (
                <div key={s.name} className="card cat-card">
                  <div className="name">{s.name}</div>
                  {s.description && <p>{s.description}</p>}
                </div>
              ))}
            </div>
          </Section>
        </div>
        <div className="grow" style={{ minWidth: 300 }}>
          <Section icon={Box} title="Runtimes">
            <div className="stack">
              {d.runtimes.map((r) => (
                <div key={r.name} className="card cat-card">
                  <div className="name">{r.name}</div>
                  <div className="chips">
                    <span className="chip mono">{r.image}</span>
                  </div>
                  {r.description && <p>{r.description}</p>}
                </div>
              ))}
            </div>
          </Section>
        </div>
      </div>

      <Section icon={FolderGit2} title="Projects">
        <div className="stack">
          {d.projects.map((p) => (
            <div key={p.name} className="card cat-card">
              <div className="name">{p.name}</div>
              <div className="chips">
                <span className="chip mono">{p.repo}</span>
                <span className="chip">base {p.base}</span>
                <span className="chip">start: {p.start}</span>
                {p.linear && (
                  <span className="chip">
                    <LinearMark /> {p.linear.team} · {p.linear.label} · {p.linear.states.join("/")}
                  </span>
                )}
              </div>
              {p.description && <p>{p.description}</p>}
              {p.guidance && (
                <>
                  <div className="label mt">Planner guidance</div>
                  <pre className="code" style={{ marginTop: 6 }}>{p.guidance}</pre>
                </>
              )}
            </div>
          ))}
          {d.planner.guidance && (
            <div className="card cat-card">
              <div className="name">Global planner guidance</div>
              <pre className="code" style={{ marginTop: 8 }}>{d.planner.guidance}</pre>
            </div>
          )}
        </div>
      </Section>
      </details>
    </div>
  );
}

type Preset = Overview["presets"][number];

export const presetCategory = (preset: Preset) => preset.category?.trim() || "Other";

export function matchesPreset(preset: Preset, query: string) {
  const text = [preset.name, preset.type, presetCategory(preset), preset.description, preset.when_to_use,
    ...(preset.requires ?? []), ...(preset.outcomes ?? []), ...Object.keys(preset.outputs ?? {})].join(" ").toLowerCase();
  return query.toLowerCase().trim().split(/\s+/).every((word) => text.includes(word));
}

export function PresetDetails({ preset }: { preset: Preset }) {
  const definition = preset.definition;
  const outputs = preset.outputs ?? definition?.outputs;
  return (
    <div className="preset-details">
      {preset.when_to_use && <div><div className="label">Use when</div><p>{preset.when_to_use}</p></div>}
      <div>
        <div className="label">Prerequisites</div>
        {preset.requires?.length ? <ul>{preset.requires.map((requirement) => <li key={requirement}>{requirement}</li>)}</ul>
          : <p className="muted">No prerequisites listed. Check the step's inputs and project access.</p>}
        <p className="hint">Grant access explicitly in the editor; adding a preset does not enable grants.</p>
      </div>
      <div>
        <div className="label">Output schema</div>
        {outputs && typeof outputs === "object" && Object.keys(outputs).length
          ? <pre className="code">{JSON.stringify(outputs, null, 2)}</pre>
          : <p className="muted">No structured outputs declared.</p>}
      </div>
      {([ ["prompt", "Prompt"], ["run", "Command"], ["action", "Action"] ] as const).map(([key, label]) =>
        typeof definition?.[key] === "string" && definition[key] ? <div key={key}>
          <div className="label">{label}</div><pre className="code">{definition[key]}</pre>
        </div> : null)}
      {!definition && <p className="hint">Prompt and command previews are unavailable from this server.</p>}
    </div>
  );
}

function PresetLibrary({ presets }: { presets: Preset[] }) {
  const [query, setQuery] = useState("");
  const [type, setType] = useState("");
  const [category, setCategory] = useState("");
  const categories = [...new Set(presets.map(presetCategory))].sort();
  const types = [...new Set(presets.map((preset) => preset.type))].sort();
  const filtered = presets.filter((preset) => matchesPreset(preset, query) && (!type || preset.type === type) && (!category || presetCategory(preset) === category));
  return (
    <div className="preset-library">
      <p className="muted small">Find a step by purpose, outcome or prerequisite. In a flow, choose <b>Add node → Preset</b> to use it, then wire every outcome.</p>
      <div className="library-filters">
        <label className="field"><span>Search presets</span><input className="input" type="search" value={query} onChange={(e) => setQuery(e.target.value)} placeholder="e.g. review, repo write, fail" /></label>
        <label className="field"><span>Node type</span><select className="input" value={type} onChange={(e) => setType(e.target.value)}>
          <option value="">All types</option>{types.map((value) => <option key={value} value={value}>{typeMeta[value]?.label ?? value}</option>)}
        </select></label>
        <label className="field"><span>Category</span><select className="input" value={category} onChange={(e) => setCategory(e.target.value)}>
          <option value="">All categories</option>{categories.map((value) => <option key={value}>{value}</option>)}
        </select></label>
      </div>
      <div className="library-results" role="status">{filtered.length} of {presets.length} presets</div>
      {!filtered.length && <div className="card card-pad library-empty">
        <p>{presets.length ? "No presets match these filters." : "No presets configured yet. You can still add a blank node in the flow editor."}</p>
        {presets.length > 0 && <button className="btn sm" onClick={() => { setQuery(""); setType(""); setCategory(""); }}>Clear filters</button>}
      </div>}
      {categories.map((group) => {
        const members = filtered.filter((preset) => presetCategory(preset) === group);
        if (!members.length) return null;
        return <section className="library-category" key={group} aria-label={group}>
          <h3>{group} <span className="muted small">{members.length}</span></h3>
          <div className="library-grid">{members.map((preset) => <article className="card cat-card preset-card" key={preset.name}>
            <div className="name"><TypeIcon type={preset.type} size={24} /><h4>preset/{preset.name}</h4></div>
            {preset.description && <p>{preset.description}</p>}
            <div className="chips"><span className="chip">{typeMeta[preset.type]?.label ?? preset.type}</span>{preset.min_size && <span className="chip">min model: {preset.min_size}</span>}</div>
            <div className="label mt">Outcomes to wire</div>
            <div className="chips">{preset.outcomes?.length ? preset.outcomes.map((outcome) => <span className="chip mono" key={outcome}>{outcome}</span>) : <span className="hint">Derived from the node type; check validation after adding.</span>}</div>
            {preset.when_to_use && preset.when_to_use !== preset.description && <p className="preset-cue">{preset.when_to_use}</p>}
            <details><summary>Details · prerequisites, outputs & instructions</summary><PresetDetails preset={{ ...preset, when_to_use: undefined }} /></details>
          </article>)}</div>
        </section>;
      })}
    </div>
  );
}

function Section({ icon: Icon, title, children }: { icon: typeof Cpu; title: string; children: React.ReactNode }) {
  return (
    <section style={{ marginBottom: 26 }}>
      <h2 className="row" style={{ fontSize: 14, margin: "0 0 12px", gap: 8 }}>
        <Icon size={16} className="muted" />
        {title}
      </h2>
      {children}
    </section>
  );
}
