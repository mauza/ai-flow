import { test, expect, type Page } from "@playwright/test";
import type { AttentionReport, Graph, ProjectSummary, Run, RunView, Visit } from "../src/api";

const graph: Graph = {
  start: "work",
  nodes: [
    { id: "work", type: "agent", model: "gpt-6.1-sol", max_visits: 2, outcomes: ["done"] },
    { id: "value", type: "llm", model: "gpt-6-luna", outcomes: ["delivers_value", "needs_human"] },
    { id: "approve", type: "gate", prompt: "Should we ship this UX?", outcomes: ["approve", "revise", "stop"] },
  ],
  edges: [
    { from: "work", to: "value", outcome: "done", kind: "next" },
    { from: "value", to: "approve", outcome: "needs_human", kind: "next" },
    { from: "value", to: "$success", outcome: "delivers_value", kind: "next" },
    { from: "approve", to: "$success", outcome: "approve", kind: "next" },
    { from: "approve", to: "work", outcome: "revise", kind: "next" },
    { from: "approve", to: "$fail", outcome: "stop", kind: "next" },
  ],
};

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

test("inbox surfaces waiting gates, allows decision with loop extension, and shows open questions", async ({ page }, info) => {
  await stubEventSource(page);
  const posts: { path: string; body: unknown }[] = [];
  const attention: AttentionReport = {
    counts: { total: 3, gates: 1, failed_runs: 0, plan_issues: 0, shipped_unmarked: 1, open_questions: 1 },
    gates: [
      {
        run_id: "r-wait1",
        project: "web-games",
        flow_name: "pause-game",
        flow_version: 1,
        task_title: "Pause a game",
        seq: 3,
        node: "approve",
        prompt: "Should pausing blur the board to prevent cheating?",
        outcomes: ["approve", "revise", "stop"],
        started_at: Date.now() - 120_000,
        cost_usd: 0.42,
        diff: { files_changed: 3, lines_added: 64, lines_removed: 12 },
        prev: {
          node: "value",
          outcome: "needs_human",
          summary: "Implemented pause overlay; product tradeoff needs direction.",
          outputs: {
            user_impact: "Players can pause mid-round with Escape or tap.",
            questions: ["Blur board on pause or leave it visible?"],
          },
        },
        loops: [{ outcome: "revise", target: "work", used: 2, max_visits: 2 }],
      },
    ],
    failed_runs: [],
    plan_issues: [],
    shipped_unmarked: [
      { project: "web-games", map_id: "arcade", map_title: "Arcade", user_task_id: "home-grid", title: "See every game", release: "mvp", run_id: "r-ok1", metrics: ["weekly"] },
    ],
    open_questions: [
      { project: "web-games", map_id: "arcade", map_title: "Arcade", kind: "task", id: "leaderboard", title: "Daily leaderboard", questions: ["Reset at UTC midnight or local time?"] },
    ],
  };

  await page.route("**/api/**", async (route) => {
    const req = route.request();
    const path = new URL(req.url()).pathname;
    if (req.method() === "POST") {
      posts.push({ path, body: req.postDataJSON() });
      return route.fulfill({ status: 204 });
    }
    if (path === "/api/attention") return route.fulfill({ json: attention });
    if (path === "/api/overview")
      return route.fulfill({
        json: { projects: [{ name: "web-games", repo: "repo/web-games", base: "main", start: "manual", allowed_models: [], allowed_grants: [] }], models: [], presets: [], runtimes: [], grants: [], skills: [], actions: [], node_types: [], planner: { model: "x" }, projectless: { allowed_models: [], allowed_grants: [] }, linear: false, local: true, operations: { as_of: 0, active: 0, waiting: 1, queued: 0, oldest_queued_age_ms: 0, nodes: [] } },
      });
    return route.fulfill({ json: [] });
  });

  await page.goto("/inbox");
  await expect(page.getByRole("heading", { name: "Inbox" })).toBeVisible();
  await expect(page.getByText("Blur board on pause or leave it visible?")).toBeVisible();
  await expect(page.getByText("Reset at UTC midnight or local time?")).toBeVisible();
  await expect(page.getByText("See every game")).toBeVisible();
  await noHorizontalScroll(page);
  await page.screenshot({ path: info.outputPath("inbox.png"), fullPage: true });

  await page.getByLabel("Grant +1 loop round (limit reached)").check();
  await page.getByLabel("Direction note for r-wait1").fill("Yes, blur the board while paused");
  await page.getByRole("button", { name: "revise", exact: true }).click();
  await expect.poll(() => posts).toEqual([
    { path: "/api/runs/r-wait1/gates/3", body: { outcome: "revise", note: "Yes, blur the board while paused", extend_visits: true } },
  ]);
});

