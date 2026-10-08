// Story map helpers shared by the map page's parts.
import type { MapWork, StoryMap, UserTask } from "../api";

export const UNSCHEDULED = "";

/** Rows of the map: each release top to bottom, then tasks with no (known) release. */
export function releaseRows(map: StoryMap): { id: string; title: string; goal?: string; status?: string }[] {
  return [...map.releases, { id: UNSCHEDULED, title: "Unscheduled" }];
}

export function rowOf(map: StoryMap, t: UserTask): string {
  return t.release && map.releases.some((r) => r.id === t.release) ? t.release : UNSCHEDULED;
}

export function cellTasks(map: StoryMap, tasks: UserTask[], activity: string, release: string): UserTask[] {
  return tasks.filter((t) => t.activity === activity && rowOf(map, t) === release).sort((a, b) => (a.order ?? 0) - (b.order ?? 0));
}

/** The newest piece of work covering each user task. */
export function latestWork(works: MapWork[]): Map<string, MapWork> {
  const out = new Map<string, MapWork>();
  for (const w of works) for (const id of w.scope.tasks) if (!out.has(id)) out.set(id, w);
  return out;
}

/** What a piece of work is doing, for a badge. */
export function workState(w: MapWork): { status: string; label: string; href: string } {
  const t = w.task;
  const href = w.run ? `/runs/${w.run.id}` : t.flow_name ? `/flows/${t.flow_name}` : `/?task=${t.id}`;
  if (t.planning || t.status === "planning") return { status: "planning", label: "planning", href };
  if (w.run && ["running", "waiting", "queued"].includes(w.run.status)) return { status: w.run.status, label: w.run.status === "waiting" ? "waiting for you" : w.run.status, href };
  if (w.run?.status === "succeeded") return { status: "succeeded", label: "flow succeeded", href };
  if (w.run?.status === "failed" || w.run?.status === "canceled") return { status: "failed", label: `flow ${w.run.status}`, href };
  if (t.status === "flow_ready") return { status: "flow_ready", label: "flow ready", href };
  if (t.status === "plan_failed" || t.status === "plan_interrupted") return { status: "plan_failed", label: "needs attention", href };
  return { status: t.status, label: t.status.replace(/_/g, " "), href };
}

/** Which open user tasks a send would cover (mirrors the server's Select). */
export function scopeTasks(map: StoryMap, tasks: UserTask[], kind: string, id: string, release: string): UserTask[] {
  if (kind === "task") return tasks.filter((t) => t.id === id);
  const acts = new Set(kind === "activity" ? [id] : map.journey.find((p) => p.id === id)?.activities.map((a) => a.id) ?? []);
  const order: UserTask[] = [];
  for (const ph of map.journey)
    for (const a of ph.activities)
      if (acts.has(a.id))
        for (const r of [UNSCHEDULED, ...map.releases.map((r) => r.id)])
          for (const t of cellTasks(map, tasks, a.id, r)) if (t.status !== "done" && (!release || t.release === release)) order.push(t);
  return order;
}

export function uniqueID(base: string, taken: Iterable<string>): string {
  const set = new Set(taken);
  const root = base || "item";
  if (!set.has(root)) return root;
  for (let i = 2; ; i++) if (!set.has(`${root}-${i}`)) return `${root}-${i}`;
}
