import { memo, useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  Background,
  BackgroundVariant,
  BaseEdge,
  Controls,
  EdgeLabelRenderer,
  Handle,
  MarkerType,
  Position,
  ReactFlow,
  ReactFlowProvider,
  useReactFlow,
  type Connection,
  type Edge,
  type EdgeProps,
  type FinalConnectionState,
  type Node,
  type NodeChange,
  type NodeProps,
} from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import { AlertTriangle, CheckCircle2, Flag, KeyRound, Puzzle, Repeat, XCircle } from "lucide-react";
import type { Graph, GraphNode, Issue } from "../api";
import { typeMeta } from "../ui";
import { edgeId, layoutGraph, outcomeX, roundedPath, terminalId, type Layout, type Point } from "./layout";

export interface NodeRunState {
  status: string; // latest visit status
  visits: number;
  outcome?: string;
  progress?: string;
  taken: string[]; // outcomes taken at least once
}

export interface RunOverlay {
  nodes: Record<string, NodeRunState>;
  takenEdges: Set<string>; // edgeId(from, outcome)
  lastEdge?: string;
  terminal?: "$success" | "$fail";
}

interface Props {
  graph: Graph;
  issues?: Issue[];
  selected?: string | null;
  onSelect?: (id: string | null) => void;
  run?: RunOverlay;
  editable?: boolean;
  onConnect?: (from: string, outcome: string, to: string) => void;
}

type FlowNodeData = {
  node: GraphNode;
  start: boolean;
  errors: number;
  warnings: number;
  selected: boolean;
  run?: NodeRunState;
  runMode: boolean;
  editable: boolean;
  unrouted: string[];
};

type TerminalData = { kind: "$success" | "$fail"; reached: boolean };

const FlowNode = memo(function FlowNode({ data }: NodeProps<Node<FlowNodeData>>) {
  const { node, run, runMode } = data;
  const meta = typeMeta[node.type] ?? { color: "var(--muted)", icon: Puzzle, label: node.type || "?" };
  const Icon = meta.icon;
  const outcomes = node.outcomes.length ? node.outcomes : [];
  const cls = ["fnode"];
  if (data.selected) cls.push("selected");
  if (data.errors) cls.push("has-error");
  if (runMode) cls.push(run ? `st-${run.status}` : "st-unvisited");

  const subtitle = [node.preset && `preset/${node.preset}`, node.model].filter(Boolean).join(" · ");
  return (
    <div className={cls.join(" ")} style={{ ["--type-color" as string]: meta.color }}>
      <Handle type="target" position={Position.Top} id="in" isConnectable={data.editable} />
      <div className="fnode-badges">
        {data.start && (
          <span className="fbadge start" title="Start node">
            <Flag />
            start
          </span>
        )}
        {run && run.visits > 1 && (
          <span className="fbadge" title={`Ran ${run.visits} times`}>
            <Repeat />×{run.visits}
          </span>
        )}
        {!run && node.max_visits ? (
          <span className="fbadge" title={`At most ${node.max_visits} visits, then ${node.on_exhausted}`}>
            <Repeat />≤{node.max_visits}
          </span>
        ) : null}
        {data.errors > 0 && (
          <span className="fbadge err" title={`${data.errors} validation error(s)`}>
            <AlertTriangle />
            {data.errors}
          </span>
        )}
      </div>
      <div className="fnode-head">
        <span className="fnode-icon">
          <Icon />
        </span>
        <div style={{ minWidth: 0 }}>
          <div className="fnode-title">{node.id}</div>
          <div className="fnode-type">
            {meta.label}
            {subtitle ? <span style={{ textTransform: "none", letterSpacing: 0, fontWeight: 500 }}> · {subtitle}</span> : null}
          </div>
        </div>
      </div>
      <div className="fnode-body">
        {node.description && <div className="fnode-desc">{node.description}</div>}
        <NodeChips node={node} />
        {runMode && run && (
          <div className="fnode-status">
            {run.status === "running" || run.status === "pending" ? (
              <span className="progress">{run.progress || "starting…"}</span>
            ) : run.status === "waiting" ? (
              <span style={{ color: "var(--gate)", fontWeight: 600 }}>Waiting for you</span>
            ) : run.status === "error" ? (
              <span style={{ color: "var(--danger)", fontWeight: 600 }}>Failed</span>
            ) : run.outcome ? (
              <span>
                → <b style={{ color: "var(--success)" }}>{run.outcome}</b>
              </span>
            ) : null}
          </div>
        )}
      </div>
      <div className="fnode-outcomes">
        {outcomes.map((o) => (
          <div
            key={o}
            className={`fnode-outcome${run?.taken.includes(o) ? " taken" : ""}${data.unrouted.includes(o) ? " unrouted" : ""}`}
            title={data.unrouted.includes(o) ? `${o}: no transition` : o}
          >
            {o}
          </div>
        ))}
        {outcomes.length === 0 && <div className="fnode-outcome unrouted">no outcomes</div>}
      </div>
      {outcomes.map((o, i) => (
        <Handle
          key={o}
          type="source"
          position={Position.Bottom}
          id={o}
          isConnectable={data.editable}
          title={data.editable ? `Drag to connect "${o}"` : o}
          style={{ left: outcomeX(i, outcomes.length) }}
        />
      ))}
      {node.max_visits && node.on_exhausted ? (
        <Handle type="source" position={Position.Right} id="exhausted" isConnectable={false} style={{ opacity: 0 }} />
      ) : null}
    </div>
  );
});

