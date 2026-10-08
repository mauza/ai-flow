import { test, expect, type Page } from "@playwright/test";
import type { ConfigView, MapMessage, MapView, ProductView, RepoRow, Retro, RunView, StoryMap, UserTask } from "../src/api";

// A stateful mock of the product API: enough behaviour to drive the pages.
function backend() {
  const map: StoryMap = {
    title: "Arcade",
    description: "Casual games anyone can pick up.",
    personas: [{ id: "player", name: "Casual player" }],
    journey: [
      { id: "discover", title: "Discover", activities: [{ id: "browse", title: "Browse games", persona: "player" }] },
      { id: "play", title: "Play", activities: [{ id: "play-game", title: "Play a game" }, { id: "compete", title: "Compete" }] },
    ],
    releases: [
      { id: "mvp", title: "MVP", goal: "One great loop", status: "in_progress" },
      { id: "next", title: "Next", status: "planned" },
    ],
    metrics: [
      { id: "weekly", title: "Weekly players", kind: "product", query: "sum(x)", target: 100, direction: "up" },
      { id: "mvp-done", title: "MVP done", kind: "delivery", measure: "done_ratio", release: "mvp", unit: "%" },
    ],
  };
  const tasks: UserTask[] = [
    { id: "home-grid", title: "See every game on the home page", activity: "browse", release: "mvp", order: 1, status: "done", story: "As a player I want to see all games" },
    { id: "restart", title: "Restart after losing", activity: "play-game", release: "mvp", order: 1 },
    { id: "pause", title: "Pause a game", activity: "play-game", release: "mvp", order: 2 },
    { id: "leaderboard", title: "Daily leaderboard", activity: "compete", release: "next", order: 1 },
  ];
  const s = {
    map,
    tasks,
    dirty: [] as string[],
    posts: [] as { path: string; body: any }[],
    chat: [] as MapMessage[],
    retro: null as Retro | null,
    configEdits: [] as any[],
    linked: false,
  };
  const config: ConfigView = {
    sections: {
      models: { "gpt-6.1-sol": "upstream: home\nmodel: gpt-6.1-sol\nsize: frontier\ncontext_tokens: 100000\nnotes: Default planner.\n" },
      presets: { implement: "type: agent\ncategory: Implementation\ndescription: Make the change.\noutcomes: [done, stuck]\n" },
      runtimes: {}, harnesses: { pi: "description: pi coding agent\n" }, grants: { "repo/web-games": "kind: git\nurl: https://github.com/o/web-games.git\nmodes: [read, write]\n" }, skills: {},
    },
    settings: { planner: "model: gpt-6.1-sol\nstream: true\n" },
    projects: { "web-games": "spec:\n  repo: repo/web-games\n  base: main\n  start: manual\n" },
    seeded_at: Date.now() - 3600_000,
    env: {
      git_hosts: [{ host: "github.com", token_env: "GITHUB_TOKEN", token_set: true }],
      git_author: "ai-flow <ai-flow@example.com>",
      github: { api_url: "https://api.github.com", token_env: "GITHUB_TOKEN", token_set: true },
      upstreams: [{ name: "home", base_url: "http://litellm:4000/v1" }],
      mcp_servers: [],
      metrics_url: "http://vm:8428",
    },
  };
  const product = (): ProductView => ({
    project: { name: "web-games", description: "Browser games", repo: "o/web-games", branch: "main" },
    workspace: { repo: "o/web-games", branch: "main", head: "abc1234def", pulled_at: Date.now() - 60_000, changes: s.dirty.map((p) => ({ path: p, kind: "modified", base: "a\n", work: "b\n" })) },
    docs: ["product/README.md", "product/vision.md"],
    maps: [{ id: "arcade", title: "Arcade", description: map.description, tasks: s.tasks.length, done: s.tasks.filter((t) => t.status === "done").length }],
  });
  const mapView = (): MapView => ({
    map: { id: "arcade", map: s.map, tasks: s.tasks, problems: [] },
    works: s.posts.some((p) => p.path.endsWith("/send"))
      ? [{ scope: { map: "arcade", kind: "task", id: "restart", tasks: ["restart"] }, task: { id: "t-1", source: "storymap", title: "Restart after losing", body: "", project: "web-games", status: "running", created_at: Date.now(), updated_at: Date.now() }, run: { id: "r-1", flow_name: "restart", flow_version: 1, status: "running", branch: "b", base: "main", diff: {}, cost_usd: 0, tokens: 0, created_at: Date.now() } }]
      : [],
    dirty: s.dirty,
    measures: ["tasks_done", "done_ratio"],
    statuses: ["todo", "in_progress", "done", "blocked"],
  });
  const repos: RepoRow[] = [
    { full_name: "o/web-games", default_branch: "main", private: false, archived: false, project: "web-games" },
    { full_name: "o/new-thing", description: "A fresh idea", default_branch: "trunk", private: true, archived: false, pushed_at: new Date().toISOString() },
  ];
  return { s, config, product, mapView, repos };
}

