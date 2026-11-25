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
  resume_seq?: number;
  resumes?: number;
  resume_note?: string;
  pause_requested?: boolean;
  steer_note?: string;
  steer_seq?: number;
  extra_visits?: number;
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
  tokens_cached?: number;
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
  mcp?: { endpoint: string; allow_steer: boolean };
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
  files_error?: string;
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
  open_questions?: string[];
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
  hypothesis?: string;
  open_questions?: string[];
  value?: string;
  confidence?: string;
  outcome?: string;
  outcome_note?: string;
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

// ---- human attention & project digests ----

export interface GateLoopInfo {
  outcome: string;
  target: string;
  used: number;
  max_visits: number;
}

export interface GatePrevStep {
  node: string;
  outcome?: string;
  summary?: string;
  outputs?: Record<string, unknown>;
}

export interface AttentionGate {
  run_id: string;
  project?: string;
  flow_name: string;
  flow_version: number;
  task_id?: string;
  task_title?: string;
  seq: number;
  node: string;
  is_pause?: boolean;
  next_node?: string;
  prompt: string;
  outcomes: string[];
  deadline?: number;
  started_at: number;
  pr_url?: string;
  branch?: string;
  cost_usd: number;
  diff?: { files_changed?: number; lines_added?: number; lines_removed?: number };
  extra_visits?: number;
  prev?: GatePrevStep;
  loops?: GateLoopInfo[];
}

export interface AttentionFailedRun {
  run_id: string;
  project?: string;
  flow_name: string;
  flow_version: number;
  task_id?: string;
  task_title?: string;
  last_node?: string;
  error?: string;
  cost_usd: number;
  finished_at?: number;
}

export interface AttentionPlanIssue {
  task_id: string;
  project: string;
  title: string;
  status: string;
  flow_name?: string;
  error?: string;
}

export interface AttentionShipped {
  project: string;
  map_id: string;
  map_title: string;
  user_task_id: string;
  title: string;
  release?: string;
  run_id: string;
  pr_url?: string;
  metrics?: string[];
  finished_at?: number;
}

export interface AttentionQuestion {
  project: string;
  map_id: string;
  map_title: string;
  kind: "task" | "release";
  id: string;
  title: string;
  release?: string;
  questions: string[];
}

export interface AttentionProposal {
  id: string;
  target_kind: string;
  target: string;
  theme?: string;
  title: string;
  reason: string;
  before_text: string;
  after_text: string;
  evidence_runs: string[];
  created_at: number;
}

export interface AttentionReport {
  counts: {
    total: number;
    gates: number;
    failed_runs: number;
    proposals?: number;
    plan_issues: number;
    shipped_unmarked: number;
    open_questions: number;
  };
  gates: AttentionGate[];
  failed_runs: AttentionFailedRun[];
  proposals?: AttentionProposal[];
  plan_issues: AttentionPlanIssue[];
  shipped_unmarked: AttentionShipped[];
  open_questions: AttentionQuestion[];
}

export interface NamedMetric {
  id: string;
  title: string;
  kind: string;
  unit?: string;
  target?: number;
  direction?: string;
  value?: number;
  series?: [number, number][];
  detail?: string;
  error?: string;
  on_target?: boolean;
}

export interface MapHealthSummary {
  id: string;
  title: string;
  description?: string;
  tasks_total: number;
  tasks_done: number;
  releases: { id: string; title: string; goal?: string; status?: string; total: number; done: number; open_questions?: string[] }[];
  metrics: NamedMetric[];
  open_questions: string[];
  risky_bets: { id: string; title: string; release?: string; value?: string; confidence?: string; hypothesis?: string; open_questions?: string[] }[];
  outcomes: { id: string; title: string; outcome: string; outcome_note?: string }[];
}

export interface ProjectSummary {
  project: string;
  description?: string;
  repo?: string;
  branch?: string;
  generated_at: number;
  delivery: {
    total_runs: number;
    active_runs: number;
    waiting_runs: number;
    succeeded_runs: number;
    failed_runs: number;
    success_rate_pct?: number;
    total_cost_usd: number;
    total_tokens: number;
    median_cycle_hours?: number;
  };
  attention?: AttentionReport;
  maps: MapHealthSummary[];
  recent_runs: { run_id: string; flow_name: string; task_title?: string; status: string; pr_url?: string; cost_usd: number; created_at: number; finished_at?: number; error?: string }[];
  retros: { run_id: string; flow_name: string; summary: string; suggestions?: { kind: string; target?: string; change: string; why: string; priority?: string }[] }[];
}

