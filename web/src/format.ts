export function timeAgo(ms: number | undefined, now = Date.now()): string {
  if (!ms) return "";
  const s = Math.max(0, Math.round((now - ms) / 1000));
  if (s < 10) return "just now";
  if (s < 60) return `${s}s ago`;
  const m = Math.round(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.round(m / 60);
  if (h < 24) return `${h}h ago`;
  const d = Math.round(h / 24);
  if (d < 30) return `${d}d ago`;
  return new Date(ms).toLocaleDateString();
}

export function duration(fromMs?: number, toMs?: number, now = Date.now()): string {
  if (!fromMs) return "";
  const s = Math.max(0, Math.round(((toMs || now) - fromMs) / 1000));
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${s % 60}s`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m`;
}

export function tokens(n: number): string {
  if (!n) return "0";
  if (n < 1000) return String(n);
  if (n < 1_000_000) return `${(n / 1000).toFixed(n < 10_000 ? 1 : 0)}k`;
  return `${(n / 1_000_000).toFixed(1)}M`;
}

export function usd(n: number): string {
  if (!n) return "$0";
  return n < 0.01 ? "<$0.01" : `$${n.toFixed(2)}`;
}

export function clock(ms?: number): string {
  if (!ms) return "";
  return new Date(ms).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}

export const taskStatusLabel: Record<string, string> = {
  new: "New",
  planning: "Planning",
  plan_failed: "Needs attention",
  flow_ready: "Flow ready",
  running: "Running",
  succeeded: "Done",
  failed: "Failed",
};
