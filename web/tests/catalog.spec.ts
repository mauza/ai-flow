import { test, expect, type Page } from "@playwright/test";
import { parse } from "yaml";
import type { FlowView, Graph, GraphNode, Issue, Overview } from "../src/api";

// Independent library fixture: never shared with the editor regression tests.
const overview: Overview = {
  projectless: { allowed_models: ["external-large", "local-small", "local-medium", "other-medium"], allowed_grants: [] },
  projects: [{ name: "library-project", repo: "repo/library", base: "main", start: "manual", allowed_models: ["local-small", "local-medium", "other-medium"], allowed_grants: ["repo/library:read"] }],
  models: [
    { name: "external-large", model: "external", upstream: "remote", size: "large" },
    { name: "local-small", model: "small", upstream: "local", size: "small" },
    { name: "local-medium", model: "medium", upstream: "local", size: "medium" },
    { name: "other-medium", model: "medium-2", upstream: "local", size: "medium" },
  ],
  presets: [
    { name: "repair", type: "agent", category: "Implementation", description: "Make a focused code change.", when_to_use: "After a failing check identifies a repairable defect.", min_size: "medium", outcomes: ["done", "stuck"], requires: ["Repository checkout", "repo write access: repo/library:write"], outputs: { summary: { type: "string", description: "What changed" } }, definition: { type: "agent", prompt: "Repair the reported defect and explain the change.", llm: { model: "external-large" }, grants: ["repo/library:write"], next: { done: "$success", stuck: "$success" } } },
    { name: "review", type: "agent", category: "Verification", description: "Review a patch for correctness.", when_to_use: "Before publishing a change that needs independent review.", min_size: "large", outcomes: ["approve", "revise"], requires: ["A patch to review"], outputs: { findings: "array" }, definition: { type: "agent", prompt: "Review the patch." } },
    { name: "test", type: "check", category: "Verification", description: "Run the project's test suite.", when_to_use: "After implementation, before review.", outcomes: ["pass", "fail"], requires: ["Test runtime"], outputs: {}, definition: { type: "check", run: "npm test", next: { pass: "$success", fail: "$success" } } },
    { name: "publish", type: "action", category: "Delivery", description: "Open a pull request.", outcomes: ["done"], requires: ["Repository branch"], definition: { type: "action", action: "open_pull_request", next: { done: "$success" } } },
    { name: "legacy", type: "gate", description: "A preset from an older server.", outcomes: ["approve", "reject"] },
  ],
  runtimes: [], grants: [{ name: "repo/library", kind: "git" }], skills: [],
  planner: { model: "local-medium" }, actions: ["open_pull_request"], node_types: ["agent", "check", "gate", "action"], linear: false, local: true,
  operations: { as_of: 0, active: 0, waiting: 0, queued: 0, oldest_queued_age_ms: 0, nodes: [] },
};

const source = `# Preserve this existing flow comment
apiVersion: ai-flow/v1
kind: Flow
metadata:
  name: library-demo
  project: library-project
spec:
  description: Library fixture
  start: existing
  nodes:
    existing:
      type: check
      run: npm test
      next: {pass: $success, fail: $fail}
`;