function NodeChips({ node }: { node: GraphNode }) {
  const chips: { key: string; el: React.ReactNode }[] = [];
  if (node.type === "check" && node.run) chips.push({ key: "run", el: <span className="chip mono">$ {node.run}</span> });
  if (node.type === "action" && node.action) chips.push({ key: "act", el: <span className="chip mono">{node.action}</span> });
  if (node.type === "switch") chips.push({ key: "sw", el: <span className="chip mono">{(node.cases?.length ?? 0) + " case(s)"}</span> });
  const writes = node.grants?.filter((g) => g.endsWith(":write")).length ?? 0;
  const other = (node.grants?.length ?? 0) - writes;
  if (writes) {
    chips.push({
      key: "w",
      el: (
        <span className="chip" title={node.grants?.join("\n")}>
          <KeyRound />
          writes repo
        </span>
      ),
    });
  }
  if (other > 0) {
    chips.push({
      key: "g",
      el: (
        <span className="chip" title={node.grants?.join("\n")}>
          <KeyRound />
          {other} grant{other > 1 ? "s" : ""}
        </span>
      ),
    });
  }
  if (node.skills?.length) {
    chips.push({ key: "s", el: <span className="chip" title={node.skills.join(", ")}>✦ {node.skills.length} skill{node.skills.length > 1 ? "s" : ""}</span> });
  }
  if (!chips.length) return null;
  return <div className="fnode-chips">{chips.map((c) => <span key={c.key} style={{ display: "contents" }}>{c.el}</span>)}</div>;
}

const TerminalNode = memo(function TerminalNode({ data }: NodeProps<Node<TerminalData>>) {
  const ok = data.kind === "$success";
  return (
    <div className={`terminal ${ok ? "success" : "fail"}${data.reached ? " reached" : ""}`}>
      <Handle type="target" position={Position.Top} id="in" isConnectable />
      {ok ? <CheckCircle2 /> : <XCircle />}
      {data.kind}
    </div>
  );
});

type RouteData = { points?: Point[]; kind: "next" | "exhausted"; taken: boolean; last: boolean; runMode: boolean };

function RouteEdge({ id, data, markerEnd, sourceX, sourceY, targetX, targetY }: EdgeProps<Edge<RouteData>>) {
  const pts = data?.points && data.points.length > 1 ? data.points : [{ x: sourceX, y: sourceY }, { x: sourceX, y: (sourceY + targetY) / 2 }, { x: targetX, y: (sourceY + targetY) / 2 }, { x: targetX, y: targetY }];
  const path = roundedPath(pts);
  let stroke = "var(--border-strong)";
  let width = 1.6;
  let dash: string | undefined;
  if (data?.kind === "exhausted") {
    stroke = "var(--warn)";
    dash = "5 4";
  }
  if (data?.runMode && !data.taken) stroke = "color-mix(in srgb, var(--border-strong) 60%, transparent)";
  if (data?.taken) {
    stroke = "var(--success)";
    width = 2.4;
  }
  // label only for on_exhausted edges; outcomes are labelled on the node itself
  const mid = pts[Math.floor(pts.length / 2)];
  return (
    <>
      <BaseEdge
        id={id}
        path={path}
        markerEnd={markerEnd}
        style={{ stroke, strokeWidth: width, strokeDasharray: data?.last ? "6 5" : dash, animation: data?.last ? "dash 0.9s linear infinite" : undefined }}
      />
      {data?.kind === "exhausted" && (
        <EdgeLabelRenderer>
          <div className="edge-label exhausted" style={{ transform: `translate(-50%, -50%) translate(${mid.x}px, ${mid.y}px)` }}>
            max visits
          </div>
        </EdgeLabelRenderer>
      )}
    </>
  );
}