async function setup(page: Page) {
  const b = backend();
  await page.addInitScript(() => {
    window.EventSource = class {
      onopen?: () => void;
      constructor() { setTimeout(() => this.onopen?.(), 0); }
      close() {}
    } as unknown as typeof EventSource;
  });
  await page.route("**/api/**", async (route) => {
    const req = route.request();
    const url = new URL(req.url());
    const path = url.pathname;
    const body = req.postData() ? req.postDataJSON() : undefined;
    if (req.method() !== "GET") b.s.posts.push({ path, body });
    const { s } = b;
    if (path === "/api/overview")
      return route.fulfill({ json: { projects: [{ name: "web-games", description: "Browser games", repo: "repo/web-games", base: "main", start: "manual", allowed_models: [], allowed_grants: [] }], models: [], presets: [], runtimes: [], grants: [], skills: [], actions: [], node_types: [], planner: { model: "x" }, projectless: { allowed_models: [], allowed_grants: [] }, linear: false, local: true, operations: { as_of: 0, active: 0, waiting: 0, queued: 0, oldest_queued_age_ms: 0, nodes: [] } } });
    if (path === "/api/runs") return route.fulfill({ json: [] });
    if (path === "/api/config" && req.method() === "GET") return route.fulfill({ json: b.config });
    if (path === "/api/config") {
      s.configEdits.push(...body.edits);
      if (body.edits[0].yaml.includes("upstream: nowhere")) return route.fulfill({ status: 400, json: { error: 'config: model broken: unknown upstream "nowhere"' } });
      return route.fulfill({ json: { warnings: [] } });
    }
    if (path === "/api/repos") return route.fulfill({ json: b.repos });
    if (path === "/api/repos/link") return route.fulfill({ status: 201, json: { project: body.name } });
    if (path === "/api/projects/web-games/product") return route.fulfill({ json: b.product() });
    if (path === "/api/projects/web-games/files" && req.method() === "GET")
      return route.fulfill({ json: { path: url.searchParams.get("path"), content: "# Web games\n\nA **small** arcade. See [the site](https://games.example).\n\n- six games\n- no build step\n" } });
    if (path === "/api/projects/web-games/files") {
      s.dirty.push(body.path);
      return route.fulfill({ status: 204 });
    }
    if (path === "/api/projects/web-games/workspace/commit") {
      s.dirty = [];
      return route.fulfill({ json: { sha: "fff0001", url: "" } });
    }
    if (path === "/api/projects/web-games/maps/arcade") return route.fulfill({ json: b.mapView() });
    if (path === "/api/projects/web-games/maps/arcade/metrics")
      return route.fulfill({ json: [{ id: "weekly", value: 120, series: [[1, 80], [2, 95], [3, 120]] }, { id: "mvp-done", value: 33.3, detail: "1 of 3 user tasks done" }] });
    if (path === "/api/projects/web-games/maps/arcade/tasks") {
      for (const t of body.tasks as UserTask[]) {
        const i = s.tasks.findIndex((x) => x.id === t.id);
        if (i >= 0) s.tasks[i] = t;
        else s.tasks.push(t);
        s.dirty.push(`product/user-story-maps/arcade/tasks/${t.id}.yaml`);
      }
      s.tasks = s.tasks.filter((t) => !(body.delete ?? []).includes(t.id));
      return route.fulfill({ status: 204 });
    }
    if (path === "/api/projects/web-games/maps/arcade" && req.method() === "PUT") {
      s.map = body.map;
      return route.fulfill({ status: 204 });
    }
    if (path === "/api/projects/web-games/maps/arcade/send") return route.fulfill({ status: 201, json: { id: "t-1" } });
    if (path === "/api/projects/web-games/maps/arcade/chat" && req.method() === "GET") return route.fulfill({ json: s.chat });
    if (path === "/api/projects/web-games/maps/arcade/chat") {
      const answer: MapMessage = {
        role: "assistant",
        content: "The **Compete** activity has no MVP task. I added one.",
        changes: [{ path: "product/user-story-maps/arcade/tasks/share-score.yaml", content: "title: Share a score\nactivity: compete\nrelease: mvp\n" }],
        at: Date.now(),
      };
      s.chat.push({ role: "user", content: body.message, at: Date.now() }, answer);
      return route.fulfill({ json: answer });
    }
    if (path === "/api/projects/web-games/maps/arcade/apply") {
      s.tasks.push({ id: "share-score", title: "Share a score", activity: "compete", release: "mvp" });
      return route.fulfill({ json: { problems: [] } });
    }
    if (path === "/api/runs/r-1/retro" && req.method() === "GET") return route.fulfill({ json: { retro: s.retro } });
    if (path === "/api/runs/r-1/retro") {
      s.retro = {
        status: "done", model: "gpt-6.1-sol", started_at: Date.now() - 20_000, finished_at: Date.now(), note: body?.note,
        report: {
          summary: "The fix loop ran three times on the same failing test.",
          went_well: ["The failing test was written first."],
          problems: [{ node: "fix", evidence: "3 visits, same TestParse failure", impact: "$1.20 and 9 minutes" }],
          suggestions: [{ kind: "primitive", target: "loop history", change: "Hand the fixer every earlier test failure, not just the last.", why: "It repeated the same mistake.", priority: "high" }],
        },
      };
      return route.fulfill({ status: 202, json: { retro: s.retro } });
    }
    if (path === "/api/runs/r-1") {
      const view: RunView = { run: { id: "r-1", flow_name: "fix", flow_version: 1, status: "failed", branch: "ai-flow/fix", base: "main", diff: {}, cost_usd: 1.2, tokens: 9000, created_at: Date.now() - 600_000, started_at: Date.now() - 600_000, finished_at: Date.now() }, visits: [], events: [], graph: { start: "fix", nodes: [{ id: "fix", type: "agent", outcomes: ["done"] }], edges: [{ from: "fix", to: "$success", outcome: "done", kind: "next" }] } };
      return route.fulfill({ json: view });
    }
    return route.fulfill({ status: 404, json: { error: "unmocked " + path } });
  });
  return b.s;
}

