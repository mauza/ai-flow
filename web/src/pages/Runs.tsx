import { useNavigate } from "react-router-dom";
import { Play } from "lucide-react";
import { api, type Overview, type Run } from "../api";
import { useNow, useResource } from "../hooks";
import { duration, timeAgo, tokens, usd } from "../format";
import { Empty, PRLink, Pill, Spinner } from "../ui";

export default function Runs() {
  const runs = useResource(() => api.runs(), [], (e) => e.type === "run");
  const overview = useResource(() => api.overview(), [], (e) => e.type === "run" || e.type === "visit");
  return (
    <div className="page">
      <div className="page-head">
        <h1>Runs</h1>
        <span className="sub">Every execution of a flow version.</span>
      </div>
      {overview.error && <div className="warn-box mb">Operations unavailable: {overview.error}</div>}
      {overview.data?.operations && <Operations data={overview.data.operations} />}
      {runs.error && <div className="error-box mb">{runs.error}</div>}
      {!runs.data ? (
        <Spinner lg />
      ) : runs.data.length === 0 ? (
        <div className="card">
          <Empty icon={Play} title="No runs yet">
            <p>Open a flow and press Run.</p>
          </Empty>
        </div>
      ) : (
        <div className="card" style={{ overflowX: "auto" }}>
          <RunsTable runs={runs.data} />
        </div>
      )}
    </div>
  );
}

function Operations({ data }: { data: Overview["operations"] }) {
  const now = useNow(15000);
  const queueAge = data.oldest_queued_age_ms + Math.max(0, now - data.as_of);
  return <section aria-label="Operations" className="card card-pad mb">
    <div className="row wrap">
      <b>{data.active} running</b><span>{data.waiting} waiting for a decision</span><span>{data.queued} queued</span>
      {data.queued > 0 && <span>Oldest queued: {duration(1, queueAge + 1)}</span>}
      <span className="spacer" /><span className="small muted">Updated {timeAgo(data.as_of, now)}</span>
    </div>
    {data.nodes.length > 0 && <details className="mt"><summary>Step durations and failures · all history</summary>
      <div style={{ overflowX: "auto" }}><table className="table"><thead><tr><th>Project / flow / step</th><th>Visits</th><th>Mean</th><th>Max</th><th>Failures</th></tr></thead>
        <tbody>{data.nodes.map((n) => <tr key={JSON.stringify([n.project, n.flow_name, n.node, n.type])}>
          <td><span className="small muted">{n.project} / {n.flow_name}</span><div className="mono">{n.node}</div></td><td>{n.visits}</td>
          <td>{n.duration_samples ? duration(1, 1 + n.duration_total_ms / n.duration_samples) : "—"}</td>
          <td>{n.duration_samples ? duration(1, 1 + n.duration_max_ms) : "—"}</td>
          <td>{Object.entries(n.failures).map(([kind, count]) => `${count} ${kind.replaceAll("_", " ")}`).join(" · ") || "—"}</td>
        </tr>)}</tbody></table></div>
      <p className="small muted">Recorded step failures can be recovered by later steps; they do not necessarily mean the run failed.</p>
    </details>}
  </section>;
}

export function RunsTable({ runs, compact }: { runs: Run[]; compact?: boolean }) {
  const navigate = useNavigate();
  const now = useNow(1000);
  if (!runs.length) return <div className="muted small">No runs yet.</div>;
  return (
    <table className="table">
      <thead>
        <tr>
          <th>Status</th>
          {!compact && <th>Flow</th>}
          <th>Run</th>
          {!compact && <th className="hide-sm">Step</th>}
          <th className="hide-sm">Duration</th>
          {!compact && <th className="hide-sm">Tokens</th>}
          {!compact && <th className="hide-sm">Cost</th>}
          <th>PR</th>
          <th>Started</th>
        </tr>
      </thead>
      <tbody>
        {runs.map((r) => (
          <tr key={r.id} onClick={() => navigate(`/runs/${r.id}`)}>
            <td>
              <Pill status={r.status} />
            </td>
            {!compact && (
              <td className="mono" style={{ maxWidth: 280 }}>
                <div className="ellipsis">{r.flow_name}</div>
                <div className="faint small">v{r.flow_version}</div>
              </td>
            )}
            <td className="mono small">{r.id}</td>
            {!compact && <td className="mono small hide-sm">{r.current_node}</td>}
            <td className="num hide-sm">{duration(r.started_at || r.created_at, r.finished_at, now)}</td>
            {!compact && <td className="num hide-sm">{tokens(r.tokens)}</td>}
            {!compact && <td className="num hide-sm">{usd(r.cost_usd)}</td>}
            <td>
              <PRLink url={r.pr_url} />
            </td>
            <td className="muted small nowrap">{timeAgo(r.created_at, now)}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}
