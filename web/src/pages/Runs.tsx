import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { Play, RefreshCw } from "lucide-react";
import { api, type Overview, type QuotaMode, type QuotaSnapshot, type Run } from "../api";
import { useNow, useResource } from "../hooks";
import { duration, timeAgo, tokens, usd } from "../format";
import { Empty, PRLink, Pill, Spinner } from "../ui";

export default function Runs() {
  const runs = useResource(() => api.runs(), [], (e) => e.type === "run");
  const overview = useResource(() => api.overview(), [], (e) => e.type === "run" || e.type === "visit");
  const [refreshingQuota, setRefreshingQuota] = useState(false);
  const refreshQuota = async () => {
    setRefreshingQuota(true);
    try {
      await api.quota(true);
      overview.reload();
    } finally {
      setRefreshingQuota(false);
    }
  };
  return (
    <div className="page">
      <div className="page-head">
        <h1>Runs</h1>
        <span className="sub">Every execution of a flow version.</span>
      </div>
      {overview.error && <div className="warn-box mb">Operations unavailable: {overview.error}</div>}
      {overview.data?.operations && <Operations data={overview.data.operations} />}
      {overview.data?.quota && overview.data.quota.upstreams.length > 0 && (
        <QuotaCard data={overview.data.quota} refreshing={refreshingQuota} onRefresh={refreshQuota} />
      )}
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

function quotaPillClass(mode: QuotaMode): string {
  switch (mode) {
    case "boost":
      return "succeeded";
    case "throttle":
      return "warning";
    case "hold":
      return "failed";
    default:
      return "canceled";
  }
}

function quotaBarColor(mode: QuotaMode): string | undefined {
  switch (mode) {
    case "hold":
      return "var(--danger)";
    case "throttle":
      return "var(--warn)";
    case "boost":
      return "var(--success)";
    default:
      return undefined;
  }
}

function formatReset(sec?: number): string {
  if (!sec || sec <= 0) return "";
  const d = Math.floor(sec / 86400);
  const h = Math.floor((sec % 86400) / 3600);
  const m = Math.max(1, Math.floor((sec % 3600) / 60));
  if (d > 0) return `resets in ${d}d ${h}h`;
  if (h > 0) return `resets in ${h}h ${m}m`;
  return `resets in ${m}m`;
}

function QuotaCard({ data, refreshing, onRefresh }: { data: QuotaSnapshot; refreshing: boolean; onRefresh: () => void }) {
  const now = useNow(30000);
  const speedText =
    data.mode === "hold"
      ? `autonomous schedules held (reserve ≥${Math.round(data.hold_percent)}%)`
      : data.mode === "boost" && data.interval_factor > 0
        ? `pacing ${(1 / data.interval_factor).toFixed(1)}× faster`
        : data.mode === "throttle" && data.interval_factor > 1
          ? `pacing ${data.interval_factor.toFixed(1)}× slower`
          : data.enabled
            ? "pacing normal"
            : "pacing off";
  return (
    <section aria-label="Quota and pacing" className="card card-pad mb stack" style={{ gap: 12 }}>
      <div className="row wrap" style={{ gap: 8 }}>
        <b>Quota &amp; pacing</b>
        <span className={`pill ${quotaPillClass(data.mode)}`}>
          <span className="dot" />
          {data.mode}
        </span>
        <span className="small muted">{data.reason || speedText}</span>
        <span className="spacer" />
        <span className="small muted">Updated {timeAgo(data.updated_at, now)}</span>
        <button className="btn sm" disabled={refreshing} onClick={onRefresh} title="Refresh upstream quota">
          <RefreshCw size={12} />
          Refresh
        </button>
      </div>
      <div className="kpi-grid">
        {data.upstreams.map((u) => (
          <div key={u.name} className="card card-pad stack" style={{ gap: 8 }}>
            <div className="row wrap" style={{ gap: 6 }}>
              <b className="mono">{u.name}</b>
              {u.plan_type && <span className="chip">{u.plan_type}</span>}
              <span className={`pill ${quotaPillClass(u.mode)}`}>{u.mode}</span>
              {u.stale && <span className="chip">stale</span>}
            </div>
            {u.error && <div className="small muted">probe: {u.error}</div>}
            {u.windows.map((w) => (
              <div key={`${w.source}-${w.name}`} className="stack" style={{ gap: 3 }}>
                <div className="row small">
                  <span className="mono">{w.name}</span>
                  <span className="spacer" />
                  <span>{Math.round(w.used_percent)}%</span>
                  {w.reset_after_seconds ? (
                    <span className="muted">· {formatReset(w.reset_after_seconds)}</span>
                  ) : w.tokens_limit ? (
                    <span className="muted">
                      · {tokens(w.tokens_used ?? 0)} / {tokens(w.tokens_limit)}
                    </span>
                  ) : null}
                </div>
                <div className="progress" title={`${w.used_percent}% used (${w.mode})`}>
                  <span style={{ width: `${Math.min(100, Math.max(0, w.used_percent))}%`, background: quotaBarColor(w.mode) }} />
                </div>
              </div>
            ))}
            <div
              className="small faint"
              title={`Uncached: ${tokens(u.local.tokens_uncached_5h)} (5h) · ${tokens(u.local.tokens_uncached_weekly)} (7d)`}
            >
              ai-flow: {tokens(u.local.tokens_5h)} (5h) · {tokens(u.local.tokens_weekly)} (7d)
            </div>
          </div>
        ))}
      </div>
    </section>
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
