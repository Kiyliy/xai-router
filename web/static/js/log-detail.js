// Detail views of the unified log (v0.4.1, pm-spec 9.5): the request drawer (summary → tokens &
// cost → upstream attempts → request → response) and the audit / system event drawer, plus the
// body / JSON / SSE viewers they use.
import {
  h, fmtInt, fmtUSD, fmtMs, fmtCompact, fmtBytes, fmtDateTime, relTime, tryParseJSON, highlightJSON,
  statusClass, copyText, downloadText, utf8Bytes,
} from './util.js';
import { icon } from './icons.js';
import { t } from './i18n.js';
import { get } from './api.js';
import { toast, toastError, badge, copyButton, busy, tabs, kv, statusCode, statusDot, bigNum, menuButton } from './ui.js';
import { cred } from './detail-drawer.js';
import {
  actionLabel, actorLabel, targetTypeLabel, targetText, targetHint, eventSentence, levelBadge, requestReason, tokenCats, usdT,
  fieldLabel, fmtChangeValue, attemptWhy, isChainRow, isRetriedRow,
} from './events.js';

// Styles of the log page and drawers; linked here too in case index.html does not link them yet.
if (!document.querySelector('link[href$="logs-v041.css"]')) {
  document.head.appendChild(h('link', { rel: 'stylesheet', href: new URL('../logs-v041.css', import.meta.url).href }));
}


export function statusBadge(code) {
  return badge(code ? String(code) : t('lg.failed'), statusClass(code), { class: 'badge-status mono' });
}
export function methodTag(m) { return h('span', { class: `method method-${(m || 'GET').toLowerCase()}` }, m || 'GET'); }

// ---------- detail view ----------
function emptyPane(label) { return h('div', { class: 'pane-empty' }, icon('inbox', 18), h('span', null, label)); }

/** "Expand" toggle for a bounded code pane: collapsed panes scroll inside, expanded ones grow with the drawer. */
function expandToggle(pre) {
  const btn = h('button', { type: 'button', class: 'pane-tool', 'aria-pressed': 'false' });
  const paint = () => {
    const on = pre.classList.contains('expanded');
    btn.replaceChildren(icon(on ? 'shrink' : 'expand', 12), h('span', null, on ? t('logs.collapse') : t('logs.expand')));
    btn.setAttribute('aria-pressed', on ? 'true' : 'false');
  };
  btn.addEventListener('click', () => { pre.classList.toggle('expanded'); paint(); });
  paint();
  // Hide the toggle when the content already fits.
  requestAnimationFrame(() => { if (pre.scrollHeight <= pre.clientHeight + 2 && !pre.classList.contains('expanded')) btn.hidden = true; });
  return btn;
}

// Bodies can be megabytes. Never build per-token DOM for more than HL_LIMIT characters;
// beyond VIEW_LIMIT show a plain-text preview with "Show all" / "Download".
const HL_LIMIT = 64 * 1024;
const VIEW_LIMIT = 64 * 1024;
const CHUNK = 32 * 1024;

/** Fill `el` with plain text. Long text is split into blocks the browser can skip laying out while off-screen. */
function paintPlain(el, text) {
  if (text.length <= CHUNK * 2) { el.textContent = text; return; }
  const frag = document.createDocumentFragment();
  for (let i = 0; i < text.length;) {
    let end = Math.min(text.length, i + CHUNK);
    if (end < text.length) { const nl = text.lastIndexOf('\n', end); if (nl > i + CHUNK / 2) end = nl + 1; }
    const piece = text.slice(i, end);
    let lines = 0;
    for (let j = piece.indexOf('\n'); j >= 0; j = piece.indexOf('\n', j + 1)) lines++;
    const est = Math.max(lines, piece.length / 110) * 19.2;
    frag.appendChild(h('span', { class: 'code-chunk', style: { containIntrinsicSize: `auto ${Math.round(est)}px` } }, piece));
    i = end;
  }
  el.replaceChildren(frag);
}

function downloadBody(text, filename) {
  downloadText(filename, text, /\.json$/.test(filename) ? 'application/json' : 'text/plain');
}
function downloadTool(getText, filename) {
  return h('button', { type: 'button', class: 'pane-tool', title: t('logs.downloadHint'), onclick: () => downloadBody(getText(), filename) },
    icon('download', 12), h('span', null, t('logs.download')));
}

/** Bar above a preview: "Showing 64 KB of 1.2 MB — Show all · Download". */
function limitBar({ shownBytes, totalBytes, all, onToggle, onDownload }) {
  return h('div', { class: 'pane-limit', role: 'status' },
    icon('info', 13),
    h('span', null, all ? t('logs.showingAll', { total: fmtBytes(totalBytes) }) : t('logs.showingPart', { shown: fmtBytes(shownBytes), total: fmtBytes(totalBytes) })),
    h('span', { class: 'pane-meta-spacer' }),
    h('button', { type: 'button', class: 'pane-tool', onclick: onToggle }, icon(all ? 'shrink' : 'expand', 12), h('span', null, all ? t('logs.showLess') : t('logs.showAll'))),
    h('button', { type: 'button', class: 'pane-tool', onclick: onDownload }, icon('download', 12), h('span', null, t('logs.download'))));
}

/** Preview state shared by the body / SSE / text views. */
function previewer(el, getText, filename) {
  let showAll = false;
  const slot = h('div');
  const paint = () => {
    const text = getText();
    const over = text.length > VIEW_LIMIT;
    paintPlain(el, showAll || !over ? text : text.slice(0, VIEW_LIMIT));
    if (!over) { slot.replaceChildren(); return; }
    slot.replaceChildren(limitBar({
      shownBytes: utf8Bytes(text.slice(0, VIEW_LIMIT)), totalBytes: utf8Bytes(text), all: showAll,
      onToggle: () => { showAll = !showAll; paint(); }, onDownload: () => downloadBody(text, filename),
    }));
  };
  return { slot, paint };
}


let sseFormatPref = (() => { try { return localStorage.getItem('xr.logs.sseFmt') === '1'; } catch { return false; } })();

