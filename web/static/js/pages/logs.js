import {
  h, fmtInt, fmtUSD, fmtMs, fmtCompact, fmtBytes, fmtDateTime, fmtTimeShort, relTime, timeEl, debounce, tryParseJSON, highlightJSON,
  statusClass, toLocalInput, fromLocalInput, copyText, downloadText, utf8Bytes, fmtTotal, latestSignal,
} from '../util.js';
import { icon } from '../icons.js';
import { t } from '../i18n.js';
import { get } from '../api.js';
import {
  card, toast, toastError, openModal, badge, select, input, pager as pagerEl, setTableLoading, skeletonRows, tableEmptyRow, tableWrap,
  segmented, switchEl, button, copyButton, busy, tabs, kv, spinner, errorMessage,
} from '../ui.js';
import { cred, credText } from '../detail-drawer.js';

const PRESETS = ['15m', '1h', '24h', '7d', 'all', 'custom'];
const PRESET_MS = { '15m': 15 * 60e3, '1h': 3600e3, '24h': 86400e3, '7d': 7 * 86400e3 };
const STATUS_OPTS = ['', 'success', 'error', '2xx', '4xx', '5xx', '429', 'custom'];
const PATHS = ['/v1/chat/completions', '/v1/responses', '/v1/messages', '/v1/realtime', '/v1/tts', '/v1/stt', '/v1/completions', '/v1/embeddings', '/v1/images/generations', '/v1/images/edits', '/v1/videos/generations', '/v1/models'];
const ACTION_VARIANT = { success: 'success', retry: 'warning', return: 'neutral', cooldown: 'warning', disable: 'danger', verify: 'warning' };

function statusBadge(code) {
  return badge(code ? String(code) : 'ERR', statusClass(code), { class: 'badge-status mono' });
}
function methodTag(m) { return h('span', { class: `method method-${(m || 'GET').toLowerCase()}` }, m || 'GET'); }

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

let prettyPref = (() => { try { return localStorage.getItem('xr.logs.pretty') !== '0'; } catch { return true; } })();

