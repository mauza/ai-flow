// browse: drive headless Chromium from the shell for QA.
//
//   browse <url> [--viewport 390x844] [--wait 1500] [--shot out.png]
//          [--tap x,y]... [--click <selector>]... [--key <key>]...
//          [--eval <js>] [--steps steps.json]
//
// Loads the page, runs the actions in order (each followed by --wait ms),
// and prints one JSON report: title, final URL, console errors and warnings,
// page errors, failed requests, the --eval result and the screenshot path.
// Exits 1 when the page threw or logged console errors.
//
// --steps takes a JSON array of actions for longer scripts:
//   [{"tap":[195,600]}, {"wait":500}, {"key":"Space"}, {"click":"#restart"},
//    {"eval":"window.game?.score"}, {"shot":"after.png"}]
import { chromium } from 'playwright-core';
import { accessSync, constants, mkdtempSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const args = process.argv.slice(2);
if (!args.length || args[0] === '-h' || args[0] === '--help') {
  console.log(readFileSync(new URL(import.meta.url)).toString().split('\n').filter((l) => l.startsWith('//')).map((l) => l.slice(3)).join('\n'));
  process.exit(args.length ? 0 : 2);
}
const url = args.shift();
let viewport = { width: 390, height: 844 };
let wait = 1000;
const steps = [];
let finalShot = null;
for (let i = 0; i < args.length; i++) {
  const a = args[i], v = args[i + 1];
  switch (a) {
    case '--viewport': { const [w, h] = v.split('x').map(Number); viewport = { width: w, height: h }; i++; break; }
    case '--wait': wait = Number(v); i++; break;
    case '--shot': finalShot = v; i++; break;
    case '--tap': steps.push({ tap: v.split(',').map(Number) }); i++; break;
    case '--click': steps.push({ click: v }); i++; break;
    case '--key': steps.push({ key: v }); i++; break;
    case '--eval': steps.push({ eval: v }); i++; break;
    case '--steps': steps.push(...JSON.parse(readFileSync(v, 'utf8'))); i++; break;
    default: console.error(`unknown argument ${a}`); process.exit(2);
  }
}

const report = { url, console: [], pageErrors: [], failedRequests: [], evals: [], shots: [] };
// Chromium writes its profile and crash database under HOME and refuses to
// start (crashpad "--database is required") when HOME is not writable.
let home = process.env.HOME || '';
try { accessSync(home, constants.W_OK); } catch { home = mkdtempSync(join(tmpdir(), 'browse-home-')); }
const browser = await chromium.launch({
  env: { ...process.env, HOME: home },
  executablePath: process.env.CHROMIUM_PATH || '/usr/bin/chromium',
  args: ['--no-sandbox', '--disable-dev-shm-usage'],
});
try {
  const page = await browser.newPage({ viewport, hasTouch: true, isMobile: viewport.width < 600 });
  page.on('console', (m) => { if (m.type() === 'error' || m.type() === 'warning') report.console.push(`${m.type()}: ${m.text()}`); });
  page.on('pageerror', (e) => report.pageErrors.push(String(e)));
  page.on('requestfailed', (r) => report.failedRequests.push(`${r.url()} ${r.failure()?.errorText}`));
  const res = await page.goto(url, { waitUntil: 'load', timeout: 30000 });
  report.status = res?.status();
  await page.waitForTimeout(wait);
  for (const s of steps) {
    if (s.tap) await page.touchscreen.tap(s.tap[0], s.tap[1]);
    else if (s.click) await page.click(s.click, { timeout: 5000 });
    else if (s.key) await page.keyboard.press(s.key);
    else if (s.eval) report.evals.push(await page.evaluate(s.eval).catch((e) => `error: ${e.message}`));
    else if (s.shot) { await page.screenshot({ path: s.shot }); report.shots.push(s.shot); }
    else if (s.wait) { await page.waitForTimeout(s.wait); continue; }
    await page.waitForTimeout(s.wait ?? wait);
  }
  report.title = await page.title();
  report.finalUrl = page.url();
  if (finalShot) { await page.screenshot({ path: finalShot }); report.shots.push(finalShot); }
} finally {
  await browser.close();
}
console.log(JSON.stringify(report, null, 2));
process.exit(report.pageErrors.length || report.console.some((c) => c.startsWith('error')) ? 1 : 0);