test("running flow supports mid-flight pause and steering note", async ({ page }, info) => {
  await stubEventSource(page);
  const posts: { path: string; body: unknown }[] = [];
  const run: Run = {
    id: "r-live1",
    flow_name: "pause-game",
    flow_version: 1,
    status: "running",
    current_node: "work",
    branch: "ai-flow/pause-game",
    base: "main",
    diff: {},
    cost_usd: 0.15,
    tokens: 5100,
    created_at: Date.now() - 30_000,
    started_at: Date.now() - 30_000,
  };
  const visits: Visit[] = [{ run_id: run.id, seq: 1, node: "work", visit: 1, type: "agent", status: "running", outputs: {}, tokens_in: 0, tokens_out: 0, cost_usd: 0, llm_calls: 0, created_at: Date.now() }];
  const view: RunView = { run, visits, events: [], graph };

  await page.route("**/api/**", async (route) => {
    const req = route.request();
    const path = new URL(req.url()).pathname;
    if (req.method() === "POST") {
      const body = req.postDataJSON();
      posts.push({ path, body });
      if (body.pause !== undefined) run.pause_requested = body.pause;
      if (body.note !== undefined) run.steer_note = body.note;
      return route.fulfill({ json: run });
    }
    if (path === `/api/runs/${run.id}`) return route.fulfill({ json: view });
    if (path === "/api/attention") return route.fulfill({ json: { counts: { total: 0, gates: 0, failed_runs: 0, plan_issues: 0, shipped_unmarked: 0, open_questions: 0 }, gates: [], failed_runs: [], plan_issues: [], shipped_unmarked: [], open_questions: [] } });
    if (path === "/api/overview") return route.fulfill({ json: { projects: [], models: [], presets: [], runtimes: [], grants: [], skills: [], actions: [], node_types: [] } });
    return route.fulfill({ json: [] });
  });

  await page.goto(`/runs/${run.id}`);
  await page.getByRole("button", { name: "Pause after step" }).click();
  await expect.poll(() => posts[0]).toEqual({ path: `/api/runs/${run.id}/steer`, body: { pause: true } });
  await page.getByRole("button", { name: "Steer" }).click();
  await page.getByLabel("Steering note for next step").fill("Keep touch targets at least 44px");
  await noHorizontalScroll(page);
  await page.screenshot({ path: info.outputPath("run-steer.png") });
  await page.getByRole("button", { name: "Send direction" }).click();
  await expect.poll(() => posts[1]).toEqual({ path: `/api/runs/${run.id}/steer`, body: { note: "Keep touch targets at least 44px" } });
});

test("project health & value tab and shareable digest modal work", async ({ page }, info) => {
  await stubEventSource(page);
  const summary: ProjectSummary = {
    project: "web-games",
    description: "Browser games",
    repo: "o/web-games",
    branch: "main",
    generated_at: Date.now(),
    delivery: { total_runs: 8, active_runs: 0, waiting_runs: 1, succeeded_runs: 6, failed_runs: 1, success_rate_pct: 85.7, total_cost_usd: 3.45, total_tokens: 120000, median_cycle_hours: 1.2 },
    attention: { counts: { total: 1, gates: 1, failed_runs: 0, plan_issues: 0, shipped_unmarked: 0, open_questions: 0 }, gates: [], failed_runs: [], plan_issues: [], shipped_unmarked: [], open_questions: [] },
    maps: [
      {
        id: "arcade",
        title: "Arcade",
        tasks_total: 4,
        tasks_done: 1,
        releases: [{ id: "mvp", title: "MVP", goal: "One great loop", status: "in_progress", total: 3, done: 1 }],
        metrics: [{ id: "weekly", title: "Weekly players", kind: "product", value: 120, target: 100, direction: "up", on_target: true }],
        open_questions: ["Daily leaderboard: Reset at UTC midnight?"],
        risky_bets: [{ id: "pause", title: "Pause a game", value: "high", confidence: "low", hypothesis: "Keeps players in longer sessions" }],
        outcomes: [{ id: "home-grid", title: "See every game", outcome: "validated", outcome_note: "Trial plays up 24%" }],
      },
    ],
    recent_runs: [{ run_id: "r-1", flow_name: "home-grid", task_title: "See every game", status: "succeeded", cost_usd: 0.45, created_at: Date.now() - 3600_000 }],
    retros: [{ run_id: "r-1", flow_name: "home-grid", summary: "Shipped cleanly in one pass.", suggestions: [{ kind: "flow", change: "Keep qa-test in parallel", why: "Saved 4 minutes", priority: "low" }] }],
  };

  await page.route("**/api/**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path === "/api/projects/web-games/product")
      return route.fulfill({ json: { project: { name: "web-games", description: "Browser games", repo: "o/web-games", branch: "main" }, workspace: { repo: "o/web-games", branch: "main", head: "abc", pulled_at: Date.now(), changes: [] }, docs: ["product/README.md"], maps: [{ id: "arcade", title: "Arcade", tasks: 4, done: 1 }] } });
    if (path === "/api/projects/web-games/summary") return route.fulfill({ json: summary });
    if (path === "/api/projects/web-games/digest") return route.fulfill({ contentType: "text/markdown", body: "# web-games — Project Status & Value Digest\n\n- Success rate: 85.7%\n" });
    if (path === "/api/attention") return route.fulfill({ json: summary.attention });
    if (path === "/api/overview") return route.fulfill({ json: { projects: [], models: [], presets: [], runtimes: [], grants: [], skills: [], actions: [], node_types: [] } });
    return route.fulfill({ json: [] });
  });

  await page.goto("/products/web-games?tab=overview");
  await expect(page.getByText("85.7%")).toBeVisible();
  await expect(page.getByText("Keeps players in longer sessions")).toBeVisible();
  await expect(page.getByText("Trial plays up 24%")).toBeVisible();
  await noHorizontalScroll(page);
  await page.screenshot({ path: info.outputPath("project-health.png"), fullPage: true });

  await page.getByRole("button", { name: "Share digest" }).click();
  await expect(page.getByRole("dialog", { name: "Share web-games digest" })).toBeVisible();
  await expect(page.getByText("# web-games — Project Status & Value Digest")).toBeVisible();
});