export interface Schedule {
  project: string;
  name: string;
  enabled: boolean;
  paused: boolean;
  every: string;
  flow?: string;
  task_title?: string;
  storymap?: { map?: string; release?: string };
  auto_start?: boolean;
  max_active_runs: number;
  max_open_tasks: number;
  max_open_prs: number;
  next_due_at?: number;
  state: { last_run_at?: number; retry_at?: number; last_status?: string; last_reason?: string; last_run_id?: string; last_task_id?: string };
}

// ---- improvement loop ----

export interface RunFacts {
  run_id: string;
  project?: string;
  flow_name: string;
  flow_version: number;
  status: string;
  wall_ms: number;
  cost_usd: number;
  tokens: number;
  visits: number;
  loop_visits: number;
  max_visit: number;
  step_errors: number;
  resumes: number;
  gates: number;
  steers: number;
  extra_visits: number;
  prompt_versions?: Record<string, string>;
  created_at: number;
  finished_at: number;
}

export interface RunGrade {
  id: string;
  run_id: string;
  project?: string;
  flow_name?: string;
  rubric: string;
  rubric_version: string;
  pass: boolean;
  blame?: "" | "agent" | "environment";
  theme?: string;
  target_kind?: string;
  target?: string;
  node?: string;
  reason: string;
  prompt_versions?: Record<string, string>;
  model?: string;
  tokens_in?: number;
  tokens_out?: number;
  cost_usd?: number;
  disagreed?: boolean;
  disagreed_by?: string;
  disagreed_note?: string;
  disagreed_at?: number;
  reviewed_at?: number;
  created_at: number;
}

export interface RunGradesView {
  run_id: string;
  grading: boolean;
  facts?: RunFacts;
  grades: RunGrade[];
}

export interface ImprovementProposal {
  id: string;
  status: "pending" | "applied" | "rejected";
  target_kind: string;
  target: string;
  theme?: string;
  title: string;
  reason: string;
  before_text: string;
  after_text: string;
  before_hash: string;
  after_hash: string;
  evidence_runs: string[];
  evidence_grades: string[];
  model?: string;
  tokens_in?: number;
  tokens_out?: number;
  cost_usd?: number;
  decided_by?: string;
  decided_at?: number;
  decision_note?: string;
  created_at: number;
}

export interface PromptRevision {
  id: number;
  target_kind: string;
  target: string;
  version_hash: string;
  prev_hash?: string;
  author?: string;
  note?: string;
  proposal_id?: string;
  created_at: number;
}

export interface FailureCluster {
  blame: "agent" | "environment";
  target_kind: string;
  target: string;
  theme: string;
  themes: string[];
  editable: boolean;
  eligible: boolean;
  count: number;
  min_required: number;
  rubrics: string[];
  run_ids: string[];
  grade_ids: string[];
  sample_reason: string;
  latest_at: number;
}

export interface RubricStats {
  name: string;
  description?: string;
  version: string;
  total: number;
  passed: number;
  failed: number;
  pass_rate_pct: number;
  agent_failures: number;
  environment_failures: number;
  disagreed: number;
  disagreement_rate_pct: number;
  audited: number;
  confirmed: number;
  cohens_kappa?: number;
  calibrated: boolean;
}

export interface TrendPoint {
  date: string;
  runs: number;
  succeeded: number;
  outcome_rate_pct: number;
  grades: number;
  passed: number;
  pass_rate_pct: number;
  revisions?: PromptRevision[];
}

export interface TargetVersionScore {
  version_hash: string;
  runs: number;
  succeeded: number;
  outcome_rate_pct: number;
  grades: number;
  passed: number;
  pass_rate_pct: number;
  first_seen: number;
  last_seen: number;
  author?: string;
  note?: string;
  proposal_id?: string;
  current?: boolean;
}

export interface TargetScoreHistory {
  target_kind: string;
  target: string;
  current_hash?: string;
  versions: TargetVersionScore[];
}

export interface ImproverRunStatus {
  started_at: number;
  finished_at: number;
  trigger: string;
  model?: string;
  clusters_checked: number;
  proposals_created: number;
  skipped_clusters: number;
  proposal_ids?: string[];
  error?: string;
}

