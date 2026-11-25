import { test, expect, type Page } from "@playwright/test";
import type { AttentionReport, ImprovementProposal, ImprovementSummary, RunGrade } from "../src/api";

function stubEventSource(page: Page) {
  return page.addInitScript(() => {
    window.EventSource = class {
      onopen?: () => void;
      constructor() {
        setTimeout(() => this.onopen?.(), 0);
      }
      close() {}
    } as unknown as typeof EventSource;
  });
}

async function noHorizontalScroll(page: Page) {
  await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
}

const sampleSummary: ImprovementSummary = {
  config: {
    auto_grade: true,
    model: "gpt-6.2-sol",
    improver_model: "gpt-6.2-sol",
    sample_pct: 100,
    min_failures: 3,
    schedule_interval: "24h0m0s",
    rubrics: [
      { name: "efficiency", description: "Avoidable waste and blind retries.", criteria: "...", version: "a1b2c3d4e5" },
      { name: "instruction_adherence", description: "Follows pinned prompt rules.", criteria: "...", version: "b2c3d4e5f6" },
      { name: "unmentioned_workarounds", description: "Catches quiet skipped checks.", criteria: "...", version: "c3d4e5f607" },
    ],
  },
  stats: {
    finished_runs: 12,
    graded_runs: 10,
    ungraded_runs: 2,
    succeeded_runs: 9,
    failed_runs: 3,
    outcome_rate_pct: 75,
    total_grades: 30,
    passed_grades: 24,
    pass_rate_pct: 80,
    agent_failures: 4,
    environment_failures: 2,
    disagreed_grades: 1,
    pending_proposals: 1,
    applied_proposals: 1,
    rejected_proposals: 0,
    grader_cost_usd: 0.14,
    grader_tokens: 48200,
  },
  rubrics: [
    { name: "efficiency", description: "Avoidable waste and blind retries.", version: "a1b2c3d4e5", total: 10, passed: 9, failed: 1, pass_rate_pct: 90, agent_failures: 1, environment_failures: 0, disagreed: 0, disagreement_rate_pct: 0 },
    { name: "instruction_adherence", description: "Follows pinned prompt rules.", version: "b2c3d4e5f6", total: 10, passed: 7, failed: 3, pass_rate_pct: 70, agent_failures: 3, environment_failures: 0, disagreed: 1, disagreement_rate_pct: 10 },
    { name: "unmentioned_workarounds", description: "Catches quiet skipped checks.", version: "c3d4e5f607", total: 10, passed: 8, failed: 2, pass_rate_pct: 80, agent_failures: 0, environment_failures: 2, disagreed: 0, disagreement_rate_pct: 0 },
  ],
  trend: [
    { date: "2026-11-20", runs: 5, succeeded: 3, outcome_rate_pct: 60, grades: 15, passed: 11, pass_rate_pct: 73.3 },
    {
      date: "2026-11-21",
      runs: 4,
      succeeded: 3,
      outcome_rate_pct: 75,
      grades: 12,
      passed: 10,
      pass_rate_pct: 83.3,
      revisions: [{ id: 1, target_kind: "preset", target: "code-review", version_hash: "2222222222", prev_hash: "1111111111", author: "casey", note: "Applied proposal", created_at: Date.now() - 86_400_000 }],
    },
    { date: "2026-11-22", runs: 3, succeeded: 3, outcome_rate_pct: 100, grades: 9, passed: 9, pass_rate_pct: 100 },
  ],
  target_versions: [
    {
      target_kind: "preset",
      target: "code-review",
      current_hash: "2222222222",
      versions: [
        { version_hash: "1111111111", runs: 6, succeeded: 4, outcome_rate_pct: 66.7, grades: 18, passed: 13, pass_rate_pct: 72.2, first_seen: Date.now() - 200_000, last_seen: Date.now() - 100_000 },
        { version_hash: "2222222222", runs: 4, succeeded: 4, outcome_rate_pct: 100, grades: 12, passed: 11, pass_rate_pct: 91.7, first_seen: Date.now() - 90_000, last_seen: Date.now(), author: "casey", note: "Require file:line citations", current: true },
      ],
    },
  ],
  agent_clusters: [
    {
      blame: "agent",
      target_kind: "preset",
      target: "code-review",
      theme: "missing-line-citations",
      themes: ["missing-line-citations", "no-file-paths"],
      editable: true,
      eligible: true,
      count: 3,
      min_required: 3,
      rubrics: ["instruction_adherence"],
      run_ids: ["r-1", "r-2", "r-3"],
      grade_ids: ["g-1", "g-2", "g-3"],
      sample_reason: "Review findings omitted file paths and line numbers.",
      latest_at: Date.now() - 3600_000,
    },
  ],
  environment_clusters: [
    {
      blame: "environment",
      target_kind: "infra",
      target: "kubernetes",
      theme: "pod-evicted",
      themes: ["pod-evicted"],
      editable: false,
      eligible: false,
      count: 2,
      min_required: 3,
      rubrics: ["unmentioned_workarounds"],
      run_ids: ["r-4", "r-5"],
      grade_ids: ["g-4", "g-5"],
      sample_reason: "Pod was evicted by descheduler mid-step.",
      latest_at: Date.now() - 7200_000,
    },
  ],
  revisions: [],
};

