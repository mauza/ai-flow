import { test, expect, type Page } from "@playwright/test";
import type { Graph, Run, RunView, Visit } from "../src/api";

const graph: Graph = {
  start: "work",
  nodes: [
    { id: "work", type: "agent", model: "gpt-6-sol", outcomes: ["done"] },
    { id: "test", type: "check", run: "make test", outcomes: ["pass", "fail"] },
    { id: "approve", type: "gate", prompt: "Ship it?", outcomes: ["approve", "revise"] },
  ],
  edges: [
    { from: "work", to: "test", outcome: "done", kind: "next" },
    { from: "test", to: "approve", outcome: "pass", kind: "next" },
    { from: "test", to: "work", outcome: "fail", kind: "next" },
    { from: "approve", to: "$success", outcome: "approve", kind: "next" },
    { from: "approve", to: "work", outcome: "revise", kind: "next" },
  ],
};

const baseRun: Run = { id: "r-abc123", flow_name: "demo", flow_version: 2, status: "failed", branch: "ai-flow/demo-abc123", base: "main", diff: {}, cost_usd: 0.12, tokens: 4200, created_at: Date.now() - 60_000, started_at: Date.now() - 60_000, finished_at: Date.now() };

function visit(seq: number, node: string, type: string, status: string, extra: Partial<Visit> = {}): Visit {
  return { run_id: baseRun.id, seq, node, visit: 1, type, status, outputs: {}, tokens_in: 0, tokens_out: 0, cost_usd: 0, llm_calls: 0, created_at: Date.now(), ...extra };
}

async function setup(page: Page, view: RunView) {
  const posts: { path: string; body: unknown }[] = [];
  await page.addInitScript(() => {
    window.EventSource = class {
      onopen?: () => void;
      constructor() { setTimeout(() => this.onopen?.(), 0); }
      close() {}
    } as unknown as typeof EventSource;
  });
  await page.route("**/api/**", async (route) => {
    const req = route.request();
    const path = new URL(req.url()).pathname;
    if (req.method() === "POST") {
      posts.push({ path, body: req.postDataJSON() });
      return route.fulfill(path.endsWith("/resume") ? { json: { ...view.run, status: "running" } } : { status: 204 });
    }
    if (path === `/api/runs/${view.run.id}`) return route.fulfill({ json: view });
    if (path === "/api/overview") return route.fulfill({ json: { projects: [], models: [], presets: [], runtimes: [], grants: [], skills: [], actions: [], node_types: [] } });
    return route.fulfill({ json: [] });
  });
  return posts;
}

async function noHorizontalScroll(page: Page) {
  await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
}

test("a failed run resumes at a chosen step with a note", async ({ page }, info) => {
  const run = { ...baseRun, error: "test: node pod failed: OOMKilled" };
  const posts = await setup(page, { run, graph, events: [], visits: [visit(1, "work", "agent", "succeeded", { outcome: "done" }), visit(2, "test", "check", "error", { error: "node pod failed: OOMKilled" })] });
  await page.goto(`/runs/${run.id}`);
  await expect(page.getByText("Run failed")).toBeVisible();
  await page.getByRole("button", { name: "Resume", exact: true }).click();
  await expect(page.getByLabel("Resume at")).toHaveValue("test");
  await page.getByLabel("Resume at").selectOption("work");
  await page.getByLabel("Note for the next steps (optional)").fill("Raise the memory limit first");
  await noHorizontalScroll(page);
  await page.screenshot({ path: info.outputPath("resume.png") });
  await page.locator("form").getByRole("button", { name: "Resume" }).click();
  await expect.poll(() => posts).toEqual([{ path: `/api/runs/${run.id}/resume`, body: { node: "work", note: "Raise the memory limit first" } }]);
});

test("a gate decision carries the note", async ({ page }, info) => {
  const run = { ...baseRun, status: "waiting", finished_at: undefined };
  const posts = await setup(page, { run, graph, events: [], visits: [visit(1, "work", "agent", "succeeded", { outcome: "done" }), visit(2, "test", "check", "succeeded", { outcome: "pass" }), visit(3, "approve", "gate", "waiting", { prompt: "Ship it?" })] });
  await page.goto(`/runs/${run.id}`);
  await expect(page.getByText("needs your decision")).toBeVisible();
  await page.getByLabel("Note for the next step").fill("Use the existing slug helper");
  await noHorizontalScroll(page);
  await page.screenshot({ path: info.outputPath("gate-note.png") });
  await page.getByRole("button", { name: "revise", exact: true }).click();
  await expect.poll(() => posts).toEqual([{ path: `/api/runs/${run.id}/gates/3`, body: { outcome: "revise", note: "Use the existing slug helper" } }]);
});