async function noHorizontalScroll(page: Page) {
  await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
}

test("settings edits a catalog entry and shows validation errors", async ({ page }, info) => {
  const s = await setup(page);
  await page.goto("/settings");
  await expect(page.getByRole("heading", { name: "gpt-6.1-sol" })).toBeVisible();
  await page.getByRole("button", { name: "Edit gpt-6.1-sol" }).click();
  await expect(page.getByRole("dialog", { name: "Edit model gpt-6.1-sol" })).toBeVisible();
  await page.getByRole("button", { name: "Save" }).click();
  await expect.poll(() => s.configEdits.length).toBe(1);
  expect(s.configEdits[0]).toMatchObject({ section: "models", name: "gpt-6.1-sol" });

  await page.getByRole("button", { name: "New model" }).click();
  await page.getByLabel("Name").fill("broken");
  await page.locator(".cm-content").click();
  await page.keyboard.press("ControlOrMeta+a");
  await page.keyboard.type("upstream: nowhere\nmodel: x\n");
  await page.getByRole("button", { name: "Save" }).click();
  await expect(page.getByText('unknown upstream "nowhere"')).toBeVisible();
  await page.screenshot({ path: info.outputPath("settings-editor.png") });
  await page.getByRole("button", { name: "Cancel" }).click();

  await page.getByRole("tab", { name: /Connections/ }).click();
  await expect(page.getByText("token set").first()).toBeVisible();
  await noHorizontalScroll(page);
  await page.screenshot({ path: info.outputPath("settings-connections.png"), fullPage: true });
});

