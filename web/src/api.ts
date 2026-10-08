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
  branches?: string[];
  join?: string;
}

export interface GraphEdge {
  from: string;
  to: string;
  outcome: string;
  kind: "next" | "exhausted" | "branch";
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
  resumes?: number;
  resume_note?: string;
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
  projectless: { allowed_models: string[]; allowed_grants: string[] };
  projects: {
    name: string;
    description?: string;
    repo: string;
    base: string;
    start: string;
    allowed_models: string[];
    allowed_grants: string[];
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
  presets: {
    name: string;
    type: string;
    description?: string;
    outcomes?: string[];
    min_size?: string;
    category?: string;
    when_to_use?: string;
    requires?: string[];
    outputs?: Record<string, unknown>;
    definition?: Record<string, unknown>;
  }[];
  planner: { model: string; guidance?: string };
  actions: string[];
  node_types: string[];
  linear: boolean;
  local: boolean;
  operations: {
    as_of: number;
    active: number;
    waiting: number;
    queued: number;
    oldest_queued_age_ms: number;
    nodes: {
      project: string;
      flow_name: string;
      node: string;
      type: string;
      visits: number;
      duration_samples: number;
      duration_total_ms: number;
      duration_max_ms: number;
      failures: Partial<Record<"error" | "timeout" | "canceled" | "fail_outcome", number>>;
    }[];
  };
}

export interface ChatResult {
  yaml: string;
  explanation: string;
  valid: boolean;
  issues: Issue[];
  graph: Graph;
  attempts: number;
}

// ---- configuration ----

export interface ConfigView {
  sections: Record<string, Record<string, string>>;
  settings: Record<string, string>;
  projects: Record<string, string>;
  seeded_at: number;
  env: {
    git_hosts: { host: string; token_env: string; token_set: boolean }[];
    git_author: string;
    github: { api_url: string; token_env: string; token_set: boolean };
    upstreams: { name: string; base_url: string }[];
    mcp_servers: string[];
    metrics_url?: string;
  };
}

export interface ConfigEdit {
  section: string;
  name: string;
  yaml: string; // empty deletes
}

export interface RepoRow {
  full_name: string;
  description?: string;
  default_branch: string;
  html_url?: string;
  private: boolean;
  archived: boolean;
  pushed_at?: string;
  project?: string;
}

// ---- product workspaces and story maps ----

export interface WorkspaceChange {
  path: string;
  kind: "added" | "modified" | "deleted";
  base?: string;
  work?: string;
}

export interface WorkspaceStatus {
  repo: string;
  branch: string;
  head: string;
  pulled_at: number;
  changes: WorkspaceChange[];
}

export interface MapSummary {
  id: string;
  title: string;
  description?: string;
  tasks: number;
  done: number;
  error?: string;
}

export interface ProductView {
  project: { name: string; description?: string; repo: string; branch: string };
  workspace: WorkspaceStatus;
  docs: string[];
  maps: MapSummary[];
}

export interface Persona {
  id: string;
  name: string;
  description?: string;
}
export interface Activity {
  id: string;
  title: string;
  description?: string;
  persona?: string;
}
export interface Phase {
  id: string;
  title: string;
  description?: string;
  activities: Activity[];
}
export interface Release {
  id: string;
  title: string;
  goal?: string;
  status?: string;
  date?: string;
}
export interface Metric {
  id: string;
  title: string;
  description?: string;
  kind: "product" | "delivery";
  query?: string;
  measure?: string;
  release?: string;
  target?: number;
  direction?: "up" | "down";
  unit?: string;
}
export interface StoryMap {
  title: string;
  description?: string;
  personas: Persona[];
  journey: Phase[];
  releases: Release[];
  metrics: Metric[];
}
export interface UserTask {
  id: string;
  title: string;
  activity: string;
  release?: string;
  order?: number;
  persona?: string;
  story?: string;
  description?: string;
  acceptance?: string[];
  metrics?: string[];
  status?: string;
}
export interface MapWork {
  scope: { map: string; kind: string; id: string; release?: string; tasks: string[] };
  task: Task;
  run?: Run;
}
export interface MapView {
  map: { id: string; map: StoryMap; tasks: UserTask[]; problems: string[] };
  works: MapWork[];
  dirty: string[];
  measures: string[];
  statuses: string[];
}
export interface MetricValue {
  id: string;
  value?: number;
  series?: [number, number][];
  detail?: string;
  error?: string;
}
export interface FileChange {
  path: string;
  content: string | null;
}
export interface MapMessage {
  role: "user" | "assistant";
  content: string;
  changes?: FileChange[];
  issues?: string[];
  at: number;
}

// ---- run retrospectives ----

export interface Retro {
  status: "pending" | "done" | "failed";
  model?: string;
  started_at: number;
  finished_at?: number;
  error?: string;
  note?: string;
  report?: {
    summary: string;
    went_well: string[];
    problems: { node?: string; evidence: string; impact?: string }[];
    suggestions: { kind: string; target?: string; change: string; why: string; priority?: string }[];
  };
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
  deleteFlow: (name: string) => request<{ versions: number; runs: string[] }>("DELETE", `/api/flows/${name}`),
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
  resumeRun: (id: string, node: string, note: string) => request<Run>("POST", `/api/runs/${id}/resume`, { node, note }),
  decide: (id: string, seq: number, outcome: string, note = "") => request<void>("POST", `/api/runs/${id}/gates/${seq}`, { outcome, note }),
  config: () => request<ConfigView>("GET", "/api/config"),
  editConfig: (edits: ConfigEdit[]) => request<{ warnings: string[] }>("POST", "/api/config", { edits }),
  repos: () => request<RepoRow[]>("GET", "/api/repos"),
  linkRepo: (full_name: string, name: string, description: string) =>
    request<{ project: string }>("POST", "/api/repos/link", { full_name, name, description }),
  product: (p: string) => request<ProductView>("GET", `/api/projects/${p}/product`),
  pull: (p: string) => request<{ head: string; updated: string[] }>("POST", `/api/projects/${p}/workspace/pull`, {}),
  commit: (p: string, message: string) => request<{ sha: string; url: string }>("POST", `/api/projects/${p}/workspace/commit`, { message }),
  discard: (p: string, paths: string[] = []) => request<void>("POST", `/api/projects/${p}/workspace/discard`, { paths }),
  readFile: (p: string, path: string) => request<{ path: string; content: string }>("GET", `/api/projects/${p}/files?path=${encodeURIComponent(path)}`),
  writeFile: (p: string, path: string, content: string) => request<void>("PUT", `/api/projects/${p}/files`, { path, content }),
  removeFile: (p: string, path: string) => request<void>("DELETE", `/api/projects/${p}/files?path=${encodeURIComponent(path)}`),
  createMap: (p: string, id: string, title: string, description: string) =>
    request<{ id: string }>("POST", `/api/projects/${p}/maps`, { id, title, description }),
  map: (p: string, m: string) => request<MapView>("GET", `/api/projects/${p}/maps/${m}`),
  saveMap: (p: string, m: string, map: StoryMap) => request<void>("PUT", `/api/projects/${p}/maps/${m}`, { map }),
  deleteMap: (p: string, m: string) => request<void>("DELETE", `/api/projects/${p}/maps/${m}`),
  saveTasks: (p: string, m: string, tasks: UserTask[], del: string[] = []) =>
    request<void>("POST", `/api/projects/${p}/maps/${m}/tasks`, { tasks, delete: del }),
  sendToFlow: (p: string, m: string, kind: string, id: string, release: string, plan = true) =>
    request<Task>("POST", `/api/projects/${p}/maps/${m}/send`, { kind, id, release, plan }),
  mapMetrics: (p: string, m: string) => request<MetricValue[]>("GET", `/api/projects/${p}/maps/${m}/metrics`),
  mapChat: (p: string, m: string) => request<MapMessage[]>("GET", `/api/projects/${p}/maps/${m}/chat`),
  askMap: (p: string, m: string, message: string) => request<MapMessage>("POST", `/api/projects/${p}/maps/${m}/chat`, { message }),
  clearMapChat: (p: string, m: string) => request<void>("DELETE", `/api/projects/${p}/maps/${m}/chat`),
  applyChanges: (p: string, m: string, changes: FileChange[]) =>
    request<{ problems: string[] }>("POST", `/api/projects/${p}/maps/${m}/apply`, { changes }),
  retro: (id: string) => request<{ retro: Retro | null }>("GET", `/api/runs/${id}/retro`),
  startRetro: (id: string, note: string) => request<{ retro: Retro }>("POST", `/api/runs/${id}/retro`, { note }),
  transcript: async (id: string, seq: number): Promise<string> => {
    const res = await fetch(`/api/runs/${id}/visits/${seq}/transcript`);
    if (!res.ok) throw new ApiError(res.status, await res.text());
    return res.text();
  },
};
