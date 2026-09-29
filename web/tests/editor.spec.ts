import { test, expect, type Page, type Route } from "@playwright/test";
import type { FlowView, Graph, Overview } from "../src/api";

const yaml = `apiVersion: ai-flow/v1
kind: Flow
metadata:
  name: demo
  project: sandbox
spec:
  description: Original goal
  start: implement
  nodes:
    implement:
      type: agent
      model: small
      prompt: Implement the task
      outcomes: [done]
      next:
        done: $success
`;
const graph: Graph = {
  start: "implement",
  nodes: [{ id: "implement", type: "agent", model: "small", outcomes: ["done"], grants: [] }],
  edges: [{ from: "implement", to: "$success", outcome: "done", kind: "next" }],
};
const overview: Overview = {
  projectless: { allowed_models: ["big", "small"], allowed_grants: ["repo/demo", "repo/demo:read", "repo/demo:write", "tools/admin"] },
  projects: [{ name: "sandbox", repo: "repo/demo", base: "main", start: "manual", allowed_models: ["small"], allowed_grants: ["repo/demo:read"] }],
  models: [{ name: "big", model: "big", upstream: "local" }, { name: "small", model: "small", upstream: "local" }],
  grants: [{ name: "repo/demo", kind: "git" }, { name: "tools/admin", kind: "mcp" }],
  runtimes: [], skills: [], presets: [], actions: [], node_types: ["agent"],
  planner: { model: "small" }, linear: false, local: true,
  operations: { as_of: Date.now(), active: 0, waiting: 0, queued: 0, oldest_queued_age_ms: 0, nodes: [] },
};

async function setup(page: Page) {
  // Drive disconnect/reconnect deterministically without a real control plane.
  await page.addInitScript(() => {
    class FakeEventSource {
      onopen?: () => void;
      onerror?: () => void;
      onmessage?: (event: { data: string }) => void;
      constructor() {
        (window as any).events = this;
        setTimeout(() => this.onopen?.(), 0);
      }
      close() {}
    }
    (window as any).EventSource = FakeEventSource;
  });
  const state = {
    view: { flow: { name: "demo", version: 1, yaml, created_at: Date.now() }, versions: [{ name: "demo", version: 1, yaml, created_at: Date.now() }], graph, issues: [], runs: [], planning: false } as FlowView,
    flowReads: 0,
    startedVersion: 0,
    validate: async (route: Route) => route.fulfill({ json: { graph, issues: [] } }),
    chat: async (route: Route) => route.fulfill({ json: [] }),
    afterSave: () => {},
    beforeSave: async () => {},
  };
  await page.route("**/api/**", async (route) => {
    const req = route.request();
    const path = new URL(req.url()).pathname;
    if (path === "/api/overview") return route.fulfill({ json: overview });
    if (path === "/api/validate") return state.validate(route);
    if (path === "/api/flows/demo/chat") return req.method() === "POST" ? state.chat(route) : route.fulfill({ json: [] });
    if (path === "/api/flows/demo/runs") {
      state.startedVersion = req.postDataJSON().version;
      return route.fulfill({ json: { id: "r-test" } });
    }
    if (path === "/api/flows/demo" || path.includes("/versions/")) {
      if (req.method() === "POST") {
        await state.beforeSave();
        const flow = { ...state.view.flow, version: state.view.flow.version + 1, yaml: req.postDataJSON().yaml };
        state.view = { ...state.view, flow, versions: [flow, ...state.view.versions] };
        state.afterSave();
        return route.fulfill({ json: { flow, issues: [] } });
      }
      state.flowReads++;
      return route.fulfill({ json: state.view });
    }
    if (path === "/api/runs/r-test") return route.fulfill({ json: { run: { id: "r-test", flow_name: "demo", flow_version: state.startedVersion, status: "queued", cost_usd: 0, tokens: 0, diff: {}, created_at: Date.now(), branch: "test", base: "main" }, visits: [], events: [], graph, yaml } });
    if (path === "/api/runs" || path === "/api/tasks") return route.fulfill({ json: [] });
    return route.fulfill({ status: 404, json: { error: `Unhandled test API ${path}` } });
  });
  await page.goto("/flows/demo");
  await expect(page.getByPlaceholder("One line: what this flow achieves")).toHaveValue("Original goal");
  return state;
}

async function editGoal(page: Page, goal: string) {
  const input = page.getByPlaceholder("One line: what this flow achieves");
  await input.fill(goal);
  await input.press("Tab");
}

test("Save and run pins the returned version despite another save", async ({ page }) => {
  const state = await setup(page);
  state.afterSave = () => {
    state.view.flow = { ...state.view.flow, version: 3, yaml: yaml.replace("Original goal", "Someone else's edit") };
  };
  await editGoal(page, "My reviewed change");
  await expect(page.getByRole("button", { name: "Save & run", exact: true })).toBeEnabled();
  await page.getByRole("button", { name: "Save & run", exact: true }).click();
  await expect.poll(() => state.startedVersion).toBe(2);
  await expect(page).toHaveURL(/\/runs\/r-test$/);
});