export interface ImprovementSummary {
  project?: string;
  config: {
    auto_grade: boolean;
    model?: string;
    improver_model?: string;
    sample_pct: number;
    min_failures: number;
    schedule_interval?: string;
    rubrics: { name: string; description: string; criteria: string; version: string }[];
  };
  stats: {
    finished_runs: number;
    graded_runs: number;
    ungraded_runs: number;
    succeeded_runs: number;
    failed_runs: number;
    outcome_rate_pct: number;
    total_grades: number;
    passed_grades: number;
    pass_rate_pct: number;
    agent_failures: number;
    environment_failures: number;
    disagreed_grades: number;
    pending_proposals: number;
    applied_proposals: number;
    rejected_proposals: number;
    grader_cost_usd: number;
    grader_tokens: number;
  };
  last_improver_run?: ImproverRunStatus;
  rubrics: RubricStats[];
  trend: TrendPoint[];
  target_versions: TargetScoreHistory[];
  agent_clusters: FailureCluster[];
  environment_clusters: FailureCluster[];
  revisions: PromptRevision[];
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
  attention: (project = "") => request<AttentionReport>("GET", project ? `/api/attention?project=${encodeURIComponent(project)}` : "/api/attention"),
  schedules: (project: string) => request<Schedule[]>("GET", `/api/projects/${encodeURIComponent(project)}/schedules`),
  runSchedule: (project: string, name: string) =>
    request<Schedule["state"]>("POST", `/api/projects/${encodeURIComponent(project)}/schedules/${encodeURIComponent(name)}/run`, {}),
  pauseSchedule: (project: string, name: string, paused: boolean) =>
    request<Schedule[]>("POST", `/api/projects/${encodeURIComponent(project)}/schedules/${encodeURIComponent(name)}/pause`, { paused }),
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
  resumeRun: (id: string, node: string, note: string, flow_version = 0, refresh = false) =>
    request<Run>("POST", `/api/runs/${id}/resume`, {
      node,
      note,
      ...(flow_version ? { flow_version } : {}),
      ...(refresh ? { refresh } : {}),
    }),
  decide: (id: string, seq: number, outcome: string, note = "", extendVisits = false) =>
    request<void>("POST", `/api/runs/${id}/gates/${seq}`, extendVisits ? { outcome, note, extend_visits: true } : { outcome, note }),
  steerRun: (id: string, body: { pause?: boolean; note?: string }) => request<Run>("POST", `/api/runs/${id}/steer`, body),
  projectSummary: (p: string) => request<ProjectSummary>("GET", `/api/projects/${p}/summary`),
  projectDigest: async (p: string, format: "md" | "html" = "md"): Promise<string> => {
    const res = await fetch(`/api/projects/${p}/digest?format=${format}`);
    if (!res.ok) throw new ApiError(res.status, await res.text());
    return res.text();
  },
  config: () => request<ConfigView>("GET", "/api/config"),
  editConfig: (edits: ConfigEdit[], force = false) => request<{ warnings: string[] }>("POST", "/api/config", { edits, force }),
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
  runGrades: (id: string) => request<RunGradesView>("GET", `/api/runs/${id}/grades`),
  gradeRun: (id: string, sync = false) => request<{ run_id: string; started?: boolean; grading?: boolean; grades?: RunGrade[] }>("POST", `/api/runs/${id}/grade${sync ? "?sync=1" : ""}`),
  improvementSummary: (project = "") =>
    request<ImprovementSummary>("GET", project ? `/api/improvement/summary?project=${encodeURIComponent(project)}` : "/api/improvement/summary"),
  improvementGrades: (q: { run?: string; project?: string; rubric?: string; blame?: string; pass?: string; limit?: number } = {}) => {
    const p = new URLSearchParams();
    if (q.run) p.set("run", q.run);
    if (q.project) p.set("project", q.project);
    if (q.rubric) p.set("rubric", q.rubric);
    if (q.blame) p.set("blame", q.blame);
    if (q.pass) p.set("pass", q.pass);
    if (q.limit) p.set("limit", String(q.limit));
    return request<RunGrade[]>("GET", `/api/improvement/grades?${p}`);
  },
  gradeUngraded: (project = "", limit = 10) => request<{ queued: string[] }>("POST", "/api/improvement/grade-ungraded", { project, limit }),
  confirmGrade: (id: string) => request<RunGrade>("POST", `/api/improvement/grades/${id}/confirm`, {}),
  disagreeGrade: (id: string, disagreed: boolean, note = "") =>
    request<RunGrade>("POST", `/api/improvement/grades/${id}/disagree`, { disagreed, note }),
  improvementProposals: (status = "") =>
    request<ImprovementProposal[]>("GET", status ? `/api/improvement/proposals?status=${encodeURIComponent(status)}` : "/api/improvement/proposals"),
  runImprover: (body: { project?: string; min_failures?: number } = {}) =>
    request<ImproverRunStatus>("POST", "/api/improvement/run", body),
  applyProposal: (id: string, body: { note?: string; after_text?: string; force?: boolean } = {}) =>
    request<{ proposal: ImprovementProposal; warnings: string[] }>("POST", `/api/improvement/proposals/${id}/apply`, body),
  rejectProposal: (id: string, note = "") =>
    request<{ proposal: ImprovementProposal }>("POST", `/api/improvement/proposals/${id}/reject`, { note }),
  transcript: async (id: string, seq: number): Promise<string> => {
    const res = await fetch(`/api/runs/${id}/visits/${seq}/transcript`);
    if (!res.ok) throw new ApiError(res.status, await res.text());
    return res.text();
  },
};
