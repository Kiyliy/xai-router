// The unified log (v0.4.1, pm-spec 9): requests, audit (operations) and system events in one
// table. Type multi-select, "only warnings and errors", second-row filters that follow the
// selected types, everything in the URL. type=request alone pages by number; any other
// selection is merged newest first and pages with a cursor.
import {
  h, fmtInt, fmtUSD, fmtMs, fmtCompact, fmtDateTime, fmtTimeShort, relTime, debounce, toLocalInput, fromLocalInput, downloadText,
  fmtTotal, latestSignal,
} from '../util.js';
import { icon } from '../icons.js';
import { t, hasT } from '../i18n.js';
import { get } from '../api.js';
import {
  card, toast, toastError, openModal, badge, select, input, pager as pagerEl, setTableLoading, skeletonRows, tableEmptyRow, tableWrap,
  segmented, switchEl, button, spinner, errorMessage,
} from '../ui.js';
import { cred, credText } from '../detail-drawer.js';
import { statusBadge, methodTag, requestDetailView, eventDetailView, findRequestLog } from '../log-detail.js';
import {
  actionLabel, actorLabel, targetTypeLabel, targetText, targetHint, eventSentence, typeTag, levelDot, requestReason, tokenCats,
} from '../events.js';

const TYPES = ['request', 'audit', 'system'];
const PRESETS = ['15m', '1h', '24h', '7d', 'all', 'custom'];
const PRESET_MS = { '15m': 15 * 60e3, '1h': 3600e3, '24h': 86400e3, '7d': 7 * 86400e3 };
const STATUS_OPTS = ['', 'success', 'error', '2xx', '4xx', '5xx', '429', 'custom'];
const PATHS = ['/v1/chat/completions', '/v1/responses', '/v1/messages', '/v1/realtime', '/v1/tts', '/v1/stt', '/v1/completions', '/v1/embeddings', '/v1/images/generations', '/v1/images/edits', '/v1/videos/generations', '/v1/models'];
const TARGET_TYPES = ['key', 'token', 'admin_token', 'model', 'settings', 'logs', 'database', 'disk'];
// Action filter: groups (prefix) and their actions.
const ACTION_GROUPS = [
  ['auth.', ['auth.login', 'auth.login_failed', 'auth.logout', 'auth.password_change']],
  ['key.', ['key.create', 'key.import', 'key.update', 'key.enable', 'key.disable', 'key.restore', 'key.delete', 'key.test', 'key.test_many', 'key.auto_disable', 'key.cooldown', 'key.circuit_open']],
  ['token.', ['token.create', 'token.update', 'token.enable', 'token.disable', 'token.regenerate', 'token.delete', 'token.quota_', 'token.quota_initial', 'token.quota_increase', 'token.quota_decrease', 'token.quota_set_limited', 'token.quota_set_unlimited', 'token.quota_migrate']],
  ['admin_token.', ['admin_token.create', 'admin_token.revoke']],
  ['model.', ['model.create', 'model.update', 'model.delete', 'model.sync']],
  ['settings.', ['settings.update']],
  ['logs.', ['logs.purge', 'logs.retention_start', 'logs.retention_end']],
  ['system.', ['system.start', 'system.migrate', 'system.compact']],
  ['disk.', ['disk.low']],
];
const REQ_FILTERS = ['model', 'status', 'path'];
const EV_FILTERS = ['target_type', 'target_id', 'action', 'actor'];
const PAGE_SIZES = [20, 50, 100, 200];
const DEF_PS = 50;
const lsGet = (k) => { try { return localStorage.getItem(k); } catch { return null; } };
const lsSet = (k, v) => { try { localStorage.setItem(k, v); } catch { /* ignore */ } };

/** View state from URL params. A bare #/logs shows all types and the remembered page size. */
function stateFromParams(p) {
  const bare = !Object.keys(p).length;
  const ps = Number(p.page_size) || (bare ? Number(lsGet('xr.logs.ps')) : 0) || DEF_PS;
  const types = (p.type || '').split(',').map((x) => x.trim()).filter((x) => TYPES.includes(x));
  return {
    types: types.length ? TYPES.filter((x) => types.includes(x)) : [...TYPES],
    problems: p.problems === '1',
    preset: PRESETS.includes(p.range) ? p.range : (p.from || p.to ? 'custom' : '24h'),
    from: p.from || '', to: p.to || '', q: p.q || '',
    model: p.model || '', status: p.status || '', key_id: p.key_id || '', token_id: p.token_id || '', path: p.path || '',
    target_type: p.target_type || '', target_id: p.target_id || '', action: p.action || '', actor: p.actor || '',
    page: Math.max(1, Math.floor(Number(p.page)) || 1), pageSize: PAGE_SIZES.includes(ps) ? ps : DEF_PS,
    openId: p.open ? Number(p.open) : null, openEvent: p.event ? Number(p.event) : null,
  };
}