function bodyView(text, { truncated, note, filename = 'body.txt' } = {}) {
  if (text == null || text === '') return emptyPane(t('logs.noBody'));
  const bytes = utf8Bytes(text);
  const looksJSON = /^\s*[[{]/.test(text);
  const big = text.length > HL_LIMIT;
  // Pretty-printing is on by default only for small bodies; large ones stay raw until asked.
  let pretty = looksJSON && !big && prettyPref;
  let parsed = null, prettyText = null;
  const parseOk = () => {
    if (parsed === null) { parsed = tryParseJSON(text); if (parsed.ok && big) prettyText = JSON.stringify(parsed.value, null, 2); }
    return parsed.ok;
  };
  const fname = filename.replace(/\.\w+$/, looksJSON ? '.json' : '.txt');
  const pre = h('pre', { class: 'code code-body' });
  const kindEl = h('span');
  const prev = previewer(pre, () => (pretty && big && prettyText ? prettyText : text), fname);
  const prettyBtn = looksJSON ? h('button', { type: 'button', class: 'pane-tool', title: t('logs.prettyHint') }, icon('braces', 12), h('span', null, t('logs.pretty'))) : null;
  const paint = () => {
    if (pretty && !parseOk()) pretty = false;
    if (pretty && !big) { pre.replaceChildren(highlightJSON(parsed.value)); prev.slot.replaceChildren(); }
    else prev.paint();
    const invalid = parsed !== null && !parsed.ok;
    kindEl.textContent = looksJSON && !invalid ? (pretty ? 'JSON' : `JSON · ${t('logs.raw')}`) : t('logs.raw');
    if (prettyBtn) { prettyBtn.classList.toggle('on', pretty); prettyBtn.setAttribute('aria-pressed', pretty ? 'true' : 'false'); prettyBtn.hidden = invalid; }
  };
  if (prettyBtn) prettyBtn.addEventListener('click', () => {
    pretty = !pretty;
    if (!big) { prettyPref = pretty; try { localStorage.setItem('xr.logs.pretty', pretty ? '1' : '0'); } catch { /* ignore */ } }
    paint();
  });
  paint();
  return h('div', { class: 'pane-content' },
    h('div', { class: 'pane-meta' }, kindEl, h('span', null, fmtBytes(bytes)), truncated ? badge(t('logs.truncated'), 'warning') : null,
      h('span', { class: 'pane-meta-spacer' }), prettyBtn, downloadTool(() => text, fname), expandToggle(pre)),
    note || null,
    prev.slot,
    pre);
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

function tokenBreakdown(d) {
  const items = [
    [t('tok.prompt'), d.prompt_tokens], [t('tok.cached'), d.cached_tokens], [t('tok.completion'), d.completion_tokens],
    [t('tok.reasoning'), d.reasoning_tokens], [t('tok.image'), d.image_tokens], [t('tok.total'), d.total_tokens],
  ].filter(([, v], i) => i === 0 || i === 2 || i === 5 || v);
  const extra = [];
  if (d.images) extra.push([t('logs.images'), fmtInt(d.images)]);
  if (d.video_seconds) extra.push([t('logs.videoSeconds'), `${d.video_seconds}s`]);
  return h('div', { class: 'tok-grid' }, items.map(([k, v]) => h('div', { class: 'tok-item' }, h('div', { class: 'tok-k' }, k), h('div', { class: 'tok-v mono' }, fmtInt(v || 0)))),
    extra.map(([k, v]) => h('div', { class: 'tok-item' }, h('div', { class: 'tok-k' }, k), h('div', { class: 'tok-v mono' }, v))));
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

/** 503 "no upstream API keys are available" (proxy error code no_upstream_keys). */
function isNoUpstreamKeys(d) {
  return /no_upstream_keys|no upstream API keys are available/i.test(d.error || '') ||
    (d.status === 503 && /no_upstream_keys/.test(d.response_body || ''));
}

function detailView(d, ctx) {
  // The gateway adds stream_options.include_usage to streaming chat completions (setting
  // inject_stream_usage); the logged body is the client's original, so say so.
  const isChat = /\/chat\/completions$/.test(d.path || '');
  const usageNote = d.stream && isChat && ctx && ctx.injectUsage !== false
    ? h('div', { class: 'pane-note' }, icon('info', 13), h('span', null, t('logs.includeUsageNote'))) : null;
  const stat = (label, value, sub, cls = '') => h('div', { class: `dstat ${cls}` }, h('div', { class: 'dstat-l' }, label), h('div', { class: 'dstat-v' }, value), sub ? h('div', { class: 'dstat-s' }, sub) : null);
  const reqTabs = [
    { id: 'body', label: t('logs.body'), render: () => bodyView(d.request_body, { truncated: d.truncated, note: usageNote, filename: `log-${d.id}-request.json` }) },
    { id: 'headers', label: t('logs.headers'), count: Object.keys(d.request_headers || {}).length, render: () => headersView(d.request_headers) },
  ];
  const resTabs = d.websocket
    ? [
      { id: 'messages', label: t('logs.wsMessages'), render: () => bodyView(d.response_body, { truncated: d.truncated, filename: `log-${d.id}-messages.json` }) },
      ...(d.response_text ? [{ id: 'text', label: t('logs.text'), render: () => textView(d) }] : []),
      { id: 'headers', label: t('logs.headers'), count: Object.keys(d.response_headers || {}).length, render: () => headersView(d.response_headers) },
    ]
    : d.stream
    ? [
      { id: 'text', label: t('logs.text'), render: () => textView(d) },
      { id: 'sse', label: t('logs.rawSse'), render: () => sseView(d.response_body, { filename: `log-${d.id}-response.sse.txt` }) },
      { id: 'headers', label: t('logs.headers'), count: Object.keys(d.response_headers || {}).length, render: () => headersView(d.response_headers) },
    ]
    : [
      { id: 'body', label: t('logs.body'), render: () => bodyView(d.response_body, { truncated: d.truncated, filename: `log-${d.id}-response.json` }) },
      ...(d.response_text || d.reasoning_text ? [{ id: 'text', label: t('logs.text'), render: () => textView(d) }] : []),
      { id: 'headers', label: t('logs.headers'), count: Object.keys(d.response_headers || {}).length, render: () => headersView(d.response_headers) },
    ];
  const curlBtn = h('button', { class: 'btn btn-sm', onclick: (e) => copyCurl(d.id, e.currentTarget) }, icon('terminal', 14), h('span', null, t('logs.copyCurl')));
  const link = (label, hash) => h('a', { href: hash, class: 'link', onclick: () => ctx && ctx.closeDetail && ctx.closeDetail() }, label);
  const attempts = attemptsView(d);
  return h('div', { class: 'log-detail' },
    h('div', { class: 'detail-actions' },
      copyButton(() => d.request_body || '', { label: t('logs.copyReq') }),
      copyButton(() => d.response_body || '', { label: t('logs.copyRes') }),
      curlBtn,
      copyButton(d.request_id || '', { label: t('logs.copyReqId') })),
    d.error ? h('div', { class: 'callout callout-danger' }, icon('xCircle', 16), h('div', { class: 'break' }, d.error)) : null,
    isNoUpstreamKeys(d) ? h('div', { class: 'callout callout-danger ll-nokeys' }, icon('key', 16),
      h('div', null, t('ll.noKeys'), ' ', link(t('ll.noKeysLink'), '#/keys'))) : null,
    d.truncated ? h('div', { class: 'callout callout-warning' }, icon('alert', 16), h('span', null, t('logs.truncatedNotice'))) : null,
    h('div', { class: 'dstats' },
      stat(t('common.status'), statusBadge(d.status), d.status === 499 ? t('logs.clientGone') : null),
      stat(t('common.latency'), fmtMs(d.latency_ms), h('span', { title: t('logs.ttfbClientHint') }, `${t('logs.ttfbClient')} ${fmtMs(d.ttfb_ms)}`)),
      stat(t('common.cost'), fmtUSD(d.cost_usd), d.cost_source ? badge(t(`costsrc.${d.cost_source}`) || d.cost_source, d.cost_source === 'upstream' ? 'success' : d.cost_source === 'computed' ? 'info' : 'neutral') : null),
      stat(t('ov.tokens'), fmtInt(d.total_tokens || 0), `${fmtCompact(d.prompt_tokens || 0)} → ${fmtCompact(d.completion_tokens || 0)}`),
      stat(t('logs.attempts'), String(d.attempts || (d.upstream_attempts || []).length || 0), (d.attempts || 1) > 1 ? t('logs.retried') : null, (d.attempts || 1) > 1 ? 'dstat-warn' : '')),
    h('div', { class: 'detail-grid' },
      h('div', { class: 'detail-block' }, h('h4', null, t('logs.request')), kv([
        [t('common.model'), d.model ? h('span', { class: 'mono' }, d.model) : null],
        [t('logs.stream'), d.stream ? badge(t('logs.streamYes'), 'accent') : t('logs.streamNo')],
        [t('common.token'), d.token_name ? (d.token_id != null ? link(cred(d.token_name, d.token_prefix ? `${d.token_prefix}…` : ''), `#/logs?token_id=${d.token_id}`) : cred(d.token_name, d.token_prefix ? `${d.token_prefix}…` : '')) : null],
        [t('common.key'), d.key_name ? (d.key_id != null ? link(d.key_name, `#/logs?key_id=${d.key_id}`) : d.key_name) : null],
        [t('logs.clientIp'), d.client_ip ? h('span', { class: 'mono' }, d.client_ip) : null],
        [t('logs.userAgent'), d.user_agent ? h('span', { class: 'mono small break' }, d.user_agent) : null],
        d.query ? [t('logs.query'), h('span', { class: 'mono small break' }, d.query)] : null,
        [t('logs.requestId'), d.request_id ? h('span', { class: 'id-line' }, h('span', { class: 'mono small break' }, d.request_id), copyButton(d.request_id, { iconOnly: true })) : null],
        [t('logs.upstreamId'), d.upstream_id ? h('span', { class: 'id-line' }, h('span', { class: 'mono small break' }, d.upstream_id), copyButton(d.upstream_id, { iconOnly: true })) : null],
        [t('logs.time'), h('span', null, fmtDateTime(d.created_at), h('span', { class: 'muted' }, ` · ${relTime(d.created_at)}`))],
      ])),
      h('div', { class: 'detail-block' }, h('h4', null, t('logs.usage')), tokenBreakdown(d))),
    d.websocket_session ? wsSessionView(d.websocket_session) : null,
    attempts ? h('div', { class: 'detail-block' }, h('h4', null, t('logs.attemptsTimeline'), h('span', { class: 'count-pill' }, String((d.upstream_attempts || []).length))), attempts) : null,
    h('div', { class: 'panes' },
      pane(t('logs.request'), reqTabs, () => d.request_body || '', t('logs.copyBody')),
      pane(t('logs.response'), resTabs, () => d.response_body || '', t('logs.copyBody'))));
}

// ---------- page ----------
const PAGE_SIZES = [20, 50, 100, 200];
const DEF_PS = 50;
const lsGet = (k) => { try { return localStorage.getItem(k); } catch { return null; } };
const lsSet = (k, v) => { try { localStorage.setItem(k, v); } catch { /* ignore */ } };

/** View state from URL params. A bare #/logs falls back to the remembered page size. */
function stateFromParams(p) {
  const bare = !Object.keys(p).length;
  const ps = Number(p.page_size) || (bare ? Number(lsGet('xr.logs.ps')) : 0) || DEF_PS;
  return {
    preset: PRESETS.includes(p.range) ? p.range : (p.from || p.to ? 'custom' : '24h'),
    from: p.from || '', to: p.to || '',
    model: p.model || '', status: p.status || '', key_id: p.key_id || '', token_id: p.token_id || '', path: p.path || '', q: p.q || '',
    page: Math.max(1, Math.floor(Number(p.page)) || 1), pageSize: PAGE_SIZES.includes(ps) ? ps : DEF_PS,
    openId: p.open ? Number(p.open) : null,
  };
}

export function render(el, ctx) {
  const st = { ...stateFromParams(ctx.params), total: 0, totalKind: 'exact', rows: [], auto: lsGet('xr.logs.auto') === '1', loaded: false };
  const syncParams = (push = false) => ctx.setParams({
    range: st.preset !== '24h' && st.preset !== 'custom' ? st.preset : '', from: st.preset === 'custom' ? st.from : '', to: st.preset === 'custom' ? st.to : '',
    model: st.model, status: st.status, key_id: st.key_id, token_id: st.token_id, path: st.path, q: st.q,
    page: st.page > 1 ? st.page : '', page_size: st.pageSize !== DEF_PS ? st.pageSize : '', open: st.openId || '',
  }, { push });

  // --- controls ---
  const autoSw = switchEl({ checked: st.auto, label: t('logs.autoRefresh'), onChange: (v) => { st.auto = v; lsSet('xr.logs.auto', v ? '1' : '0'); setupAuto(); } });
  const refreshBtn = button('', { icon: 'refresh', variant: 'ghost', size: 'icon', attrs: { title: t('common.refresh'), 'aria-label': t('common.refresh') }, onClick: () => load() });
  ctx.setActions(autoSw, refreshBtn);

  // Discrete filter changes add a history entry; typing replaces the current one.
  const changed = (push = true) => { st.page = 1; syncParams(push); paintFilterToggle(); load(); };
  const fromIn = input({ type: 'datetime-local', onchange: (e) => { st.from = fromLocalInput(e.target.value) || ''; changed(); } });
  const toIn = input({ type: 'datetime-local', onchange: (e) => { st.to = fromLocalInput(e.target.value) || ''; changed(); } });
  const customRange = h('div', { class: 'custom-range' }, fromIn, h('span', { class: 'muted' }, '→'), toIn);
  const presetSel = segmented({
    size: 'seg-sm', options: PRESETS.map((v) => ({ value: v, label: t(`logs.preset.${v}`) })), value: st.preset,
    onChange: (v) => {
      st.preset = v;
      customRange.hidden = v !== 'custom';
      if (v === 'custom' && !st.from) { st.from = new Date(Date.now() - 86400e3).toISOString(); fromIn.value = toLocalInput(st.from); }
      changed();
    },
  });
  const qIn = input({ type: 'search', placeholder: t('logs.qPh'), oninput: debounce((e) => { st.q = e.target.value.trim(); changed(false); }, 350) });
  const dlModels = h('datalist', { id: 'dl-log-models' });
  const modelIn = input({ placeholder: t('logs.anyModel'), list: 'dl-log-models', class: 'input mono', oninput: debounce((e) => { st.model = e.target.value.trim(); changed(false); }, 400) });
  const customStatus = input({ type: 'number', min: 100, max: 599, placeholder: '503', class: 'input input-status',
    oninput: debounce((e) => { st.status = e.target.value.trim(); changed(false); }, 400) });
  const statusSel = select(STATUS_OPTS.map((v) => ({ value: v, label: t(`logs.status.${v || 'all'}`) })), '', {
    onchange: (e) => {
      const v = e.target.value;
      customStatus.hidden = v !== 'custom';
      if (v === 'custom') { customStatus.focus(); st.status = customStatus.value.trim(); if (!st.status) return; } else st.status = v;
      changed();
    },
  });
  const keySel = select([{ value: '', label: t('logs.anyKey') }], '', { onchange: (e) => { st.key_id = e.target.value; changed(); } });
  const tokenSel = select([{ value: '', label: t('logs.anyToken') }], '', { onchange: (e) => { st.token_id = e.target.value; changed(); } });
  const dlPaths = h('datalist', { id: 'dl-log-paths' }, PATHS.map((x) => h('option', { value: x })));
  const pathIn = input({ placeholder: t('logs.anyPath'), list: 'dl-log-paths', class: 'input mono', oninput: debounce((e) => { st.path = e.target.value.trim(); changed(false); }, 400) });
  const resetBtn = button(t('logs.resetFilters'), { icon: 'x', variant: 'ghost', size: 'sm', onClick: () => {
    Object.assign(st, { preset: '24h', from: '', to: '', model: '', status: '', key_id: '', token_id: '', path: '', q: '' });
    paintControls();
    changed();
  } });
  const lbl = (text, ctl) => h('label', { class: 'filter' }, h('span', { class: 'filter-label' }, text), ctl);

  /** Ensure a <select> has an option for `v` (e.g. a key id from the URL before the list loads). */
  const ensureOpt = (sel, v) => { if (v && ![...sel.options].some((o) => o.value === String(v))) sel.appendChild(h('option', { value: String(v) }, `#${v}`)); };
  /** Push st into every filter control (initial render, reset, back/forward). */
  function paintControls() {
    presetSel.setValue(st.preset);
    customRange.hidden = st.preset !== 'custom';
    fromIn.value = toLocalInput(st.from); toIn.value = toLocalInput(st.to);
    qIn.value = st.q; modelIn.value = st.model; pathIn.value = st.path;
    const custom = st.status && !STATUS_OPTS.includes(st.status);
    statusSel.value = custom ? 'custom' : st.status;
    customStatus.hidden = !custom && st.status !== 'custom';
    customStatus.value = custom ? st.status : '';
    ensureOpt(keySel, st.key_id); keySel.value = st.key_id;
    ensureOpt(tokenSel, st.token_id); tokenSel.value = st.token_id;
    paintFilterToggle();
  }

  const filterToggle = h('button', { type: 'button', class: 'btn btn-sm filters-toggle', 'aria-expanded': 'false', onclick: () => {
    const open = !filterBar.classList.contains('open');
    filterBar.classList.toggle('open', open);
    filterToggle.setAttribute('aria-expanded', open ? 'true' : 'false');
  } });
  const paintFilterToggle = () => {
    const n = [st.preset !== '24h', st.model, st.status, st.key_id, st.token_id, st.path].filter(Boolean).length;
    filterToggle.replaceChildren(icon('filter', 14), h('span', null, n ? t('logs.filtersN', { n }) : t('logs.filters')));
    filterToggle.classList.toggle('btn-active', n > 0);
  };
  const filterBar = h('div', { class: 'filters' },
    h('div', { class: 'filters-row' },
      presetSel, customRange,
      h('div', { class: 'filters-q-row' }, h('div', { class: 'search-input filters-q' }, icon('search', 15), qIn), filterToggle)),
    h('div', { class: 'filters-row filters-grid' },
      lbl(t('common.model'), modelIn),
      lbl(t('common.status'), h('div', { class: 'row-sm' }, statusSel, customStatus)),
      lbl(t('common.key'), keySel),
      lbl(t('common.token'), tokenSel),
      lbl(t('logs.path'), pathIn),
      h('div', { class: 'filter filter-reset' }, resetBtn)),
    dlModels, dlPaths);

  const COLS = 8;
  const tbody = h('tbody', null, skeletonRows(COLS, 10));
  const table = h('table', { class: 'table table-logs table-cards' },
    h('thead', null, h('tr', null,
      h('th', null, t('logs.time')), h('th', null, t('logs.request')), h('th', null, t('common.model')), h('th', null, t('common.status')),
      h('th', null, `${t('common.token')} / ${t('common.key')}`),
      h('th', { class: 'num', title: t('logs.ttfbClientHint') }, t('common.latency')), h('th', { class: 'num' }, t('ov.tokens')), h('th', { class: 'num col-cost' }, t('common.cost')))),
    tbody);
  const pager = pagerEl({
    sizes: PAGE_SIZES,
    onChange: (pg, ps) => {
      if (ps !== st.pageSize) lsSet('xr.logs.ps', ps);
      st.page = pg; st.pageSize = ps; syncParams(true); load({ keepScroll: false });
    },
  });
  const countEl = h('span', { class: 'toolbar-count' });
  const liveDot = h('span', { class: 'live-dot', hidden: !st.auto, title: t('logs.autoRefresh') });
  const exportBtn = button(t('logs.exportJson'), { icon: 'download', size: 'sm', variant: 'ghost', attrs: { title: t('logs.exportHint') }, onClick: () => {
    if (!st.rows.length) { toast(t('logs.exportEmpty'), 'info'); return; }
    const d = new Date(); const p2 = (n) => String(n).padStart(2, '0');
    const name = `xai-router-logs-${d.getFullYear()}${p2(d.getMonth() + 1)}${p2(d.getDate())}-${p2(d.getHours())}${p2(d.getMinutes())}-p${st.page}.json`;
    downloadText(name, JSON.stringify({ exported_at: d.toISOString(), filters: query(), total: st.total, total_kind: st.totalKind, page: st.page, page_size: st.pageSize, data: st.rows }, null, 2));
    toast(t('logs.exported', { n: st.rows.length }), 'success', 2500);
  } });
  const wrap = tableWrap(table, { maxHeight: 'calc(100vh - 330px)' });
  const region = h('div', { class: 'table-region' }, h('div', { class: 'loading-bar', 'aria-hidden': 'true' }), wrap);
  el.append(card({ flush: true, cls: 'card-logs', body: [filterBar, h('div', { class: 'toolbar toolbar-thin' }, countEl, liveDot, h('div', { class: 'toolbar-spacer' }), h('span', { class: 'muted small hide-sm' }, t('logs.clickHint')), exportBtn), region, pager] }));
  paintControls();

  function row(r) {
    const tokTip = [`${t('tok.prompt')}: ${fmtInt(r.prompt_tokens)}`, `${t('tok.completion')}: ${fmtInt(r.completion_tokens)}`, `${t('tok.cached')}: ${fmtInt(r.cached_tokens)}`, `${t('tok.reasoning')}: ${fmtInt(r.reasoning_tokens)}`, r.image_tokens ? `${t('tok.image')}: ${fmtInt(r.image_tokens)}` : null, `${t('tok.total')}: ${fmtInt(r.total_tokens)}`].filter(Boolean).join('\n');
    let tokCell;
    if (r.images) tokCell = h('span', null, `${r.images} ${t('logs.imgShort')}`);
    else if (r.video_seconds) tokCell = h('span', null, `${r.video_seconds}s`);
    else if (r.total_tokens || r.prompt_tokens) tokCell = h('span', null, fmtCompact(r.prompt_tokens || 0), h('span', { class: 'muted' }, ' → '), fmtCompact(r.completion_tokens || 0));
    else tokCell = h('span', { class: 'muted' }, '—');
    const retried = (r.attempts || 0) > 1;
    const tr = h('tr', { class: `clickable ${r.status >= 400 || !r.status ? 'row-err' : ''} ${r.id === st.openId ? 'row-active' : ''}`, 'data-id': r.id, onclick: () => openDetail(r.id) },
      h('td', { class: 'nowrap m-meta m-time' }, h('div', { class: 'mono', title: fmtDateTime(r.created_at) }, fmtTimeShort(r.created_at)), h('div', { class: 'cell-sub', 'data-rel': r.created_at }, relTime(r.created_at))),
      h('td', { class: 'cell-req m-title' }, h('div', { class: 'req-line' }, methodTag(r.method), h('span', { class: 'mono path', title: r.path }, r.path)), r.error ? h('div', { class: 'cell-sub text-danger truncate hide-m', title: r.error }, r.error) : null),
      h('td', { class: 'cell-model m-meta' }, h('div', { class: 'model-line' }, r.model ? h('span', { class: 'mono model-name', title: r.model }, r.model) : h('span', { class: 'muted' }, '—'), r.stream ? h('span', { class: 'stream-tag', title: t('logs.streamYes') }, icon('stream', 12), 'SSE') : null, r.websocket ? h('span', { class: 'stream-tag', title: 'WebSocket' }, icon('stream', 12), 'WS') : null)),
      h('td', { class: 'nowrap m-badge' }, statusBadge(r.status), retried ? badge(`×${r.attempts}`, 'warning', { class: 'badge-tries', title: t('logs.retriedN', { n: r.attempts }) }) : null),
      h('td', { class: 'truncate-cell m-meta m-who' },
        h('div', { class: 'truncate', title: `${t('common.token')}: ${credText(r.token_name, r.token_prefix ? `${r.token_prefix}…` : '')}` }, r.token_name ? cred(r.token_name, r.token_prefix ? `${r.token_prefix}…` : '') : h('span', { class: 'muted' }, '—')),
        h('div', { class: 'cell-sub truncate', title: `${t('common.key')}: ${r.key_name || '—'}` }, icon('key', 10), ' ', r.key_name || '—')),
      h('td', { class: `num mono m-meta ${r.latency_ms > 30000 ? 'text-warning' : ''}` }, h('div', null, fmtMs(r.latency_ms)), r.ttfb_ms ? h('div', { class: 'cell-sub', title: t('logs.ttfbClientHint') }, `${t('logs.ttfbShort')} ${fmtMs(r.ttfb_ms)}`) : null),
      h('td', { class: 'num mono m-meta', title: tokTip }, tokCell),
      h('td', { class: 'num mono m-meta col-cost' }, r.cost_usd ? fmtUSD(r.cost_usd) : h('span', { class: 'muted' }, '$0')),
      r.error ? h('td', { class: 'm-only-cell m-full' }, h('div', { class: 'cell-sub text-danger truncate', title: r.error }, r.error)) : null);
    return tr;
  }

  function query() {
    const q = { model: st.model, status: st.status, key_id: st.key_id, token_id: st.token_id, path: st.path, q: st.q, page: st.page, page_size: st.pageSize };
    if (st.preset === 'custom') { q.from = st.from; q.to = st.to; }
    else if (PRESET_MS[st.preset]) q.from = new Date(Date.now() - PRESET_MS[st.preset]).toISOString();
    return q;
  }

  // Each load aborts the one before it; the previous rows stay visible (dimmed) until the new ones arrive.
  const nextSignal = latestSignal(ctx.signal);
  let loading = false;
  async function load({ silent = false, keepScroll = true } = {}) {
    const signal = nextSignal();
    loading = true;
    if (!silent) { refreshBtn.classList.add('spin'); if (st.loaded) { setTableLoading(region, true); pager.setLoading(true); } }
    const t0 = performance.now();
    try {
      const res = await get('/api/v1/logs', query(), { signal });
      if (signal.aborted) return;
      st.rows = res.data || []; st.total = res.total || 0;
      st.totalKind = res.total_kind || (res.total_exact === false ? 'approx' : 'exact');
      st.loaded = true;
      st.lastMs = Math.round(performance.now() - t0);
      countEl.textContent = t('logs.matchN', { n: fmtTotal(st.total, st.totalKind) });
      countEl.title = st.totalKind === 'approx' ? t('common.totalApproxHint') : st.totalKind === 'at_least' ? t('common.totalAtLeastHint') : '';
      const filtered = st.model || st.status || st.key_id || st.token_id || st.path || st.q;
      let empty;
      if (!st.rows.length && st.page > 1) {
        const est = Math.max(1, Math.ceil(st.total / st.pageSize));
        const goPage = (pg) => { st.page = pg; syncParams(true); load({ keepScroll: false }); };
        empty = { icon: 'inbox', title: t('common.pastEnd'), desc: t('common.pastEndDesc'), action: h('div', { class: 'row-sm' },
          button(t('common.firstPage'), { icon: 'chevronsLeft', size: 'sm', onClick: () => goPage(1) }),
          est < st.page ? button(t('common.lastPageN', { n: fmtInt(est) }), { icon: 'chevronLeft', size: 'sm', onClick: () => goPage(est) }) : null) };
      }
      else if (filtered) empty = { icon: 'filter', title: t('logs.noMatch'), desc: t('logs.noMatchDesc') };
      else empty = { icon: 'logs', title: t('logs.empty'), desc: t('logs.emptyDesc') };
      tbody.replaceChildren(...(st.rows.length ? st.rows.map(row) : [tableEmptyRow(COLS, empty)]));
      if (!keepScroll) wrap.scrollTop = 0;
      pager.update({ page: st.page, pageSize: st.pageSize, total: st.total, totalKind: st.totalKind, hasMore: st.rows.length >= st.pageSize, rowCount: st.rows.length, loading: false });
    } catch (e) {
      if (e.name === 'AbortError' || signal.aborted) return;
      pager.setLoading(false);
      if (!silent) { toastError(e); tbody.replaceChildren(tableEmptyRow(COLS, { icon: 'alert', title: t('common.loadFailed'), desc: errorMessage(e) })); }
    } finally {
      if (!signal.aborted) { loading = false; refreshBtn.classList.remove('spin'); setTableLoading(region, false); }
    }
  }

  let injectUsage = null;
  get('/api/v1/settings', null, { signal: ctx.signal }).then((x) => { injectUsage = x ? x.inject_stream_usage !== false : null; }).catch(() => {});

  // --- selects data ---
  async function loadFilterData() {
    const [ks, ts, ms] = await Promise.allSettled([
      get('/api/v1/keys', { page_size: 200 }, { signal: ctx.signal }),
      get('/api/v1/tokens', { page_size: 200 }, { signal: ctx.signal }),
      get('/api/v1/models', null, { signal: ctx.signal }),
    ]);
    // Options read "name · prefix" so same-named tokens / keys can be told apart.
    const fill = (sel, items, anyLabel, cur, hint) => {
      const opts = [h('option', { value: '' }, anyLabel), ...items.map((x) => h('option', { value: String(x.id), selected: String(x.id) === String(cur) }, credText(x.name || `#${x.id}`, hint(x))))];
      if (cur && !items.some((x) => String(x.id) === String(cur))) opts.push(h('option', { value: cur, selected: true }, `#${cur}`));
      sel.replaceChildren(...opts);
    };
    if (ks.status === 'fulfilled') fill(keySel, ks.value.data || [], t('logs.anyKey'), st.key_id, (x) => x.key_masked || '');
    if (ts.status === 'fulfilled') fill(tokenSel, ts.value.data || [], t('logs.anyToken'), st.token_id, (x) => (x.prefix ? `${x.prefix}…` : ''));
    if (ms.status === 'fulfilled') dlModels.replaceChildren(...(ms.value.data || []).map((m) => h('option', { value: m.id })));
  }

  // --- detail drawer ---
  let drawer = null;
  const nextDetailSignal = latestSignal(ctx.signal);
  function markActive() { tbody.querySelectorAll('tr[data-id]').forEach((tr) => tr.classList.toggle('row-active', Number(tr.dataset.id) === st.openId)); }
  async function openDetail(id) {
    st.openId = id; syncParams(); markActive();
    const idx = st.rows.findIndex((r) => r.id === id);
    const prev = h('button', { class: 'btn btn-ghost btn-icon', title: `${t('logs.newer')} (↑ / k)`, 'aria-label': t('logs.newer'), disabled: idx <= 0, onclick: () => step(-1) }, icon('chevronLeft', 17));
    const next = h('button', { class: 'btn btn-ghost btn-icon', title: `${t('logs.older')} (↓ / j)`, 'aria-label': t('logs.older'), disabled: idx < 0 || idx >= st.rows.length - 1, onclick: () => step(1) }, icon('chevronRight', 17));
    const summary = st.rows[idx];
    const titleText = summary ? `${summary.method} ${summary.path}` : `${t('logs.log')} #${id}`;
    const loadingBody = h('div', { class: 'drawer-loading' }, spinner(22), h('span', { class: 'muted' }, t('common.loading')));
    if (drawer) { drawer.setTitle(titleText); drawer.setBody(loadingBody); drawer.el.querySelector('.modal-header-actions').replaceChildren(prev, next, drawer.el.querySelector('.modal-header-actions > button:last-child')); }
    else {
      drawer = openModal({
        kind: 'drawer', size: 'xl', title: titleText, subtitle: summary ? fmtDateTime(summary.created_at) : '', body: loadingBody, headerExtra: [prev, next],
        onClose: () => { drawer = null; st.openId = null; nextDetailSignal.abort(); syncParams(); markActive(); document.removeEventListener('keydown', onKey); },
      });
      document.addEventListener('keydown', onKey);
    }
    const thisDrawer = drawer;
    const signal = nextDetailSignal();
    try {
      const d = await get(`/api/v1/logs/${id}`, null, { signal });
      if (drawer !== thisDrawer || st.openId !== id) return;
      drawer.setTitle(`${d.method || 'GET'} ${d.path || ''}`);
      const sub = drawer.el.querySelector('.modal-subtitle');
      if (sub) sub.textContent = `${fmtDateTime(d.created_at)} · ${relTime(d.created_at)}${d.model ? ' · ' + d.model : ''}`;
      drawer.setBody(detailView(d, { closeDetail: () => drawer && drawer.close(), injectUsage }));
      drawer.body.scrollTop = 0;
    } catch (e) {
      if (e.name === 'AbortError' || signal.aborted) return;
      drawer && drawer.setBody(h('div', { class: 'drawer-loading' }, icon('alert', 22), h('span', null, errorMessage(e))));
      toastError(e);
    }
  }
  function step(dir) {
    const idx = st.rows.findIndex((r) => r.id === st.openId);
    const n = st.rows[idx + dir];
    if (n) openDetail(n.id);
  }
  function onKey(e) {
    if (!drawer || e.target.closest('input, textarea, select')) return;
    if (e.key === 'ArrowDown' || e.key === 'j') { e.preventDefault(); step(1); }
    else if (e.key === 'ArrowUp' || e.key === 'k') { e.preventDefault(); step(-1); }
  }

  // --- back / forward: apply the URL in place ---
  ctx.onParams((p) => {
    const next = stateFromParams(p);
    const openId = next.openId;
    delete next.openId;
    Object.assign(st, next);
    paintControls();
    load({ keepScroll: false });
    if (openId && openId !== st.openId) openDetail(openId);
    else if (!openId && drawer) drawer.close();
  });

  // --- auto refresh ---
  let autoTimer = null;
  function setupAuto() {
    clearInterval(autoTimer); autoTimer = null;
    liveDot.hidden = !st.auto;
    if (st.auto) autoTimer = setInterval(() => { if (!document.hidden && !loading && st.page === 1 && !document.querySelector('.overlay-modal')) load({ silent: true }); }, 5000);
  }

  syncParams();
  loadFilterData();
  load().then(() => { if (st.openId) openDetail(st.openId); });
  setupAuto();
  return () => { clearInterval(autoTimer); document.removeEventListener('keydown', onKey); };
}