function sseView(text, { filename = 'response.sse.txt' } = {}) {
  if (!text) return emptyPane(t('logs.noBody'));
  const pre = h('pre', { class: 'code code-body code-sse' });
  const lines = text.length > HL_LIMIT ? null : text.split('\n');
  const events = lines ? lines.filter((l) => l.startsWith('data:')).length : 0;
  const size = h('span', null, lines ? t('logs.eventsN', { n: events }) : fmtBytes(utf8Bytes(text)));
  if (!lines) {
    // Large streams: plain text only (no per-line spans), preview first.
    const prev = previewer(pre, () => text, filename);
    prev.paint();
    return h('div', { class: 'pane-content' },
      h('div', { class: 'pane-meta' }, h('span', null, t('logs.rawSse')), size, h('span', { class: 'pane-meta-spacer' }), downloadTool(() => text, filename), expandToggle(pre)),
      prev.slot, pre);
  }
  const paint = () => {
    pre.replaceChildren();
    lines.forEach((line, i) => {
      const m = /^(data|event|id|retry):\s?/.exec(line);
      if (m) {
        pre.appendChild(h('span', { class: 'sse-field' }, m[0]));
        const rest = line.slice(m[0].length);
        const parsed = sseFormatPref && m[1] === 'data' ? tryParseJSON(rest) : { ok: false };
        if (parsed.ok) pre.appendChild(h('span', { class: 'sse-json' }, highlightJSON(parsed.value)));
        else pre.appendChild(h('span', { class: rest === '[DONE]' ? 'sse-done' : 'sse-data' }, rest));
      } else if (line.startsWith(':')) pre.appendChild(h('span', { class: 'sse-comment' }, line));
      else pre.appendChild(document.createTextNode(line));
      if (i < lines.length - 1) pre.appendChild(document.createTextNode('\n'));
    });
  };
  paint();
  const fmtBtn = h('button', { type: 'button', class: `pane-tool ${sseFormatPref ? 'on' : ''}`, 'aria-pressed': sseFormatPref ? 'true' : 'false', title: t('logs.formatEventsHint') },
    icon('braces', 12), h('span', null, t('logs.formatEvents')));
  fmtBtn.addEventListener('click', () => {
    sseFormatPref = !sseFormatPref;
    try { localStorage.setItem('xr.logs.sseFmt', sseFormatPref ? '1' : '0'); } catch { /* ignore */ }
    fmtBtn.classList.toggle('on', sseFormatPref); fmtBtn.setAttribute('aria-pressed', sseFormatPref ? 'true' : 'false');
    paint();
  });
  return h('div', { class: 'pane-content' }, h('div', { class: 'pane-meta' }, h('span', null, t('logs.rawSse')), size,
    h('span', { class: 'pane-meta-spacer' }), fmtBtn, downloadTool(() => text, filename), expandToggle(pre)), pre);
}

/** Text block that previews the first VIEW_LIMIT characters of very long text. */
function longText(text, cls, filename) {
  const box = h('div', { class: cls });
  if (text.length <= VIEW_LIMIT) { box.textContent = text; return box; }
  const prev = previewer(box, () => text, filename);
  prev.paint();
  return h('div', null, prev.slot, box);
}

function textView(d) {
  if (!d.response_text && !d.reasoning_text) return emptyPane(t('logs.noText'));
  return h('div', { class: 'pane-content text-pane' },
    d.reasoning_text ? h('details', { class: 'reasoning', open: !d.response_text },
      h('summary', null, icon('layers', 14), t('logs.reasoning'), h('span', { class: 'muted small' }, ` · ${fmtCompact(d.reasoning_text.length)} ${t('logs.chars')}`)),
      longText(d.reasoning_text, 'reasoning-text', `log-${d.id}-reasoning.txt`)) : null,
    d.response_text ? longText(d.response_text, 'assistant-text', `log-${d.id}-text.txt`) : h('div', { class: 'muted small' }, t('logs.noText')));
}

function headersView(obj) {
  const entries = Object.entries(obj || {}).sort((a, b) => a[0].localeCompare(b[0]));
  if (!entries.length) return emptyPane(t('logs.noHeaders'));
  return h('div', { class: 'pane-content' }, h('table', { class: 'headers-table' },
    h('tbody', null, entries.map(([k, v]) => h('tr', null, h('th', { class: 'mono' }, k), h('td', { class: 'mono' }, Array.isArray(v) ? v.join(', ') : String(v)))))));
}

function pane(title, tabItems, copyGetter, copyLabel) {
  return h('div', { class: 'pane' },
    h('div', { class: 'pane-title' }, h('span', null, title), copyGetter ? copyButton(copyGetter, { label: copyLabel }) : null),
    tabs(tabItems, { cls: 'tabs-sm' }));
}

async function copyCurl(id, btn) {
  await busy(btn, async () => {
    try {
      const r = await get(`/api/v1/logs/${id}/curl`, { base_url: location.origin });
      const ok = await copyText(r.curl || '');
      toast(ok ? t('logs.curlCopied') : t('common.copyFailed'), ok ? 'success' : 'error');
    } catch (e) { toastError(e); }
  });
}

function wsSessionView(s) {
  const dir = (msgs, bytes) => `${fmtInt(msgs || 0)} ${t('logs.wsMsgs')} · ${fmtBytes(bytes || 0)}`;
  return h('div', { class: 'detail-block' }, h('h4', null, t('logs.wsSession')), kv([
    [t('logs.wsDuration'), fmtMs(s.duration_ms || 0)],
    [t('logs.wsClient'), dir(s.client_messages, s.client_bytes)],
    [t('logs.wsUpstream'), dir(s.server_messages, s.server_bytes)],
    [t('logs.wsClosedBy'), s.closed_by || '—'],
    s.close_code ? [t('logs.wsClose'), h('span', { class: 'mono' }, `${s.close_code}${s.close_reason ? ` · ${s.close_reason}` : ''}`)] : null,
    s.subprotocol ? [t('logs.wsSubprotocol'), h('span', { class: 'mono small break' }, s.subprotocol.replace(/xai-client-secret\.\S+/, 'xai-client-secret.…'))] : null,
    s.text_inputs ? [t('logs.wsTextInputs'), fmtInt(s.text_inputs)] : null,
    s.tts_chars ? [t('logs.wsTtsChars'), fmtInt(s.tts_chars)] : null,
    s.audio_seconds ? [t('logs.wsAudio'), t('lg.seconds', { n: s.audio_seconds.toFixed(1) })] : null,
    [t('logs.wsLogged'), fmtInt(s.logged_messages || 0)],
  ]));
}


// ---------- JSON viewer: tree (formatted) or raw text, fold to level 2, wrap, line cap ----------
const LINE_CAP = 200;
const lsGet = (k) => { try { return localStorage.getItem(k); } catch { return null; } };
const lsSet = (k, v) => { try { localStorage.setItem(k, v); } catch { /* ignore */ } };
let prettyPref = lsGet('xr.logs.pretty') !== '0';
let wrapPref = lsGet('xr.logs.wrap') !== '0';

