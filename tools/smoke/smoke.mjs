// Command smoke opens the console in headless Chrome, signs in and visits
// every page, failing on any uncaught exception, console error or failed
// script. It talks to Chrome over the DevTools protocol, so it needs no npm
// packages (Node 22+ for the global WebSocket).
//
//	node tools/smoke/smoke.mjs --base http://127.0.0.1:18080 --password … [--chrome path]
import { spawn } from 'node:child_process';
import { mkdtempSync, rmSync, existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const args = Object.fromEntries(process.argv.slice(2).reduce((a, v, i, all) => (v.startsWith('--') ? [...a, [v.slice(2), all[i + 1]]] : a), []));
const base = (args.base || 'http://127.0.0.1:18080').replace(/\/$/, '');
const password = args.password || process.env.XR_ADMIN_PASSWORD;
const routes = ['#/overview', '#/keys', '#/tokens', '#/models', '#/logs', '#/logs?type=request', '#/settings', '#/data', '#/access', '#/docs', '#/docs?tab=admin'];
const candidates = [args.chrome, process.env.CHROME, '/usr/bin/google-chrome', '/usr/bin/chromium', '/usr/bin/chromium-browser',
  'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe', 'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe'];
const chrome = candidates.find((p) => p && existsSync(p));
if (!password || !chrome) {
  console.error(!password ? 'smoke: --password (or XR_ADMIN_PASSWORD) is required' : 'smoke: no Chrome/Edge found (--chrome)');
  process.exit(2);
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const profile = mkdtempSync(join(tmpdir(), 'xr-smoke-'));
const port = 9300 + Math.floor(Math.random() * 500);
const proc = spawn(chrome, ['--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check', `--user-data-dir=${profile}`,
  `--remote-debugging-port=${port}`, '--window-size=1440,900', 'about:blank'], { stdio: 'ignore' });
const deadline = setTimeout(() => { console.error('smoke: timed out'); finish(1); }, 120_000);

let ws; let seq = 0; const pending = new Map(); const problems = [];
function finish(code) {
  clearTimeout(deadline);
  try { ws && ws.close(); } catch { /* ignore */ }
  proc.kill();
  setTimeout(() => { try { rmSync(profile, { recursive: true, force: true }); } catch { /* ignore */ } process.exit(code); }, 500);
}
const send = (method, params = {}) => new Promise((resolve, reject) => {
  const id = ++seq;
  pending.set(id, { resolve, reject });
  ws.send(JSON.stringify({ id, method, params }));
});
const evaluate = async (expression) => {
  const r = await send('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true });
  if (r.exceptionDetails) throw new Error(r.exceptionDetails.exception?.description || r.exceptionDetails.text);
  return r.result.value;
};

try {
  let target;
  for (let i = 0; i < 150 && !target; i++) { // up to 30 s: CI runners can be slow to start Chrome
    await sleep(200);
    try { target = (await (await fetch(`http://127.0.0.1:${port}/json`)).json()).find((t) => t.type === 'page'); } catch { /* not up yet */ }
  }
  if (!target) throw new Error('Chrome did not start');
  ws = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
  let where = 'load';
  ws.onmessage = (m) => {
    const msg = JSON.parse(m.data);
    if (msg.id && pending.has(msg.id)) { const p = pending.get(msg.id); pending.delete(msg.id); msg.error ? p.reject(new Error(msg.error.message)) : p.resolve(msg.result); return; }
    if (msg.method === 'Runtime.exceptionThrown') problems.push(`${where}: exception ${msg.params.exceptionDetails.exception?.description || msg.params.exceptionDetails.text}`);
    if (msg.method === 'Runtime.consoleAPICalled' && msg.params.type === 'error') problems.push(`${where}: console.error ${msg.params.args.map((a) => a.value ?? a.description).join(' ')}`);
    if (msg.method === 'Log.entryAdded' && msg.params.entry.level === 'error' && !/favicon/.test(msg.params.entry.url || '')) problems.push(`${where}: ${msg.params.entry.text} ${msg.params.entry.url || ''}`);
  };
  await send('Runtime.enable'); await send('Log.enable'); await send('Page.enable');
  await send('Page.navigate', { url: `${base}/#/overview` });
  await sleep(2500);
  const hasLogin = await evaluate(`!!document.querySelector('input[type=password]')`);
  if (!hasLogin) throw new Error('the login form did not render (the console failed to load)');
  await evaluate(`(async () => {
    const el = document.querySelector('input[type=password]');
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(el, ${JSON.stringify(password)});
    el.dispatchEvent(new Event('input', { bubbles: true }));
    el.form.requestSubmit();
    await new Promise((r) => setTimeout(r, 2000));
  })()`);
  if (await evaluate(`!!document.querySelector('input[type=password]')`)) throw new Error('sign-in failed');
  for (const r of routes) {
    where = r;
    await evaluate(`location.hash = ${JSON.stringify(r)}`);
    await sleep(1500);
    const n = await evaluate(`(document.querySelector('main') || document.body).innerText.trim().length`);
    console.log(`${r.padEnd(22)} ${n} chars`);
    if (n < 20) problems.push(`${r}: page is empty`);
  }
} catch (e) {
  problems.push(`fatal: ${e.message}`);
}
if (problems.length) {
  console.error(`smoke: ${problems.length} problem(s)`);
  for (const p of problems) console.error('  ' + p);
  finish(1);
} else {
  console.log('smoke: ok');
  finish(0);
}
