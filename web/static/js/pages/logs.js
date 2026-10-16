import {
  h, fmtInt, fmtUSD, fmtMs, fmtCompact, fmtBytes, fmtDateTime, fmtTimeShort, relTime, timeEl, debounce, tryParseJSON, highlightJSON,
  statusClass, toLocalInput, fromLocalInput, copyText,
} from '../util.js';
import { icon } from '../icons.js';
import { t } from '../i18n.js';
import { get } from '../api.js';
import {
  card, toast, toastError, openModal, badge, select, input, pagination, skeletonRows, tableEmptyRow, tableWrap,
  segmented, switchEl, button, copyButton, busy, tabs, kv, spinner,
} from '../ui.js';

const PRESETS = ['15m', '1h', '24h', '7d', 'all', 'custom'];
const PRESET_MS = { '15m': 15 * 60e3, '1h': 3600e3, '24h': 86400e3, '7d': 7 * 86400e3 };
const STATUS_OPTS = ['', 'success', 'error', '2xx', '4xx', '5xx', '429', 'custom'];
const PATHS = ['/v1/chat/completions', '/v1/responses', '/v1/messages', '/v1/realtime', '/v1/tts', '/v1/stt', '/v1/completions', '/v1/embeddings', '/v1/images/generations', '/v1/images/edits', '/v1/videos/generations', '/v1/models'];
const ACTION_VARIANT = { success: 'success', retry: 'warning', return: 'neutral', cooldown: 'warning', disable: 'danger' };

function statusBadge(code) {
  return badge(code ? String(code) : 'ERR', statusClass(code), { class: 'badge-status mono' });
}
function methodTag(m) { return h('span', { class: `method method-${(m || 'GET').toLowerCase()}` }, m || 'GET'); }

// ---------- detail view ----------
function emptyPane(label) { return h('div', { class: 'pane-empty' }, icon('inbox', 18), h('span', null, label)); }

function bodyView(text, { truncated } = {}) {
  if (text == null || text === '') return emptyPane(t('logs.noBody'));
  const parsed = tryParseJSON(text);
  const pre = h('pre', { class: 'code code-body' });
  if (parsed.ok) pre.appendChild(highlightJSON(parsed.value)); else pre.textContent = text;
  const sz = new Blob([text]).size;
  return h('div', { class: 'pane-content' },
    h('div', { class: 'pane-meta' }, h('span', null, parsed.ok ? 'JSON' : t('logs.raw')), h('span', null, `${fmtCompact(sz)} B`), truncated ? badge(t('logs.truncated'), 'warning') : null),
    pre);
}

function sseView(text) {
  if (!text) return emptyPane(t('logs.noBody'));
  const pre = h('pre', { class: 'code code-body code-sse' });
  const lines = text.split('\n');
  const limit = 6000;
  lines.slice(0, limit).forEach((line, i) => {
    const m = /^(data|event|id|retry):\s?/.exec(line);
    if (m) {
      pre.appendChild(h('span', { class: 'sse-field' }, m[0]));
      const rest = line.slice(m[0].length);
      pre.appendChild(h('span', { class: rest === '[DONE]' ? 'sse-done' : 'sse-data' }, rest));
    } else if (line.startsWith(':')) pre.appendChild(h('span', { class: 'sse-comment' }, line));
    else pre.appendChild(document.createTextNode(line));
    if (i < lines.length - 1) pre.appendChild(document.createTextNode('\n'));
  });
  if (lines.length > limit) pre.appendChild(h('span', { class: 'sse-comment' }, `\n… ${lines.length - limit} more lines (copy to see all)`));
  const events = lines.filter((l) => l.startsWith('data:')).length;
  return h('div', { class: 'pane-content' }, h('div', { class: 'pane-meta' }, h('span', null, 'text/event-stream'), h('span', null, t('logs.eventsN', { n: events }))), pre);
}