function jPrim(v) {
  if (v === null) return h('span', { class: 'j-null' }, 'null');
  if (typeof v === 'string') return h('span', { class: 'j-str' }, JSON.stringify(v));
  if (typeof v === 'boolean') return h('span', { class: 'j-bool' }, String(v));
  return h('span', { class: 'j-num' }, String(v));
}
function jtNode(key, val, depth, last, foldDepth) {
  const keyEl = key != null ? [h('span', { class: 'j-key' }, JSON.stringify(key)), ': '] : [];
  const comma = last ? '' : ',';
  if (val === null || typeof val !== 'object') return h('div', { class: 'jt-line' }, keyEl, jPrim(val), comma);
  const isArr = Array.isArray(val);
  const entries = isArr ? val.map((v) => [null, v]) : Object.entries(val);
  const [o, c] = isArr ? ['[', ']'] : ['{', '}'];
  if (!entries.length) return h('div', { class: 'jt-line' }, keyEl, o + c + comma);
  const det = h('details', { class: 'jt', open: depth < foldDepth, 'data-depth': depth });
  det.append(
    h('summary', { class: 'jt-line' }, keyEl, o, h('span', { class: 'jt-more' }, ` ${t(isArr ? 'lg.json.items' : 'lg.json.keys', { n: entries.length })} ${c}${comma}`)),
    h('div', { class: 'jt-kids' }, entries.map(([k, v], i) => jtNode(k, v, depth + 1, i === entries.length - 1, foldDepth))),
    h('div', { class: 'jt-line' }, c + comma));
  return det;
}

/**
 * Body viewer. JSON is shown formatted as a foldable tree (or raw); bodies over 200 lines start
 * folded to level 2 (tree) or cut at 200 lines with "Show all N lines" (raw). Every block has
 * copy and download in its toolbar.
 */
