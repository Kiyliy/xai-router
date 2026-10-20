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
import {
  statusBadge, methodTag, requestDetailView, eventDetailView, findRequestLog, requestTitle, requestHead, requestActions, eventTitle, eventKicker,
} from '../log-detail.js';
import {
  actionLabel, actorLabel, targetTypeLabel, targetText, targetHint, eventSentence, typeTag, levelDot, requestReason, tokenCats,
  isChainRow, isRetriedRow,
} from '../events.js';

const TYPES = ['request', 'audit', 'system'];
// Shown: 1 小时 / 24 小时 / 7 天 / 30 天 / 自定义 (pm-spec 10.8 G6). 15m and "all" still work in links.
const PRESETS = ['1h', '24h', '7d', '30d', 'custom'];
const URL_PRESETS = [...PRESETS, '15m', 'all'];
const PRESET_MS = { '15m': 15 * 60e3, '1h': 3600e3, '24h': 86400e3, '7d': 7 * 86400e3, '30d': 30 * 86400e3 };
const STATUS_OPTS = ['', 'success', 'error', '2xx', '4xx', '5xx', '429', 'custom'];
// Common gateway paths for the path dropdown; anything else is typed under "Other…" (prefix match).
const PATHS = ['/v1/chat/completions', '/v1/responses', '/v1/messages', '/v1/images/generations', '/v1/images/edits', '/v1/videos/generations', '/v1/models'];
const PATH_OTHER = '__other';
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
    problems: p.problems === '1', final: p.attempts === 'final',
    preset: URL_PRESETS.includes(p.range) ? p.range : (p.from || p.to ? 'custom' : '24h'),
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
    type: st.types.length === TYPES.length ? '' : st.types.join(','), problems: st.problems ? '1' : '', attempts: st.final ? 'final' : '',
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
    type: 'button', class: 'filter-chip', 'data-type': ty, 'aria-pressed': 'false',
    onclick: () => {
      const on = has(ty);
      if (on && st.types.length === 1) return; // at least one type
      st.types = on ? st.types.filter((x) => x !== ty) : TYPES.filter((x) => x === ty || has(x));
      changed();
    },
  }, icon('check', 13), h('span', null, t(`lg.type.${ty}`))));
  const typeSel = h('div', { class: 'lg-types', role: 'group', 'aria-label': t('lg.types') }, h('span', { class: 'lg-label' }, t('lg5.typeLabel')), typeBtns);
  const problemsSw = switchEl({ checked: st.problems, label: t('lg.problems'), onChange: (v) => { st.problems = v; changed(); } });
  // v0.6.0: every upstream attempt is a row; this shows only the last row of each request (pm-spec 10.9).
  const finalSw = switchEl({ checked: st.final, label: t('logs.finalOnly'), onChange: (v) => { st.final = v; changed(); } });
  const qIn = input({ type: 'search', placeholder: t('lg.qPh'), oninput: debounce((e) => { st.q = e.target.value.trim(); changed(false); }, 350) });

  // --- row 2: per-type filters ---
  const lbl = (text, ctl) => h('label', { class: 'filter lg-chipctl', title: text }, h('span', { class: 'sr-only' }, text), ctl);
  // Model: the known models from /api/v1/models (filled by loadFilterData); a model typed into the URL is kept as an option.
  const modelSel = select([{ value: '', label: t('common.model') }], '', { onchange: (e) => { st.model = e.target.value; changed(); } });
  const fillModels = () => {
    const ids = modelList.includes(st.model) || !st.model ? modelList : [...modelList, st.model];
    modelSel.replaceChildren(h('option', { value: '' }, t('common.model')), ...ids.map((m) => h('option', { value: m }, m)));
    modelSel.value = st.model;
  };
  const customStatus = input({ type: 'number', min: 100, max: 599, placeholder: '503', class: 'input input-status',
    oninput: debounce((e) => { st.status = e.target.value.trim(); changed(false); }, 400) });
  const statusSel = select(STATUS_OPTS.map((v) => ({ value: v, label: v ? t(`logs.status.${v}`) : t('common.status') })), '', {
    onchange: (e) => {
      const v = e.target.value;
      customStatus.hidden = v !== 'custom';
      if (v === 'custom') { customStatus.focus(); st.status = customStatus.value.trim(); if (!st.status) return; } else st.status = v;
      changed();
    },
  });
  const keySel = select([{ value: '', label: t('common.key') }], '', { onchange: (e) => { st.key_id = e.target.value; changed(); } });
  const tokenSel = select([{ value: '', label: t('common.token') }], '', { onchange: (e) => { st.token_id = e.target.value; changed(); } });
  // Path: common gateway paths, or "Other…" to type a prefix (kept open while typing, even if it matches a listed path).
  let pathOther = false;
  const customPath = input({ placeholder: t('lg5.pathPrefixPh'), class: 'input mono input-path', hidden: true, 'aria-label': t('lg5.pathPrefix'),
    oninput: debounce((e) => { st.path = e.target.value.trim(); changed(false); }, 400) });
  const pathSel = select([{ value: '', label: t('logs.path') }, ...PATHS.map((x) => ({ value: x, label: x })), { value: PATH_OTHER, label: t('lg5.pathOther') }], '', {
    onchange: (e) => {
      const v = e.target.value;
      pathOther = v === PATH_OTHER;
      customPath.hidden = !pathOther;
      if (pathOther) { customPath.focus(); st.path = customPath.value.trim(); if (!st.path) return; } else st.path = v;
      changed();
    },
  });
  const ttSel = select([{ value: '', label: t('lg.col.targetType') }, ...TARGET_TYPES.map((x) => ({ value: x, label: targetTypeLabel(x) }))], '', {
    onchange: (e) => { st.target_type = e.target.value; st.target_id = ''; fillTargetList(); changed(); },
  });
  const dlTargets = h('datalist', { id: 'dl-log-targets' });
  // Object: free text, edited in a small popover under a chip that looks like the dropdowns.
  const targetIn = input({ placeholder: t('lg5.targetPh'), list: 'dl-log-targets', class: 'input', 'aria-label': t('lg.col.target'),
    oninput: debounce((e) => { st.target_id = e.target.value.trim(); changed(false); }, 400),
    onkeydown: (e) => { if (e.key === 'Enter' || e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); closeTargetPop(true); } } });
  const targetClear = button(t('lg5.clear'), { variant: 'ghost', size: 'sm', onClick: () => { targetIn.value = ''; if (st.target_id) { st.target_id = ''; changed(); } closeTargetPop(true); } });
  const targetPop = h('div', { class: 'menu-pop lg-filter-pop', role: 'dialog', 'aria-label': t('lg.col.target') },
    h('div', { class: 'lg-filter-pop-title' }, t('lg.col.target')), targetIn, h('div', { class: 'lg-filter-pop-foot' }, h('span', { class: 'muted small' }, t('lg5.targetHint')), targetClear));
  const targetBtn = h('button', { type: 'button', class: 'input select lg-chipbtn', 'aria-haspopup': 'dialog', 'aria-expanded': 'false',
    onclick: (e) => { e.preventDefault(); if (targetPop.isConnected) closeTargetPop(); else openTargetPop(); } });
  const onTargetDoc = (e) => { if (!targetPop.contains(e.target) && !targetBtn.contains(e.target)) closeTargetPop(); };
  const onTargetScroll = (e) => { if (!targetPop.contains(e.target)) closeTargetPop(); };
  function openTargetPop() {
    document.body.appendChild(targetPop);
    const r = targetBtn.getBoundingClientRect();
    const pw = targetPop.offsetWidth;
    targetPop.style.left = `${Math.max(8, Math.min(r.left, window.innerWidth - pw - 8))}px`;
    targetPop.style.top = `${r.bottom + 4}px`;
    targetBtn.setAttribute('aria-expanded', 'true');
    targetIn.value = st.target_id;
    targetIn.focus();
    document.addEventListener('mousedown', onTargetDoc);
    window.addEventListener('scroll', onTargetScroll, true);
  }
  function closeTargetPop(refocus = false) {
    if (!targetPop.isConnected) return;
    targetPop.remove();
    targetBtn.setAttribute('aria-expanded', 'false');
    document.removeEventListener('mousedown', onTargetDoc);
    window.removeEventListener('scroll', onTargetScroll, true);
    if (refocus) targetBtn.focus();
  }
  ctx.signal.addEventListener('abort', () => closeTargetPop());
  const targetLabel = () => {
    if (!st.target_id) return t('lg.col.target');
    const o = [...dlTargets.options].find((x) => x.value === st.target_id);
    return o && o.textContent ? o.textContent : st.target_id;
  };
  const actionSel = h('select', { class: 'input select', onchange: (e) => { st.action = e.target.value; changed(); } },
    h('option', { value: '' }, t('lg.col.action')),
    ACTION_GROUPS.map(([grp, acts]) => h('optgroup', { label: t(`lg.ag.${grp.slice(0, -1)}`) },
      h('option', { value: grp }, t('lg.allIn', { g: t(`lg.ag.${grp.slice(0, -1)}`) })),
      acts.map((a) => h('option', { value: a }, a.endsWith('_') ? t('lg.allQuota') : actionLabel(a))))));
  const actorSel = select([{ value: '', label: t('lg.col.actor') }, { value: 'password', label: t('ev.actor.password') }, { value: 'system', label: t('ev.actor.system') }, { value: 'anonymous', label: t('ev.actor.anonymous') }], '', {
    onchange: (e) => { st.actor = e.target.value; changed(); },
  });
  const resetBtn = button(t('logs.resetFilters'), { icon: 'x', variant: 'ghost', size: 'sm', onClick: () => {
    Object.assign(st, { preset: '24h', from: '', to: '', q: '', model: '', status: '', key_id: '', token_id: '', path: '', target_type: '', target_id: '', action: '', actor: '', problems: false, final: false });
    pathOther = false; customPath.value = '';
    changed();
  } });
  const fModel = lbl(t('common.model'), modelSel), fStatus = lbl(t('common.status'), h('div', { class: 'row-sm' }, statusSel, customStatus));
  const fToken = lbl(t('common.token'), tokenSel), fKey = lbl(t('common.key'), keySel), fPath = lbl(t('logs.path'), h('div', { class: 'row-sm' }, pathSel, customPath));
  const fTT = lbl(t('lg.col.targetType'), ttSel), fAction = lbl(t('lg.col.action'), actionSel), fActor = lbl(t('lg.col.actor'), actorSel);
  const fTarget = h('div', { class: 'filter lg-chipctl' }, targetBtn);
  const hintEl = h('div', { class: 'filters-hint', hidden: true }, icon('info', 13), h('span'));

  const filterToggle = h('button', { type: 'button', class: 'btn btn-sm filters-toggle', 'aria-expanded': 'false', onclick: () => {
    const open = !filterBar.classList.contains('open');
    filterBar.classList.toggle('open', open);
    filterToggle.setAttribute('aria-expanded', open ? 'true' : 'false');
  } });
  const grid = h('div', { class: 'filters-row lg-grid' });
  const filterBar = h('div', { class: 'filters lg-filters' },
    h('div', { class: 'filters-row lg-row1' }, presetSel, customRange, typeSel, problemsSw, finalSw,
      h('div', { class: 'filters-q-row' }, h('div', { class: 'search-input filters-q' }, icon('search', 15), qIn), filterToggle)),
    grid, hintEl, dlTargets);

  /** Ensure a <select> has an option for `v` (e.g. an id from the URL before the list loads). */
  const ensureOpt = (sel, v) => { if (v && ![...sel.options].some((o) => o.value === String(v))) sel.appendChild(h('option', { value: String(v) }, `#${v}`)); };
  function paintControls() {
    presetSel.setValue(st.preset);
    customRange.hidden = st.preset !== 'custom';
    fromIn.value = toLocalInput(st.from); toIn.value = toLocalInput(st.to);
    typeBtns.forEach((b) => { const on = has(b.dataset.type); b.classList.toggle('active', on); b.setAttribute('aria-pressed', on ? 'true' : 'false'); });
    problemsSw.input.checked = st.problems;
    finalSw.input.checked = st.final;
    finalSw.hidden = mode() === 'events';
    qIn.value = st.q;
    if (st.model && ![...modelSel.options].some((o) => o.value === st.model)) modelSel.appendChild(h('option', { value: st.model }, st.model));
    modelSel.value = st.model;
    if (!st.path) pathOther = pathOther && !customPath.hidden && document.activeElement === customPath;
    const otherPath = pathOther || (!!st.path && !PATHS.includes(st.path));
    pathSel.value = otherPath ? PATH_OTHER : st.path;
    customPath.hidden = !otherPath;
    if (document.activeElement !== customPath) customPath.value = otherPath ? st.path : '';
    if (document.activeElement !== targetIn) targetIn.value = st.target_id;
    targetBtn.textContent = targetLabel();
    targetBtn.title = st.target_id ? `${t('lg.col.target')}: ${st.target_id}` : t('lg.col.target');
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
    const reset = h('div', { class: 'filter filter-reset' }, resetBtn);
    grid.replaceChildren(h('span', { class: 'lg-label' }, t('lg5.filter')), ...items, reset);
    let hint = '';
    if (m === 'mixed' && REQ_FILTERS.some((k) => st[k])) hint = t('lg.hintReqOnly');
    else if (m === 'mixed' && EV_FILTERS.some((k) => st[k])) hint = t('lg.hintEvOnly');
    hintEl.hidden = !hint; hintEl.lastChild.textContent = hint;
    const n = [st.preset !== '24h', st.problems, ...(m !== 'events' ? [st.final, st.model, st.status, st.key_id, st.token_id, st.path] : []), ...(m !== 'request' ? EV_FILTERS.map((k) => st[k]) : [])].filter(Boolean).length;
    filterToggle.replaceChildren(icon('filter', 14), h('span', null, n ? t('logs.filtersN', { n }) : t('logs.filters')));
    filterToggle.classList.toggle('btn-active', n > 0);
    reset.hidden = n === 0;
    [modelSel, statusSel, keySel, tokenSel, ttSel, actionSel, actorSel].forEach((x) => x.classList.toggle('is-set', !!x.value));
    pathSel.classList.toggle('is-set', !!st.path || otherPath);
    customPath.classList.toggle('is-set', !!st.path);
    targetBtn.classList.toggle('is-set', !!st.target_id);
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
    table.querySelector(':scope > colgroup')?.remove();
    if (m === 'request') {
      // Fixed layout sizes columns from the first header row, where the token columns share one
      // colspan cell; a colgroup pins them narrow so the request column gets the rest.
      table.insertBefore(h('colgroup', null, ['time', 'req', 'status', 'who', 'tk', 'tk', 'tk', 'tk', 'cost', 'lat'].map((c) => h('col', { class: `cg-${c}` }))), thead);
      // Four token columns under one "Token 数" group header (pm-spec 10.8 #66).
      const span = (label, cls, title) => h('th', { class: cls || null, title: title || null, rowspan: 2 }, label);
      cols = 10;
      thead.replaceChildren(
        h('tr', { class: 'lg-head-group' }, span(t('logs.time'), 'col-time'), span(t('logs.request'), 'col-req'), span(t('common.status'), 'col-status'), span(t('lg.col.tokenKey'), 'col-who'),
          h('th', { colspan: 4, class: 'num col-tokgroup' }, t('lg5.tokGroup')), span(t('common.cost'), 'num col-cost'), span(t('common.latency'), 'num col-lat', t('logs.ttfbClientHint'))),
        h('tr', { class: 'lg-head-sub' }, th(t('lg.tok.input'), 'num col-tk'), th(t('lg.tok.cacheWrite'), 'num col-tk', t('lg.tok.cacheWriteTip')), th(t('lg.tok.cacheRead'), 'num col-tk'), th(t('lg.tok.output'), 'num col-tk')));
      return;
    } else if (m === 'mixed') {
      cells = [th(t('logs.time'), 'col-time'), th(t('lg.col.type'), 'col-type'), th(t('lg.col.target'), 'col-subj'), th(t('lg.col.summary'), 'col-summ'), th(t('lg.col.who'), 'col-who'), th(t('lg.col.costLat'), 'num col-cost')];
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
  void cFirst; void cInfo; void cNum; void cSize;
  // Merged views page by cursor: just "较新" / "较早" (pm-spec 10.8 #65).
  const cNewer = button(t('logs.newer'), { icon: 'chevronLeft', size: 'sm', onClick: () => cPrev.click() });
  const cOlder = button(t('logs.older'), { icon: 'chevronRight', size: 'sm', onClick: () => cNext.click() });
  const cursorPager = h('div', { class: 'pagination lg-cursor' }, h('div', { class: 'page-controls' }, cNewer, cOlder));
  function paintCursorPager() {
    cNewer.disabled = st.cursors.length === 0;
    cOlder.disabled = !st.next;
    cursorPager.hidden = cNewer.disabled && cOlder.disabled;
  }

  const countEl = h('span', { class: 'topbar-meta lg-count' });
  const liveDot = h('span', { class: 'live-dot', hidden: !st.auto, title: t('logs.autoRefresh') });
  const exportBtn = button(t('logs.exportJson'), { icon: 'download', attrs: { title: t('logs.exportHint') }, onClick: () => {
    if (!st.rows.length) { toast(t('logs.exportEmpty'), 'info'); return; }
    const d = new Date(); const p2 = (n) => String(n).padStart(2, '0');
    const name = `xai-router-logs-${d.getFullYear()}${p2(d.getMonth() + 1)}${p2(d.getDate())}-${p2(d.getHours())}${p2(d.getMinutes())}.json`;
    downloadText(name, JSON.stringify({ exported_at: d.toISOString(), filters: query(), total: st.total, total_kind: st.totalKind, data: st.rows }, null, 2));
    toast(t('logs.exported', { n: st.rows.length }), 'success', 2500);
  } });
  // The table grows with the page (no inner scroll box) and the pager sits in its own footer bar below it.
  const wrap = tableWrap(table);
  const region = h('div', { class: 'table-region' }, h('div', { class: 'loading-bar', 'aria-hidden': 'true' }), wrap);
  const pagerSlot = h('footer', { class: 'lg-pager' });
  void card;
  ctx.setActions(countEl, liveDot, autoSw, exportBtn);
  el.classList.add('lg5');
  el.append(filterBar, region, pagerSlot);
  paintControls();

  // --- cells ---
  const tokPfx = (r) => (r.token_prefix ? `${r.token_prefix}…` : '');
  const stop = (e) => e.stopPropagation();
  const tokenLink = (r) => (r.token_name
    ? (r.token_id != null ? h('a', { class: 'link cell-link', href: `#/tokens?open=${r.token_id}`, title: `${t('lg.openToken')}${t('lg.labelSep')}${credText(r.token_name, tokPfx(r))}`, onclick: stop }, cred(r.token_name, tokPfx(r))) : cred(r.token_name, tokPfx(r)))
    : h('span', { class: 'muted' }, '—'));
  // "上游密钥 prod-01"; requests the gateway answered itself or rejected say so instead.
  const keyLink = (r) => {
    if (!r.key_name) return h('span', { class: 'muted' }, r.status && r.status < 400 ? t('lg5.local') : t('lg5.notForwarded'));
    const text = t('lg5.keyLine', { name: r.key_name });
    return r.key_id != null ? h('a', { class: 'link cell-link', href: `#/keys?open=${r.key_id}`, title: `${t('lg.openKey')}${t('lg.labelSep')}${r.key_name}`, onclick: stop }, text) : h('span', null, text);
  };
  // Today: "14:02:41", earlier: "10月18日 14:02" (pm-spec 10.3). Mixed views add the relative time below.
  // A chain's rows share created_at (the end of the client request); each shows its own start (v0.6.0).
  const timeCell = (r0) => {
    const r = r0.started_at && isChainRow(r0) ? { ...r0, created_at: r0.started_at } : r0;
    const d = new Date(r.created_at);
    const today = d.toDateString() === new Date().toDateString();
    const recent = Date.now() - d.getTime() < 86400e3;
    return h('td', { class: 'nowrap m-meta m-time col-time' }, h('div', { class: 'lg-time', title: fmtDateTime(r.created_at) }, today ? fmtTimeShort(r.created_at) : fmtDateTime(r.created_at, { seconds: false })),
      recent ? h('div', { class: 'cell-sub', 'data-rel': r.created_at }, relTime(r.created_at)) : null);
  };
  // Error rows get the 2px red line: server-side failures and error-level records, not client 4xx.
  // A retried attempt is de-emphasized instead (v0.6.0): the final row carries the outcome.
  const isErr = (r) => (r.type === 'request' || r.type === undefined ? !isRetriedRow(r) && (!r.status || r.status >= 500) : r.level === 'error');
  // The token total only where the four token columns are not shown (pm-spec 10.8 #54).
  const okText = (r, withTokens = true) => (!isChainRow(r) && (r.attempts || 0) > 1 ? t('lg5.okAttempt', { n: r.attempts })
    : r.images ? t('lg5.images', { n: r.images }) : r.video_seconds ? t('lg5.video', { n: r.video_seconds })
      : withTokens && r.total_tokens ? t('lg5.okTokens', { n: fmtInt(r.total_tokens) }) : t('lg5.ok'));
  function tokCells(r) {
    const td = (cls, ...kids) => h('td', { class: `num col-tk ${cls || ''}` }, ...kids);
    if (isRetriedRow(r)) return [td(), td(), td(), td('muted', '—')];
    if (r.images || r.video_seconds) return [td(), td(), td(), td('muted small nowrap', t(r.images ? 'lg5.perImage' : 'lg5.perSecond'))];
    if (!r.total_tokens && !r.prompt_tokens) return [td(), td(), td(), td('muted', '—')];
    const c = tokenCats(r);
    const v = (n, cls) => (n == null ? h('span', { class: 'muted', title: t('lg.tok.cacheWriteTip') }, '—') : h('span', { class: 'tk' }, h('span', { class: `tok-sw ${cls}` }), fmtInt(n)));
    return [td('', v(c.input + c.image, 'in')), td('', v(c.cacheWrite, 'cw')), td('', v(c.cacheRead, 'cr')),
      td('', h('div', null, v(c.output, 'out')), h('div', { class: 'cell-sub nowrap' }, c.reasoningKnown ? t('lg.inclReasoning', { n: fmtInt(c.reasoning) }) : t('lg5.noReasoning')))];
  }
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
      h('div', { class: 'req-line mono', title: `${r.method || 'GET'} ${r.path}` }, h('span', { class: 'lg-method' }, r.method || 'GET'), h('span', { class: 'path' }, r.path)),
      h('div', { class: 'cell-sub model-line' }, r.model ? h('span', { class: 'mono model-name', title: r.model }, r.model) : null,
        r.stream ? h('span', { class: 'stream-tag stream-icon', title: t('logs.streamYes'), 'aria-label': t('logs.streamYes') }, icon('stream', 12)) : null,
        r.websocket ? h('span', { class: 'stream-tag', title: 'WebSocket' }, icon('stream', 12), 'WebSocket') : null));
  }
  // Chain rows (v0.6.0) show "k/n"; historical single rows keep "×n" (pm-spec 10.9).
  const tries = (r) => (isChainRow(r)
    ? badge(`${r.attempt_index || 1}/${r.attempt_total}`, 'neutral', { class: 'badge-tries mono', title: t('lg6.attemptTip', { k: r.attempt_index || 1, n: r.attempt_total }) })
    : (r.attempts || 0) > 1 ? badge(`×${r.attempts}`, 'neutral', { class: 'badge-tries', title: t('logs.retriedN', { n: r.attempts }) }) : null);
  function targetCell(e) {
    const name = targetText(e), hint = targetHint(e);
    // No object (sign-in, start-up) or a whole-catalogue action (price sync): name the area instead.
    if (['settings', 'logs', 'database', 'disk'].includes(e.target_type)) return h('span', null, targetTypeLabel(e.target_type));
    if (!e.target_type || (e.target_type === 'model' && !e.target_id)) {
      const g = (e.action || '').split('.')[0];
      return h('span', null, hasT(`lg.ag.${g}`) ? t(`lg.ag.${g}`) : targetTypeLabel(e.target_type));
    }
    // The event's own name and hint come first (they survive deletion); the live list only fills a
    // missing name and tells whether the object is gone. Plain inline text, so a long name truncates
    // character by character instead of collapsing to a bare "…".
    const live = liveObj(e.target_type, e.target_id);
    const shown = e.target_name ? name : (live && live.name) || name;
    const shownHint = hint || (live && live.hint) || '';
    const gone = live === null;
    const node = h('span', { class: 'lg-target', title: [`${targetTypeLabel(e.target_type)} ${shown}`, shownHint, gone ? t('common.deleted') : ''].filter(Boolean).join(' · ') },
      h('span', { class: 'muted' }, `${targetTypeLabel(e.target_type)} `), h('span', { class: 'lg-target-name' }, shown),
      gone ? h('span', { class: 'lg-target-gone' }, ` ${t('common.deleted')}`) : null,
      shownHint ? h('span', { class: 'lg-target-hint mono' }, ` ${shownHint}`) : null);
    if ((e.target_type === 'key' || e.target_type === 'token') && e.target_id) {
      return h('a', { class: 'link cell-link', href: `#/${e.target_type === 'key' ? 'keys' : 'tokens'}?open=${e.target_id}`, onclick: stop, title: t(e.target_type === 'key' ? 'lg.openKey' : 'lg.openToken') }, node);
    }
    return node;
  }

  function row(r, i) {
    const isReq = r.type === 'request' || r.type === undefined;
    const active = isReq ? r.id === st.openId : r.id === st.openEvent;
    const tr = h('tr', { class: `clickable ${isErr(r) ? 'row-err' : ''} ${active ? 'row-active' : ''} ${isReq ? chainClass(r, i) : ''}`, 'data-id': r.id, 'data-type': isReq ? 'request' : 'event', onclick: () => (isReq ? openDetail(r.id) : openEvent(r.id)) });
    const m = mode();
    if (m === 'request') {
      const rr = requestReason(r);
      tr.append(timeCell(r),
        h('td', { class: 'cell-req m-title' }, reqLine(r)),
        h('td', { class: 'col-status m-badge' }, h('div', { class: `lg-sum ${isChainRow(r) && rr ? 'is-stacked' : ''}` }, statusBadge(r.status), tries(r),
          h('span', { class: 'lg-sum-text', title: rr ? [rr.tip, rr.raw || rr.text].filter(Boolean).join('\n') : null }, rr ? rr.text : okText(r, false)))),
        h('td', { class: 'truncate-cell m-meta m-who' }, h('div', { class: 'truncate' }, tokenLink(r)), h('div', { class: 'cell-sub truncate' }, keyLink(r))),
        ...tokCells(r),
        h('td', { class: 'num mono m-meta col-cost' }, r.cost_usd ? fmtUSD(r.cost_usd) : h('span', { class: 'muted' }, r.status >= 400 || !r.status || isRetriedRow(r) ? '—' : '$0')),
        h('td', { class: `num mono m-meta col-lat ${r.latency_ms > 30000 ? 'text-warning' : ''}` }, h('div', null, fmtMs(r.latency_ms)), r.ttfb_ms && !isRetriedRow(r) ? h('div', { class: 'cell-sub', title: t('logs.ttfbClientHint') }, t('lg.ttfb', { v: fmtMs(r.ttfb_ms) })) : null));
    } else if (m === 'mixed') {
      if (isReq) {
        const rr = requestReason(r);
        tr.append(timeCell(r), h('td', { class: 'col-type' }, typeTag('request', r.level)),
          h('td', { class: 'cell-req m-title' }, reqLine(r)),
          h('td', { class: 'cell-sum' }, h('div', { class: 'lg-sum' }, statusBadge(r.status), tries(r),
            h('span', { class: 'lg-sum-text', title: rr ? [rr.tip, rr.raw || rr.text].filter(Boolean).join('\n') : null }, rr ? rr.text : okText(r)))),
          h('td', { class: 'truncate-cell m-meta' }, h('div', { class: 'truncate' }, tokenLink(r)), h('div', { class: 'cell-sub truncate' }, keyLink(r))),
          h('td', { class: 'num mono m-meta col-cost' }, h('div', null, r.cost_usd ? fmtUSD(r.cost_usd) : h('span', { class: 'muted' }, '—')), h('div', { class: 'cell-sub' }, fmtMs(r.latency_ms))));
      } else {
        tr.append(timeCell(r), h('td', { class: 'col-type' }, typeTag(r.type, r.level)),
          h('td', { class: 'm-title' }, h('div', { class: 'truncate' }, targetCell(r)), h('div', { class: 'cell-sub' }, actionLabel(r.action))),
          h('td', { class: 'cell-sum' }, h('div', { class: 'clamp-2', title: eventSentence(r) }, eventSentence(r))),
          h('td', { class: 'truncate-cell m-meta' }, ...actorCell(r)),
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
  /** Chain marks (v0.6.0, Figma 12b/13b): rows of one request next to each other are joined by a line. */
  function chainClass(r, i) {
    if (!isChainRow(r) || i == null) return '';
    const cls = [isRetriedRow(r) ? 'row-retried' : ''];
    const same = (o) => !!o && (o.type === 'request' || !o.type) && !!r.request_id && o.request_id === r.request_id && isChainRow(o);
    const up = same(st.rows[i - 1]), down = same(st.rows[i + 1]);
    if (up || down) cls.push('chain', up ? '' : 'chain-top', down ? '' : 'chain-bottom', isRetriedRow(r) ? 'node-retry' : r.status && r.status < 400 ? 'node-ok' : 'node-err');
    return cls.filter(Boolean).join(' ');
  }
  /**
   * Keep each chain's rows together, the final row first and then the earlier attempts, at the place
   * of its first row (the backend lists a chain together; this guards page edges and interleaved events).
   */
  function groupChains(rows) {
    const groups = new Map();
    const out = [];
    for (const r of rows) {
      if ((r.type === 'request' || !r.type) && isChainRow(r) && r.request_id) {
        let g = groups.get(r.request_id);
        if (!g) { g = []; groups.set(r.request_id, g); out.push(g); }
        g.push(r);
      } else out.push(r);
    }
    return out.flatMap((x) => (Array.isArray(x) ? x.sort((a, b) => (b.attempt_index || 0) - (a.attempt_index || 0)) : [x]));
  }
  function actorCell(e) {
    if ((e.actor === 'anonymous' || !e.actor) && e.ip) return [h('div', { class: 'truncate' }, t('lg5.anonIp', { ip: e.ip }))];
    return [h('div', { class: 'truncate' }, actorLabel(e)), e.ip && e.actor !== 'system' ? h('div', { class: 'cell-sub mono' }, e.ip) : null];
  }
  void tok4; void reasonLine;
  function tokCompact(r) {
    const c = tokenCats(r);
    return [`${t('lg.tok.input')} ${fmtCompact(c.input + c.image)}`, `${t('lg.tok.cacheRead')} ${fmtCompact(c.cacheRead)}`, `${t('lg.tok.output')} ${fmtCompact(c.output)}`].join(t('lg.sep'));
  }

  function query() {
    const m = mode();
    const q = { type: st.types.join(','), problems: st.problems ? 1 : '', q: st.q, key_id: st.key_id, token_id: st.token_id, page_size: st.pageSize };
    if (m !== 'events') Object.assign(q, { model: st.model, status: st.status, path: st.path, attempts: st.final ? 'final' : '' });
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
      st.rows = groupChains(res.data || []);
      if (res.total != null) { st.total = res.total || 0; st.totalKind = res.total_kind || (res.total_exact === false ? 'approx' : 'exact'); }
      st.next = res.next_cursor || '';
      st.loaded = true;
      countEl.textContent = PRESET_MS[st.preset] ? t('lg5.countRange', { r: t(`logs.preset.${st.preset}`), n: fmtTotal(st.total, st.totalKind) }) : t('lg5.count', { n: fmtTotal(st.total, st.totalKind) });
      countEl.title = st.totalKind === 'approx' ? t('common.totalApproxHint') : st.totalKind === 'at_least' ? t('common.totalAtLeastHint') : '';
      const filtered = st.problems || (m !== 'events' && st.final) || st.q || REQ_FILTERS.some((k) => st[k]) || EV_FILTERS.some((k) => st[k]) || st.key_id || st.token_id;
      let empty;
      if (!st.rows.length && m === 'request' && st.page > 1) {
        const goPage = (pg) => { st.page = pg; syncParams(true); load({ keepScroll: false }); };
        empty = { icon: 'inbox', title: t('common.pastEnd'), desc: t('common.pastEndDesc'), action: button(t('common.firstPage'), { icon: 'chevronsLeft', size: 'sm', onClick: () => goPage(1) }) };
      } else if (filtered) empty = { icon: 'filter', title: t('logs.noMatch'), desc: t('logs.noMatchDesc'), action: button(t('lg.clearFilters'), { icon: 'x', size: 'sm', onClick: () => resetBtn.click() }) };
      else empty = { icon: 'logs', title: t('logs.empty'), desc: t('logs.emptyDesc') };
      paintHead();
      tbody.replaceChildren(...(st.rows.length ? st.rows.map((r, i) => row(r, i)) : [tableEmptyRow(cols, empty)]));
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
  let keyList = [], tokenList = [], modelList = [], adminTokenList = [];
  const listsLoaded = { key: false, token: false, admin_token: false };
  /** Live object for an event target: {name, hint}; null when it no longer exists; undefined when unknown. */
  function liveObj(tt, id) {
    if (!id || !(tt in listsLoaded) || !listsLoaded[tt]) return undefined;
    const list = tt === 'key' ? keyList : tt === 'token' ? tokenList : adminTokenList;
    const x = list.find((o) => String(o.id) === String(id));
    if (x) return { name: x.name || '', hint: tt === 'key' ? x.key_masked || '' : tt === 'token' ? (x.prefix ? `${x.prefix}…` : '') : x.prefix || '' };
    return tt !== 'admin_token' && list.length >= 200 ? undefined : null; // a full page may not hold every object
  }
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
    if (ks.status === 'fulfilled') { keyList = ks.value.data || []; listsLoaded.key = true; fill(keySel, keyList, t('common.key'), st.key_id, (x) => x.key_masked || ''); }
    if (ts.status === 'fulfilled') { tokenList = ts.value.data || []; listsLoaded.token = true; fill(tokenSel, tokenList, t('common.token'), st.token_id, (x) => (x.prefix ? `${x.prefix}…` : '')); }
    if (ms.status === 'fulfilled') { modelList = (ms.value.data || []).map((m) => m.id).filter(Boolean); fillModels(); }
    if (as.status === 'fulfilled') {
      const ats = Array.isArray(as.value) ? as.value : (as.value && as.value.data) || [];
      adminTokenList = ats; listsLoaded.admin_token = true;
      ats.forEach((a) => actorSel.appendChild(h('option', { value: `admin_token:${a.id}` }, `${t('ev.actor.adminToken')} · ${a.name}`)));
      actorSel.value = st.actor;
    }
    fillTargetList();
    targetBtn.textContent = targetLabel();
    // Rows drawn before the lists arrived: redraw so object names and "deleted" marks appear.
    if (st.loaded && st.rows && st.rows.some((r) => r.type && r.type !== 'request')) { tbody.replaceChildren(...(st.rows.length ? st.rows.map((r, i) => row(r, i)) : [])); markActive(); }
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
    const extra = h('span', { class: 'ld5-acts' });
    if (drawer) {
      drawer.setTitle(titleText); drawer.setBody(loadingBody);
      const acts = drawer.el.querySelector('.modal-header-actions');
      acts.replaceChildren(extra, prev, next, acts.querySelector(':scope > button:last-child'));
    } else {
      drawer = openModal({
        kind: 'drawer', size: 'lg', title: titleText, subtitle: sub || ' ', body: loadingBody, headerExtra: [extra, prev, next],
        onClose: () => { drawer = null; st.openId = null; st.openEvent = null; nextDetailSignal.abort(); syncParams(); markActive(); document.removeEventListener('keydown', onKey); },
      });
      document.addEventListener('keydown', onKey);
    }
    const s = drawer.el.querySelector('.modal-subtitle');
    if (s) s.textContent = sub || '';
    drawerPanel().classList.add('ld5-drawer');
    drawer.extra = extra;
    return drawer;
  }
  const drawerPanel = () => (drawer.el.matches('.drawer-panel') ? drawer.el : drawer.el.querySelector('.drawer-panel') || drawer.el);
  /** v0.5 drawer header: a title node, the block under it and the header actions. */
  function paintDrawerHead(kind, titleNode, headNode, actions) {
    const panel = drawerPanel();
    panel.classList.toggle('ld5-req', kind === 'req');
    panel.classList.toggle('ld5-evd', kind === 'ev');
    drawer.el.querySelector('.modal-title')?.replaceChildren(titleNode);
    drawer.el.querySelector('.modal-subtitle')?.replaceChildren(headNode);
    drawer.extra.replaceChildren(...(actions || []));
  }
  // A chain entry opened from the drawer may not be on this page: arrows then step from the row it was opened from.
  let anchorIdx = -1;
  function curIndex() {
    const i = st.rows.findIndex((r) => (r.type === 'request' || !r.type ? r.id === st.openId && !st.openEvent : r.id === st.openEvent));
    if (i >= 0) anchorIdx = i;
    return i >= 0 ? i : Math.min(anchorIdx, st.rows.length - 1);
  }
  async function openDetail(id) {
    st.openId = id; st.openEvent = null; syncParams(); markActive();
    const summary = st.rows.find((r) => (r.type === 'request' || !r.type) && r.id === id);
    const d0 = ensureDrawer(summary ? `${summary.method} ${summary.path}` : `${t('logs.log')} #${id}`, summary ? fmtDateTime(summary.created_at) : '');
    const signal = nextDetailSignal();
    try {
      const d = await get(`/api/v1/logs/${id}`, null, { signal });
      if (drawer !== d0 || st.openId !== id) return;
      paintDrawerHead('req', requestTitle(d), requestHead(d), requestActions(d));
      drawer.setBody(requestDetailView(d, { injectUsage, openRow: (rid) => openDetail(rid) }));
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
      paintDrawerHead('ev', h('span', { class: 'ld5-evtitle' }, eventKicker(e), h('span', null, eventTitle(e))), h('div', { class: 'ld5-head' }, h('div', { class: 'ld5-con' }, eventSentence(e))), []);
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
    pathOther = false; closeTargetPop();
    fillTargetList();
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