async function setup(page: Page, options: { projectless?: boolean; presets?: Overview["presets"]; models?: Overview["models"] } = {}) {
  const catalog = { ...overview, presets: options.presets ?? overview.presets, models: options.models ?? overview.models };
  const yaml = options.projectless ? source.replace("  project: library-project\n", "") : source;
  const initial: Graph = { start: "existing", nodes: [{ id: "existing", type: "check", run: "npm test", outcomes: ["pass", "fail"] }], edges: [
    { from: "existing", to: "$success", outcome: "pass", kind: "next" }, { from: "existing", to: "$fail", outcome: "fail", kind: "next" },
  ] };
  const flow = { name: "library-demo", version: 1, yaml, created_at: Date.now() };
  const view: FlowView = { flow, versions: [flow], graph: initial, issues: [], runs: [], planning: false };
  const state = { yaml, requests: [] as string[] };
  await page.addInitScript(() => {
    window.EventSource = class {
      onopen?: () => void;
      constructor() { setTimeout(() => this.onopen?.(), 0); }
      close() {}
    } as unknown as typeof EventSource;
  });
  await page.route("**/api/**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    state.requests.push(path);
    if (path === "/api/overview") return route.fulfill({ json: catalog });
    if (path === "/api/flows/library-demo") return route.fulfill({ json: view });
    if (path === "/api/validate") {
      state.yaml = route.request().postDataJSON().yaml;
      const doc = parse(state.yaml);
      const graph: Graph = { start: doc.spec.start, nodes: [], edges: [] };
      const issues: Issue[] = [];
      // Mock the relevant resolver contract: explicit empty grants/next override inheritance.
      for (const [id, value] of Object.entries(doc.spec.nodes)) {
        const raw = value as Record<string, any>;
        const preset = catalog.presets.find((p) => `preset/${p.name}` === raw.uses);
        const effective = { ...preset?.definition, ...raw };
        const outcomes = raw.outcomes ?? preset?.outcomes ?? (effective.type === "check" ? ["pass", "fail"] : ["done"]);
        const node: GraphNode = { ...effective, id, type: preset?.type ?? raw.type, preset: preset?.name, model: effective.llm?.model ?? effective.model, outcomes };
        graph.nodes.push(node);
        for (const outcome of outcomes) {
          const target = effective.next?.[outcome];
          if (target) graph.edges.push({ from: id, to: target, outcome, kind: "next" });
          else issues.push({ severity: "error", node: id, field: "next", message: `Outcome ${outcome} has no transition` });
        }
      }
      return route.fulfill({ json: { graph, issues } });
    }
    if (path === "/api/tasks" || path === "/api/runs" || path.endsWith("/chat")) return route.fulfill({ json: [] });
    return route.fulfill({ status: 404, json: { error: `Unexpected library API: ${path}` } });
  });
  return state;
}

async function openPicker(page: Page) {
  await page.goto("/flows/library-demo");
  await page.getByRole("button", { name: "Add node", exact: true }).click();
}

test("library filters by purpose, type and category, with useful expanded details", async ({ page }, info) => {
  await setup(page);
  await page.goto("/catalog");
  await expect(page.getByRole("status")).toHaveText("5 of 5 presets");
  await page.getByLabel("Search presets").fill("repo write");
  await expect(page.getByRole("status")).toHaveText("1 of 5 presets");
  const repair = page.getByRole("article").filter({ has: page.getByRole("heading", { name: "preset/repair" }) });
  await repair.locator("summary").click();
  await expect(repair.getByText("repo write access: repo/library:write", { exact: true })).toBeVisible();
  await expect(repair.locator("pre").first()).toContainText('"description": "What changed"');
  await expect(repair.getByText("Repair the reported defect and explain the change.", { exact: true })).toBeVisible();
  await expect(repair.getByText("After a failing check identifies a repairable defect.", { exact: true })).toHaveCount(1);
  await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await page.screenshot({ path: info.outputPath("library-filter.png"), fullPage: true });
  await repair.locator("pre").last().scrollIntoViewIfNeeded();
  await page.screenshot({ path: info.outputPath("library-details.png"), fullPage: true });

  await page.getByLabel("Search presets").fill("");
  await page.getByRole("combobox", { name: "Category", exact: true }).selectOption("Verification");
  await expect(page.getByRole("status")).toHaveText("2 of 5 presets");
  await page.getByRole("combobox", { name: "Node type", exact: true }).selectOption("check");
  await expect(page.getByRole("status")).toHaveText("1 of 5 presets");
  await page.getByRole("article").locator("summary").click();
  await expect(page.getByRole("article").getByText("npm test", { exact: true })).toBeVisible();
  await page.getByLabel("Search presets").fill("nothing-matches");
  await expect(page.getByText("No presets match these filters.")).toBeVisible();
  await page.getByRole("button", { name: "Clear filters" }).click();
  await expect(page.getByRole("status")).toHaveText("5 of 5 presets");
  await page.getByLabel("Search presets").fill("gate other");
  await page.getByRole("article").locator("summary").click();
  await expect(page.getByText("Prompt and command previews are unavailable from this server.")).toBeVisible();
});