test("external-save conflict preserves edits and Revert adopts the remote baseline", async ({ page }, info) => {
  const state = await setup(page);
  await editGoal(page, "My unsaved goal");
  const remote = { ...state.view.flow, version: 2, yaml: yaml.replace("Original goal", "Remote goal") };
  state.view = { ...state.view, flow: remote, versions: [remote, ...state.view.versions] };
  await page.evaluate(() => (window as any).events.onmessage({ data: JSON.stringify({ type: "flow", id: "demo" }) }));
  await expect(page.getByRole("status")).toContainText("You are editing v1");
  await expect(page.getByPlaceholder("One line: what this flow achieves")).toHaveValue("My unsaved goal");
  await page.getByRole("button", { name: "Revert", exact: true }).click();
  await expect(page.getByPlaceholder("One line: what this flow achieves")).toHaveValue("Remote goal");
  await expect(page.getByTitle("Version", { exact: true })).toHaveValue("2");
  await expect(page.getByRole("button", { name: "Run", exact: true })).toBeEnabled();
  await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await page.screenshot({ path: info.outputPath("editor.png"), fullPage: true });
});

test("validation ignores obsolete responses and exposes failure/retry", async ({ page }) => {
  const state = await setup(page);
  let old: Route | undefined;
  state.validate = async (route) => {
    const src = route.request().postDataJSON().yaml as string;
    if (src.includes("First edit")) { old = route; return; }
    return route.fulfill({ json: { graph, issues: [{ severity: "error", message: "Newer validation error" }] } });
  };
  await editGoal(page, "First edit");
  await expect.poll(() => !!old).toBe(true);
  await editGoal(page, "Second edit");
  await expect(page.getByText("Newer validation error", { exact: true })).toBeVisible();
  await old!.fulfill({ json: { graph, issues: [] } });
  await expect(page.getByRole("button", { name: "Save & run", exact: true })).toBeDisabled();
  await expect(page.getByText("Newer validation error", { exact: true })).toBeVisible();
  state.validate = async (route) => route.fulfill({ status: 503, json: { error: "temporarily unavailable" } });
  await editGoal(page, "Third edit");
  await expect(page.getByRole("status")).toContainText("temporarily unavailable");
  await expect(page.getByRole("button", { name: "Save & run", exact: true })).toBeDisabled();
  state.validate = async (route) => route.fulfill({ json: { graph, issues: [] } });
  await page.getByRole("button", { name: "Retry validation" }).click();
  await expect(page.getByRole("button", { name: "Save & run", exact: true })).toBeEnabled();
});

test("planner proposals survive tab switches while pending and after completion", async ({ page }, info) => {
  const state = await setup(page);
  let pending: Route | undefined;
  state.chat = async (route) => { pending = route; };
  await page.getByRole("tab", { name: "Planner" }).click();
  await page.getByPlaceholder("e.g. add a lint check after the fix step").fill("Improve the goal");
  await page.getByRole("button", { name: "Send", exact: true }).click();
  await expect.poll(() => !!pending).toBe(true);
  await page.getByRole("tab", { name: "YAML", exact: true }).click();
  await pending!.fulfill({ json: { yaml: yaml.replace("Original goal", "Proposed goal"), graph, valid: false, issues: [{ severity: "error", message: "Needs a review" }], attempts: 3, explanation: "Updated goal" } });
  await page.getByRole("tab", { name: "Planner" }).click();
  await expect(page.getByText("Proposed change", { exact: false })).toBeVisible();
  await expect(page.getByText("Needs a review", { exact: true })).toBeVisible();
  await page.getByRole("tab", { name: "Flow", exact: true }).click();
  await page.getByRole("tab", { name: "Planner" }).click();
  await expect(page.getByRole("button", { name: "Apply to editor" })).toBeEnabled();
  await page.getByRole("button", { name: "Apply to editor" }).scrollIntoViewIfNeeded();
  await page.screenshot({ path: info.outputPath("planner.png"), fullPage: true });
  await page.getByRole("button", { name: "Apply to editor" }).click();
  await page.getByRole("tab", { name: "Flow", exact: true }).click();
  await expect(page.getByPlaceholder("One line: what this flow achieves")).toHaveValue("Proposed goal");
});

test("project-aware choices disable unavailable models and grants", async ({ page }) => {
  await setup(page);
  await page.getByRole("button", { name: "implement Agent", exact: true }).click();
  await expect(page.getByRole("combobox", { name: "Model", exact: true }).locator('option[value="big"]')).toBeDisabled();
  await expect(page.getByRole("combobox", { name: "Model", exact: true }).locator('option[value="small"]')).toBeEnabled();
  await expect(page.getByRole("checkbox", { name: /Commit changes/ })).toBeDisabled();
  await expect(page.getByRole("checkbox", { name: /tools\/admin/ })).toBeDisabled();
  await page.getByRole("button", { name: "Close", exact: true }).click();
  await page.getByRole("button", { name: "Add node", exact: true }).click();
  await page.getByPlaceholder("e.g. run_linter").fill("new_agent");
  await page.getByRole("button", { name: "Add", exact: true }).click();
  await page.getByRole("tab", { name: "YAML", exact: true }).click();
  await expect(page.locator(".cm-content")).toContainText("model: small");
  await expect(page.locator(".cm-content")).not.toContainText("model: big");
});