export function render(el, ctx) {
  const st = { ...stateFromParams(ctx.params), total: 0, totalKind: 'exact', rows: [], auto: lsGet('xr.logs.auto') === '1', loaded: false, cursors: [], next: '' };
  const has = (ty) => st.types.includes(ty);
  const mode = () => (st.types.length === 1 && has('request') ? 'request' : has('request') ? 'mixed' : 'events');
  const paramsObj = () => ({
    type: st.types.length === TYPES.length ? '' : st.types.join(','), problems: st.problems ? '1' : '',
    range: st.preset !== '24h' && st.preset !== 'custom' ? st.preset : '', from: st.preset === 'custom' ? st.from : '', to: st.preset === 'custom' ? st.to : '',
    q: st.q, model: st.model, status: st.status, key_id: st.key_id, token_id: st.token_id, path: st.path,
    target_type: st.target_type, target_id: st.target_id, action: st.action, actor: st.actor,
    page: mode() === 'request' && st.page > 1 ? st.page : '', page_size: st.pageSize !== DEF_PS ? st.pageSize : '',
    open: st.openId || '', event: st.openEvent || '',
  });
  const syncParams = (push = false) => ctx.setParams(paramsObj(), { push });

  // --- top actions ---
  const autoSw = switchEl({ checked: st.auto, label: t('logs.autoRefresh'), onChange: (v) => { st.auto = v; lsSet('xr.logs.auto', v ? '1' : '0'); setupAuto(); } });
  const refreshBtn = button('', { icon: 'refresh', variant: 'ghost', size: 'icon', attrs: { title: t('common.refresh'), 'aria-label': t('common.refresh') }, onClick: () => load() });
  ctx.setActions(autoSw, refreshBtn);

  // Discrete filter changes add a history entry; typing replaces the current one.
  const changed = (push = true) => { st.page = 1; st.cursors = []; syncParams(push); paintControls(); load({ keepScroll: false }); };

  // --- row 1: range, types, problems, search ---
  const fromIn = input({ type: 'datetime-local', onchange: (e) => { st.from = fromLocalInput(e.target.value) || ''; changed(); } });
  const toIn = input({ type: 'datetime-local', onchange: (e) => { st.to = fromLocalInput(e.target.value) || ''; changed(); } });
  const customRange = h('div', { class: 'custom-range' }, fromIn, h('span', { class: 'muted' }, '→'), toIn);
  const presetSel = segmented({
    size: 'seg-sm', options: PRESETS.map((v) => ({ value: v, label: t(`logs.preset.${v}`) })), value: st.preset,
    onChange: (v) => {
      st.preset = v;
      if (v === 'custom' && !st.from) st.from = new Date(Date.now() - 86400e3).toISOString();
      changed();
    },
  });
  const typeBtns = TYPES.map((ty) => h('button', {
    type: 'button', class: 'seg-btn', 'data-type': ty, 'aria-pressed': 'false',
    onclick: () => {
      const on = has(ty);
      if (on && st.types.length === 1) return; // at least one type
      st.types = on ? st.types.filter((x) => x !== ty) : TYPES.filter((x) => x === ty || has(x));
      changed();
    },
  }, t(`lg.type.${ty}`)));
  const typeSel = h('div', { class: 'segmented seg-sm type-multi', role: 'group', 'aria-label': t('lg.types') }, typeBtns);
  const problemsSw = switchEl({ checked: st.problems, label: t('lg.problems'), onChange: (v) => { st.problems = v; changed(); } });
  const qIn = input({ type: 'search', placeholder: t('lg.qPh'), oninput: debounce((e) => { st.q = e.target.value.trim(); changed(false); }, 350) });

  // --- row 2: per-type filters ---
  const lbl = (text, ctl) => h('label', { class: 'filter' }, h('span', { class: 'filter-label' }, text), ctl);
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
  const ttSel = select([{ value: '', label: t('lg.anyTargetType') }, ...TARGET_TYPES.map((x) => ({ value: x, label: targetTypeLabel(x) }))], '', {
    onchange: (e) => { st.target_type = e.target.value; st.target_id = ''; fillTargetList(); changed(); },
  });
  const dlTargets = h('datalist', { id: 'dl-log-targets' });
  const targetIn = input({ placeholder: t('lg.anyTarget'), list: 'dl-log-targets', class: 'input', oninput: debounce((e) => { st.target_id = e.target.value.trim(); changed(false); }, 400) });
  const actionSel = h('select', { class: 'input select', onchange: (e) => { st.action = e.target.value; changed(); } },
    h('option', { value: '' }, t('lg.anyAction')),
    ACTION_GROUPS.map(([grp, acts]) => h('optgroup', { label: t(`lg.ag.${grp.slice(0, -1)}`) },
      h('option', { value: grp }, t('lg.allIn', { g: t(`lg.ag.${grp.slice(0, -1)}`) })),
      acts.map((a) => h('option', { value: a }, a.endsWith('_') ? t('lg.allQuota') : actionLabel(a))))));
  const actorSel = select([{ value: '', label: t('lg.anyActor') }, { value: 'password', label: t('ev.actor.password') }, { value: 'system', label: t('ev.actor.system') }, { value: 'anonymous', label: t('ev.actor.anonymous') }], '', {
    onchange: (e) => { st.actor = e.target.value; changed(); },
  });
  const resetBtn = button(t('logs.resetFilters'), { icon: 'x', variant: 'ghost', size: 'sm', onClick: () => {
    Object.assign(st, { preset: '24h', from: '', to: '', q: '', model: '', status: '', key_id: '', token_id: '', path: '', target_type: '', target_id: '', action: '', actor: '', problems: false });
    changed();
  } });
  const fModel = lbl(t('common.model'), modelIn), fStatus = lbl(t('common.status'), h('div', { class: 'row-sm' }, statusSel, customStatus));
  const fToken = lbl(t('common.token'), tokenSel), fKey = lbl(t('common.key'), keySel), fPath = lbl(t('logs.path'), pathIn);
  const fTT = lbl(t('lg.col.targetType'), ttSel), fTarget = lbl(t('lg.col.target'), targetIn), fAction = lbl(t('lg.col.action'), actionSel), fActor = lbl(t('lg.col.actor'), actorSel);
  const hintEl = h('div', { class: 'filters-hint', hidden: true }, icon('info', 13), h('span'));

  const filterToggle = h('button', { type: 'button', class: 'btn btn-sm filters-toggle', 'aria-expanded': 'false', onclick: () => {
    const open = !filterBar.classList.contains('open');
    filterBar.classList.toggle('open', open);
    filterToggle.setAttribute('aria-expanded', open ? 'true' : 'false');
  } });
  const grid = h('div', { class: 'filters-row filters-grid lg-grid' });
  const filterBar = h('div', { class: 'filters' },
    h('div', { class: 'filters-row' }, presetSel, customRange, typeSel, problemsSw,
      h('div', { class: 'filters-q-row' }, h('div', { class: 'search-input filters-q' }, icon('search', 15), qIn), filterToggle)),
    grid, hintEl, dlModels, dlPaths, dlTargets);

  /** Ensure a <select> has an option for `v` (e.g. an id from the URL before the list loads). */
  const ensureOpt = (sel, v) => { if (v && ![...sel.options].some((o) => o.value === String(v))) sel.appendChild(h('option', { value: String(v) }, `#${v}`)); };
  function paintControls() {
    presetSel.setValue(st.preset);
    customRange.hidden = st.preset !== 'custom';
    fromIn.value = toLocalInput(st.from); toIn.value = toLocalInput(st.to);
    typeBtns.forEach((b) => { const on = has(b.dataset.type); b.classList.toggle('active', on); b.setAttribute('aria-pressed', on ? 'true' : 'false'); });
    problemsSw.input.checked = st.problems;
    qIn.value = st.q; modelIn.value = st.model; pathIn.value = st.path; targetIn.value = st.target_id;
    const custom = st.status && !STATUS_OPTS.includes(st.status);
    statusSel.value = custom ? 'custom' : st.status;
    customStatus.hidden = !custom;
    customStatus.value = custom ? st.status : '';
    ensureOpt(keySel, st.key_id); keySel.value = st.key_id;
    ensureOpt(tokenSel, st.token_id); tokenSel.value = st.token_id;
    ttSel.value = st.target_type; actionSel.value = st.action;
    ensureOpt(actorSel, st.actor); actorSel.value = st.actor;
    if (st.action && actionSel.value !== st.action) { actionSel.appendChild(h('option', { value: st.action }, st.action)); actionSel.value = st.action; }
    // Second row follows the selected types (key / token filter both requests and events in mixed views).
    const m = mode();
    const items = [];
    if (m !== 'events') items.push(fModel, fStatus, fToken, fKey, fPath);
    if (m !== 'request') items.push(fTT, fTarget, fAction, fActor);
    grid.replaceChildren(...items, h('div', { class: 'filter filter-reset' }, resetBtn));
    let hint = '';
    if (m === 'mixed' && REQ_FILTERS.some((k) => st[k])) hint = t('lg.hintReqOnly');
    else if (m === 'mixed' && EV_FILTERS.some((k) => st[k])) hint = t('lg.hintEvOnly');
    hintEl.hidden = !hint; hintEl.lastChild.textContent = hint;
    const n = [st.preset !== '24h', st.problems, ...(m !== 'events' ? [st.model, st.status, st.key_id, st.token_id, st.path] : []), ...(m !== 'request' ? EV_FILTERS.map((k) => st[k]) : [])].filter(Boolean).length;
    filterToggle.replaceChildren(icon('filter', 14), h('span', null, n ? t('logs.filtersN', { n }) : t('logs.filters')));
    filterToggle.classList.toggle('btn-active', n > 0);
    paintHead();
  }

  // --- table ---
  const thead = h('thead');
  const tbody = h('tbody', null, skeletonRows(7, 10));
  const table = h('table', { class: 'table table-logs table-ulog table-cards' }, thead, tbody);
  let cols = 7;
  function paintHead() {
    const m = mode();
    table.dataset.mode = m;
    const th = (label, cls, title) => h('th', { class: cls || null, title: title || null }, label);
    let cells;
    if (m === 'request') {
      cells = [th(t('logs.time'), 'col-time'), th(t('logs.request')), th(t('common.status'), 'col-status'), th(t('lg.col.tokenKey'), 'col-who'),
        th(h('span', { class: 'tok4 tok4-head' }, h('span', null, t('lg.tok.input')), h('span', { title: t('lg.tok.cacheWriteTip') }, t('lg.tok.cacheWrite')), h('span', null, t('lg.tok.cacheRead')), h('span', null, t('lg.tok.output'))), 'num col-tok'),
        th(t('common.cost'), 'num col-cost'), th(t('common.latency'), 'num col-lat', t('logs.ttfbClientHint'))];
    } else if (m === 'mixed') {
      cells = [th(t('logs.time'), 'col-time'), th(t('lg.col.typeLevel'), 'col-type'), th(t('lg.col.subject')), th(t('lg.col.summary')), th(t('lg.col.who')), th(t('lg.col.costLat'), 'num col-cost')];
    } else {
      cells = [th(t('logs.time'), 'col-time'), th(t('lg.col.level'), 'col-type'), th(t('lg.col.target')), th(t('lg.col.action')), th(t('lg.col.summary')), th(t('lg.col.actor')), th(t('lg.col.ip'))];
    }
    cols = cells.length;
    thead.replaceChildren(h('tr', null, cells));
  }

  const pager = pagerEl({
    sizes: PAGE_SIZES,
    onChange: (pg, ps) => {
      if (ps !== st.pageSize) lsSet('xr.logs.ps', ps);
      st.page = pg; st.pageSize = ps; syncParams(true); load({ keepScroll: false });
    },
  });
  // Cursor pager for merged views: previous / next, page number, page size.
  const cPrev = h('button', { class: 'page-btn', type: 'button', 'aria-label': t('common.prev'), title: t('common.prev'), onclick: () => { st.cursors.pop(); load({ keepScroll: false }); } }, icon('chevronLeft', 15));
  const cNext = h('button', { class: 'page-btn', type: 'button', 'aria-label': t('common.next'), title: t('common.next'), onclick: () => { if (st.next) { st.cursors.push(st.next); load({ keepScroll: false }); } } }, icon('chevronRight', 15));
  const cFirst = h('button', { class: 'page-btn', type: 'button', 'aria-label': t('common.firstPage'), title: t('common.firstPage'), onclick: () => { st.cursors = []; load({ keepScroll: false }); } }, icon('chevronsLeft', 15));
  const cInfo = h('div', { class: 'page-info' });
  const cNum = h('span', { class: 'page-btn active' });
  const cSize = select(PAGE_SIZES.map((s) => ({ value: s, label: t('common.perPage', { n: s }) })), st.pageSize, {
    class: 'input select select-sm', 'aria-label': t('common.pageSize'), onchange: (e) => { st.pageSize = Number(e.target.value); lsSet('xr.logs.ps', st.pageSize); st.cursors = []; syncParams(true); load({ keepScroll: false }); },
  });
  const cursorPager = h('div', { class: 'pagination' }, cInfo, h('div', { class: 'page-controls' }, cSize, cFirst, cPrev, cNum, cNext));
  function paintCursorPager() {
    const pg = st.cursors.length + 1;
    cInfo.replaceChildren(h('span', null, t('lg.pageInfo', { p: fmtInt(pg), n: fmtInt(st.rows.length), total: fmtTotal(st.total, st.totalKind) })));
    cNum.textContent = fmtInt(pg);
    cFirst.disabled = cPrev.disabled = pg <= 1;
    cNext.disabled = !st.next;
    cSize.value = String(st.pageSize);
  }

  const countEl = h('span', { class: 'toolbar-count' });
  const liveDot = h('span', { class: 'live-dot', hidden: !st.auto, title: t('logs.autoRefresh') });
  const exportBtn = button(t('logs.exportJson'), { icon: 'download', size: 'sm', variant: 'ghost', attrs: { title: t('logs.exportHint') }, onClick: () => {
    if (!st.rows.length) { toast(t('logs.exportEmpty'), 'info'); return; }
    const d = new Date(); const p2 = (n) => String(n).padStart(2, '0');
    const name = `xai-router-logs-${d.getFullYear()}${p2(d.getMonth() + 1)}${p2(d.getDate())}-${p2(d.getHours())}${p2(d.getMinutes())}.json`;
    downloadText(name, JSON.stringify({ exported_at: d.toISOString(), filters: query(), total: st.total, total_kind: st.totalKind, data: st.rows }, null, 2));
    toast(t('logs.exported', { n: st.rows.length }), 'success', 2500);
  } });
  const wrap = tableWrap(table, { maxHeight: 'calc(100vh - 300px)' });
  const region = h('div', { class: 'table-region' }, h('div', { class: 'loading-bar', 'aria-hidden': 'true' }), wrap);
  const pagerSlot = h('div');
  el.append(card({ flush: true, cls: 'card-logs', body: [filterBar, h('div', { class: 'toolbar toolbar-thin' }, countEl, liveDot, h('div', { class: 'toolbar-spacer' }), h('span', { class: 'muted small hide-sm' }, t('logs.clickHint')), exportBtn), region, pagerSlot] }));
  paintControls();

  // --- cells ---
  const tokPfx = (r) => (r.token_prefix ? `${r.token_prefix}…` : '');
  const stop = (e) => e.stopPropagation();
  const tokenLink = (r) => (r.token_name
    ? (r.token_id != null ? h('a', { class: 'link cell-link', href: `#/tokens?open=${r.token_id}`, title: `${t('lg.openToken')}: ${credText(r.token_name, tokPfx(r))}`, onclick: stop }, cred(r.token_name, tokPfx(r))) : cred(r.token_name, tokPfx(r)))
    : h('span', { class: 'muted' }, '—'));
  const keyLink = (r) => (r.key_name
    ? (r.key_id != null ? h('a', { class: 'link cell-link', href: `#/keys?open=${r.key_id}`, title: `${t('lg.openKey')}: ${r.key_name}`, onclick: stop }, icon('key', 10), ' ', r.key_name) : h('span', null, icon('key', 10), ' ', r.key_name))
    : h('span', { class: 'muted' }, icon('key', 10), ' —'));
  const timeCell = (r) => h('td', { class: 'nowrap m-meta m-time' }, h('div', { class: 'mono', title: fmtDateTime(r.created_at) }, fmtTimeShort(r.created_at)), h('div', { class: 'cell-sub', 'data-rel': r.created_at }, relTime(r.created_at)));
  const isErr = (r) => (r.type === 'request' ? (r.status >= 400 || !r.status) : r.level === 'error');
  function tok4(r) {
    if (r.images) return h('span', null, `${r.images} ${t('logs.imgShort')}`);
    if (r.video_seconds) return h('span', null, t('lg.seconds', { n: r.video_seconds }));
    if (!r.total_tokens && !r.prompt_tokens) return h('span', { class: 'muted' }, '—');
    const c = tokenCats(r);
    return h('div', null,
      h('span', { class: 'tok4' },
        h('span', { title: t('lg.tok.input') }, fmtCompact(c.input + c.image)),
        h('span', { title: t('lg.tok.cacheWriteTip'), class: c.cacheWrite == null ? 'muted' : '' }, c.cacheWrite == null ? '—' : fmtCompact(c.cacheWrite)),
        h('span', { title: t('lg.tok.cacheRead') }, fmtCompact(c.cacheRead)),
        h('span', { title: t('lg.tok.output') }, fmtCompact(c.output))),
      h('div', { class: 'cell-sub' }, t('lg.inclReasoning', { n: c.reasoningKnown ? fmtCompact(c.reasoning) : '—' })));
  }
  function reasonLine(r) {
    const rr = requestReason(r);
    return rr ? h('div', { class: `cell-sub truncate ${r.status >= 500 || !r.status ? 'text-danger' : 'text-warning'}`, title: rr.raw || rr.text }, rr.text) : null;
  }
  function reqLine(r) {
    return h('div', null,
      h('div', { class: 'req-line' }, methodTag(r.method), h('span', { class: 'mono path', title: r.path }, r.path)),
      h('div', { class: 'cell-sub model-line' }, r.model ? h('span', { class: 'mono model-name', title: r.model }, r.model) : null,
        r.stream ? h('span', { class: 'stream-tag', title: t('logs.streamYes') }, icon('stream', 12), t('lg.stream')) : null,
        r.websocket ? h('span', { class: 'stream-tag', title: 'WebSocket' }, icon('stream', 12), 'WebSocket') : null));
  }
  const tries = (r) => ((r.attempts || 0) > 1 ? badge(`×${r.attempts}`, 'warning', { class: 'badge-tries', title: t('logs.retriedN', { n: r.attempts }) }) : null);
  function targetCell(e) {
    const name = targetText(e), hint = targetHint(e);
    // No object (sign-in, start-up) or a whole-catalogue action (price sync): name the area instead.
    if (['settings', 'logs', 'database', 'disk'].includes(e.target_type)) return h('span', null, targetTypeLabel(e.target_type));
    if (!e.target_type || (e.target_type === 'model' && !e.target_id)) {
      const g = (e.action || '').split('.')[0];
      return h('span', null, hasT(`lg.ag.${g}`) ? t(`lg.ag.${g}`) : targetTypeLabel(e.target_type));
    }
    const node =h('span', null, h('span', { class: 'muted small' }, `${targetTypeLabel(e.target_type)} `), e.target_type && !['settings', 'logs', 'database', 'disk'].includes(e.target_type) ? cred(name, hint) : null);
    if ((e.target_type === 'key' || e.target_type === 'token') && e.target_id) {
      return h('a', { class: 'link cell-link', href: `#/${e.target_type === 'key' ? 'keys' : 'tokens'}?open=${e.target_id}`, onclick: stop, title: t(e.target_type === 'key' ? 'lg.openKey' : 'lg.openToken') }, node);
    }
    return node;
  }

  function row(r) {
    const isReq = r.type === 'request' || r.type === undefined;
    const active = isReq ? r.id === st.openId : r.id === st.openEvent;
    const tr = h('tr', { class: `clickable ${isErr(r) ? 'row-err' : ''} ${active ? 'row-active' : ''}`, 'data-id': r.id, 'data-type': isReq ? 'request' : 'event', onclick: () => (isReq ? openDetail(r.id) : openEvent(r.id)) });
    const m = mode();
    if (m === 'request') {
      tr.append(timeCell(r),
        h('td', { class: 'cell-req m-title' }, reqLine(r)),
        h('td', { class: 'col-status m-badge' }, h('div', { class: 'nowrap' }, statusBadge(r.status), tries(r)), reasonLine(r)),
        h('td', { class: 'truncate-cell m-meta m-who' }, h('div', { class: 'truncate' }, tokenLink(r)), h('div', { class: 'cell-sub truncate' }, keyLink(r))),
        h('td', { class: 'num mono m-meta col-tok' }, tok4(r)),
        h('td', { class: 'num mono m-meta col-cost' }, r.cost_usd ? fmtUSD(r.cost_usd) : h('span', { class: 'muted' }, '$0')),
        h('td', { class: `num mono m-meta col-lat ${r.latency_ms > 30000 ? 'text-warning' : ''}` }, h('div', null, fmtMs(r.latency_ms)), r.ttfb_ms ? h('div', { class: 'cell-sub', title: t('logs.ttfbClientHint') }, t('lg.ttfb', { v: fmtMs(r.ttfb_ms) })) : null));
    } else if (m === 'mixed') {
      if (isReq) {
        const rr = requestReason(r);
        tr.append(timeCell(r), h('td', { class: 'col-type' }, typeTag('request', r.level)),
          h('td', { class: 'cell-req m-title' }, reqLine(r)),
          h('td', { class: 'cell-sum' }, h('div', { class: 'nowrap' }, statusBadge(r.status), tries(r), ' ',
            rr ? h('span', { class: `small ${r.status >= 500 || !r.status ? 'text-danger' : 'text-warning'}`, title: rr.raw || rr.text }, rr.text) : null),
            !rr && (r.total_tokens || r.prompt_tokens) ? h('div', { class: 'cell-sub' }, tokCompact(r)) : null),
          h('td', { class: 'truncate-cell m-meta' }, h('div', { class: 'truncate' }, tokenLink(r)), h('div', { class: 'cell-sub truncate' }, keyLink(r))),
          h('td', { class: 'num mono m-meta col-cost' }, h('div', null, r.cost_usd ? fmtUSD(r.cost_usd) : '$0'), h('div', { class: 'cell-sub' }, fmtMs(r.latency_ms))));
      } else {
        tr.append(timeCell(r), h('td', { class: 'col-type' }, typeTag(r.type, r.level)),
          h('td', { class: 'm-title' }, h('div', { class: 'truncate' }, targetCell(r)), h('div', { class: 'cell-sub' }, actionLabel(r.action))),
          h('td', { class: 'cell-sum' }, h('div', { class: 'clamp-2', title: eventSentence(r) }, eventSentence(r))),
          h('td', { class: 'truncate-cell m-meta' }, h('div', { class: 'truncate' }, actorLabel(r))),
          h('td', { class: 'num muted' }, '—'));
      }
    } else {
      tr.append(timeCell(r), h('td', { class: 'col-type' }, h('span', { class: 'type-tag' }, levelDot(r.level), t(`ev.level.${r.level || 'info'}`))),
        h('td', { class: 'm-title' }, h('div', { class: 'truncate' }, targetCell(r))),
        h('td', { class: 'nowrap' }, actionLabel(r.action), h('div', { class: 'cell-sub' }, t(`lg.type.${r.type}`))),
        h('td', { class: 'cell-sum' }, h('div', { class: 'clamp-2', title: eventSentence(r) }, eventSentence(r))),
        h('td', { class: 'truncate-cell' }, h('div', { class: 'truncate' }, actorLabel(r))),
        h('td', { class: 'mono small nowrap' }, r.ip || h('span', { class: 'muted' }, '—')));
    }
    return tr;
  }
  function tokCompact(r) {
    const c = tokenCats(r);
    return [`${t('lg.tok.input')} ${fmtCompact(c.input + c.image)}`, `${t('lg.tok.cacheRead')} ${fmtCompact(c.cacheRead)}`, `${t('lg.tok.output')} ${fmtCompact(c.output)}`].join(t('lg.sep'));
  }

  function query() {
    const m = mode();
    const q = { type: st.types.join(','), problems: st.problems ? 1 : '', q: st.q, key_id: st.key_id, token_id: st.token_id, page_size: st.pageSize };
    if (m !== 'events') Object.assign(q, { model: st.model, status: st.status, path: st.path });
    if (m !== 'request') Object.assign(q, { target_type: st.target_type, target_id: st.target_id, action: st.action, actor: st.actor });
    if (m === 'request') q.page = st.page; else if (st.cursors.length) q.cursor = st.cursors[st.cursors.length - 1];
    if (st.preset === 'custom') { q.from = st.from; q.to = st.to; }
    else if (PRESET_MS[st.preset]) q.from = new Date(Date.now() - PRESET_MS[st.preset]).toISOString();
    return q;
  }

  // Each load aborts the one before it; the previous rows stay visible (dimmed) until the new ones arrive.
  const nextSignal = latestSignal(ctx.signal);
  let loading = false;
  async function load({ silent = false, keepScroll = true } = {}) {
    const signal = nextSignal();
    const m = mode();
    loading = true;
    if (!silent) { refreshBtn.classList.add('spin'); if (st.loaded) { setTableLoading(region, true); pager.setLoading(true); } }
    try {
      const res = await get('/api/v1/logs', query(), { signal });
      if (signal.aborted) return;
      st.rows = res.data || [];
      if (res.total != null) { st.total = res.total || 0; st.totalKind = res.total_kind || (res.total_exact === false ? 'approx' : 'exact'); }
      st.next = res.next_cursor || '';
      st.loaded = true;
      countEl.textContent = t('logs.matchN', { n: fmtTotal(st.total, st.totalKind) });
      countEl.title = st.totalKind === 'approx' ? t('common.totalApproxHint') : st.totalKind === 'at_least' ? t('common.totalAtLeastHint') : '';
      const filtered = st.problems || st.q || REQ_FILTERS.some((k) => st[k]) || EV_FILTERS.some((k) => st[k]) || st.key_id || st.token_id;
      let empty;
      if (!st.rows.length && m === 'request' && st.page > 1) {
        const goPage = (pg) => { st.page = pg; syncParams(true); load({ keepScroll: false }); };
        empty = { icon: 'inbox', title: t('common.pastEnd'), desc: t('common.pastEndDesc'), action: button(t('common.firstPage'), { icon: 'chevronsLeft', size: 'sm', onClick: () => goPage(1) }) };
      } else if (filtered) empty = { icon: 'filter', title: t('logs.noMatch'), desc: t('logs.noMatchDesc'), action: button(t('lg.clearFilters'), { icon: 'x', size: 'sm', onClick: () => resetBtn.click() }) };
      else empty = { icon: 'logs', title: t('logs.empty'), desc: t('logs.emptyDesc') };
      paintHead();
      tbody.replaceChildren(...(st.rows.length ? st.rows.map(row) : [tableEmptyRow(cols, empty)]));
      if (!keepScroll) wrap.scrollTop = 0;
      if (m === 'request') {
        pagerSlot.replaceChildren(pager);
        pager.update({ page: st.page, pageSize: st.pageSize, total: st.total, totalKind: st.totalKind, hasMore: st.rows.length >= st.pageSize, rowCount: st.rows.length, loading: false });
      } else { pagerSlot.replaceChildren(cursorPager); paintCursorPager(); }
    } catch (e) {
      if (e.name === 'AbortError' || signal.aborted) return;
      pager.setLoading(false);
      if (!silent) { toastError(e); tbody.replaceChildren(tableEmptyRow(cols, { icon: 'alert', title: t('common.loadFailed'), desc: errorMessage(e) })); }
    } finally {
      if (!signal.aborted) { loading = false; refreshBtn.classList.remove('spin'); setTableLoading(region, false); }
    }
  }

  let injectUsage = null;
  get('/api/v1/settings', null, { signal: ctx.signal }).then((x) => { injectUsage = x ? x.inject_stream_usage !== false : null; }).catch(() => {});

  // --- filter option data ---
  let keyList = [], tokenList = [], modelList = [];
  function fillTargetList() {
    const list = st.target_type === 'key' ? keyList.map((x) => [x.id, credText(x.name, x.key_masked)])
      : st.target_type === 'token' ? tokenList.map((x) => [x.id, credText(x.name, x.prefix ? `${x.prefix}…` : '')])
        : st.target_type === 'model' ? modelList.map((m) => [m, m]) : [];
    dlTargets.replaceChildren(...list.map(([v, l]) => h('option', { value: String(v) }, l)));
  }
  async function loadFilterData() {
    const [ks, ts, ms, as] = await Promise.allSettled([
      get('/api/v1/keys', { page_size: 200 }, { signal: ctx.signal }),
      get('/api/v1/tokens', { page_size: 200 }, { signal: ctx.signal }),
      get('/api/v1/models', null, { signal: ctx.signal }),
      get('/api/v1/admin-tokens', null, { signal: ctx.signal }),
    ]);
    // Options read "name · prefix" so same-named tokens / keys can be told apart.
    const fill = (sel, items, anyLabel, cur, hint) => {
      const opts = [h('option', { value: '' }, anyLabel), ...items.map((x) => h('option', { value: String(x.id), selected: String(x.id) === String(cur) }, credText(x.name || `#${x.id}`, hint(x))))];
      if (cur && !items.some((x) => String(x.id) === String(cur))) opts.push(h('option', { value: cur, selected: true }, `#${cur}`));
      sel.replaceChildren(...opts);
    };
    if (ks.status === 'fulfilled') { keyList = ks.value.data || []; fill(keySel, keyList, t('logs.anyKey'), st.key_id, (x) => x.key_masked || ''); }
    if (ts.status === 'fulfilled') { tokenList = ts.value.data || []; fill(tokenSel, tokenList, t('logs.anyToken'), st.token_id, (x) => (x.prefix ? `${x.prefix}…` : '')); }
    if (ms.status === 'fulfilled') { modelList = (ms.value.data || []).map((m) => m.id); dlModels.replaceChildren(...modelList.map((m) => h('option', { value: m }))); }
    if (as.status === 'fulfilled') {
      const ats = Array.isArray(as.value) ? as.value : (as.value && as.value.data) || [];
      ats.forEach((a) => actorSel.appendChild(h('option', { value: `admin_token:${a.id}` }, `${t('ev.actor.adminToken')} · ${a.name}`)));
      actorSel.value = st.actor;
    }
    fillTargetList();
  }

  // --- detail drawer (requests and events share one drawer; ↑ / ↓ step through the rows) ---
  let drawer = null;
  const nextDetailSignal = latestSignal(ctx.signal);
  function markActive() {
    tbody.querySelectorAll('tr[data-id]').forEach((tr) => tr.classList.toggle('row-active',
      tr.dataset.type === 'request' ? Number(tr.dataset.id) === st.openId : Number(tr.dataset.id) === st.openEvent));
  }
  function ensureDrawer(titleText, sub) {
    const idx = curIndex();
    const prev = h('button', { class: 'btn btn-ghost btn-icon', title: `${t('logs.newer')} (↑ / k)`, 'aria-label': t('logs.newer'), disabled: idx <= 0, onclick: () => step(-1) }, icon('chevronLeft', 17));
    const next = h('button', { class: 'btn btn-ghost btn-icon', title: `${t('logs.older')} (↓ / j)`, 'aria-label': t('logs.older'), disabled: idx < 0 || idx >= st.rows.length - 1, onclick: () => step(1) }, icon('chevronRight', 17));
    const loadingBody = h('div', { class: 'drawer-loading' }, spinner(22), h('span', { class: 'muted' }, t('common.loading')));
    if (drawer) {
      drawer.setTitle(titleText); drawer.setBody(loadingBody);
      const acts = drawer.el.querySelector('.modal-header-actions');
      acts.replaceChildren(prev, next, acts.querySelector(':scope > button:last-child'));
    } else {
      drawer = openModal({
        kind: 'drawer', size: 'xl', title: titleText, subtitle: sub || ' ', body: loadingBody, headerExtra: [prev, next],
        onClose: () => { drawer = null; st.openId = null; st.openEvent = null; nextDetailSignal.abort(); syncParams(); markActive(); document.removeEventListener('keydown', onKey); },
      });
      document.addEventListener('keydown', onKey);
    }
    const s = drawer.el.querySelector('.modal-subtitle');
    if (s) s.textContent = sub || '';
    return drawer;
  }
  function curIndex() {
    return st.rows.findIndex((r) => (r.type === 'request' || !r.type ? r.id === st.openId && !st.openEvent : r.id === st.openEvent));
  }
  async function openDetail(id) {
    st.openId = id; st.openEvent = null; syncParams(); markActive();
    const summary = st.rows.find((r) => (r.type === 'request' || !r.type) && r.id === id);
    const d0 = ensureDrawer(summary ? `${summary.method} ${summary.path}` : `${t('logs.log')} #${id}`, summary ? fmtDateTime(summary.created_at) : '');
    const signal = nextDetailSignal();
    try {
      const d = await get(`/api/v1/logs/${id}`, null, { signal });
      if (drawer !== d0 || st.openId !== id) return;
      drawer.setTitle(`${d.method || 'GET'} ${d.path || ''}`);
      const sub = drawer.el.querySelector('.modal-subtitle');
      if (sub) sub.textContent = `${fmtDateTime(d.created_at)} · ${relTime(d.created_at)}${d.model ? ' · ' + d.model : ''}`;
      drawer.setBody(requestDetailView(d, { injectUsage }));
      drawer.body.scrollTop = 0;
    } catch (e) {
      if (e.name === 'AbortError' || signal.aborted) return;
      drawer && drawer.setBody(h('div', { class: 'drawer-loading' }, icon('alert', 22), h('span', null, errorMessage(e))));
      toastError(e);
    }
  }
  async function openEvent(id) {
    st.openEvent = id; st.openId = null; syncParams(); markActive();
    const summary = st.rows.find((r) => r.type !== 'request' && r.id === id);
    const d0 = ensureDrawer(summary ? actionLabel(summary.action) : `${t('lg.event')} #${id}`, summary ? fmtDateTime(summary.created_at) : '');
    const signal = nextDetailSignal();
    try {
      const e = await get(`/api/v1/events/${id}`, null, { signal });
      if (drawer !== d0 || st.openEvent !== id) return;
      drawer.setTitle(actionLabel(e.action));
      const sub = drawer.el.querySelector('.modal-subtitle');
      if (sub) sub.textContent = `${fmtDateTime(e.created_at)} · ${relTime(e.created_at)} · ${t(`lg.type.${e.type}`)}`;
      drawer.setBody(eventDetailView(e, { openRequest: openRequestById }));
      drawer.body.scrollTop = 0;
    } catch (err) {
      if (err.name === 'AbortError' || signal.aborted) return;
      drawer && drawer.setBody(h('div', { class: 'drawer-loading' }, icon('alert', 22), h('span', null, errorMessage(err))));
      toastError(err);
    }
  }
  /** "Open the request" of an event (e.g. the request that got a key disabled). */
  async function openRequestById(requestId, btn) {
    if (btn) btn.disabled = true;
    try {
      const id = await findRequestLog(requestId, ctx.signal);
      if (id) openDetail(id); else toast(t('lg.requestGone'), 'info');
    } catch (e) { toastError(e); } finally { if (btn) btn.disabled = false; }
  }
  function step(dir) {
    const n = st.rows[curIndex() + dir];
    if (!n) return;
    if (n.type === 'request' || !n.type) openDetail(n.id); else openEvent(n.id);
  }
  function onKey(e) {
    if (!drawer || e.target.closest('input, textarea, select')) return;
    if (e.key === 'ArrowDown' || e.key === 'j') { e.preventDefault(); step(1); }
    else if (e.key === 'ArrowUp' || e.key === 'k') { e.preventDefault(); step(-1); }
  }

  // --- back / forward: apply the URL in place ---
  ctx.onParams((p) => {
    const next = stateFromParams(p);
    const { openId, openEvent: openEv } = next;
    delete next.openId; delete next.openEvent;
    Object.assign(st, next, { cursors: [] });
    paintControls();
    load({ keepScroll: false });
    if (openId && openId !== st.openId) openDetail(openId);
    else if (openEv && openEv !== st.openEvent) openEvent(openEv);
    else if (!openId && !openEv && drawer) drawer.close();
  });

  // --- auto refresh (first page only) ---
  let autoTimer = null;
  function setupAuto() {
    clearInterval(autoTimer); autoTimer = null;
    liveDot.hidden = !st.auto;
    if (st.auto) autoTimer = setInterval(() => { if (!document.hidden && !loading && st.page === 1 && !st.cursors.length && !document.querySelector('.overlay-modal')) load({ silent: true }); }, 5000);
  }

  syncParams();
  loadFilterData();
  load().then(() => { if (st.openId) openDetail(st.openId); else if (st.openEvent) openEvent(st.openEvent); });
  setupAuto();
  return () => { clearInterval(autoTimer); document.removeEventListener('keydown', onKey); };
}