test("link a repository from the products page", async ({ page }, info) => {
  const s = await setup(page);
  await page.goto("/products");
  await page.getByRole("button", { name: "Link a repository" }).click();
  await expect(page.getByRole("option", { name: /o\/web-games/ })).toBeDisabled();
  await page.getByLabel("Filter repositories").fill("new");
  await page.getByRole("option", { name: /o\/new-thing/ }).click();
  await expect(page.getByLabel("Project name")).toHaveValue("new-thing");
  await page.screenshot({ path: info.outputPath("link-repo.png") });
  await page.getByRole("button", { name: "Link o/new-thing" }).click();
  await expect.poll(() => s.posts.find((p) => p.path === "/api/repos/link")?.body).toMatchObject({ full_name: "o/new-thing", name: "new-thing" });
  await expect(page).toHaveURL(/\/products\/new-thing$/);
});

test("product docs render, edit locally and commit", async ({ page }, info) => {
  const s = await setup(page);
  await page.goto("/products/web-games?tab=docs");
  await expect(page.locator(".markdown strong", { hasText: "small" })).toBeVisible();
  await expect(page.getByRole("link", { name: "the site" })).toHaveAttribute("href", "https://games.example");
  await page.getByRole("button", { name: "Edit" }).click();
  await page.getByLabel("Markdown").fill("# Web games\n\nUpdated.\n");
  await page.getByRole("button", { name: "Save" }).click();
  await expect(page.getByRole("button", { name: "Commit 1 change" })).toBeVisible();
  await noHorizontalScroll(page);
  await page.screenshot({ path: info.outputPath("product-docs.png"), fullPage: true });
  await page.getByRole("button", { name: "Commit 1 change" }).click();
  await expect(page.getByRole("dialog", { name: "Commit to main" })).toBeVisible();
  await page.screenshot({ path: info.outputPath("commit-dialog.png") });
  await page.getByRole("button", { name: "Commit & push" }).click();
  await expect.poll(() => s.posts.find((p) => p.path.endsWith("/workspace/commit"))?.body).toEqual({ message: "Update web-games product docs" });
  await expect(page.getByRole("button", { name: "No changes" })).toBeVisible();
});