test("reconnecting refreshes resources without a subsequent event", async ({ page }) => {
  const state = await setup(page);
  const remote = { ...state.view.flow, version: 2, yaml: yaml.replace("Original goal", "Changed while offline") };
  state.view = { ...state.view, flow: remote, versions: [remote, ...state.view.versions] };
  await page.evaluate(() => { (window as any).events.onerror(); (window as any).events.onopen(); });
  await expect(page.getByPlaceholder("One line: what this flow achieves")).toHaveValue("Changed while offline");
});

test("choosing a conflicted remote version preserves subsequent edits on refresh", async ({ page }) => {
  const state = await setup(page);
  await editGoal(page, "Unsaved goal");
  const remote = { ...state.view.flow, version: 2, yaml: yaml.replace("Original goal", "Remote goal") };
  state.view = { ...state.view, flow: remote, versions: [remote, ...state.view.versions] };
  await page.evaluate(() => (window as any).events.onmessage({ data: JSON.stringify({ type: "flow", id: "demo" }) }));
  await expect(page.getByRole("status")).toContainText("You are editing v1");
  page.once("dialog", (dialog) => dialog.accept());
  await page.getByTitle("Version", { exact: true }).selectOption("2");
  await expect(page.getByPlaceholder("One line: what this flow achieves")).toHaveValue("Remote goal");
  await editGoal(page, "Further edits");
  const response = page.waitForResponse((r) => r.url().endsWith("/api/flows/demo"));
  await page.evaluate(() => (window as any).events.onmessage({ data: JSON.stringify({ type: "run", id: "r-other" }) }));
  await response;
  await expect(page.getByPlaceholder("One line: what this flow achieves")).toHaveValue("Further edits");
});

test("leaving during save does not start a run or navigate later", async ({ page }) => {
  const state = await setup(page);
  let release!: () => void;
  let saving = false;
  state.beforeSave = () => new Promise<void>((resolve) => { saving = true; release = resolve; });
  await editGoal(page, "Saved later");
  await expect(page.getByRole("button", { name: "Save & run", exact: true })).toBeEnabled();
  await page.getByRole("button", { name: "Save & run", exact: true }).click();
  await expect.poll(() => saving).toBe(true);
  await page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: "Runs", exact: true }).click();
  await expect(page).toHaveURL(/\/runs$/);
  const response = page.waitForResponse((r) => r.request().method() === "POST" && r.url().endsWith("/api/flows/demo"));
  release();
  await response;
  await page.evaluate(() => new Promise<void>((resolve) => requestAnimationFrame(() => requestAnimationFrame(() => resolve()))));
  expect(state.startedVersion).toBe(0);
  await expect(page).toHaveURL(/\/runs$/);
  await expect(page.getByRole("region", { name: "Operations" })).toBeVisible();
});

test("same-version refresh adopts new validation after configuration changes", async ({ page }) => {
  const state = await setup(page);
  state.view = { ...state.view, issues: [{ severity: "error", message: "Model permission was revoked" }] };
  await page.evaluate(() => { (window as any).events.onerror(); (window as any).events.onopen(); });
  await expect(page.getByText("Model permission was revoked", { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Run", exact: true })).toBeDisabled();
  state.view = { ...state.view, issues: [] };
  await page.evaluate(() => { (window as any).events.onerror(); (window as any).events.onopen(); });
  await expect(page.getByRole("button", { name: "Run", exact: true })).toBeEnabled();
});

test("projectless flows keep catalog choices", async ({ page }) => {
  const state = await setup(page);
  state.view = { ...state.view, flow: { ...state.view.flow, yaml: yaml.replace("  project: sandbox\n", "") } };
  await page.reload();
  await page.getByRole("button", { name: "implement Agent", exact: true }).click();
  await expect(page.getByRole("combobox", { name: "Model", exact: true }).locator('option[value="big"]')).toBeEnabled();
  await expect(page.getByRole("checkbox", { name: /Commit changes/ })).toBeEnabled();
});

test("removing the last inherited grant writes an explicit empty override", async ({ page }) => {
  const state = await setup(page);
  state.view = { ...state.view, graph: { ...graph, nodes: [{ ...graph.nodes[0], grants: ["tools/admin"] }] } };
  await page.reload();
  await page.getByRole("button", { name: "implement Agent", exact: true }).click();
  await page.getByRole("button", { name: "Remove grant", exact: true }).click();
  await page.getByRole("tab", { name: "YAML", exact: true }).click();
  await expect(page.locator(".cm-content")).toContainText("grants: []");
});