function bodyView(text, { truncated, note, filename = 'body.txt' } = {}) {
  if (text == null || text === '') return emptyPane(t('logs.noBody'));
  const bytes = utf8Bytes(text);
  const looksJSON = /^\s*[[{]/.test(text);
  const big = text.length > HL_LIMIT;
  const parsed = looksJSON ? tryParseJSON(text) : { ok: false };
  const isJSON = parsed.ok;
  const prettyText = isJSON ? JSON.stringify(parsed.value, null, 2) : null;
  const lineCount = (s) => { let n = 1; for (let i = s.indexOf('\n'); i >= 0; i = s.indexOf('\n', i + 1)) n++; return n; };
  const prettyLines = isJSON ? lineCount(prettyText) : 0;
  let pretty = isJSON && prettyPref;
  let folded = prettyLines > LINE_CAP;
  let allLines = false;
  const fname = filename.replace(/\.\w+$/, isJSON ? '.json' : '.txt');
  const pre = h('pre', { class: `code code-body ${wrapPref ? '' : 'nowrap'}` });
  const kindEl = h('span');
  const capSlot = h('div');
  const prev = previewer(pre, () => (pretty ? prettyText : text), fname);
  const tool = (ic, label, title) => h('button', { type: 'button', class: 'pane-tool', title: title || null }, icon(ic, 12), h('span', null, label));
  const prettyBtn = isJSON ? tool('braces', t('lg.json.formatted'), t('logs.prettyHint')) : null;
  const foldBtn = isJSON && !big ? tool('layers', '') : null;
  const wrapBtn = tool('wrap', t('lg.json.wrap'));
  const copyBtn = copyButton(() => (pretty ? prettyText : text), { label: t('common.copy'), cls: 'pane-tool' });
  const paint = () => {
    capSlot.replaceChildren();
    if (pretty && !big) {
      pre.replaceChildren(jtNode(null, parsed.value, 0, true, folded ? 2 : Infinity));
      prev.slot.replaceChildren();
    } else {
      const src = pretty ? prettyText : text;
      const n = src.length <= VIEW_LIMIT ? lineCount(src) : 0;
      if (n > LINE_CAP && !allLines) {
        let cut = 0; for (let i = 0; i < LINE_CAP; i++) cut = src.indexOf('\n', cut) + 1;
        paintPlain(pre, src.slice(0, cut));
        prev.slot.replaceChildren();
        capSlot.replaceChildren(h('div', { class: 'pane-limit' }, icon('info', 13), h('span', null, t('lg.json.firstLines', { n: LINE_CAP })),
          h('span', { class: 'pane-meta-spacer' }),
          h('button', { type: 'button', class: 'pane-tool', onclick: () => { allLines = true; paint(); } }, icon('expand', 12), h('span', null, t('lg.json.showAll', { n: fmtInt(n) })))));
      } else prev.paint();
    }
    kindEl.textContent = isJSON ? (pretty ? t('lg.json.kindFormatted') : t('lg.json.kindRaw')) : t('logs.raw');
    if (prettyBtn) { prettyBtn.classList.toggle('on', pretty); prettyBtn.setAttribute('aria-pressed', pretty ? 'true' : 'false'); }
    if (foldBtn) {
      foldBtn.hidden = !pretty;
      foldBtn.lastChild.textContent = folded ? t('lg.json.unfold') : t('lg.json.fold');
    }
    wrapBtn.classList.toggle('on', wrapPref);
    wrapBtn.setAttribute('aria-pressed', wrapPref ? 'true' : 'false');
    pre.classList.toggle('nowrap', !wrapPref);
  };
  if (prettyBtn) prettyBtn.addEventListener('click', () => { pretty = !pretty; prettyPref = pretty; lsSet('xr.logs.pretty', pretty ? '1' : '0'); paint(); });
  if (foldBtn) foldBtn.addEventListener('click', () => {
    folded = !folded;
    pre.querySelectorAll('details.jt').forEach((d) => { d.open = !folded || Number(d.dataset.depth) < 2; });
    paint();
  });
  wrapBtn.addEventListener('click', () => { wrapPref = !wrapPref; lsSet('xr.logs.wrap', wrapPref ? '1' : '0'); paint(); });
  paint();
  return h('div', { class: 'pane-content' },
    h('div', { class: 'pane-meta' }, kindEl, h('span', null, fmtBytes(bytes)), isJSON ? h('span', null, t('lg.json.lines', { n: fmtInt(prettyLines) })) : null,
      truncated ? badge(t('logs.truncated'), 'warning') : null,
      h('span', { class: 'pane-meta-spacer' }), prettyBtn, foldBtn, wrapBtn, copyBtn, downloadTool(() => (pretty ? prettyText : text), fname), expandToggle(pre)),
    note || null,
    prev.slot, capSlot,
    pre);
}

/** A collapsible JSON block for small objects (event detail, raw record). */
export function jsonBlock(value, filename = 'record.json') {
  return bodyView(JSON.stringify(value), { filename });
}

// ---------- tokens & cost ----------
const perM = (ticks) => (ticks == null ? '—' : fmtUSD(ticks / 1e4)); // ticks per token → USD per 1M tokens
const costOf = (n, ticks) => (ticks == null ? null : n * ticks);

function costSection(d) {
  const snap = d.price_snapshot && typeof d.price_snapshot === 'object' ? d.price_snapshot : null;
  const totalTicks = Math.round((d.cost_usd || 0) * 1e10);
  // Cost source as quiet secondary text, like "费用来源：xAI 返回" in the KPI band.
  const srcBadge = d.cost_source ? h('span', { class: 'ld5-cost-src' }, t(`costsrc.${d.cost_source}`) || d.cost_source) : null;
  // Images and videos: count × unit price, no token table.
  if (d.images || d.video_seconds) {
    const rows = [];
    if (d.images) rows.push([t('logs.images'), fmtInt(d.images), snap && snap.image != null ? fmtUSD(snap.image / 1e10) : '—', snap && snap.image != null ? fmtUSD((d.images * snap.image) / 1e10) : '—']);
    if (d.video_seconds) rows.push([t('logs.videoSeconds'), t('lg.seconds', { n: d.video_seconds }), snap && snap.video_per_second != null ? fmtUSD(snap.video_per_second / 1e10) : '—', snap && snap.video_per_second != null ? fmtUSD((d.video_seconds * snap.video_per_second) / 1e10) : '—']);
    return h('table', { class: 'cost-table' },
      h('thead', null, h('tr', null, h('th', null, t('lg.cost.cat')), h('th', { class: 'num' }, t('lg.cost.qty')), h('th', { class: 'num' }, t('lg.cost.unit')), h('th', { class: 'num' }, t('lg.cost.cost')))),
      h('tbody', null, rows.map((r) => h('tr', null, r.map((c, i) => h('td', { class: i ? 'num mono' : '' }, c))))),
      h('tfoot', null, h('tr', null, h('td', null, t('lg.cost.total')), h('td'), h('td'), h('td', { class: 'num mono' }, fmtUSD(d.cost_usd), ' ', srcBadge))));
  }
  const c = tokenCats(d);
  const rows = [
    { k: 'input', label: t('lg.tok.input'), n: c.input, ticks: snap ? snap.input : null },
    c.image ? { k: 'image', label: t('lg.tok.image'), n: c.image, ticks: snap ? snap.image_input : null } : null,
    { k: 'cacheWrite', label: t('lg.tok.cacheWrite'), n: c.cacheWrite, ticks: null, tip: t('lg.tok.cacheWriteTip') },
    { k: 'cacheRead', label: t('lg.tok.cacheRead'), n: c.cacheRead, ticks: snap ? snap.cached_input : null },
    { k: 'output', label: t('lg.tok.output'), n: c.output, ticks: snap ? snap.output : null },
  ].filter(Boolean);
  let sum = 0;
  const body = rows.map((r) => {
    const cost = r.k === 'cacheWrite' ? null : costOf(r.n || 0, r.ticks);
    if (cost != null) sum += cost;
    return h('tr', { 'data-k': r.k },
      h('td', { title: r.tip || null }, r.label, r.tip ? h('span', { class: 'muted small' }, ' ⓘ') : null),
      h('td', { class: 'num mono' }, r.n == null ? h('span', { class: 'muted', title: r.tip }, '—') : fmtInt(r.n)),
      snap ? h('td', { class: 'num mono' }, r.k === 'cacheWrite' ? h('span', { class: 'muted' }, t('lg.cost.notBilled')) : perM(r.ticks)) : null,
      snap ? h('td', { class: 'num mono' }, cost == null ? '—' : fmtUSD(cost / 1e10)) : null);
  });
  // Reasoning: an indented sub-row of output (billed at the output price).
  body.push(h('tr', { class: 'cost-sub', 'data-k': 'reasoning' },
    h('td', null, `↳ ${t('ld5.ofReasoning')}`),
    h('td', { class: 'num mono' }, c.reasoningKnown ? fmtInt(c.reasoning) : h('span', { class: 'muted' }, '—')),
    snap ? h('td', { class: 'num muted small' }, t('lg.cost.inOutput')) : null,
    snap ? h('td') : null));
  const notes = [];
  if (snap && snap.long_context) notes.push(badge(t('lg.cost.long'), 'warning'));
  if (snap && snap.priority) notes.push(badge(t('lg.cost.priority'), 'warning'));
  const mismatch = snap && Math.abs(Math.round(sum) - totalTicks) > 1;
  const foot = snap
    ? [h('tr', null, h('td', null, t('lg.cost.total')), h('td', { class: 'num mono' }, fmtInt(d.total_tokens || 0)), h('td'),
      h('td', { class: 'num mono' }, fmtUSD(mismatch ? Math.round(sum) / 1e10 : d.cost_usd), ' ', mismatch ? null : srcBadge))]
    : [h('tr', null, h('td', null, t('lg.cost.total')), h('td', { class: 'num mono' }, fmtInt(d.total_tokens || 0))),
      h('tr', null, h('td', null, t('lg.cost.totalCost')), h('td', { class: 'num mono' }, fmtUSD(d.cost_usd), ' ', srcBadge))];
  if (mismatch) {
    foot.push(h('tr', { class: 'cost-actual' },
      h('td', { colspan: 3 }, t(d.cost_source === 'upstream' ? 'lg.cost.upstreamActual' : 'lg.cost.actual', { v: fmtUSD(d.cost_usd) })),
      h('td', { class: 'num mono strong' }, fmtUSD(d.cost_usd), ' ', srcBadge)));
  }
  return h('div', null,
    h('table', { class: 'cost-table' },
      h('thead', null, h('tr', null, h('th', null, t('lg.cost.cat')), h('th', { class: 'num' }, t('lg.cost.tokens')),
        snap ? h('th', { class: 'num' }, t('lg.cost.unitM')) : null, snap ? h('th', { class: 'num' }, t('lg.cost.cost')) : null)),
      h('tbody', null, body),
      h('tfoot', null, foot)),
    notes.length ? h('div', { class: 'row-sm cost-notes' }, notes) : null,
    snap || !(d.cost_usd || d.total_tokens) ? null : h('div', { class: 'muted small cost-note' }, t('lg.cost.noSnapshot')));
}

// ---------- request detail (v0.5, Figma 14/15) ----------
/** "被限流，冷却 60 秒" style effect of a failed attempt on its upstream key. */
function attemptEffect(a) {
  if (a.action === 'disable') return t('ld5.att.disabled');
  if (a.action === 'cooldown') return a.cooldown_seconds ? t('ld5.att.cooldownN', { n: a.cooldown_seconds }) : t('ld5.att.cooldown');
  return '';
}
const attemptsOf = (d) => (d.upstream_attempts || []).slice().sort((a, b) => (a.seq || 0) - (b.seq || 0));
const attKey = (a) => a.key_name || (a.key_id != null ? `#${a.key_id}` : '—');
const isOk = (s) => s === 101 || (s >= 200 && s < 400);

/** The rows of this request's retry chain (v0.6.0), first attempt first; [] for a single attempt. */
const chainOf = (d) => (Array.isArray(d.chain) && d.chain.length > 1 && isChainRow(d) ? d.chain.slice().sort((a, b) => (a.attempt_index || 0) - (b.attempt_index || 0)) : []);
const chainPos = (d) => ({ k: d.attempt_index || 1, n: d.attempt_total || 1 });

/**
 * Historical rows: "第 3 次尝试成功。前两次：prod-07 密钥无效（已自动禁用），prod-03 被限流（冷却 60 秒）。"
 * Chain rows (v0.6.0): "成功（第 3 次 / 共 3 次）。前两次：…" (Figma 14b).
 */
function conclusion(d) {
  const chain = chainOf(d);
  const prevText = (failed) => (failed.length ? t(failed.length === 1 ? 'ld5.prev1' : 'ld5.prevN', { n: failed.length }) + failed.map((a) => {
    const eff = attemptEffect(a);
    return `${attKey(a)} ${attemptWhy(a)}${eff ? t('ld5.paren', { x: eff }) : ''}`;
  }).join(t('lg.sep')) + t('ld5.stop') : '');
  if (chain.length) {
    const { k, n } = chainPos(d);
    const ofN = t('ld6.ofN', { k, n });
    const prev = prevText(chain.filter((a) => (a.attempt_index || 0) < k && !isOk(a.status)));
    if (isRetriedRow(d)) return { text: `${requestReason(d).text}${ofN}${t('ld5.stop')}${prev}`, tone: 'warning' };
    if (isOk(d.status)) return { text: k > 1 ? `${t('lg5.okAttempt', { n: k })}${t('ld5.stop')}${prev}` : `${t('lg.ok')}${t('ld5.stop')}`, tone: prev ? 'warning' : 'success' };
    const r = requestReason(d);
    return { text: `${r ? r.text : t('lg.failed')}${ofN}${t('ld5.stop')}${prev}`, tone: d.status >= 500 || !d.status ? 'danger' : 'warning', link: r && r.link };
  }
  const at = attemptsOf(d);
  const failed = at.filter((a) => !isOk(a.status));
  const prev = prevText(failed);
  if (isOk(d.status)) {
    const n = Math.max(d.attempts || 0, at.length);
    return { text: n > 1 ? `${t('lg5.okAttempt', { n })}${t('ld5.stop')}${prev}` : `${t('lg.ok')}${t('ld5.stop')}`, tone: n > 1 ? 'warning' : 'success' };
  }
  const r = requestReason(d);
  return { text: `${r ? r.text : t('lg.failed')}${t('ld5.stop')}${at.length > 1 ? prev : ''}`, tone: d.status >= 500 || !d.status ? 'danger' : 'warning', link: r && r.link };
}

const navLink = (node, hash, title) => h('a', { href: hash, class: 'link', title: title || null, onclick: (e) => e.stopPropagation() }, node);
const splitUnit = (s) => { const m = /^([-\d.,$]+)\s*(.*)$/.exec(String(s)); return m ? [m[1], m[2]] : [String(s), '']; };

/** Drawer title: status code, tries, method and path. */
export function requestTitle(d) {
  return h('span', { class: 'ld5-title' }, statusCode(d.status),
    isChainRow(d) ? h('span', { class: 'ld5-tries', title: t('lg6.attemptTip', { k: d.attempt_index || 1, n: d.attempt_total }) }, `${d.attempt_index || 1}/${d.attempt_total}`)
      : (d.attempts || 0) > 1 ? h('span', { class: 'ld5-tries', title: t('logs.retriedN', { n: d.attempts }) }, `×${d.attempts}`) : null,
    h('span', { class: 'ld5-method' }, d.method || 'GET'), h('span', { class: 'ld5-path' }, d.path || ''));
}

/** Header actions: copy as curl, and a menu with the other copy actions. */
export function requestActions(d) {
  const curlBtn = h('button', { class: 'btn btn-sm', type: 'button', 'data-copy': 'curl', onclick: (e) => copyCurl(d.id, e.currentTarget) }, icon('terminal', 14), h('span', null, t('logs.copyCurl')));
  const copy = async (text, okMsg) => { const ok = await copyText(text || ''); toast(ok ? okMsg : t('common.copyFailed'), ok ? 'success' : 'error', 2000); };
  const menu = menuButton(() => [
    { icon: 'copy', label: t('logs.copyReq'), onClick: () => copy(d.request_body, t('common.copied')) },
    { icon: 'copy', label: t('logs.copyRes'), onClick: () => copy(d.response_body, t('common.copied')) },
    d.response_text ? { icon: 'copy', label: t('lg.copyText'), onClick: () => copy(d.response_text, t('common.copied')) } : null,
    d.request_id ? { icon: 'copy', label: t('logs.copyReqId'), onClick: () => copy(d.request_id, t('common.copied')) } : null,
  ], t('common.actions'));
  return [curlBtn, menu];
}

/** Header block under the title: the conclusion and two meta lines. */
export function requestHead(d) {
  const con = conclusion(d);
  const tokPfx = d.token_prefix ? `${d.token_prefix}…` : '';
  const m = (label, val) => (val ? h('span', { class: 'ld5-m' }, h('span', { class: 'ld5-ml' }, label), val) : null);
  const idv = (v, label) => (v ? h('span', { class: 'ld5-id' }, h('span', { class: 'mono' }, v), copyButton(v, { iconOnly: true, label })) : null);
  const moreBody = h('div', { class: 'ld5-morebody', hidden: true }, kv([
    [t('logs.clientIp'), d.client_ip ? h('span', { class: 'mono' }, d.client_ip) : null],
    [t('logs.userAgent'), d.user_agent ? h('span', { class: 'mono small break' }, d.user_agent) : null],
    d.query ? [t('logs.query'), h('span', { class: 'mono small break' }, d.query)] : null,
  ]));
  const moreBtn = h('button', { type: 'button', class: 'ld5-more', 'aria-expanded': 'false', onclick: (e) => {
    moreBody.hidden = !moreBody.hidden; e.currentTarget.setAttribute('aria-expanded', moreBody.hidden ? 'false' : 'true');
  } }, t('ld5.more'), icon('chevronDown', 12));
  return h('div', { class: 'ld5-head' },
    h('div', { class: `ld5-con ld-${con.tone}` }, con.text, con.link ? h('a', { class: 'link', href: con.link.hash }, ` ${con.link.label} →`) : null),
    h('div', { class: 'ld5-meta' },
      m(t('logs.time'), h('span', { class: 'mono', title: relTime(d.created_at) }, fmtDateTime(isChainRow(d) && d.started_at ? d.started_at : d.created_at))),
      m(t('common.model'), d.model ? h('span', { class: 'mono' }, d.model, d.stream ? h('span', { class: 'stream-tag' }, t('lg.stream')) : null, d.websocket ? h('span', { class: 'stream-tag' }, 'WebSocket') : null) : null),
      m(t('ld5.token'), d.token_name ? (d.token_id != null ? navLink(d.token_name, `#/tokens?open=${d.token_id}`, `${t('lg.openToken')}: ${d.token_name} ${tokPfx}`) : d.token_name) : null),
      m(t('common.key'), d.key_name ? (d.key_id != null ? navLink(d.key_name, `#/keys?open=${d.key_id}`, t('lg.openKey')) : d.key_name) : null)),
    h('div', { class: 'ld5-meta ld5-meta2' },
      m(t('logs.requestId'), idv(d.request_id, t('logs.copyReqId'))),
      m(t('ld5.upstreamId'), idv(d.upstream_id, t('lg.copyUpstreamId'))),
      d.client_ip || d.user_agent || d.query ? moreBtn : null),
    moreBody);
}

/** Four-cell band: latency, cost, tokens, upstream attempts. */
function kpiBand(d) {
  const cell = (label, value, sub, bar, cls) => h('div', { class: `ld5-kpi ${cls || ''}` }, h('div', { class: 'eyebrow' }, h('span', null, label)), value, bar || null, sub ? h('div', { class: 'ld5-kpi-sub' }, sub) : null);
  const big = (s, accent) => { const [n, u] = splitUnit(s); return bigNum(n, { unit: u || undefined, size: 'md', accent }); };
  const lat = d.latency_ms || 0;
  const ttfbPct = lat && d.ttfb_ms ? Math.min(100, (d.ttfb_ms / lat) * 100) : 0;
  const latBar = h('div', { class: 'ld5-bar' }, h('span', { class: 'ld5-bar-a', style: { width: `${ttfbPct}%` } }), h('span', { class: 'ld5-bar-b', style: { width: `${100 - ttfbPct}%` } }));
  const c = tokenCats(d);
  const tot = (c.input + c.image + (c.cacheWrite || 0) + c.cacheRead + c.output) || 1;
  const seg = (n, k) => (n ? h('span', { class: `tok-sw ${k}`, style: { flex: `${n / tot} 1 0` } }) : null);
  const tokBar = d.total_tokens ? h('div', { class: 'tok-bar ld5-tokbar' }, seg(c.input + c.image, 'in'), seg(c.cacheWrite, 'cw'), seg(c.cacheRead, 'cr'), seg(c.output, 'out')) : null;
  const tokVal = d.images ? big(t('lg5.images', { n: d.images })) : d.video_seconds ? big(t('lg5.video', { n: d.video_seconds })) : big(d.total_tokens ? fmtInt(d.total_tokens) : '—');
  const at = attemptsOf(d);
  const n = Math.max(d.attempts || 0, at.length);
  const failed = at.filter((a) => !isOk(a.status)).length;
  let attSub = !n ? t('ld5.notForwarded') : isOk(d.status) ? (failed ? t('ld5.prevFailed', { n: failed }) : t('ld5.firstOk')) : t('ld5.allFailed');
  let attVal = bigNum(String(n), { unit: t('ld5.timesUnit'), size: 'md' });
  let attCls = n > 1 ? 'is-warn' : '';
  // A chain row (v0.6.0): "第 k 次" with "共 n 次，前 m 次失败" / "共 n 次，全部失败" (pm-spec 10.9).
  const chain = chainOf(d);
  if (chain.length) {
    const { k, n: total } = chainPos(d);
    const m = chain.filter((a) => (a.attempt_index || 0) < k && !isOk(a.status)).length;
    const allFailed = !chain.some((a) => isOk(a.status));
    attSub = allFailed ? t('ld6.kpiAttemptSubAll', { n: total }) : m ? t('ld6.kpiAttemptSub', { n: total, m }) : t('ld6.kpiAttemptSub0', { n: total });
    attVal = big(t('ld6.kpiAttempt', { k }));
    attCls = '';
  }
  return h('div', { class: 'hairline-grid ld5-band' },
    cell(t('common.latency'), big(fmtMs(lat)), d.ttfb_ms ? h('span', { title: t('logs.ttfbClientHint') }, t('lg.ttfb', { v: fmtMs(d.ttfb_ms) })) : null, lat ? latBar : null),
    cell(t('common.cost'), big(d.cost_usd ? fmtUSD(d.cost_usd) : (isOk(d.status) ? '$0' : '—')), d.cost_source ? t('ld5.costSrc', { s: t(`costsrc.${d.cost_source}`) || d.cost_source }) : null),
    cell(t('lg5.tokGroup'), tokVal, d.total_tokens ? t('ld5.inOut', { i: fmtInt(c.input + c.image + c.cacheRead + (c.cacheWrite || 0)), o: fmtInt(c.output) }) : null, tokBar),
    cell(t('logs.attemptsTimeline'), attVal, attSub, null, attCls));
}

/** Upstream attempts on one time axis (Figma 14): info on the left, the bar on the right. */
function attemptsView(d) {
  const at = attemptsOf(d);
  if (!at.length) return null;
  const starts = at.map((a) => (a.started_at ? new Date(a.started_at).getTime() : NaN));
  const valid = starts.every((x) => !isNaN(x));
  const t0 = valid ? Math.min(...starts) : 0;
  let acc = 0;
  const offs = at.map((a, i) => { if (valid) return starts[i] - t0; const o = acc; acc += a.latency_ms || 0; return o; });
  const span = Math.max(1, ...at.map((a, i) => offs[i] + (a.latency_ms || 0)));
  const ticks = [0, span / 2, span].map((v, i) => h('span', { class: 'ld5-tick', style: { left: `${(i / 2) * 100}%` } }, i ? fmtMs(v) : '0'));
  return h('div', { class: 'ld5-att' },
    h('div', { class: 'ld5-att-row ld5-att-axis' }, h('div'), h('div', { class: 'ld5-track' }, ticks)),
    at.map((a, i) => {
      const ok = isOk(a.status);
      const v = ok ? 'ok' : a.status === 429 || (a.status >= 400 && a.status < 500 && a.status !== 401 && a.status !== 403) ? 'warn' : 'err';
      const last = i === at.length - 1;
      const sub = ok
        ? (a.ttfb_ms ? t('ld5.att.okTtfb', { v: fmtMs(a.ttfb_ms) }) : t('lg.ok'))
        : [attemptWhy(a), attemptEffect(a), last ? '' : t('ld5.att.next')].filter(Boolean).join(t('lg.sep'));
      const left = (offs[i] / span) * 100, width = Math.max(0.8, ((a.latency_ms || 0) / span) * 100);
      const ttfbW = a.latency_ms ? Math.min(100, ((a.ttfb_ms || 0) / a.latency_ms) * 100) : 0;
      return h('div', { class: `ld5-att-row is-${v}` },
        h('div', { class: 'ld5-att-info' },
          h('div', { class: 'ld5-att-line' }, h('span', { class: 'ld5-att-n' }, String(a.seq ?? i + 1)), h('span', { class: 'ld5-att-key' }, attKey(a)),
            statusCode(a.status), h('span', { class: 'ld5-att-ms mono' }, fmtMs(a.latency_ms))),
          h('div', { class: 'ld5-att-sub', title: a.error || null }, sub)),
        h('div', { class: 'ld5-track' }, h('div', { class: `ld5-wf is-${v}`, style: { left: `${left}%`, width: `${width}%` } }, h('span', { class: 'ld5-wf-ttfb', style: { width: `${ttfbW}%` } }))));
    }),
    at.some((a) => isOk(a.status) && a.ttfb_ms) ? h('div', { class: 'ld5-legend' }, h('span', { class: 'ld5-lg-a' }), t('ld5.lgWait'), h('span', { class: 'ld5-lg-b' }), t('ld5.lgBody')) : null);
}

/**
 * "全部尝试（n 次）" (v0.6.0, Figma 14b): one card per attempt of this request on one time axis,
 * "第 k 次 · key · status reason · duration"; the current row is highlighted, the others open their row.
 */
function chainView(d, ctx) {
  const chain = chainOf(d);
  if (!chain.length) return null;
  // Place each attempt at its own start time; without start times, one after another by duration.
  const starts = chain.map((a) => (a.started_at ? new Date(a.started_at).getTime() : NaN));
  const valid = starts.every((x) => !isNaN(x));
  const t0 = valid ? Math.min(...starts) : 0;
  let acc = 0;
  const offs = chain.map((a, i) => { if (valid) return starts[i] - t0; const o = acc; acc += a.latency_ms || 0; return o; });
  const span = Math.max(1, ...chain.map((a, i) => offs[i] + (a.latency_ms || 0)));
  const ticks = [0, span / 2, span].map((v, i) => h('span', { class: 'ld5-tick', style: { left: `${(i / 2) * 100}%` } }, i ? fmtMs(v) : '0'));
  const cards = chain.map((a, i) => {
    const cur = a.id === d.id;
    const ok = isOk(a.status);
    const retried = a.attempt_state === 'retried' || (a.attempt_state == null && i < chain.length - 1);
    const v = ok ? 'ok' : retried ? 'warn' : 'err';
    const eff = retried ? attemptEffect(a) : '';
    const reason = ok ? t('lg.ok')
      : retried ? t('lg6.retried', { r: `${attemptWhy(a)}${eff ? t('ld5.paren', { x: eff }) : ''}` })
        : ((requestReason({ status: a.status, error: a.error }) || {}).text || t('lg.failed'));
    const left = (offs[i] / span) * 100, width = Math.max(0.8, ((a.latency_ms || 0) / span) * 100);
    const ttfbW = ok && a.latency_ms ? Math.min(100, ((a.ttfb_ms || 0) / a.latency_ms) * 100) : 0;
    const open = () => { if (!cur && ctx.openRow) ctx.openRow(a.id); };
    return h('div', {
      class: `ld6-card is-${v} ${cur ? 'is-current' : ''}`, 'data-id': a.id,
      role: cur ? null : 'button', tabindex: cur ? null : '0', 'aria-current': cur ? 'true' : null,
      title: cur ? null : t('ld6.openRow'), onclick: open,
      onkeydown: (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); e.stopPropagation(); open(); } },
    },
    h('div', { class: 'ld6-card-info' },
      h('span', { class: 'ld6-n' }, t('ld6.attemptItem', { k: a.attempt_index || i + 1 })),
      h('span', { class: 'ld6-key mono', title: a.key_name || null }, attKey(a)),
      statusCode(a.status),
      h('span', { class: 'ld6-why', title: a.error || null }, reason),
      cur ? h('span', { class: 'ld6-cur' }, t('ld6.current')) : null,
      h('span', { class: 'ld6-ms mono' }, fmtMs(a.latency_ms))),
    h('div', { class: 'ld5-track' }, h('div', { class: `ld5-wf is-${v}`, style: { left: `${left}%`, width: `${width}%` } }, h('span', { class: 'ld5-wf-ttfb', style: { width: `${ttfbW}%` } }))));
  });
  return h('div', { class: 'ld6-chain' },
    h('div', { class: 'ld6-card ld6-axis' }, h('div'), h('div', { class: 'ld5-track' }, ticks)),
    cards);
}