test("preset insertion keeps a reference, picks an allowed capable model, and leaves every route open", async ({ page }, info) => {
  const state = await setup(page);
  await openPicker(page);
  await page.getByLabel("Find a preset").fill("implementation agent");
  await expect(page.getByRole("button", { name: /^Use preset\// })).toHaveCount(1);
  await page.getByRole("button", { name: "Use preset/repair", exact: true }).click();
  await expect(page.getByText("repo write access: repo/library:write", { exact: true })).toBeVisible();
  await expect(page.getByRole("combobox", { name: /Preset model/ })).toHaveValue("local-medium");
  await expect(page.getByRole("combobox", { name: /Preset model/ }).locator("option")).toHaveCount(2);
  await page.getByLabel("Node id", { exact: true }).fill("repair_step");
  await page.screenshot({ path: info.outputPath("preset-picker.png"), fullPage: true });
  await page.getByRole("button", { name: "Add", exact: true }).click();
  await expect(page.getByRole("heading", { name: "repair_step", exact: true })).toBeVisible();
  expect(parse(state.yaml).spec.nodes.repair_step).toEqual({ uses: "preset/repair", llm: { model: "local-medium" }, grants: [], next: {} });
  expect(state.yaml).toContain("# Preserve this existing flow comment");
  expect(parse(state.yaml).spec.nodes.existing).toEqual(parse(source).spec.nodes.existing);
  await expect(page.getByLabel("Route done", { exact: true })).toHaveValue("");
  await expect(page.getByLabel("Route stuck", { exact: true })).toHaveValue("");
  await expect(page.locator(".issue").filter({ hasText: "Outcome stuck has no transition" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Save & run", exact: true })).toBeDisabled();
  await expect(page.getByRole("checkbox", { name: /Commit changes/ })).not.toBeChecked();
  await expect(page.getByRole("checkbox", { name: /Commit changes/ })).toBeDisabled();
  await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await page.getByLabel("Route stuck", { exact: true }).scrollIntoViewIfNeeded();
  await page.screenshot({ path: info.outputPath("preset-unrouted.png"), fullPage: true });

  // Changing the inserted model must override llm.model, not a lower-priority shorthand.
  await page.getByRole("combobox", { name: "Model", exact: true }).selectOption("other-medium");
  await expect.poll(() => parse(state.yaml).spec.nodes.repair_step.llm.model).toBe("other-medium");
  await page.getByLabel("Route stuck", { exact: true }).selectOption("$fail");
  await expect.poll(() => parse(state.yaml).spec.nodes.repair_step.next).toEqual({ stuck: "$fail" });
  await expect(page.getByLabel("Route done", { exact: true })).toHaveValue("");
});

test("incompatible model blocks insertion and changing presets recovers", async ({ page }, info) => {
  await setup(page);
  await openPicker(page);
  await page.getByRole("button", { name: "Use preset/review", exact: true }).click();
  await page.getByLabel("Node id", { exact: true }).fill("review_step");
  await expect(page.getByRole("combobox", { name: /Preset model/ })).toHaveValue("");
  await expect(page.getByText(/No project-allowed model with a known size meeting large/)).toBeVisible();
  await expect(page.getByRole("button", { name: "Add", exact: true })).toBeDisabled();
  await page.screenshot({ path: info.outputPath("preset-model-unavailable.png"), fullPage: true });
  await page.getByRole("button", { name: "Use preset/repair", exact: true }).click();
  await expect(page.getByRole("combobox", { name: /Preset model/ })).toHaveValue("local-medium");
  await expect(page.getByRole("button", { name: "Add", exact: true })).toBeEnabled();
  await page.getByLabel("Node id", { exact: true }).fill("existing");
  await expect(page.getByRole("button", { name: "Add", exact: true })).toBeDisabled();
  await expect(page.getByText("This node id already exists. Choose a unique id.")).toBeVisible();
});

test("projectless preset insertion uses the projectless allow-list", async ({ page }) => {
  const state = await setup(page, { projectless: true });
  await openPicker(page);
  await page.getByRole("button", { name: "Use preset/review", exact: true }).click();
  await expect(page.getByRole("combobox", { name: /Preset model/ })).toHaveValue("external-large");
  await page.getByLabel("Node id", { exact: true }).fill("review_step");
  await page.getByRole("button", { name: "Add", exact: true }).click();
  await expect.poll(() => parse(state.yaml).spec.nodes.review_step?.llm?.model).toBe("external-large");
});

test("unknown model sizes cannot silently satisfy a preset minimum", async ({ page }) => {
  await setup(page, { models: overview.models.map((model) => ({ ...model, size: undefined })) });
  await openPicker(page);
  await page.getByRole("button", { name: "Use preset/repair", exact: true }).click();
  await page.getByLabel("Node id", { exact: true }).fill("repair_step");
  await expect(page.getByRole("button", { name: "Add", exact: true })).toBeDisabled();
  await expect(page.getByText(/No project-allowed model with a known size meeting medium/)).toBeVisible();
});

test("non-model presets and blank actions start with explicit empty routing", async ({ page }) => {
  const state = await setup(page);
  await openPicker(page);
  await page.getByRole("button", { name: "Use preset/publish", exact: true }).click();
  await expect(page.getByRole("combobox", { name: /Preset model/ })).toHaveCount(0);
  await page.getByLabel("Node id", { exact: true }).fill("publish_step");
  await page.getByRole("button", { name: "Add", exact: true }).click();
  await expect.poll(() => parse(state.yaml).spec.nodes.publish_step).toEqual({ uses: "preset/publish", grants: [], next: {} });
  await page.getByRole("button", { name: "Close", exact: true }).click();
  await page.getByRole("button", { name: "Add node", exact: true }).click();
  await page.getByRole("button", { name: "Blank node", exact: true }).click();
  await page.getByRole("button", { name: "Action", exact: true }).click();
  await page.getByLabel("Node id", { exact: true }).fill("blank_action");
  await page.getByRole("button", { name: "Add", exact: true }).click();
  await expect.poll(() => parse(state.yaml).spec.nodes.blank_action?.next).toEqual({});
});

test("empty preset catalogs retain blank-node creation", async ({ page }) => {
  const state = await setup(page, { presets: [] });
  await page.goto("/catalog");
  await expect(page.getByText(/No presets configured yet/)).toBeVisible();
  await openPicker(page);
  await expect(page.getByRole("button", { name: "Blank node", exact: true })).toHaveAttribute("aria-pressed", "true");
  await page.getByLabel("Node id", { exact: true }).fill("blank_agent");
  await page.getByRole("button", { name: "Add", exact: true }).click();
  await expect.poll(() => parse(state.yaml).spec.nodes.blank_agent?.model).toBe("local-small");
});

test("the preset picker responds when overview loads after the flow", async ({ page }) => {
  await setup(page);
  let release!: () => void;
  const ready = new Promise<void>((resolve) => { release = resolve; });
  await page.route("**/api/overview", async (route) => {
    await ready;
    await route.fulfill({ json: overview });
  });
  await openPicker(page);
  await expect(page.getByRole("button", { name: "Blank node", exact: true })).toHaveAttribute("aria-pressed", "true");
  release();
  await expect(page.getByRole("button", { name: "Preset", exact: true })).toHaveAttribute("aria-pressed", "true");
  await expect(page.getByLabel("Find a preset")).toBeVisible();
  await expect(page.getByRole("button", { name: "Use preset/repair", exact: true })).toBeVisible();
});
