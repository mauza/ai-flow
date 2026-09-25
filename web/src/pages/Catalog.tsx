import { Box, Cpu, FolderGit2, KeyRound, Puzzle, Sparkles, Wand2 } from "lucide-react";
import { api } from "../api";
import { useResource } from "../hooks";
import { LinearMark, Spinner, TypeIcon, typeMeta } from "../ui";

export default function Catalog() {
  const o = useResource(() => api.overview(), []);
  if (!o.data) return <div className="page">{o.error ? <div className="error-box">{o.error}</div> : <Spinner lg />}</div>;
  const d = o.data;
  return (
    <div className="page">
      <div className="page-head">
        <h1>Catalog</h1>
        <span className="sub">The menu the planner picks from. Configured in YAML (deploy/config).</span>
      </div>

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

      <Section icon={Wand2} title="Presets">
        <div className="grid-cards">
          {d.presets.map((p) => (
            <div key={p.name} className="card cat-card">
              <div className="name">
                <TypeIcon type={p.type} size={24} />
                preset/{p.name}
              </div>
              {p.description && <p>{p.description}</p>}
              <div className="chips">
                {p.outcomes?.map((o) => (
                  <span key={o} className="chip mono">
                    {o}
                  </span>
                ))}
                {p.min_size && <span className="chip">min {p.min_size}</span>}
              </div>
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
