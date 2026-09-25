import { useNavigate } from "react-router-dom";
import { Play } from "lucide-react";
import { api, type Run } from "../api";
import { useNow, useResource } from "../hooks";
import { duration, timeAgo, tokens, usd } from "../format";
import { Empty, PRLink, Pill, Spinner } from "../ui";

export default function Runs() {
  const runs = useResource(() => api.runs(), [], (e) => e.type === "run");
  return (
    <div className="page">
      <div className="page-head">
        <h1>Runs</h1>
        <span className="sub">Every execution of a flow version.</span>
      </div>
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