test("story map: open a task, edit it, send it to a flow", async ({ page }, info) => {
  const s = await setup(page);
  await page.goto("/products/web-games/maps/arcade");
  const grid = page.getByRole("region", { name: "Story map" });
  await expect(grid.getByText("Browse games")).toBeVisible();
  await expect(grid.getByText("MVP", { exact: true })).toBeVisible();
  await expect(page.locator(".metric-value").first()).toHaveText("120");
  await expect(page.locator(".metric-value").nth(1)).toHaveText("33.3%");
  await page.screenshot({ path: info.outputPath("story-map.png"), fullPage: true });

  await grid.getByRole("button", { name: /Pause a game/ }).click();
  const panel = page.getByRole("complementary", { name: "User task Pause a game" });
  await panel.getByLabel("Story").fill("As a player I want to pause so that I can answer the door.");
  await panel.getByLabel("Acceptance criteria").fill("- Escape pauses\n- the timer stops");
  await panel.getByLabel("Weekly players").check();
  await panel.getByRole("button", { name: "Save" }).click();
  await expect.poll(() => s.posts.find((p) => p.path.endsWith("/tasks"))?.body.tasks[0]).toMatchObject({
    id: "pause", story: "As a player I want to pause so that I can answer the door.", acceptance: ["Escape pauses", "the timer stops"], metrics: ["weekly"],
  });
  await page.screenshot({ path: info.outputPath("story-map-task.png"), fullPage: true });

  await panel.getByRole("button", { name: "Send to flow" }).click();
  await expect(page.getByRole("dialog", { name: "Send user task to a flow" })).toBeVisible();
  await page.getByRole("button", { name: "Plan a flow" }).click();
  await expect.poll(() => s.posts.find((p) => p.path.endsWith("/send"))?.body).toEqual({ kind: "task", id: "pause", release: "", plan: true });
  await expect(page).toHaveURL(/\/\?task=t-1$/);
});

test("story map: add a task to a cell and send an activity by release", async ({ page }) => {
  const s = await setup(page);
  await page.goto("/products/web-games/maps/arcade");
  await page.getByRole("button", { name: "Add user task to Compete, MVP" }).click();
  const panel = page.getByRole("complementary", { name: "New user task" });
  await panel.getByLabel("Title").fill("Beat a friend's score");
  await panel.getByRole("button", { name: "Save" }).click();
  await expect.poll(() => s.posts.find((p) => p.path.endsWith("/tasks"))?.body.tasks[0]).toMatchObject({ id: "beat-a-friend-s-score", activity: "compete", release: "mvp", order: 1 });

  await page.getByRole("button", { name: "Close" }).click();
  await page.getByRole("button", { name: "Send activity Play a game to a flow" }).click();
  const dialog = page.getByRole("dialog", { name: "Send activity to a flow" });
  await expect(dialog.getByText("Covers 2 user tasks")).toBeVisible();
  await dialog.getByLabel("Release").selectOption("next");
  await expect(dialog.getByText("No open user tasks in this scope")).toBeVisible();
  await expect(dialog.getByRole("button", { name: "Plan a flow" })).toBeDisabled();
});

test("story map assistant proposes changes that apply locally", async ({ page }, info) => {
  const s = await setup(page);
  await page.goto("/products/web-games/maps/arcade");
  await page.getByRole("button", { name: "Assistant" }).click();
  const panel = page.getByRole("complementary", { name: "Map assistant" });
  await panel.getByLabel("Message").fill("What is missing from MVP?");
  await panel.getByRole("button", { name: "Send" }).click();
  await expect(panel.locator(".markdown strong", { hasText: "Compete" })).toBeVisible();
  await expect(panel.locator(".diff .add", { hasText: "title: Share a score" })).toBeVisible();
  await page.screenshot({ path: info.outputPath("assistant.png"), fullPage: true });
  await panel.getByRole("button", { name: "Apply" }).click();
  await expect(panel.getByRole("button", { name: "Applied" })).toBeDisabled();
  expect(s.posts.find((p) => p.path.endsWith("/apply"))?.body.changes[0].path).toBe("product/user-story-maps/arcade/tasks/share-score.yaml");
  await expect(page.getByRole("region", { name: "Story map" }).getByText("Share a score")).toBeVisible();
});

test("run review shows suggestions", async ({ page }, info) => {
  await setup(page);
  await page.goto("/runs/r-1");
  await page.getByRole("button", { name: "Review", exact: true }).click();
  await page.getByLabel("Focus").fill("why so many loops?");
  await page.getByRole("button", { name: "Review this run" }).click();
  await expect(page.getByText("Hand the fixer every earlier test failure")).toBeVisible();
  await expect(page.getByText("ai-flow primitive")).toBeVisible();
  await expect(page.getByText("3 visits, same TestParse failure")).toBeVisible();
  await page.screenshot({ path: info.outputPath("run-review.png"), fullPage: true });
});
