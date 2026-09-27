import ELK, { type ElkExtendedEdge, type ElkNode } from "elkjs/lib/elk.bundled.js";
import type { Graph } from "../api";

const elk = new ELK();

export const NODE_WIDTH = 248;
export const TERMINAL_WIDTH = 116;
export const TERMINAL_HEIGHT = 34;

export interface Point {
  x: number;
  y: number;
}

export interface Layout {
  positions: Record<string, Point>;
  routes: Record<string, Point[]>; // edge id → polyline
  width: number;
  height: number;
}

export const edgeId = (from: string, outcome: string) => `${from}::${outcome}`;
export const terminalId = (t: string) => (t === "$success" ? "__success" : "__fail");

/** The x offset of an outcome handle on a node of the given width. */
export function outcomeX(index: number, count: number, width = NODE_WIDTH): number {
  return ((index + 0.5) * width) / Math.max(count, 1);
}

/**
 * Lays out the flow top-down. sizes holds measured node sizes (falls back to
 * an estimate). Ports sit exactly where the node renders its outcome handles.
 */
export async function layoutGraph(graph: Graph, sizes: Record<string, { width: number; height: number }>): Promise<Layout> {
  const terminals = new Set<string>();
  for (const e of graph.edges) if (e.to === "$success" || e.to === "$fail") terminals.add(e.to);

  const children: ElkNode[] = graph.nodes.map((n) => {
    const size = sizes[n.id] ?? { width: NODE_WIDTH, height: estimateHeight(n) };
    const outcomes = n.outcomes.length ? n.outcomes : ["_"];
    const exhausted = n.max_visits && n.on_exhausted;
    const ports = [
      { id: `${n.id}::in`, x: size.width / 2, y: 0, width: 1, height: 1, layoutOptions: { "elk.port.side": "NORTH" } },
      ...outcomes.map((o, i) => ({
        id: `${n.id}::${o}`,
        x: outcomeX(i, outcomes.length, size.width),
        y: size.height,
        width: 1,
        height: 1,
        layoutOptions: { "elk.port.side": "SOUTH" },
      })),
    ];
    if (exhausted) {
      ports.push({ id: `${n.id}::exhausted`, x: size.width, y: size.height / 2, width: 1, height: 1, layoutOptions: { "elk.port.side": "EAST" } });
    }
    return {
      id: n.id,
      width: size.width,
      height: size.height,
      ports,
      layoutOptions: { "elk.portConstraints": "FIXED_POS" },
    };
  });
  for (const t of terminals) {
    const id = terminalId(t);
    children.push({
      id,
      width: TERMINAL_WIDTH,
      height: TERMINAL_HEIGHT,
      ports: [{ id: `${id}::in`, x: TERMINAL_WIDTH / 2, y: 0, width: 1, height: 1, layoutOptions: { "elk.port.side": "NORTH" } }],
      layoutOptions: { "elk.portConstraints": "FIXED_POS" },
    });
  }

  const known = new Set(children.map((c) => c.id));
  const edges: ElkExtendedEdge[] = [];
  for (const e of graph.edges) {
    const target = e.to.startsWith("$") ? terminalId(e.to) : e.to;
    if (!known.has(target) || !known.has(e.from)) continue;
    const source = e.kind === "exhausted" ? `${e.from}::exhausted` : `${e.from}::${e.outcome}`;
    edges.push({ id: edgeId(e.from, e.kind === "exhausted" ? "exhausted" : e.outcome), sources: [source], targets: [`${target}::in`] });
  }

  const root: ElkNode = {
    id: "root",
    layoutOptions: {
      "elk.algorithm": "layered",
      "elk.direction": "DOWN",
      "elk.edgeRouting": "ORTHOGONAL",
      "elk.layered.spacing.nodeNodeBetweenLayers": "64",
      "elk.spacing.nodeNode": "44",
      "elk.spacing.edgeNode": "22",
      "elk.spacing.edgeEdge": "12",
      "elk.layered.spacing.edgeNodeBetweenLayers": "22",
      "elk.layered.considerModelOrder.strategy": "NODES_AND_EDGES",
      "elk.layered.cycleBreaking.strategy": "MODEL_ORDER",
      "elk.layered.nodePlacement.strategy": "NETWORK_SIMPLEX",
      "elk.layered.crossingMinimization.forceNodeModelOrder": "false",
      "elk.padding": "[top=24,left=24,bottom=24,right=24]",
    },
    children,
    edges,
  };
  const out = await elk.layout(root);
  const positions: Record<string, Point> = {};
  for (const c of out.children ?? []) positions[c.id] = { x: c.x ?? 0, y: c.y ?? 0 };
  const routes: Record<string, Point[]> = {};
  for (const e of (out.edges ?? []) as ElkExtendedEdge[]) {
    const s = e.sections?.[0];
    if (!s) continue;
    routes[e.id] = [s.startPoint, ...(s.bendPoints ?? []), s.endPoint];
  }
  return { positions, routes, width: out.width ?? 0, height: out.height ?? 0 };
}

function estimateHeight(n: Graph["nodes"][number]): number {
  let h = 44 + 30 + 28; // head, chips, outcomes
  if (n.description) h += 32;
  return h;
}

/** Rounded orthogonal path through points. */
export function roundedPath(points: Point[], radius = 10): string {
  if (points.length < 2) return "";
  let d = `M ${points[0].x} ${points[0].y}`;
  for (let i = 1; i < points.length - 1; i++) {
    const p0 = points[i - 1];
    const p1 = points[i];
    const p2 = points[i + 1];
    const d1 = Math.hypot(p1.x - p0.x, p1.y - p0.y);
    const d2 = Math.hypot(p2.x - p1.x, p2.y - p1.y);
    const r = Math.min(radius, d1 / 2, d2 / 2);
    const a = { x: p1.x + ((p0.x - p1.x) / d1) * r, y: p1.y + ((p0.y - p1.y) / d1) * r };
    const b = { x: p1.x + ((p2.x - p1.x) / d2) * r, y: p1.y + ((p2.y - p1.y) / d2) * r };
    d += ` L ${a.x} ${a.y} Q ${p1.x} ${p1.y} ${b.x} ${b.y}`;
  }
  const last = points[points.length - 1];
  d += ` L ${last.x} ${last.y}`;
  return d;
}
