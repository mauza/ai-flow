import { test, expect } from "@playwright/test";
import type { FlowView, Graph, Overview } from "../src/api";

const overview: Overview = {
  projectless: { allowed_models: [], allowed_grants: [] },
  projects: [{ name: "sandbox", repo: "repo/x", base: "main", start: "manual", allowed_models: ["m"], allowed_grants: [] }],
  models: [{ name: "m", model: "m", upstream: "home", size: "medium" }],
  presets: [], runtimes: [], grants: [], skills: [],
  planner: { model: "m" }, actions: [], node_types: ["check"], linear: false, local: true,
  operations: { as_of: 0, active: 0, waiting: 0, queued: 0, oldest_queued_age_ms: 0, nodes: [] },
};

const yaml = `apiVersion: ai-flow/v1alpha1
kind: Flow
metadata: { name: slow, project: sandbox }
spec:
  start: test
  nodes:
    test: { type: check, run: make test, next: { pass: $success, fail: $fail } }
`;

const graph: Graph = {
  start: "test",
  nodes: [{ id: "test", type: "check", run: "make test", outcomes: ["pass", "fail"] }],
  edges: [
    { from: "test", to: "$success", outcome: "pass", kind: "next" },
    { from: "test", to: "$fail", outcome: "fail", kind: "next" },
  ],
};

// Regression: the editor validated its empty initial YAML when the flow took
// longer than the 250ms validation debounce to load (slow CI runners).
test("a slow flow load never validates the empty initial document", async ({ page }) => {
  const validated: string[] = [];
  await page.addInitScript(() => {
    window.EventSource = class {
      onopen?: () => void;
      constructor() { setTimeout(() => this.onopen?.(), 0); }
      close() {}
    } as unknown as typeof EventSource;
  });
  await page.route("**/api/**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path === "/api/validate") {
      validated.push(route.request().postDataJSON().yaml);
      return route.fulfill({ json: { graph, issues: [] } });
    }
    if (path === "/api/overview") return route.fulfill({ json: overview });
    if (path === "/api/flows/slow") {
      await new Promise((r) => setTimeout(r, 900));
      const flow = { name: "slow", version: 1, yaml, created_at: Date.now() };
      return route.fulfill({ json: { flow, versions: [flow], graph, issues: [], runs: [], planning: false } satisfies FlowView });
    }
    return route.fulfill({ json: [] });
  });
  await page.goto("/flows/slow");
  await expect(page.locator(".fnode-title", { hasText: /^test$/ })).toBeVisible();
  await page.waitForTimeout(500);
  expect(validated.filter((y) => y.trim() === "")).toEqual([]);
});
