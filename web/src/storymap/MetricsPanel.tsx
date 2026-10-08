import { RefreshCw, Target } from "lucide-react";
import { api, type Metric, type StoryMap } from "../api";
import { useResource } from "../hooks";
import { Spinner } from "../ui";

function fmt(v: number, unit?: string): string {
  const n = Math.abs(v) >= 100 ? Math.round(v).toLocaleString() : (Math.round(v * 100) / 100).toString();
  return unit === "%" ? `${n}%` : unit ? `${n} ${unit}` : n;
}

function Sparkline({ points }: { points: [number, number][] }) {
  if (points.length < 2) return null;
  const xs = points.map((p) => p[0]);
  const ys = points.map((p) => p[1]);
  const [x0, x1] = [Math.min(...xs), Math.max(...xs)];
  const [y0, y1] = [Math.min(...ys), Math.max(...ys)];
  const w = 160;
  const h = 36;
  const d = points
    .map((p, i) => `${i ? "L" : "M"}${(((p[0] - x0) / (x1 - x0 || 1)) * w).toFixed(1)},${(h - 2 - ((p[1] - y0) / (y1 - y0 || 1)) * (h - 4)).toFixed(1)}`)
    .join(" ");
  return (
    <svg className="sparkline" viewBox={`0 0 ${w} ${h}`} preserveAspectRatio="none" aria-label="Last 7 days">
      <path d={d} />
    </svg>
  );
}

function onTarget(m: Metric, v: number): boolean | undefined {
  if (m.target === undefined) return undefined;
  return (m.direction ?? "up") === "down" ? v <= m.target : v >= m.target;
}

/** The map's metrics: product metrics from the metrics backend, delivery metrics from ai-flow. */
export default function MetricsPanel({ project, mapID, map }: { project: string; mapID: string; map: StoryMap }) {
  const values = useResource(() => api.mapMetrics(project, mapID), [project, mapID, JSON.stringify(map.metrics)], (e) => e.type === "run");
  if (map.metrics.length === 0)
    return <div className="metrics-empty small muted">No metrics yet. Add product (PromQL) or delivery metrics under <code>metrics:</code> with Edit map.</div>;
  return (
    <div className="metrics">
      {map.metrics.map((m) => {
        const v = values.data?.find((x) => x.id === m.id);
        const hit = v?.value !== undefined ? onTarget(m, v.value) : undefined;
        return (
          <div key={m.id} className="metric card" title={m.description}>
            <div className="row small">
              <span className="grow clamp">{m.title}</span>
              <span className="chip">{m.kind}</span>
            </div>
            <div className={`metric-value${hit === true ? " good" : hit === false ? " bad" : ""}`}>
              {!values.data ? <Spinner /> : v?.value !== undefined ? fmt(v.value, m.unit) : "—"}
            </div>
            {m.target !== undefined && (
              <div className="small faint row">
                <Target size={12} /> target {(m.direction ?? "up") === "down" ? "≤" : "≥"} {fmt(m.target, m.unit)}
              </div>
            )}
            {v?.series && <Sparkline points={v.series} />}
            {v?.error ? <div className="small danger-text clamp">{v.error}</div> : v?.detail && <div className="small faint clamp">{v.detail}</div>}
          </div>
        );
      })}
      <button className="btn ghost icon sm" onClick={values.reload} aria-label="Refresh metrics" title="Refresh">
        <RefreshCw />
      </button>
    </div>
  );
}
