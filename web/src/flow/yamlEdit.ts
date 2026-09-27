// Edits to a flow's YAML text that keep comments and layout. The text is the
// source of truth; the graph and forms call these and re-validate.
import { Document, isMap, isScalar, parseDocument, Scalar, YAMLMap } from "yaml";

// Same layout the planner writes: {done: test}, [a, b].
const toStringOpts = { lineWidth: 0, indent: 2, flowCollectionPadding: false } as const;

function load(text: string): Document {
  const doc = parseDocument(text, { keepSourceTokens: false });
  if (doc.errors.length) throw new Error(doc.errors[0].message);
  return doc;
}

function scalar(value: unknown): unknown {
  if (typeof value === "string" && value.includes("\n")) {
    const s = new Scalar(value.endsWith("\n") ? value : value);
    s.type = Scalar.BLOCK_LITERAL;
    return s;
  }
  return value;
}

function isEmpty(v: unknown): boolean {
  return v === undefined || v === null || v === "" || (Array.isArray(v) && v.length === 0);
}

const nodePath = (id: string) => ["spec", "nodes", id];

/** Set (or remove, when empty) a field of a node. */
export function setNodeField(text: string, id: string, path: (string | number)[], value: unknown): string {
  const doc = load(text);
  const full = [...nodePath(id), ...path];
  if (isEmpty(value)) {
    if (doc.hasIn(full)) doc.deleteIn(full);
  } else {
    doc.setIn(full, scalar(value));
    // keep short lists and maps on one line, like the planner writes them
    const n = doc.getIn(full, true);
    if (Array.isArray(value) && value.every((x) => typeof x === "string") && n && typeof n === "object" && "flow" in n) {
      (n as { flow: boolean }).flow = value.join(", ").length < 70;
    }
  }
  return doc.toString(toStringOpts);
}

export function setTransition(text: string, from: string, outcome: string, to: string): string {
  const doc = load(text);
  const path = [...nodePath(from), "next"];
  if (!doc.hasIn(path)) {
    const m = doc.createNode({}) as YAMLMap;
    m.flow = true;
    doc.setIn(path, m);
  }
  doc.setIn([...path, outcome], to);
  return doc.toString(toStringOpts);
}

export function removeTransition(text: string, from: string, outcome: string): string {
  const doc = load(text);
  const path = [...nodePath(from), "next", outcome];
  if (doc.hasIn(path)) doc.deleteIn(path);
  return doc.toString(toStringOpts);
}

/** Set (or remove, when empty) a flow-level field such as spec.description. */
export function setFlowField(text: string, path: string[], value: unknown): string {
  const doc = load(text);
  if (isEmpty(value)) {
    if (doc.hasIn(path)) doc.deleteIn(path);
  } else {
    doc.setIn(path, scalar(value));
  }
  return doc.toString(toStringOpts);
}

export function setStart(text: string, id: string): string {
  const doc = load(text);
  doc.setIn(["spec", "start"], id);
  return doc.toString(toStringOpts);
}

/** Node templates for "Add node". */
export function nodeTemplate(type: string, models: string[]): Record<string, unknown> {
  const model = models[0] ?? "";
  switch (type) {
    case "agent":
      return { type, description: "What this step does", model, prompt: "Instructions for this step.", outcomes: ["done", "stuck"], next: {} };
    case "llm":
      return { type, description: "What this step decides", model, prompt: "Instructions for this step.", outputs: { reason: "string" }, outcomes: ["yes", "no"], next: {} };
    case "check":
      return { type, description: "Deterministic check", run: "make test", next: {} };
    case "gate":
      return { type, description: "Why a human decides here", prompt: "Question for the human", outcomes: ["approve", "reject"], next: {} };
    case "switch":
      return { type, cases: [{ when: "run.diff.files_changed > 10", outcome: "big" }], default: "small", next: {} };
    case "action":
      return { type, action: "open_pull_request", with: { title: "${{ task.title }}" }, outcomes: ["done"], next: { done: "$success" } };
  }
  return { type };
}

export function addNode(text: string, id: string, body: Record<string, unknown>): string {
  const doc = load(text);
  if (doc.hasIn(nodePath(id))) throw new Error(`a node named ${id} already exists`);
  const node = doc.createNode(body) as YAMLMap;
  for (const item of node.items) {
    const k = isScalar(item.key) ? String(item.key.value) : "";
    if ((k === "next" || k === "outcomes" || k === "outputs") && item.value && (isMap(item.value) || "items" in (item.value as object))) {
      (item.value as { flow: boolean }).flow = true;
    }
    if (k === "prompt" && isScalar(item.value) && String(item.value.value).includes("\n")) item.value.type = Scalar.BLOCK_LITERAL;
  }
  if (!doc.hasIn(["spec", "nodes"])) doc.setIn(["spec", "nodes"], doc.createNode({}));
  doc.setIn(nodePath(id), node);
  if (!doc.getIn(["spec", "start"])) doc.setIn(["spec", "start"], id);
  return doc.toString(toStringOpts);
}

/** Delete a node and every transition into it (those outcomes become unrouted). */
export function deleteNode(text: string, id: string): string {
  const doc = load(text);
  doc.deleteIn(nodePath(id));
  const nodes = doc.getIn(["spec", "nodes"], true);
  if (isMap(nodes)) {
    for (const item of nodes.items) {
      const nid = isScalar(item.key) ? String(item.key.value) : "";
      const next = doc.getIn([...nodePath(nid), "next"], true);
      if (isMap(next)) {
        for (const t of [...next.items]) {
          if (isScalar(t.value) && t.value.value === id) next.delete(t.key);
        }
      }
      if (doc.getIn([...nodePath(nid), "on_exhausted"]) === id) doc.deleteIn([...nodePath(nid), "on_exhausted"]);
    }
  }
  return doc.toString(toStringOpts);
}

/** Rename a node and every reference to it (transitions, start, templates). */
export function renameNode(text: string, from: string, to: string): string {
  const doc = load(text);
  const nodes = doc.getIn(["spec", "nodes"], true);
  if (!isMap(nodes)) return text;
  if (doc.hasIn(nodePath(to))) throw new Error(`a node named ${to} already exists`);
  const pair = nodes.items.find((i) => isScalar(i.key) && i.key.value === from);
  if (!pair || !isScalar(pair.key)) return text;
  pair.key.value = to;
  for (const item of nodes.items) {
    const nid = isScalar(item.key) ? String(item.key.value) : "";
    const next = doc.getIn([...nodePath(nid), "next"], true);
    if (isMap(next)) for (const t of next.items) if (isScalar(t.value) && t.value.value === from) t.value.value = to;
    if (doc.getIn([...nodePath(nid), "on_exhausted"]) === from) doc.setIn([...nodePath(nid), "on_exhausted"], to);
  }
  if (doc.getIn(["spec", "start"]) === from) doc.setIn(["spec", "start"], to);
  return doc.toString(toStringOpts).replaceAll(`nodes.${from}.`, `nodes.${to}.`);
}

/** Raw (unresolved) fields of one node as written in the YAML. */
export function rawNode(text: string, id: string): Record<string, unknown> | undefined {
  try {
    const doc = load(text);
    const n = doc.getIn(nodePath(id));
    return n && typeof n === "object" ? (JSON.parse(JSON.stringify(doc.getIn(nodePath(id), false) ?? {})) as Record<string, unknown>) : undefined;
  } catch {
    return undefined;
  }
}

export function rawFlow(text: string): Record<string, unknown> | undefined {
  try {
    return load(text).toJS() as Record<string, unknown>;
  } catch {
    return undefined;
  }
}
