// Command compare puts each design frame next to the matching build
// screenshot (from shots.mjs) in one image, rendered by headless Chrome, so
// a reviewer can check them side by side. Same protocol client as smoke.mjs.
//
//	node tools/smoke/compare.mjs --design notes/xai-router-figma/pages --shots shots --out compare [--chrome path]
//
// Dark frames are named NN-*-dark.png and light frames LNN-*-light.png; a
// screenshot NN-name-<theme>.png is paired with the frame of the same NN and
// theme.
import { spawn } from 'node:child_process';
import { mkdtempSync, rmSync, existsSync, mkdirSync, readdirSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';

const args = Object.fromEntries(process.argv.slice(2).reduce((a, v, i, all) => (v.startsWith('--') ? [...a, [v.slice(2), all[i + 1]]] : a), []));
const design = resolve(args.design || 'design'), shots = resolve(args.shots || 'shots'), out = resolve(args.out || 'compare');
const candidates = [args.chrome, process.env.CHROME, '/usr/bin/google-chrome', '/usr/bin/chromium',
  'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe', 'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe'];
const chrome = candidates.find((p) => p && existsSync(p));
if (!chrome) { console.error('compare: no Chrome/Edge found'); process.exit(2); }
mkdirSync(out, { recursive: true });

const frames = readdirSync(design);
const pairs = [];
for (const f of readdirSync(shots).filter((f) => f.endsWith('.png')).sort()) {
  const m = /^(\d\d)-.*-(dark|light)\.png$/.exec(f);
  if (!m) continue;
  const [, nn, theme] = m;
  const frame = frames.find((d) => (theme === 'dark' ? new RegExp(`^${nn}-.*-dark\\.png$`) : new RegExp(`^L${nn}-.*-light\\.png$`)).test(d));
  if (frame) pairs.push({ shot: f, frame, theme });
  else console.log(`(no ${theme} frame for ${f})`);
}

const dataURL = (p) => 'data:image/png;base64,' + readFileSync(p).toString('base64');
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const profile = mkdtempSync(join(tmpdir(), 'xr-compare-'));
const port = 10300 + Math.floor(Math.random() * 500);
const proc = spawn(chrome, ['--headless=new', '--disable-gpu', '--no-first-run', `--user-data-dir=${profile}`,
  `--remote-debugging-port=${port}`, '--hide-scrollbars', 'about:blank'], { stdio: 'ignore' });
let ws; let seq = 0; const pending = new Map();
const send = (method, params = {}) => new Promise((res, rej) => { const id = ++seq; pending.set(id, { res, rej }); ws.send(JSON.stringify({ id, method, params })); });
const done = (code) => { try { ws && ws.close(); } catch { /* ignore */ } proc.kill(); setTimeout(() => { try { rmSync(profile, { recursive: true, force: true }); } catch { /* ignore */ } process.exit(code); }, 500); };
setTimeout(() => { console.error('compare: timed out'); done(1); }, 300_000);

try {
  let target;
  for (let i = 0; i < 50 && !target; i++) {
    await sleep(200);
    try { target = (await (await fetch(`http://127.0.0.1:${port}/json`)).json()).find((t) => t.type === 'page'); } catch { /* starting */ }
  }
  ws = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
  ws.onmessage = (m) => { const msg = JSON.parse(m.data); if (msg.id && pending.has(msg.id)) { const p = pending.get(msg.id); pending.delete(msg.id); msg.error ? p.rej(new Error(msg.error.message)) : p.res(msg.result); } };
  await send('Page.enable');
  await send('Emulation.setDeviceMetricsOverride', { width: 2920, height: 960, deviceScaleFactor: 1, mobile: false });
  for (const p of pairs) {
    const bg = p.theme === 'dark' ? '#1b1d21' : '#e9ebee', fg = p.theme === 'dark' ? '#c9ced6' : '#3a3f47';
    const html = `<!doctype html><html><head><meta charset="utf-8"></head><body style="margin:0;background:${bg};font:13px system-ui;color:${fg}">
<div style="display:flex;gap:20px;padding:10px 20px 20px">
<figure style="margin:0"><figcaption style="padding:4px 0 6px">Design · ${p.frame}</figcaption><img src="${dataURL(join(design, p.frame))}" style="width:1440px;display:block"></figure>
<figure style="margin:0"><figcaption style="padding:4px 0 6px">Build · ${p.shot}</figcaption><img src="${dataURL(join(shots, p.shot))}" style="width:1440px;display:block"></figure>
</div></body></html>`;
    await send('Page.navigate', { url: 'data:text/html;base64,' + Buffer.from(html).toString('base64') });
    await sleep(700);
    const shot = await send('Page.captureScreenshot', { format: 'png', captureBeyondViewport: true });
    writeFileSync(join(out, p.shot), Buffer.from(shot.data, 'base64'));
    console.log(p.shot, '<-', p.frame);
  }
  console.log(`compare: ${pairs.length} pairs`);
  done(0);
} catch (e) {
  console.error('compare:', e.message);
  done(1);
}
