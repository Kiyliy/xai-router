// Command shots signs in to the console in headless Chrome and saves a
// 1440x900 screenshot of every page and drawer, in the dark and light
// themes, for side-by-side review against the design. Same protocol client
// as smoke.mjs (no npm packages; Node 22+).
//
//	node tools/smoke/shots.mjs --base http://127.0.0.1:18143 --password … --out dir [--chrome path]
import { spawn } from 'node:child_process';
import { mkdtempSync, rmSync, existsSync, mkdirSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const args = Object.fromEntries(process.argv.slice(2).reduce((a, v, i, all) => (v.startsWith('--') ? [...a, [v.slice(2), all[i + 1]]] : a), []));
const base = (args.base || 'http://127.0.0.1:18080').replace(/\/$/, '');
const out = args.out || 'shots';
const password = args.password || process.env.XR_ADMIN_PASSWORD;
const candidates = [args.chrome, process.env.CHROME, '/usr/bin/google-chrome', '/usr/bin/chromium',
  'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe', 'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe'];
const chrome = candidates.find((p) => p && existsSync(p));
if (!password || !chrome) { console.error('shots: need --password and Chrome/Edge'); process.exit(2); }
mkdirSync(out, { recursive: true });

// name, hash (a function of ids looked up after sign-in), optional script run after load
const shots = [
  ['03-overview', () => '#/overview'],
  ['04-keys', () => '#/keys'],
  ['05-key-drawer', (ids) => `#/keys?open=${ids.key}`],
  ['07-tokens', () => '#/tokens'],
  ['08-token-drawer', (ids) => `#/tokens?open=${ids.token}`],
  ['10-adjust-quota', (ids) => `#/tokens?open=${ids.token}&adjust=1`],
  ['10-adjust-quota-increase', (ids) => `#/tokens?open=${ids.token}&adjust=1`, "(async () => {\n    const m = [...document.querySelectorAll('.modal, [role=dialog], dialog')].pop(); if (!m) return;\n    const b = [...m.querySelectorAll('button, [role=tab]')].find((x) => x.textContent.trim() === '增加'); if (b) b.click();\n    await new Promise((r) => setTimeout(r, 200));\n    const set = (el, v) => { Object.getOwnPropertyDescriptor(Object.getPrototypeOf(el), 'value').set.call(el, v); el.dispatchEvent(new Event('input', { bubbles: true })); };\n    const amt = m.querySelector('input'); if (amt) set(amt, '5');\n    const why = m.querySelector('textarea'); if (why) set(why, '月度充值');\n  })()"],
  ['11-adjust-quota-decrease', (ids) => `#/tokens?open=${ids.token}&adjust=1`, "(async () => {\n    const m = [...document.querySelectorAll('.modal, [role=dialog], dialog')].pop(); if (!m) return;\n    const b = [...m.querySelectorAll('button, [role=tab]')].find((x) => x.textContent.trim() === '减少'); if (b) b.click();\n    await new Promise((r) => setTimeout(r, 200));\n    const set = (el, v) => { Object.getOwnPropertyDescriptor(Object.getPrototypeOf(el), 'value').set.call(el, v); el.dispatchEvent(new Event('input', { bubbles: true })); };\n    const amt = m.querySelector('input'); if (amt) set(amt, '2');\n    const why = m.querySelector('textarea'); if (why) set(why, '月度充值');\n  })()"],
  ['12-logs-mixed', () => '#/logs?type=request,audit,system'],
  ['13-logs-requests', () => '#/logs?type=request'],
  ['12-logs-mixed-bottom', () => '#/logs?type=request,audit,system', 'window.scrollTo(0, document.body.scrollHeight)'],
  ['13-logs-requests-bottom', () => '#/logs?type=request', 'window.scrollTo(0, document.body.scrollHeight)'],
  ['14-log-detail-request', (ids) => `#/logs?type=request&open=${ids.log}`],
  ['16-log-detail-audit', (ids) => `#/logs?type=audit,system&event=${ids.event}`],
  ['17-models', () => '#/models'],
  ['19-admin-access', () => '#/access'],
  ['20-settings', () => '#/settings'],
  ['21-data-storage', () => '#/data'],
  ['22-docs-guide', () => '#/docs'],
  ['23-docs-admin-api', () => '#/docs?tab=admin'],
];

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const profile = mkdtempSync(join(tmpdir(), 'xr-shots-'));
const port = 9800 + Math.floor(Math.random() * 500);
const proc = spawn(chrome, ['--headless=new', '--disable-gpu', '--no-first-run', `--user-data-dir=${profile}`,
  `--remote-debugging-port=${port}`, '--hide-scrollbars', 'about:blank'], { stdio: 'ignore' });
const deadline = setTimeout(() => { console.error('shots: timed out'); done(1); }, 300_000);
let ws; let seq = 0; const pending = new Map(); const errors = [];
function done(code) {
  clearTimeout(deadline); try { ws && ws.close(); } catch { /* ignore */ } proc.kill();
  setTimeout(() => { try { rmSync(profile, { recursive: true, force: true }); } catch { /* ignore */ } process.exit(code); }, 500);
}
const send = (method, params = {}) => new Promise((resolve, reject) => {
  const id = ++seq; pending.set(id, { resolve, reject }); ws.send(JSON.stringify({ id, method, params }));
});
const evaluate = async (expression) => {
  const r = await send('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true });
  if (r.exceptionDetails) throw new Error(r.exceptionDetails.exception?.description || r.exceptionDetails.text);
  return r.result.value;
};

try {
  let target;
  for (let i = 0; i < 50 && !target; i++) {
    await sleep(200);
    try { target = (await (await fetch(`http://127.0.0.1:${port}/json`)).json()).find((t) => t.type === 'page'); } catch { /* starting */ }
  }
  ws = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
  let where = 'load';
  ws.onmessage = (m) => {
    const msg = JSON.parse(m.data);
    if (msg.id && pending.has(msg.id)) { const p = pending.get(msg.id); pending.delete(msg.id); msg.error ? p.reject(new Error(msg.error.message)) : p.resolve(msg.result); return; }
    if (msg.method === 'Runtime.exceptionThrown') errors.push(`${where}: ${msg.params.exceptionDetails.exception?.description || msg.params.exceptionDetails.text}`);
    if (msg.method === 'Runtime.consoleAPICalled' && msg.params.type === 'error') errors.push(`${where}: console.error ${msg.params.args.map((a) => a.value ?? a.description).join(' ')}`);
  };
  await send('Runtime.enable'); await send('Page.enable');
  await send('Emulation.setDeviceMetricsOverride', { width: 1440, height: 900, deviceScaleFactor: 1, mobile: false });
  await send('Page.navigate', { url: `${base}/#/overview` });
  await sleep(2500);
  await evaluate(`(async () => {
    const el = document.querySelector('input[type=password]');
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(el, ${JSON.stringify(password)});
    el.dispatchEvent(new Event('input', { bubbles: true })); el.form.requestSubmit();
    await new Promise((r) => setTimeout(r, 2000));
  })()`);
  const ids = await evaluate(`(async () => {
    const h = { Authorization: 'Bearer ' + localStorage.getItem('xr.token') };
    const j = async (p) => (await fetch(p, { headers: h })).json();
    const first = async (p) => ((await j(p)).data || [])[0] || {};
    return { key: (await first('/api/v1/keys?page_size=1')).id, token: (((await j('/api/v1/tokens?page_size=200')).data || []).find((t) => !t.unlimited) || (await first('/api/v1/tokens?page_size=1'))).id,
      log: (await first('/api/v1/logs?type=request&page_size=1')).id,
      event: (await first('/api/v1/logs?type=audit&action=token.quota_&page_size=1')).id || (await first('/api/v1/logs?type=audit,system&page_size=1')).id };
  })()`);
  for (const theme of ['dark', 'light']) {
    await evaluate(`localStorage.setItem('xr.theme', '${theme}')`);
    for (const [name, hash, after] of shots) {
      where = `${theme} ${name}`;
      await send('Page.navigate', { url: `${base}/?t=${Date.now()}${hash(ids)}` });
      await sleep(2200);
      // Wait out loading spinners and skeletons (dialogs open after their data loads).
      for (let i = 0; i < 20 && await evaluate(`!!document.querySelector('.spinner, .skeleton, .loading')`); i++) await sleep(300);
      if (after) { await evaluate(after); await sleep(600); }
      const shot = await send('Page.captureScreenshot', { format: 'png' });
      writeFileSync(join(out, `${name}-${theme}.png`), Buffer.from(shot.data, 'base64'));
      console.log(`${name}-${theme}.png`);
    }
  }
} catch (e) {
  errors.push(`fatal: ${e.message}`);
}
if (errors.length) { console.error(`shots: ${errors.length} error(s)`); errors.forEach((e) => console.error('  ' + e)); done(1); } else { console.log('shots: ok'); done(0); }