export function requestDetailView(d, ctx = {}) {
  const isChat = /\/chat\/completions$/.test(d.path || '');
  const usageNote = d.stream && isChat && ctx.injectUsage !== false
    ? h('div', { class: 'pane-note' }, icon('info', 13), h('span', null, t('logs.includeUsageNote'))) : null;
  const bothHeaders = () => h('div', { class: 'ld5-hdrs' },
    h('div', { class: 'eyebrow' }, h('span', null, t('logs.request'))), headersView(d.request_headers),
    h('div', { class: 'eyebrow' }, h('span', null, t('logs.response'))), headersView(d.response_headers));
  const hdrCount = Object.keys(d.request_headers || {}).length + Object.keys(d.response_headers || {}).length;
  const allTabs = [
    { id: 'req', label: t('logs.request'), render: () => bodyView(d.request_body, { truncated: d.truncated, note: usageNote, filename: `log-${d.id}-request.json` }) },
    d.websocket
      ? { id: 'res', label: t('logs.wsMessages'), render: () => bodyView(d.response_body, { truncated: d.truncated, filename: `log-${d.id}-messages.json` }) }
      : !d.stream ? { id: 'res', label: t('logs.response'), render: () => bodyView(d.response_body, { truncated: d.truncated, filename: `log-${d.id}-response.json` }) } : null,
    d.stream || d.response_text || d.reasoning_text ? { id: 'text', label: t('logs.text'), render: () => textView(d) } : null,
    d.stream && !d.websocket ? { id: 'sse', label: t('logs.rawSse'), render: () => sseView(d.response_body, { filename: `log-${d.id}-response.sse.txt` }) } : null,
    { id: 'headers', label: t('logs.headers'), count: hdrCount, render: bothHeaders },
  ].filter(Boolean);
  const chain = chainView(d, ctx);
  const attempts = chain ? null : attemptsView(d);
  const section = (title, body) => h('section', { class: 'ld-section ld5-sec' }, h('div', { class: 'eyebrow' }, h('span', null, title)), body);
  return h('div', { class: 'log-detail ld5' },
    d.error && !isOk(d.status) ? h('div', { class: 'ld-raw' }, h('span', { class: 'muted small' }, t('lg.rawError')), h('code', { class: 'mono small break' }, d.error)) : null,
    d.truncated ? h('div', { class: 'callout callout-warning' }, icon('alert', 16), h('span', null, t('logs.truncatedNotice'))) : null,
    kpiBand(d),
    section(t('lg.costTitle'), costSection(d)),
    d.websocket_session ? wsSessionView(d.websocket_session) : null,
    chain ? section(t('ld6.chainTitle', { n: chainOf(d).length }), chain) : null,
    attempts ? section(t('logs.attemptsTimeline'), attempts) : null,
    h('div', { class: 'ld5-tabs' }, tabs(allTabs, { cls: 'tabs-sm' })));
}