const nodeTypes = { flow: FlowNode, terminal: TerminalNode };
const edgeTypes = { route: RouteEdge };

function structureKey(g: Graph): string {
  return JSON.stringify([g.start, g.nodes.map((n) => [n.id, n.outcomes, !!n.description, n.grants?.length, n.skills?.length, n.type, n.max_visits]), g.edges]);
}

function Canvas(props: Props) {
  const { graph, issues, selected, onSelect, run, editable, onConnect } = props;
  const [layout, setLayout] = useState<Layout | null>(null);
  const sizes = useRef<Record<string, { width: number; height: number }>>({});
  const [sizeVersion, setSizeVersion] = useState(0);
  const key = useMemo(() => structureKey(graph), [graph]);
  const rf = useReactFlow();
  const fittedCount = useRef(-1);

  // React Flow reports each node's rendered size; lay out with real sizes
  // once every node has one. Sizes are kept per id, so unchanged nodes
  // don't need to re-report after an edit.
  const onNodesChange = useCallback((changes: NodeChange[]) => {
    let changed = false;
    for (const c of changes) {
      if (c.type !== "dimensions" || !c.dimensions) continue;
      const cur = sizes.current[c.id];
      if (!cur || cur.width !== c.dimensions.width || cur.height !== c.dimensions.height) {
        sizes.current[c.id] = { width: c.dimensions.width, height: c.dimensions.height };
        changed = true;
      }
    }
    if (changed) setSizeVersion((v) => v + 1);
  }, []);

  useEffect(() => {
    if (!graph.nodes.every((n) => sizes.current[n.id])) return;
    let cancelled = false;
    const measured = Object.fromEntries(graph.nodes.map((n) => [n.id, sizes.current[n.id]]));
    layoutGraph(graph, measured).then((l) => !cancelled && setLayout(l));
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, sizeVersion]);

  // Keep the whole flow in view until the user pans or zooms themselves.
  const userMoved = useRef(false);
  const wrap = useRef<HTMLDivElement>(null);
  const fit = useCallback(
    (duration = 200) => {
      // wait a beat so React Flow has applied the new positions before measuring bounds
      if (!userMoved.current) window.setTimeout(() => rf.fitView({ padding: 0.1, maxZoom: 1, duration }), 60);
    },
    [rf],
  );
  useEffect(() => {
    if (!layout) return;
    if (fittedCount.current !== graph.nodes.length) {
      userMoved.current = false; // structure changed: show all of it again
      fittedCount.current = graph.nodes.length;
    }
    fit(fittedCount.current === -1 ? 0 : 200);
  }, [layout, graph.nodes.length, fit]);
  useEffect(() => {
    const el = wrap.current;
    if (!el) return;
    const ro = new ResizeObserver(() => fit(0));
    ro.observe(el);
    return () => ro.disconnect();
  }, [fit]);

  const issueCount = useMemo(() => {
    const m: Record<string, { e: number; w: number }> = {};
    for (const i of issues ?? []) {
      if (!i.node) continue;
      m[i.node] ??= { e: 0, w: 0 };
      if (i.severity === "error") m[i.node].e++;
      else m[i.node].w++;
    }
    return m;
  }, [issues]);

  const nodes: Node[] = useMemo(() => {
    const pos = layout?.positions ?? {};
    const out: Node[] = graph.nodes.map((n) => {
      const routed = new Set(graph.edges.filter((e) => e.from === n.id && e.kind === "next").map((e) => e.outcome));
      return {
        id: n.id,
        type: "flow",
        position: pos[n.id] ?? { x: 0, y: 0 },
        draggable: false,
        selectable: false,
        style: { visibility: pos[n.id] ? "visible" : "hidden" },
        data: {
          node: n,
          start: graph.start === n.id,
          errors: issueCount[n.id]?.e ?? 0,
          warnings: issueCount[n.id]?.w ?? 0,
          selected: selected === n.id,
          run: run?.nodes[n.id],
          runMode: !!run,
          editable: !!editable,
          unrouted: n.outcomes.filter((o) => !routed.has(o)),
        } satisfies FlowNodeData,
      };
    });
    const terminals = new Set(graph.edges.filter((e) => e.to.startsWith("$")).map((e) => e.to));
    for (const t of terminals) {
      const id = terminalId(t);
      out.push({
        id,
        type: "terminal",
        position: pos[id] ?? { x: 0, y: 0 },
        draggable: false,
        selectable: false,
        style: { visibility: pos[id] ? "visible" : "hidden" },
        data: { kind: t as TerminalData["kind"], reached: run?.terminal === t } satisfies TerminalData,
      });
    }
    return out;
  }, [graph, layout, issueCount, selected, run, editable]);

  const edges: Edge[] = useMemo(() => {
    return graph.edges
      .filter((e) => e.to.startsWith("$") || graph.nodes.some((n) => n.id === e.to))
      .map((e) => {
        const id = edgeId(e.from, e.kind === "exhausted" ? "exhausted" : e.outcome);
        const taken = !!run?.takenEdges.has(id);
        const color = taken ? "var(--success)" : e.kind === "exhausted" ? "var(--warn)" : "var(--border-strong)";
        return {
          id,
          source: e.from,
          sourceHandle: e.kind === "exhausted" ? "exhausted" : e.outcome,
          target: e.to.startsWith("$") ? terminalId(e.to) : e.to,
          targetHandle: "in",
          type: "route",
          hidden: !layout?.routes[id],
          zIndex: taken ? 1 : 0,
          markerEnd: { type: MarkerType.ArrowClosed, width: 16, height: 16, color },
          data: { points: layout?.routes[id], kind: e.kind, taken, last: run?.lastEdge === id, runMode: !!run } satisfies RouteData,
        };
      });
  }, [graph, layout, run]);

  const connected = useRef(false);
  const route = useCallback(
    (source: string, outcome: string | null | undefined, target: string) => {
      if (!onConnect || !outcome || outcome === "exhausted") return;
      const to = target === "__success" ? "$success" : target === "__fail" ? "$fail" : target;
      onConnect(source, outcome, to);
    },
    [onConnect],
  );
  const handleConnect = useCallback(
    (c: Connection) => {
      connected.current = true;
      if (c.source && c.target) route(c.source, c.sourceHandle, c.target);
    },
    [route],
  );
  // Dropping anywhere on a node (not just its top handle) routes to it.
  const handleConnectEnd = useCallback(
    (e: MouseEvent | TouchEvent, state: FinalConnectionState) => {
      if (connected.current) {
        connected.current = false;
        return;
      }
      const pt = "changedTouches" in e ? e.changedTouches[0] : e;
      const el = document.elementFromPoint(pt.clientX, pt.clientY)?.closest(".react-flow__node");
      const target = el?.getAttribute("data-id");
      if (target && state.fromNode && state.fromHandle) route(state.fromNode.id, state.fromHandle.id, target);
    },
    [route],
  );

  return (
    <div ref={wrap} style={{ width: "100%", height: "100%" }}>
    <ReactFlow
      onMoveStart={(e) => {
        if (e) userMoved.current = true; // only user gestures carry an event
      }}
      nodes={nodes}
      edges={edges}
      nodeTypes={nodeTypes}
      edgeTypes={edgeTypes}
      onNodeClick={(_, n) => n.type === "flow" && onSelect?.(n.id)}
      onPaneClick={() => onSelect?.(null)}
      onNodesChange={onNodesChange}
      onConnect={handleConnect}
      onConnectEnd={handleConnectEnd}
      connectionRadius={36}
      nodesConnectable={!!editable}
      nodesDraggable={false}
      elementsSelectable={false}
      minZoom={0.2}
      maxZoom={1.6}
      proOptions={{ hideAttribution: true }}
      fitView
    >
      <Background variant={BackgroundVariant.Dots} gap={22} size={1.4} />
      <Controls showInteractive={false} position="bottom-right" onFitView={() => (userMoved.current = false)} />
    </ReactFlow>
    </div>
  );
}

export default function FlowGraph(props: Props) {
  return (
    <ReactFlowProvider>
      <Canvas {...props} />
    </ReactFlowProvider>
  );
}
