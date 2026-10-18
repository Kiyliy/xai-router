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
import { toast, toastError, badge, copyButton, busy, tabs, kv } from './ui.js';
import { cred } from './detail-drawer.js';
import {
  actionLabel, actorLabel, targetTypeLabel, targetText, targetHint, eventSentence, levelBadge, requestReason, tokenCats, usdT,
  fieldLabel, fmtChangeValue,
} from './events.js';

const ACTION_VARIANT = { success: 'success', retry: 'warning', return: 'neutral', cooldown: 'warning', disable: 'danger', verify: 'warning' };

export function statusBadge(code) {
  return badge(code ? String(code) : 'ERR', statusClass(code), { class: 'badge-status mono' });
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
      h('div', { class: 'pane-meta' }, h('span', null, 'text/event-stream'), size, h('span', { class: 'pane-meta-spacer' }), downloadTool(() => text, filename), expandToggle(pre)),
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
  return h('div', { class: 'pane-content' }, h('div', { class: 'pane-meta' }, h('span', null, 'text/event-stream'), size,
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

function attemptsView(d) {
  const at = (d.upstream_attempts || []).slice().sort((a, b) => (a.seq || 0) - (b.seq || 0));
  if (!at.length) return null;
  const starts = at.map((a) => (a.started_at ? new Date(a.started_at).getTime() : NaN));
  const valid = starts.every((x) => !isNaN(x));
  const t0 = valid ? Math.min(...starts) : 0;
  let acc = 0;
  const offs = at.map((a, i) => { if (valid) return starts[i] - t0; const o = acc; acc += a.latency_ms || 0; return o; });
  const span = Math.max(1, ...at.map((a, i) => offs[i] + (a.latency_ms || 0)));
  return h('div', { class: 'attempts' },
    at.map((a, i) => {
      const v = a.status ? statusClass(a.status) : 'danger';
      const left = (offs[i] / span) * 100, width = Math.max(0.8, ((a.latency_ms || 0) / span) * 100);
      const ttfbW = a.latency_ms ? Math.min(100, ((a.ttfb_ms || 0) / a.latency_ms) * 100) : 0;
      return h('div', { class: `attempt attempt-${v}` },
        h('div', { class: 'attempt-rail' }, h('span', { class: 'attempt-num' }, String(a.seq ?? i + 1))),
        h('div', { class: 'attempt-main' },
          h('div', { class: 'attempt-head' },
            h('span', { class: 'strong' }, a.key_name || (a.key_id != null ? `#${a.key_id}` : '—')),
            a.status ? statusBadge(a.status) : badge(t('logs.networkError'), 'danger'),
            a.action ? badge(t(`action.${a.action}`) || a.action, ACTION_VARIANT[a.action] || 'neutral', { class: 'badge-outline' }) : null,
            h('span', { class: 'attempt-times mono muted' }, `+${fmtMs(offs[i])}`, ' · ', h('span', { title: t('logs.ttfbUpstreamHint') }, `${t('logs.ttfbUpstream')} ${fmtMs(a.ttfb_ms)}`), ' · ', fmtMs(a.latency_ms))),
          a.error ? h('div', { class: 'attempt-error' }, a.error) : null,
          h('div', { class: 'waterfall' }, h('div', { class: `wf-bar wf-${v}`, style: { left: `${left}%`, width: `${width}%` } }, h('div', { class: 'wf-ttfb', style: { width: `${ttfbW}%` } })))));
    }));
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
    s.audio_seconds ? [t('logs.wsAudio'), `${s.audio_seconds.toFixed(1)} s`] : null,
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
  const srcBadge = d.cost_source ? badge(t(`costsrc.${d.cost_source}`) || d.cost_source, d.cost_source === 'upstream' ? 'success' : d.cost_source === 'computed' ? 'info' : 'neutral') : null;
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
    h('td', null, `└ ${t('lg.tok.reasoning')}`),
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
    snap ? null : h('div', { class: 'muted small cost-note' }, t('lg.cost.noSnapshot')));
}

// ---------- request detail ----------
function conclusion(d) {
  const ok = d.status === 101 || (d.status >= 200 && d.status < 400);
  const at = d.upstream_attempts || [];
  if (ok) {
    const disabled = at.filter((a) => a.action === 'disable' || a.action === 'verify');
    if (disabled.length) return { text: t('lg.okAfterDisable', { n: at.length - 1 }), tone: 'warning' };
    if (at.length > 1) return { text: t('lg.okAfterRetry', { n: at.length - 1 }), tone: 'warning' };
    return { text: t('lg.ok'), tone: 'success' };
  }
  const r = requestReason(d);
  return { text: r ? r.text : t('lg.failed'), tone: d.status >= 500 || !d.status ? 'danger' : 'warning', link: r && r.link };
}

const navLink = (node, hash, title) => h('a', { href: hash, class: 'link', title: title || null, onclick: (e) => e.stopPropagation() }, node);

export function requestDetailView(d, ctx = {}) {
  const isChat = /\/chat\/completions$/.test(d.path || '');
  const usageNote = d.stream && isChat && ctx.injectUsage !== false
    ? h('div', { class: 'pane-note' }, icon('info', 13), h('span', null, t('logs.includeUsageNote'))) : null;
  const reqTabs = [
    { id: 'body', label: t('logs.body'), render: () => bodyView(d.request_body, { truncated: d.truncated, note: usageNote, filename: `log-${d.id}-request.json` }) },
    { id: 'headers', label: t('logs.headers'), count: Object.keys(d.request_headers || {}).length, render: () => headersView(d.request_headers) },
  ];
  const hdr = { id: 'headers', label: t('logs.headers'), count: Object.keys(d.response_headers || {}).length, render: () => headersView(d.response_headers) };
  const resTabs = d.websocket
    ? [{ id: 'messages', label: t('logs.wsMessages'), render: () => bodyView(d.response_body, { truncated: d.truncated, filename: `log-${d.id}-messages.json` }) },
      ...(d.response_text ? [{ id: 'text', label: t('logs.text'), render: () => textView(d) }] : []), hdr]
    : d.stream
      ? [{ id: 'text', label: t('logs.text'), render: () => textView(d) },
        { id: 'sse', label: t('lg.rawTab'), render: () => sseView(d.response_body, { filename: `log-${d.id}-response.sse.txt` }) }, hdr]
      : [{ id: 'body', label: t('logs.body'), render: () => bodyView(d.response_body, { truncated: d.truncated, filename: `log-${d.id}-response.json` }) },
        ...(d.response_text || d.reasoning_text ? [{ id: 'text', label: t('logs.text'), render: () => textView(d) }] : []), hdr];
  const con = conclusion(d);
  const tokPfx = d.token_prefix ? `${d.token_prefix}…` : '';
  const idLine = (v, label) => (v ? h('span', { class: 'id-line' }, h('span', { class: 'mono small break' }, v), copyButton(v, { iconOnly: true, label })) : null);
  const curlBtn = h('button', { class: 'btn btn-sm', type: 'button', 'data-copy': 'curl', onclick: (e) => copyCurl(d.id, e.currentTarget) }, icon('terminal', 14), h('span', null, t('logs.copyCurl')));
  const attempts = attemptsView(d);
  const section = (title, body, extra) => h('section', { class: 'ld-section' }, h('h4', { class: 'ld-h' }, h('span', null, title), extra || null), body);
  const more = h('details', { class: 'ld-more' }, h('summary', null, t('lg.more')), kv([
    [t('logs.clientIp'), d.client_ip ? h('span', { class: 'mono' }, d.client_ip) : null],
    [t('logs.userAgent'), d.user_agent ? h('span', { class: 'mono small break' }, d.user_agent) : null],
    d.query ? [t('logs.query'), h('span', { class: 'mono small break' }, d.query)] : null,
  ]));
  return h('div', { class: 'log-detail' },
    // 1. summary
    section(t('lg.summary'), h('div', { class: 'ld-summary' },
      h('div', { class: `ld-hero ld-${con.tone}` }, statusBadge(d.status),
        (d.attempts || 0) > 1 ? badge(`×${d.attempts}`, 'warning', { class: 'badge-tries', title: t('logs.retriedN', { n: d.attempts }) }) : null,
        h('span', { class: 'ld-conclusion' }, con.text),
        con.link ? h('a', { class: 'link', href: con.link.hash }, `${con.link.label} →`) : null),
      d.error ? h('div', { class: 'ld-raw' }, h('span', { class: 'muted small' }, t('lg.rawError')), h('code', { class: 'mono small break' }, d.error)) : null,
      d.truncated ? h('div', { class: 'callout callout-warning' }, icon('alert', 16), h('span', null, t('logs.truncatedNotice'))) : null,
      kv([
        [t('logs.time'), h('span', null, fmtDateTime(d.created_at), h('span', { class: 'muted' }, ` · ${relTime(d.created_at)}`))],
        [t('common.model'), d.model ? h('span', { class: 'mono' }, d.model, d.stream ? h('span', { class: 'stream-tag' }, t('lg.stream')) : null, d.websocket ? h('span', { class: 'stream-tag' }, 'WebSocket') : null) : null],
        [t('common.token'), d.token_name ? (d.token_id != null ? navLink(cred(d.token_name, tokPfx), `#/tokens?open=${d.token_id}`, t('lg.openToken')) : cred(d.token_name, tokPfx)) : null],
        [t('common.key'), d.key_name ? (d.key_id != null ? navLink(cred(d.key_name, ''), `#/keys?open=${d.key_id}`, t('lg.openKey')) : d.key_name) : null],
        [t('logs.requestId'), idLine(d.request_id, t('logs.copyReqId'))],
        [t('logs.upstreamId'), idLine(d.upstream_id, t('lg.copyUpstreamId'))],
        [t('common.latency'), h('span', null, fmtMs(d.latency_ms), h('span', { class: 'muted', title: t('logs.ttfbClientHint') }, ` · ${t('lg.ttfb', { v: fmtMs(d.ttfb_ms) })}`))],
      ]),
      more,
      h('div', { class: 'detail-actions' },
        curlBtn,
        copyButton(() => d.request_body || '', { label: t('logs.copyReq') }),
        copyButton(() => d.response_body || '', { label: t('logs.copyRes') }),
        copyButton(() => d.response_text || '', { label: t('lg.copyText') })))),
    // 2. tokens & cost
    section(t('lg.costTitle'), costSection(d)),
    d.websocket_session ? wsSessionView(d.websocket_session) : null,
    // 3. upstream attempts
    attempts ? section(t('logs.attemptsTimeline'), attempts, h('span', { class: 'count-pill' }, String((d.upstream_attempts || []).length))) : null,
    // 4. request, 5. response
    pane(t('logs.request'), reqTabs, null),
    pane(t('logs.response'), resTabs, null));
}

// ---------- event detail ----------
export function eventDetailView(e, ctx = {}) {
  const d = e.detail || {};
  const tt = e.target_type;
  const hint = targetHint(e);
  const targetNode = h('span', null, h('span', { class: 'muted' }, `${targetTypeLabel(tt)} `), cred(targetText(e), hint));
  const target = (tt === 'key' || tt === 'token') && e.target_id
    ? navLink(targetNode, `#/${tt === 'key' ? 'keys' : 'tokens'}?open=${e.target_id}`, t(tt === 'key' ? 'lg.openKey' : 'lg.openToken'))
    : targetNode;
  const section = (title, body) => h('section', { class: 'ld-section' }, h('h4', { class: 'ld-h' }, h('span', null, title)), body);
  const changes = d.changes && typeof d.changes === 'object' ? Object.entries(d.changes) : [];
  const isQuota = (e.action || '').startsWith('token.quota_');
  const result = d.result && typeof d.result === 'object' ? d.result : null;
  const reqBtn = e.request_id && ctx.openRequest
    ? h('button', { type: 'button', class: 'btn btn-sm', onclick: (ev) => ctx.openRequest(e.request_id, ev.currentTarget) }, icon('logs', 14), h('span', null, t('lg.openRequest')))
    : null;
  return h('div', { class: 'log-detail' },
    section(t('lg.summary'), h('div', { class: 'ld-summary' },
      h('div', { class: `ld-hero ld-${e.level === 'error' ? 'danger' : e.level === 'warn' ? 'warning' : 'info'}` }, levelBadge(e.level), h('span', { class: 'ld-conclusion' }, eventSentence(e))),
      kv([
        [t('logs.time'), h('span', null, fmtDateTime(e.created_at), h('span', { class: 'muted' }, ` · ${relTime(e.created_at)}`))],
        [t('lg.col.type'), t(`lg.type.${e.type}`)],
        [t('lg.col.action'), actionLabel(e.action)],
        [t('lg.col.target'), tt ? target : null],
        [t('lg.col.actor'), actorLabel(e)],
        [t('lg.col.ip'), e.ip ? h('span', { class: 'mono' }, e.ip) : null],
        [t('logs.requestId'), e.request_id ? h('span', { class: 'id-line' }, h('span', { class: 'mono small break' }, e.request_id), copyButton(e.request_id, { iconOnly: true })) : null],
      ]))),
    changes.length ? section(t('lg.changes'), h('table', { class: 'cost-table changes-table' },
      h('thead', null, h('tr', null, h('th', null, t('lg.field')), h('th', null, t('lg.before')), h('th', null, t('lg.after')))),
      h('tbody', null, changes.map(([f, c]) => h('tr', null, h('td', null, fieldLabel(tt, f)),
        h('td', { class: 'mono small break' }, fmtChangeValue(c && c.before)), h('td', { class: 'mono small break' }, fmtChangeValue(c && c.after))))))) : null,
    isQuota ? section(t('lg.quotaRecord'), kv([
      [t('q.h.amount'), usdT(d.amount_ticks)],
      [t('q.h.before'), d.before_unlimited ? t('tokens.unlimitedState') : usdT(d.before_ticks)],
      [t('q.h.after'), d.unlimited ? t('tokens.unlimitedState') : usdT(d.after_ticks)],
      [t('q.h.used'), usdT(d.used_ticks)],
      [t('q.h.reason'), d.reason || null],
    ])) : null,
    result ? section(t('lg.result'), kv(Object.entries(result).map(([k, v]) => [k, h('span', { class: 'mono small break' }, fmtChangeValue(v))]))) : null,
    d.error ? section(t('lg.rawError'), h('code', { class: 'mono small break' }, d.error)) : null,
    reqBtn ? section(t('lg.related'), h('div', { class: 'detail-actions' }, reqBtn)) : null,
    h('details', { class: 'ld-more ld-raw-json' }, h('summary', null, t('lg.rawJson')), jsonBlock(e, `event-${e.id}.json`)));
}

/** Find the request log of a request id and return its id (null when it was purged). */
export async function findRequestLog(requestId, signal) {
  const r = await get('/api/v1/logs', { type: 'request', q: requestId, page_size: 5 }, { signal });
  const row = (r.data || []).find((x) => x.request_id === requestId) || (r.data || [])[0];
  return row ? row.id : null;
}
