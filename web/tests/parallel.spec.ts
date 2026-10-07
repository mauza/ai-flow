import { test, expect, type Page } from "@playwright/test";
import { parse } from "yaml";
import type { Graph, Run, RunView, Visit } from "../src/api";
import { deleteNode, removeTransition, renameNode, setTransition } from "../src/flow/yamlEdit";

const flowYaml = `apiVersion: ai-flow/v1alpha1
kind: Flow
metadata: { name: par, project: sandbox }
spec:
  start: checks
  nodes:
    checks:
      type: parallel
      branches: [lint, test]
      join: gather
    lint: { type: check, run: make lint, next: { pass: gather, fail: gather } }
    test: { type: check, run: make test, next: { pass: gather, fail: gather } }
    review: { type: llm, model: m, prompt: r, outcomes: [ok], next: { ok: gather } }
    gather: { type: join, next: { done: $success } }
`;

const nodes = (y: string) => parse(y).spec.nodes;

test("YAML edits treat a parallel node's outcomes as branches", () => {
  let y = setTransition(flowYaml, "checks", "test", "review");
  expect(nodes(y).checks.branches).toEqual(["lint", "review"]);
  expect(nodes(y).checks.next).toBeUndefined();
  y = setTransition(y, "checks", "", "test");
  expect(nodes(y).checks.branches).toEqual(["lint", "review", "test"]);
  expect(() => setTransition(y, "checks", "lint", "$success")).toThrow(/must start at a node/);
  y = removeTransition(y, "checks", "lint");
  expect(nodes(y).checks.branches).toEqual(["review", "test"]);
  y = renameNode(y, "review", "audit");
  expect(nodes(y).checks.branches).toEqual(["audit", "test"]);
  y = renameNode(y, "gather", "collect");
  expect(nodes(y).checks.join).toBe("collect");
  y = deleteNode(y, "test");
  expect(nodes(y).checks.branches).toEqual(["audit"]);
  y = deleteNode(y, "collect");
  expect(nodes(y).checks.join).toBeUndefined();
});

const graph: Graph = {
  start: "checks",
  nodes: [
    { id: "checks", type: "parallel", outcomes: ["lint", "test"], branches: ["lint", "test"], join: "gather" },
    { id: "lint", type: "check", run: "make lint", outcomes: ["pass", "fail"] },
    { id: "test", type: "check", run: "make test", outcomes: ["pass", "fail"] },
    { id: "gather", type: "join", outcomes: ["done"] },
  ],
  edges: [
    { from: "checks", to: "lint", outcome: "lint", kind: "branch" },
    { from: "checks", to: "test", outcome: "test", kind: "branch" },
    { from: "lint", to: "gather", outcome: "pass", kind: "next" },
    { from: "lint", to: "gather", outcome: "fail", kind: "next" },
    { from: "test", to: "gather", outcome: "pass", kind: "next" },
    { from: "test", to: "gather", outcome: "fail", kind: "next" },
    { from: "gather", to: "$success", outcome: "done", kind: "next" },
  ],
};

function visit(seq: number, node: string, type: string, status: string, extra: Partial<Visit> = {}): Visit {
  return { run_id: "r-par", seq, node, visit: 1, type, status, outputs: {}, tokens_in: 0, tokens_out: 0, cost_usd: 0, llm_calls: 0, created_at: Date.now(), ...extra };
}

async function setup(page: Page, view: RunView) {
  await page.addInitScript(() => {
    window.EventSource = class {
      onopen?: () => void;
      constructor() { setTimeout(() => this.onopen?.(), 0); }
      close() {}
    } as unknown as typeof EventSource;
  });
  await page.route("**/api/**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path === `/api/runs/${view.run.id}`) return route.fulfill({ json: view });
    return route.fulfill({ json: [] });
  });
}

test("a running parallel node shows its branches running side by side", async ({ page }, info) => {
  const run: Run = { id: "r-par", flow_name: "par", flow_version: 1, status: "running", current_node: "checks", branch: "ai-flow/par", base: "main", diff: {}, cost_usd: 0, tokens: 0, created_at: Date.now() - 5000, started_at: Date.now() - 5000 };
  await setup(page, {
    run, graph, events: [], visits: [
      visit(1, "checks", "parallel", "running"),
      visit(2, "lint", "check", "succeeded", { outcome: "pass" }),
      visit(3, "test", "check", "running", { progress: "$ make test" }),
    ],
  });
  await page.goto(`/runs/${run.id}`);
  const node = (id: string) => page.locator(".fnode").filter({ has: page.locator(".fnode-title", { hasText: new RegExp(`^${id}$`) }) });
  await expect(node("checks")).toContainText("2 branches → gather");
  await expect(node("checks")).toContainText("Parallel");
  await expect(node("gather")).toContainText("Join");
  await expect(page.locator(".fnode").filter({ hasText: "make test" }).locator(".progress")).toBeVisible();
  await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await page.screenshot({ path: info.outputPath("parallel-run.png") });
});