const sampleProposals: ImprovementProposal[] = [
  {
    id: "imp-1",
    status: "pending",
    target_kind: "preset",
    target: "code-review",
    theme: "missing-line-citations",
    title: "Require concrete file:line citations in code-review",
    reason: "Runs r-1, r-2, r-3 failed instruction_adherence because findings omitted exact locations.",
    before_text: "Review the diff carefully.\nReport any defects.",
    after_text: "Review the diff carefully.\nReport any defects with exact file:line citations.",
    before_hash: "1111111111",
    after_hash: "2222222222",
    evidence_runs: ["r-1", "r-2", "r-3"],
    evidence_grades: ["g-1", "g-2", "g-3"],
    created_at: Date.now() - 1800_000,
  },
];

const sampleGrades: RunGrade[] = [
  {
    id: "g-1",
    run_id: "r-1",
    project: "web-games",
    flow_name: "daily-board",
    rubric: "instruction_adherence",
    rubric_version: "b2c3d4e5f6",
    pass: false,
    blame: "agent",
    theme: "missing-line-citations",
    target_kind: "preset",
    target: "code-review",
    reason: "Review findings omitted file paths and line numbers.",
    created_at: Date.now() - 3600_000,
  },
];

test("improve page shows trends, applies proposal with diff, separates infra failures, and supports grade disagreement", async ({ page }, info) => {
  await stubEventSource(page);
  const posts: { path: string; body: unknown }[] = [];
  const attention: AttentionReport = {
    counts: { total: 1, gates: 0, failed_runs: 0, proposals: 1, plan_issues: 0, shipped_unmarked: 0, open_questions: 0 },
    gates: [],
    failed_runs: [],
    proposals: sampleProposals,
    plan_issues: [],
    shipped_unmarked: [],
    open_questions: [],
  };

  await page.route("**/api/**", async (route) => {
    const req = route.request();
    const path = new URL(req.url()).pathname;
    if (req.method() === "POST") {
      posts.push({ path, body: req.postDataJSON() });
      if (path.endsWith("/apply")) return route.fulfill({ json: { proposal: { ...sampleProposals[0], status: "applied" }, warnings: [] } });
      if (path.endsWith("/disagree")) return route.fulfill({ json: { ...sampleGrades[0], disagreed: true } });
      return route.fulfill({ json: { queued: [] } });
    }
    if (path === "/api/improvement/summary") return route.fulfill({ json: sampleSummary });
    if (path === "/api/improvement/proposals") return route.fulfill({ json: sampleProposals });
    if (path === "/api/improvement/grades") return route.fulfill({ json: sampleGrades });
    if (path === "/api/attention") return route.fulfill({ json: attention });
    if (path === "/api/overview")
      return route.fulfill({
        json: { projects: [{ name: "web-games", repo: "repo/web-games", base: "main", start: "manual", allowed_models: [], allowed_grants: [] }], models: [], presets: [], runtimes: [], grants: [], skills: [], actions: [], node_types: [], planner: { model: "x" }, projectless: { allowed_models: [], allowed_grants: [] }, linear: false, local: true, operations: { as_of: 0, active: 0, waiting: 0, queued: 0, oldest_queued_age_ms: 0, nodes: [] } },
      });
    return route.fulfill({ json: [] });
  });

  await page.goto("/improve");
  await expect(page.getByRole("heading", { name: "Improve" })).toBeVisible();
  await expect(page.locator(".kpi-val", { hasText: "75%" })).toBeVisible();
  await expect(page.locator(".kpi-val", { hasText: "80%" })).toBeVisible();
  await expect(page.getByText("Require file:line citations")).toBeVisible();
  await noHorizontalScroll(page);
  await page.screenshot({ path: info.outputPath("improve-overview.png"), fullPage: true });

  await page.getByRole("tab", { name: /Proposals/ }).click();
  await expect(page.getByText("Require concrete file:line citations in code-review")).toBeVisible();
  await expect(page.getByText("+ Report any defects with exact file:line citations.")).toBeVisible();
  await noHorizontalScroll(page);
  await page.getByLabel("Decision note for imp-1").fill("Looks concise");
  await page.getByRole("button", { name: "Apply", exact: true }).click();
  await expect.poll(() => posts[0]).toEqual({ path: "/api/improvement/proposals/imp-1/apply", body: { note: "Looks concise" } });

  await page.getByRole("tab", { name: /Failure themes/ }).click();
  await expect(page.getByText("missing-line-citations")).toBeVisible();
  await expect(page.getByText("pod-evicted")).toBeVisible();
  await expect(page.getByText("+1 theme")).toBeVisible();
  await noHorizontalScroll(page);

  await page.getByRole("tab", { name: /Graded runs/ }).click();
  await page.getByRole("button", { name: "Confirm", exact: true }).click();
  await expect.poll(() => posts[1]).toEqual({ path: "/api/improvement/grades/g-1/confirm", body: {} });
  await page.getByRole("button", { name: "Disagree" }).click();
  await page.getByLabel("Disagreement note for g-1").fill("Citations were in summary");
  await page.getByRole("button", { name: "Confirm disagree" }).click();
  await expect.poll(() => posts[2]).toEqual({ path: "/api/improvement/grades/g-1/disagree", body: { disagreed: true, note: "Citations were in summary" } });

  // Also verify pending proposal surfaces in the unified Inbox without overflow.
  await page.goto("/inbox");
  await expect(page.getByRole("heading", { name: "Prompt improvement proposals" })).toBeVisible();
  await expect(page.getByText("Require concrete file:line citations in code-review")).toBeVisible();
  await noHorizontalScroll(page);
});
