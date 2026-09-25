// Typed client for the ai-flow control plane API.

export type Severity = "error" | "warning";

export interface Issue {
  severity: Severity;
  node?: string;
  field?: string;
  message: string;
}

export interface GraphNode {
  id: string;
  type: string;
  preset?: string;
  description?: string;
  model?: string;
  fallbacks?: string[];
  harness?: string;
  runtime?: string;
  grants?: string[];
  skills?: string[];
  prompt?: string;
  run?: string;
  action?: string;
  cases?: { when: string; outcome: string }[];
  default?: string;
  outcomes: string[];
  outputs?: Record<string, unknown>;
  max_visits?: number;
  on_exhausted?: string;
  timeout?: string;
  limits?: { tokens?: number; usd?: number; turns?: number };
  on_limit?: Record<string, string>;
}

export interface GraphEdge {
  from: string;
  to: string;
  outcome: string;
  kind: "next" | "exhausted";
}

export interface Graph {
  start: string;
  nodes: GraphNode[];
  edges: GraphEdge[];
}

export interface Task {
  id: string;
  source: string;
  external_id?: string;
  identifier?: string;
  title: string;
  body: string;
  url?: string;
  project: string;
  status: string;
  flow_name?: string;
  error?: string;
  created_at: number;
  updated_at: number;
  planning?: boolean;
  runs?: Run[];
}

export interface FlowVersion {
  name: string;
  version: number;
  yaml: string;
  project?: string;
  task_id?: string;
  created_by?: string;
  note?: string;
  created_at: number;
}

export interface Run {
  id: string;
  flow_name: string;
  flow_version: number;
  task_id?: string;
  project?: string;
  status: string;
  current_node?: string;
  branch: string;
  base: string;
  pr_url?: string;
  error?: string;
  diff: { files_changed?: number; lines_added?: number; lines_removed?: number };
  cost_usd: number;
  tokens: number;
  created_at: number;
  started_at?: number;
  finished_at?: number;
}

export interface Visit {
  run_id: string;
  seq: number;
  node: string;
  visit: number;
  type: string;
  status: string;
  outcome?: string;
  outputs: Record<string, unknown>;
  summary?: string;
  error?: string;
  prompt?: string;
  job_name?: string;
  model?: string;
  tokens_in: number;
  tokens_out: number;
  cost_usd: number;
  llm_calls: number;
  transcript_key?: string;
  log_tail?: string;
  commit_sha?: string;
  progress?: string;
  decided_by?: string;
  deadline?: number;
  created_at: number;
  started_at?: number;
  finished_at?: number;
}

export interface FlowEvent {
  id: number;
  run_id?: string;
  task_id?: string;
  flow_name?: string;
  node?: string;
  kind: string;
  message: string;
  created_at: number;
}

export interface ChatMessage {
  id: number;
  flow_name: string;
  role: "user" | "assistant" | "system";
  content: string;
  yaml?: string;
  created_at: number;
}

export interface FlowView {
  flow: FlowVersion;
  versions: FlowVersion[];
  graph: Graph;
  issues: Issue[];
  runs: Run[];
  task?: Task;
  planning: boolean;
}

export interface RunView {
  run: Run;
  visits: Visit[];
  events: FlowEvent[];
  graph?: Graph;
  yaml?: string;
  task?: Task;
}

export interface Overview {
  projects: {
    name: string;
    description?: string;
    repo: string;
    base: string;
    start: string;
    guidance?: string;
    linear?: { team: string; label: string; states: string[] };
  }[];
  models: {
    name: string;
    model: string;
    upstream: string;
    size?: string;
    context_tokens?: number;
    tool_use?: string;
    cost?: string;
    notes?: string;
  }[];
  runtimes: { name: string; image: string; description?: string }[];
  grants: { name: string; description?: string; kind: string }[];
  skills: { name: string; description?: string }[];
  presets: { name: string; type: string; description?: string; outcomes?: string[]; min_size?: string }[];
  planner: { model: string; guidance?: string };
  actions: string[];
  node_types: string[];
  linear: boolean;
  local: boolean;
}

export interface ChatResult {
  yaml: string;
  explanation: string;
  valid: boolean;
  issues: Issue[];
  graph: Graph;
  attempts: number;
}

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: body === undefined ? undefined : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (res.status === 204) return undefined as T;
  const text = await res.text();
  let data: unknown = undefined;
  try {
    data = text ? JSON.parse(text) : undefined;
  } catch {
    data = text;
  }
  if (!res.ok) {
    const msg = (data as { error?: string })?.error ?? (typeof data === "string" ? data : res.statusText);
    throw new ApiError(res.status, msg);
  }
  return data as T;
}

export const api = {
  overview: () => request<Overview>("GET", "/api/overview"),
  tasks: () => request<Task[]>("GET", "/api/tasks"),
  task: (id: string) => request<{ task: Task; runs: Run[]; events: FlowEvent[] }>("GET", `/api/tasks/${id}`),
  createTask: (t: { title: string; body: string; project: string; plan: boolean }) => request<Task>("POST", "/api/tasks", t),
  planTask: (id: string) => request<void>("POST", `/api/tasks/${id}/plan`),
  deleteTask: (id: string) => request<void>("DELETE", `/api/tasks/${id}`),
  flows: () => request<FlowVersion[]>("GET", "/api/flows"),
  flow: (name: string, version?: number) =>
    request<FlowView>("GET", version ? `/api/flows/${name}/versions/${version}` : `/api/flows/${name}`),
  saveFlow: (name: string, yaml: string, note: string) =>
    request<{ flow: FlowVersion; issues: Issue[] }>("POST", `/api/flows/${name}`, { yaml, note }),
  validate: (yaml: string) => request<{ graph: Graph; issues: Issue[] }>("POST", "/api/validate", { yaml }),
  chat: (name: string) => request<ChatMessage[]>("GET", `/api/flows/${name}/chat`),
  sendChat: (name: string, message: string, yaml: string) => request<ChatResult>("POST", `/api/flows/${name}/chat`, { message, yaml }),
  startRun: (name: string, version?: number) => request<Run>("POST", `/api/flows/${name}/runs`, { version: version ?? 0 }),
  runs: (q: { flow?: string; task?: string } = {}) => {
    const p = new URLSearchParams();
    if (q.flow) p.set("flow", q.flow);
    if (q.task) p.set("task", q.task);
    return request<Run[]>("GET", `/api/runs?${p}`);
  },
  run: (id: string) => request<RunView>("GET", `/api/runs/${id}`),
  cancelRun: (id: string) => request<void>("POST", `/api/runs/${id}/cancel`),
  decide: (id: string, seq: number, outcome: string) => request<void>("POST", `/api/runs/${id}/gates/${seq}`, { outcome }),
  transcript: async (id: string, seq: number): Promise<string> => {
    const res = await fetch(`/api/runs/${id}/visits/${seq}/transcript`);
    if (!res.ok) throw new ApiError(res.status, await res.text());
    return res.text();
  },
};