// ---------- event detail (v0.5, Figma 16) ----------
/** "增加额度：客户端令牌 prod-app" */
export function eventTitle(e) {
  return e.target_type ? t('ld5.evTitle', { a: actionLabel(e.action), tt: targetTypeLabel(e.target_type), name: targetText(e) }) : actionLabel(e.action);
}
/** Type tag and level above the title. */
export function eventKicker(e) {
  return h('span', { class: 'ld5-kicker' }, h('span', { class: `type-tag-pill type-${e.type}` }, t(`lg.type.${e.type}`)), statusDot(t(`ev.level.${e.level || 'info'}`), e.level === 'error' ? 'danger' : e.level === 'warn' ? 'warning' : 'neutral'));
}

export function eventDetailView(e, ctx = {}) {
  const d = e.detail || {};
  const tt = e.target_type;
  const hint = targetHint(e);
  const hasPage = (tt === 'key' || tt === 'token') && e.target_id;
  const targetHash = hasPage ? `#/${tt === 'key' ? 'keys' : 'tokens'}?open=${e.target_id}` : '';
  const targetNode = cred(targetText(e), hint);
  const section = (title, body) => h('section', { class: 'ld-section ld5-sec' }, h('div', { class: 'eyebrow' }, h('span', null, title)), body);
  const isQuota = (e.action || '').startsWith('token.quota_');
  // Before / after rows: field changes, or the quota change of a quota record.
  let diffs = d.changes && typeof d.changes === 'object' ? Object.entries(d.changes).map(([f, c]) => [fieldLabel(tt, f), fmtChangeValue(c && c.before), fmtChangeValue(c && c.after)]) : [];
  if (isQuota && !diffs.length) {
    const lim = (unl, ticks) => (unl ? t('tokens.unlimitedState') : usdT(ticks));
    diffs = [[t('ld5.quota'), lim(d.before_unlimited, d.before_ticks), lim(d.unlimited, d.after_ticks)]];
    if (d.used_ticks != null && !d.unlimited) diffs.push([t('ld5.remaining'), d.before_unlimited ? '—' : usdT((d.before_ticks || 0) - (d.used_ticks || 0)), usdT((d.after_ticks || 0) - (d.used_ticks || 0))]);
    const reason = quotaReasonText(d);
    if (reason) diffs.push([t('q.h.reason'), '—', reason]);
  }
  const result = d.result && typeof d.result === 'object' ? d.result : null;
  const related = [];
  if (hasPage) related.push(h('a', { class: 'ld5-rel', href: targetHash, onclick: (ev) => ev.stopPropagation() }, icon(tt === 'key' ? 'key' : 'ticket', 15),
    h('span', { class: 'ld5-rel-main' }, h('span', null, t('ld5.openTarget', { tt: targetTypeLabel(tt), name: targetText(e) })), hint ? h('span', { class: 'ld5-rel-sub mono' }, hint) : null), icon('external', 13)));
  if (e.request_id && ctx.openRequest) related.push(h('button', { type: 'button', class: 'ld5-rel', onclick: (ev) => ctx.openRequest(e.request_id, ev.currentTarget) }, icon('logs', 15),
    h('span', { class: 'ld5-rel-main' }, h('span', null, t('lg.openRequest')), h('span', { class: 'ld5-rel-sub mono' }, e.request_id)), icon('chevronRight', 13)));
  const rawBytes = utf8Bytes(JSON.stringify(e));
  const raw = h('details', { class: 'ld5-raw' },
    h('summary', null, icon('chevronRight', 13), h('span', null, t('ld5.raw')), h('span', { class: 'muted' }, fmtBytes(rawBytes)), h('span', { class: 'pane-meta-spacer' }),
      copyButton(() => JSON.stringify(e, null, 2), { iconOnly: true, label: t('common.copy') })),
    jsonBlock(e, `event-${e.id}.json`));
  const row = (k, v) => (v == null || v === '' ? null : h('div', { class: 'ld5-kv-row' }, h('div', { class: 'ld5-kv-k' }, k), h('div', { class: 'ld5-kv-v' }, v)));
  return h('div', { class: 'log-detail ld5 ld5-ev' },
    section(t('lg.summary'), h('div', { class: 'ld5-kv' },
      row(t('logs.time'), h('span', { class: 'mono', title: relTime(e.created_at) }, fmtDateTime(e.created_at))),
      row(t('lg.col.actor'), actorLabel(e)),
      row(t('lg.col.ip'), e.ip ? h('span', { class: 'mono' }, e.ip) : null),
      row(t('lg.col.target'), tt ? (hasPage ? navLink(targetNode, targetHash, t(tt === 'key' ? 'lg.openKey' : 'lg.openToken')) : targetNode) : null),
      row(t('lg.col.action'), actionLabel(e.action)),
      row(t('logs.requestId'), e.request_id ? h('span', { class: 'ld5-id' }, h('span', { class: 'mono' }, e.request_id), copyButton(e.request_id, { iconOnly: true })) : null))),
    diffs.length ? section(t('ld5.diff'), h('table', { class: 'ld5-diff' },
      h('thead', null, h('tr', null, h('th', null, t('lg.field')), h('th', null, t('lg.before')), h('th', null, t('lg.after')))),
      h('tbody', null, diffs.map(([f, b, a]) => h('tr', null, h('td', null, f), h('td', null, h('span', { class: 'diff-del mono' }, b)), h('td', null, h('span', { class: 'diff-add mono' }, a))))))) : null,
    result ? section(t('lg.result'), kv(Object.entries(result).map(([k, v]) => [fieldLabel('', k), h('span', { class: 'mono small break' }, fmtChangeValue(v))]))) : null,
    d.error ? section(t('lg.rawError'), h('code', { class: 'mono small break' }, d.error)) : null,
    related.length ? section(t('lg.related'), h('div', { class: 'ld5-rels' }, related)) : null,
    raw);
}
function quotaReasonText(d) {
  if (d.reason_code === 'pre_v0_4_1') return t('ev.q.codePre');
  if (d.reason_code === 'reset_folded') return t('ev.q.codeReset');
  return d.reason || '';
}

/** Find the request log of a request id and return its id (null when it was purged). */
export async function findRequestLog(requestId, signal) {
  const r = await get('/api/v1/logs', { type: 'request', q: requestId, page_size: 20 }, { signal });
  const rows = r.data || [];
  const row = rows.find((x) => x.request_id === requestId && x.attempt_final !== false) || rows.find((x) => x.request_id === requestId) || rows[0];
  return row ? row.id : null;
}