function textView(d) {
  if (!d.response_text && !d.reasoning_text) return emptyPane(t('logs.noText'));
  return h('div', { class: 'pane-content text-pane' },
    d.reasoning_text ? h('details', { class: 'reasoning', open: !d.response_text },
      h('summary', null, icon('layers', 14), t('logs.reasoning'), h('span', { class: 'muted small' }, ` · ${fmtCompact(d.reasoning_text.length)} ${t('logs.chars')}`)),
      h('div', { class: 'reasoning-text' }, d.reasoning_text)) : null,
    d.response_text ? h('div', { class: 'assistant-text' }, d.response_text) : h('div', { class: 'muted small' }, t('logs.noText')));
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
            h('span', { class: 'attempt-times mono muted' }, `+${fmtMs(offs[i])}`, ' · ', `TTFB ${fmtMs(a.ttfb_ms)}`, ' · ', fmtMs(a.latency_ms))),
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

function detailView(d, ctx) {
  const stat = (label, value, sub, cls = '') => h('div', { class: `dstat ${cls}` }, h('div', { class: 'dstat-l' }, label), h('div', { class: 'dstat-v' }, value), sub ? h('div', { class: 'dstat-s' }, sub) : null);
  const reqTabs = [
    { id: 'body', label: t('logs.body'), render: () => bodyView(d.request_body, { truncated: d.truncated }) },
    { id: 'headers', label: t('logs.headers'), count: Object.keys(d.request_headers || {}).length, render: () => headersView(d.request_headers) },
  ];
  const resTabs = d.websocket
    ? [
      { id: 'messages', label: t('logs.wsMessages'), render: () => bodyView(d.response_body, { truncated: d.truncated }) },
      ...(d.response_text ? [{ id: 'text', label: t('logs.text'), render: () => textView(d) }] : []),
      { id: 'headers', label: t('logs.headers'), count: Object.keys(d.response_headers || {}).length, render: () => headersView(d.response_headers) },
    ]
    : d.stream
    ? [
      { id: 'text', label: t('logs.text'), render: () => textView(d) },
      { id: 'sse', label: t('logs.rawSse'), render: () => sseView(d.response_body) },
      { id: 'headers', label: t('logs.headers'), count: Object.keys(d.response_headers || {}).length, render: () => headersView(d.response_headers) },
    ]
    : [
      { id: 'body', label: t('logs.body'), render: () => bodyView(d.response_body, { truncated: d.truncated }) },
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
    d.truncated ? h('div', { class: 'callout callout-warning' }, icon('alert', 16), h('span', null, t('logs.truncatedNotice'))) : null,
    h('div', { class: 'dstats' },
      stat(t('common.status'), statusBadge(d.status), d.status === 499 ? t('logs.clientGone') : null),
      stat(t('common.latency'), fmtMs(d.latency_ms), `TTFB ${fmtMs(d.ttfb_ms)}`),
      stat(t('common.cost'), fmtUSD(d.cost_usd), d.cost_source ? badge(t(`costsrc.${d.cost_source}`) || d.cost_source, d.cost_source === 'upstream' ? 'success' : d.cost_source === 'computed' ? 'info' : 'neutral') : null),
      stat(t('ov.tokens'), fmtInt(d.total_tokens || 0), `${fmtCompact(d.prompt_tokens || 0)} → ${fmtCompact(d.completion_tokens || 0)}`),
      stat(t('logs.attempts'), String(d.attempts || (d.upstream_attempts || []).length || 0), (d.attempts || 1) > 1 ? t('logs.retried') : null, (d.attempts || 1) > 1 ? 'dstat-warn' : '')),
    h('div', { class: 'detail-grid' },
      h('div', { class: 'detail-block' }, h('h4', null, t('logs.request')), kv([
        [t('common.model'), d.model ? h('span', { class: 'mono' }, d.model) : null],
        [t('logs.stream'), d.stream ? badge(t('logs.streamYes'), 'accent') : t('logs.streamNo')],
        [t('common.token'), d.token_name ? (d.token_id != null ? link(d.token_name, `#/logs?token_id=${d.token_id}`) : d.token_name) : null],
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
export function render(el, ctx) {
  const p = ctx.params;
  const st = {
    preset: PRESETS.includes(p.range) ? p.range : (p.from || p.to ? 'custom' : '24h'),
    from: p.from || '', to: p.to || '',
    model: p.model || '', status: p.status || '', key_id: p.key_id || '', token_id: p.token_id || '', path: p.path || '', q: p.q || '',
    page: Number(p.page) || 1, pageSize: Number(localStorage.getItem('xr.logs.ps')) || 50,
    total: 0, rows: [], auto: localStorage.getItem('xr.logs.auto') === '1', openId: p.open ? Number(p.open) : null,
  };
  let statusCustom = st.status && !STATUS_OPTS.includes(st.status) ? st.status : '';
  const syncParams = () => ctx.setParams({
    range: st.preset !== '24h' && st.preset !== 'custom' ? st.preset : '', from: st.preset === 'custom' ? st.from : '', to: st.preset === 'custom' ? st.to : '',
    model: st.model, status: st.status, key_id: st.key_id, token_id: st.token_id, path: st.path, q: st.q, page: st.page > 1 ? st.page : '', open: st.openId || '',
  });

  // --- controls ---
  const autoSw = switchEl({ checked: st.auto, label: t('logs.autoRefresh'), onChange: (v) => { st.auto = v; try { localStorage.setItem('xr.logs.auto', v ? '1' : '0'); } catch { /* */ } setupAuto(); } });
  const refreshBtn = button('', { icon: 'refresh', variant: 'ghost', size: 'icon', attrs: { title: t('common.refresh'), 'aria-label': t('common.refresh') }, onClick: () => load() });
  ctx.setActions(autoSw, refreshBtn);

  const changed = () => { st.page = 1; syncParams(); load(); };
  const fromIn = input({ type: 'datetime-local', value: toLocalInput(st.from), onchange: (e) => { st.from = fromLocalInput(e.target.value) || ''; changed(); } });
  const toIn = input({ type: 'datetime-local', value: toLocalInput(st.to), onchange: (e) => { st.to = fromLocalInput(e.target.value) || ''; changed(); } });
  const customRange = h('div', { class: 'custom-range', hidden: st.preset !== 'custom' }, fromIn, h('span', { class: 'muted' }, '→'), toIn);
  const presetSel = segmented({
    size: 'seg-sm', options: PRESETS.map((v) => ({ value: v, label: t(`logs.preset.${v}`) })), value: st.preset,
    onChange: (v) => {
      st.preset = v;
      customRange.hidden = v !== 'custom';
      if (v === 'custom' && !st.from) { st.from = new Date(Date.now() - 86400e3).toISOString(); fromIn.value = toLocalInput(st.from); }
      changed();
    },
  });
  const qIn = input({ type: 'search', value: st.q, placeholder: t('logs.qPh'), oninput: debounce((e) => { st.q = e.target.value.trim(); changed(); }, 350) });
  const dlModels = h('datalist', { id: 'dl-log-models' });
  const modelIn = input({ value: st.model, placeholder: t('logs.anyModel'), list: 'dl-log-models', class: 'input mono', oninput: debounce((e) => { st.model = e.target.value.trim(); changed(); }, 400) });
  const customStatus = input({ type: 'number', min: 100, max: 599, value: statusCustom, placeholder: '503', class: 'input input-status', hidden: st.status === '' || STATUS_OPTS.includes(st.status) && st.status !== 'custom',
    oninput: debounce((e) => { st.status = e.target.value.trim(); changed(); }, 400) });
  const statusSel = select(STATUS_OPTS.map((v) => ({ value: v, label: t(`logs.status.${v || 'all'}`) })), statusCustom ? 'custom' : st.status, {
    onchange: (e) => {
      const v = e.target.value;
      customStatus.hidden = v !== 'custom';
      if (v === 'custom') { customStatus.focus(); st.status = customStatus.value.trim(); if (!st.status) return; } else st.status = v;
      changed();
    },
  });
  const keySel = select([{ value: '', label: t('logs.anyKey') }], st.key_id, { onchange: (e) => { st.key_id = e.target.value; changed(); } });
  const tokenSel = select([{ value: '', label: t('logs.anyToken') }], st.token_id, { onchange: (e) => { st.token_id = e.target.value; changed(); } });
  const dlPaths = h('datalist', { id: 'dl-log-paths' }, PATHS.map((x) => h('option', { value: x })));
  const pathIn = input({ value: st.path, placeholder: t('logs.anyPath'), list: 'dl-log-paths', class: 'input mono', oninput: debounce((e) => { st.path = e.target.value.trim(); changed(); }, 400) });
  const resetBtn = button(t('logs.resetFilters'), { icon: 'x', variant: 'ghost', size: 'sm', onClick: () => {
    Object.assign(st, { preset: '24h', from: '', to: '', model: '', status: '', key_id: '', token_id: '', path: '', q: '' });
    presetSel.setValue('24h'); customRange.hidden = true; fromIn.value = ''; toIn.value = ''; qIn.value = ''; modelIn.value = ''; statusSel.value = ''; customStatus.hidden = true; customStatus.value = ''; keySel.value = ''; tokenSel.value = ''; pathIn.value = '';
    changed();
  } });
  const lbl = (text, ctl) => h('label', { class: 'filter' }, h('span', { class: 'filter-label' }, text), ctl);

  const filterBar = h('div', { class: 'filters' },
    h('div', { class: 'filters-row' },
      presetSel, customRange,
      h('div', { class: 'search-input filters-q' }, icon('search', 15), qIn)),
    h('div', { class: 'filters-row filters-grid' },
      lbl(t('common.model'), modelIn),
      lbl(t('common.status'), h('div', { class: 'row-sm' }, statusSel, customStatus)),
      lbl(t('common.key'), keySel),
      lbl(t('common.token'), tokenSel),
      lbl(t('logs.path'), pathIn),
      h('div', { class: 'filter filter-reset' }, resetBtn)),
    dlModels, dlPaths);

  const COLS = 10;
  const tbody = h('tbody', null, skeletonRows(COLS, 10));
  const table = h('table', { class: 'table table-logs' },
    h('thead', null, h('tr', null,
      h('th', null, t('logs.time')), h('th', null, t('logs.request')), h('th', null, t('common.model')), h('th', null, t('common.status')),
      h('th', { class: 'num' }, t('logs.attemptsShort')), h('th', null, `${t('common.token')} / ${t('common.key')}`),
      h('th', { class: 'num' }, 'TTFB'), h('th', { class: 'num' }, t('common.latency')), h('th', { class: 'num' }, t('ov.tokens')), h('th', { class: 'num' }, t('common.cost')))),
    tbody);
  const pager = h('div');
  const countEl = h('span', { class: 'toolbar-count' });
  const liveDot = h('span', { class: 'live-dot', hidden: !st.auto, title: t('logs.autoRefresh') });
  el.append(card({ flush: true, cls: 'card-logs', body: [filterBar, h('div', { class: 'toolbar toolbar-thin' }, countEl, liveDot, h('div', { class: 'toolbar-spacer' }), h('span', { class: 'muted small hide-sm' }, t('logs.clickHint'))), tableWrap(table, { maxHeight: 'calc(100vh - 330px)' }), pager] }));

  function row(r) {
    const tokTip = [`${t('tok.prompt')}: ${fmtInt(r.prompt_tokens)}`, `${t('tok.completion')}: ${fmtInt(r.completion_tokens)}`, `${t('tok.cached')}: ${fmtInt(r.cached_tokens)}`, `${t('tok.reasoning')}: ${fmtInt(r.reasoning_tokens)}`, r.image_tokens ? `${t('tok.image')}: ${fmtInt(r.image_tokens)}` : null, `${t('tok.total')}: ${fmtInt(r.total_tokens)}`].filter(Boolean).join('\n');
    let tokCell;
    if (r.images) tokCell = h('span', null, `${r.images} ${t('logs.imgShort')}`);
    else if (r.video_seconds) tokCell = h('span', null, `${r.video_seconds}s`);
    else if (r.total_tokens || r.prompt_tokens) tokCell = h('span', null, fmtCompact(r.prompt_tokens || 0), h('span', { class: 'muted' }, ' → '), fmtCompact(r.completion_tokens || 0));
    else tokCell = h('span', { class: 'muted' }, '—');
    const tr = h('tr', { class: `clickable ${r.status >= 400 || !r.status ? 'row-err' : ''} ${r.id === st.openId ? 'row-active' : ''}`, 'data-id': r.id, onclick: () => openDetail(r.id) },
      h('td', { class: 'nowrap' }, h('div', { class: 'mono', title: fmtDateTime(r.created_at) }, fmtTimeShort(r.created_at)), h('div', { class: 'cell-sub', 'data-rel': r.created_at }, relTime(r.created_at))),
      h('td', { class: 'cell-req' }, h('div', { class: 'req-line' }, methodTag(r.method), h('span', { class: 'mono path' }, r.path)), r.error ? h('div', { class: 'cell-sub text-danger truncate', title: r.error }, r.error) : null),
      h('td', null, h('div', { class: 'model-line' }, r.model ? h('span', { class: 'mono' }, r.model) : h('span', { class: 'muted' }, '—'), r.stream ? h('span', { class: 'stream-tag', title: t('logs.streamYes') }, icon('stream', 12), 'SSE') : null, r.websocket ? h('span', { class: 'stream-tag', title: 'WebSocket' }, icon('stream', 12), 'WS') : null)),
      h('td', null, statusBadge(r.status)),
      h('td', { class: 'num' }, (r.attempts || 0) > 1 ? badge(`×${r.attempts}`, 'warning', { title: t('logs.retriedN', { n: r.attempts }) }) : h('span', { class: 'muted' }, r.attempts ? '1' : '—')),
      h('td', { class: 'truncate-cell' },
        h('div', { class: 'truncate', title: `${t('common.token')}: ${r.token_name || '—'}` }, r.token_name || h('span', { class: 'muted' }, '—')),
        h('div', { class: 'cell-sub truncate', title: `${t('common.key')}: ${r.key_name || '—'}` }, icon('key', 10), ' ', r.key_name || '—')),
      h('td', { class: 'num mono' }, r.ttfb_ms ? fmtMs(r.ttfb_ms) : h('span', { class: 'muted' }, '—')),
      h('td', { class: `num mono ${r.latency_ms > 30000 ? 'text-warning' : ''}` }, fmtMs(r.latency_ms)),
      h('td', { class: 'num mono', title: tokTip }, tokCell),
      h('td', { class: 'num mono' }, r.cost_usd ? fmtUSD(r.cost_usd) : h('span', { class: 'muted' }, '$0')));
    return tr;
  }

  function query() {
    const q = { model: st.model, status: st.status, key_id: st.key_id, token_id: st.token_id, path: st.path, q: st.q, page: st.page, page_size: st.pageSize };
    if (st.preset === 'custom') { q.from = st.from; q.to = st.to; }
    else if (PRESET_MS[st.preset]) q.from = new Date(Date.now() - PRESET_MS[st.preset]).toISOString();
    return q;
  }

  let loadSeq = 0, loading = false;
  async function load({ silent = false } = {}) {
    const seq = ++loadSeq;
    loading = true;
    if (!silent) refreshBtn.classList.add('spin');
    try {
      const res = await get('/api/v1/logs', query(), { signal: ctx.signal });
      if (seq !== loadSeq) return;
      st.rows = res.data || []; st.total = res.total || 0;
      countEl.textContent = t('logs.matchN', { n: fmtInt(st.total) });
      const filtered = st.model || st.status || st.key_id || st.token_id || st.path || st.q;
      tbody.replaceChildren(...(st.rows.length ? st.rows.map(row) : [tableEmptyRow(COLS, filtered ? { icon: 'filter', title: t('logs.noMatch'), desc: t('logs.noMatchDesc') } : { icon: 'logs', title: t('logs.empty'), desc: t('logs.emptyDesc') })]));
      pager.replaceChildren(pagination({ page: st.page, pageSize: st.pageSize, total: st.total, onChange: (pg, ps) => { st.page = pg; st.pageSize = ps; try { localStorage.setItem('xr.logs.ps', ps); } catch { /* */ } syncParams(); load(); } }));
    } catch (e) {
      if (e.name === 'AbortError') return;
      if (!silent) { toastError(e); tbody.replaceChildren(tableEmptyRow(COLS, { icon: 'alert', title: t('common.loadFailed'), desc: e.message })); }
    } finally { loading = false; refreshBtn.classList.remove('spin'); }
  }

  // --- selects data ---
  async function loadFilterData() {
    const [ks, ts, ms] = await Promise.allSettled([
      get('/api/v1/keys', { page_size: 200 }, { signal: ctx.signal }),
      get('/api/v1/tokens', { page_size: 200 }, { signal: ctx.signal }),
      get('/api/v1/models', null, { signal: ctx.signal }),
    ]);
    const fill = (sel, items, anyLabel, cur) => {
      const opts = [h('option', { value: '' }, anyLabel), ...items.map((x) => h('option', { value: String(x.id), selected: String(x.id) === String(cur) }, x.name || `#${x.id}`))];
      if (cur && !items.some((x) => String(x.id) === String(cur))) opts.push(h('option', { value: cur, selected: true }, `#${cur}`));
      sel.replaceChildren(...opts);
    };
    if (ks.status === 'fulfilled') fill(keySel, ks.value.data || [], t('logs.anyKey'), st.key_id);
    else if (st.key_id) fill(keySel, [], t('logs.anyKey'), st.key_id);
    if (ts.status === 'fulfilled') fill(tokenSel, ts.value.data || [], t('logs.anyToken'), st.token_id);
    else if (st.token_id) fill(tokenSel, [], t('logs.anyToken'), st.token_id);
    if (ms.status === 'fulfilled') dlModels.replaceChildren(...(ms.value.data || []).map((m) => h('option', { value: m.id })));
  }

  // --- detail drawer ---
  let drawer = null;
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
        onClose: () => { drawer = null; st.openId = null; syncParams(); markActive(); document.removeEventListener('keydown', onKey); },
      });
      document.addEventListener('keydown', onKey);
    }
    const thisDrawer = drawer;
    try {
      const d = await get(`/api/v1/logs/${id}`, null, { signal: ctx.signal });
      if (drawer !== thisDrawer || st.openId !== id) return;
      drawer.setTitle(`${d.method || 'GET'} ${d.path || ''}`);
      const sub = drawer.el.querySelector('.modal-subtitle');
      if (sub) sub.textContent = `${fmtDateTime(d.created_at)} · ${relTime(d.created_at)}${d.model ? ' · ' + d.model : ''}`;
      drawer.setBody(detailView(d, { closeDetail: () => drawer && drawer.close() }));
      drawer.body.scrollTop = 0;
    } catch (e) {
      if (e.name === 'AbortError') return;
      drawer && drawer.setBody(h('div', { class: 'drawer-loading' }, icon('alert', 22), h('span', null, e.message)));
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

  // --- auto refresh ---
  let autoTimer = null;
  function setupAuto() {
    clearInterval(autoTimer); autoTimer = null;
    liveDot.hidden = !st.auto;
    if (st.auto) autoTimer = setInterval(() => { if (!document.hidden && !loading && st.page === 1 && !document.querySelector('.overlay-modal')) load({ silent: true }); }, 5000);
  }

  loadFilterData();
  load().then(() => { if (st.openId) openDetail(st.openId); });
  setupAuto();
  return () => { clearInterval(autoTimer); document.removeEventListener('keydown', onKey); };
}
