import { h, fmtInt, fmtUSD, fmtMs, timeEl, debounce, fmtDateTime, fmtDuration, readableReason, latestSignal } from '../util.js';
import { jobPanel, asJob } from '../jobs.js';
import { icon } from '../icons.js';
import { t, hasT } from '../i18n.js';
import { get, post, patch, del } from '../api.js';
import {
  card, toast, toastError, openModal, confirmDialog, menuButton, badge, statusDot, switchEl, searchInput, select, pager as pagerEl, setTableLoading,
  skeletonRows, tableEmptyRow, tableWrap, field, input, checkbox, button, busy, kv, spinner, passwordInput, tapToMenu, errorMessage,
} from '../ui.js';
import { drawerState, section, usageBlock, failuresBlock, allLogsLink, goneBody, recentEventsBlock, allEventsLink, listTime, lastAutoDisable } from '../detail-drawer.js';
import { upstreamWhy } from '../events.js';
import { findRequestLog } from '../log-detail.js';

const TEST_VARIANT = { ok: 'success', rate_limited: 'warning', invalid: 'danger', blocked: 'danger', no_credits: 'danger', error: 'danger' };

/** Localized one-line explanation of a test status; the server's English message stays available as a tooltip. */
export function testMessage(r) {
  if (!r) return '';
  return hasT(`test.msg.${r.status}`) ? t(`test.msg.${r.status}`) : (r.message || '');
}

export function testBadge(r) {
  if (!r) return badge(t('kt.untested'), 'neutral');
  return badge(t(`test.${r.status}`) || r.status, TEST_VARIANT[r.status] || 'neutral');
}

/** Detailed view of a KeyTestResult. */
export function testResultView(r) {
  if (!r) return h('div', { class: 'muted' }, t('keys.neverTested'));
  const ak = r.api_key;
  const flag = (on, label) => badge(label, on ? 'danger' : 'success', { class: 'badge-flag' });
  return h('div', { class: 'test-result' },
    h('div', { class: `test-hero test-${TEST_VARIANT[r.status] || 'neutral'}` },
      icon(r.ok ? 'checkCircle' : 'xCircle', 22),
      h('div', null,
        h('div', { class: 'test-hero-title' }, r.key_name ? `${r.key_name} — ` : '', t(`test.${r.status}`) || r.status),
        testMessage(r) ? h('div', { class: 'test-hero-msg', title: r.message || '' }, testMessage(r)) : null,
        r.message && r.status !== 'ok' && r.message !== testMessage(r) ? h('div', { class: 'test-hero-detail mono', title: r.message }, readableReason(r.message).short) : null),
      h('div', { class: 'test-hero-meta' }, r.latency_ms != null ? h('span', { class: 'mono' }, fmtMs(r.latency_ms)) : null, r.tested_at ? timeEl(r.tested_at, 'muted') : null)),
    h('div', { class: 'test-sections' },
      h('div', { class: 'test-section' },
        h('h4', null, t('keys.apiKeyInfo')),
        ak ? [
          kv([
            [t('common.name'), ak.name],
            [t('keys.teamId'), ak.team_id ? h('span', { class: 'mono' }, ak.team_id) : null],
          ]),
          h('div', { class: 'flag-row' },
            flag(ak.api_key_blocked, ak.api_key_blocked ? t('keys.flagKeyBlocked') : t('keys.flagKeyNotBlocked')),
            flag(ak.api_key_disabled, ak.api_key_disabled ? t('keys.flagKeyDisabled') : t('keys.flagKeyEnabled')),
            flag(ak.team_blocked, ak.team_blocked ? t('keys.flagTeamBlocked') : t('keys.flagTeamOk'))),
          ak.acls && ak.acls.length ? h('div', { class: 'chips' }, ak.acls.map((a) => h('span', { class: 'chip mono' }, a))) : null,
        ] : h('div', { class: 'muted' }, t('keys.noApiKeyInfo'))),
      h('div', { class: 'test-section' },
        h('h4', null, t('kt.fullCheck')),
        r.probe ? kv([
          [t('common.model'), h('span', { class: 'mono' }, r.probe.model)],
          [t('common.status'), r.probe.status ? badge(String(r.probe.status), r.probe.status < 300 ? 'success' : 'danger') : null],
          [t('common.latency'), fmtMs(r.probe.latency_ms)],
          [t('common.cost'), fmtUSD(r.probe.cost_usd)],
          r.probe.error ? [t('common.error'), h('span', { class: 'text-danger break' }, r.probe.error)] : null,
        ]) : h('div', { class: 'muted' }, t('kt.noFullCheck'))),
    ),
    h('div', { class: 'test-section' },
      h('h4', null, t('keys.modelsAvailable'), r.models ? h('span', { class: 'count-pill' }, String(r.models.length)) : null),
      r.models && r.models.length ? h('div', { class: 'chips' }, r.models.map((m) => h('span', { class: 'chip mono' }, m))) : h('div', { class: 'muted' }, '—')));
}

function countdownText(iso) {
  const left = (new Date(iso).getTime() - Date.now()) / 1000;
  return left > 0 ? fmtDuration(left) : t('keys.cooldownEnding');
}

function showTestResult(r) {
  openModal({ title: t('keys.testResult'), size: 'lg', body: testResultView(r) });
}

/** Menu label with a second explanatory line (quick check / full check). */
function descLabel(label, desc, costs = false) {
  return h('span', { class: 'menu-label-desc' }, h('span', null, label), h('span', { class: `menu-item-desc ${costs ? 'costs' : ''}` }, desc));
}

function testResultsTable(results, onOpen) {
  return tableWrap(h('table', { class: 'table' },
    h('thead', null, h('tr', null, h('th', null, t('common.key')), h('th', null, t('common.status')), h('th', null, t('common.message')), h('th', { class: 'num' }, t('common.latency')), h('th', { class: 'num' }, t('kt.fullCheck')), h('th', { class: 'num' }, t('common.cost')))),
    h('tbody', null, results.length ? results.map((r) => h('tr', { class: 'clickable', onclick: () => onOpen(r) },
      h('td', null, h('span', { class: 'strong' }, r.key_name || `#${r.key_id}`)),
      h('td', null, testBadge(r)),
      h('td', { class: 'cell-wrap muted', title: r.message || '' }, testMessage(r)),
      h('td', { class: 'num mono' }, fmtMs(r.latency_ms)),
      h('td', { class: 'num mono' }, r.probe ? fmtMs(r.probe.latency_ms) : '—'),
      h('td', { class: 'num mono' }, r.probe ? fmtUSD(r.probe.cost_usd) : '—'))) : tableEmptyRow(6, {}))), { maxHeight: '50vh' });
}

const PAGE_SIZES = [10, 20, 50, 100, 200];
const DEF_PS = 20;
const STATUSES = ['', 'active', 'cooldown', 'disabled'];
function stateFromParams(p) {
  let saved = 0;
  if (!Object.keys(p).length) try { saved = Number(localStorage.getItem('xr.keys.ps')); } catch { /* ignore */ }
  const ps = Number(p.page_size) || saved || DEF_PS;
  return {
    q: p.q || '', status: STATUSES.includes(p.status) ? p.status : '',
    page: Math.max(1, Math.floor(Number(p.page)) || 1), pageSize: PAGE_SIZES.includes(ps) ? ps : DEF_PS,
  };
}

export function render(el, ctx) {
  const st = { ...stateFromParams(ctx.params), total: 0, totalKind: 'exact', rows: [], loaded: false };
  const sel = new Set();
  let bulkRunning = false;
  const listParams = () => ({ q: st.q, status: st.status, page: st.page > 1 ? st.page : '', page_size: st.pageSize !== DEF_PS ? st.pageSize : '' });
  const syncParams = (push = false) => ctx.setParams({ ...listParams(), open: drawer.id || '' }, { push });
  // Key detail drawer, URL #/keys?open={id}.
  const drawer = drawerState({ ctx, getParams: listParams, build: buildDrawer });

  const testAllBtn = button(t('kt.testAll'), { icon: 'zap', attrs: { title: t('kt.testAllHint') }, onClick: (e) => testAll(e.currentTarget) });
  // "Test all" / bulk test run as a background job; the panel survives navigation.
  const testJob = jobPanel({
    kind: 'test_keys', signal: ctx.signal,
    describe: (j) => {
      const results = (j.result && j.result.results) || [];
      return t('keys.testSummary', { ok: results.filter((r) => r.ok).length, total: results.length });
    },
    actions: (j) => {
      const results = (j.result && j.result.results) || [];
      return results.length ? button(t('jobs.viewResults'), { icon: 'logs', size: 'sm', onClick: () => showResults(results) }) : null;
    },
    onFinish: (j) => {
      const results = (j.result && j.result.results) || [];
      if (j.state === 'done') {
        toast(t('keys.testSummary', { ok: results.filter((r) => r.ok).length, total: results.length }), 'success');
        if (results.length && ctx.alive()) showResults(results);
      } else toast(j.error || t('jobs.failed'), 'error', 6000);
      if (ctx.alive()) { load({ silent: true }); drawer.refresh(); }
    },
    onState: (running) => { testAllBtn.disabled = running; syncSelection(); },
  });
  const showResults = (results) => openModal({
    title: t('keys.testAllResults'), subtitle: t('keys.testSummary', { ok: results.filter((r) => r.ok).length, total: results.length }), size: 'xl',
    body: testResultsTable(results, showTestResult),
  });

  ctx.setActions(
    testAllBtn,
    button(t('keys.bulkImport'), { icon: 'upload', onClick: openImport }),
    button(t('keys.add'), { icon: 'plus', variant: 'primary', onClick: () => openForm() }),
  );

  const search = searchInput({ value: st.q, placeholder: t('keys.searchPh'), onInput: debounce((v) => { st.q = v.trim(); st.page = 1; syncParams(); load(); }, 300) });
  const statusSel = select([{ value: '', label: t('keys.allStatuses') }, { value: 'active', label: t('status.active') }, { value: 'cooldown', label: t('status.cooldown') }, { value: 'disabled', label: t('status.disabled') }], st.status,
    { onchange: (e) => { st.status = e.target.value; st.page = 1; syncParams(true); load(); } });
  // Pool status counts (可用 / 冷却中 / 自动禁用 / 已禁用) over every key, independent of the filter.
  const poolSum = h('div', { class: 'kl-summary', 'aria-live': 'polite' });
  const footEl = h('div', { class: 'kl-foot' });
  const COLS = 9;
  const allBox = h('input', { type: 'checkbox', class: 'row-check', 'aria-label': t('common.selectAll'), title: t('common.selectAll'), onchange: () => { if (allBox.checked) st.rows.forEach((k) => sel.add(k.id)); else sel.clear(); syncSelection(); } });
  const tbody = h('tbody', null, skeletonRows(COLS));
  // Column rules live in hover hints on the header (pm-spec 10.8 D8).
  const thHint = (label, hint, cls) => h('th', { class: cls || null }, h('span', { class: 'th-hint', tabindex: 0, 'data-tip': hint }, label));
  const table = h('table', { class: 'table table-keys table-cards' },
    h('thead', null, h('tr', null,
      h('th', { class: 'col-check' }, allBox),
      h('th', { class: 'col-name' }, t('common.name')), h('th', { class: 'col-status' }, t('common.status')),
      thHint(t('keys.priority'), t('keys.priorityHint'), 'num col-prio'),
      thHint(t('keys.weight'), t('keys.weightHint'), 'num col-weight'),
      h('th', { class: 'num col-req' }, t('kl.req24')),
      h('th', { class: 'num col-cost' }, t('kl.cost24')),
      h('th', { class: 'col-test' }, t('keys.lastTest')), h('th', { class: 'col-actions' }, h('span', { class: 'sr-only' }, t('common.actions'))))),
    tbody);
  let use24 = new Map();
  const pager = pagerEl({
    sizes: PAGE_SIZES,
    onChange: (p, ps) => {
      if (ps !== st.pageSize) try { localStorage.setItem('xr.keys.ps', ps); } catch { /* */ }
      st.page = p; st.pageSize = ps; sel.clear(); syncParams(true); load();
    },
  });

  // ---- bulk bar ----
  const bulkCount = h('span', { class: 'bulk-count' });
  const bulkProgress = h('span', { class: 'bulk-progress', hidden: true });
  const bulkBtns = [
    button(t('common.enable'), { icon: 'checkCircle', size: 'sm', onClick: () => bulkToggle(true) }),
    button(t('common.disable'), { icon: 'xCircle', size: 'sm', onClick: () => bulkToggle(false) }),
    button(t('kt.quickCheck'), { icon: 'zap', size: 'sm', onClick: () => bulkTest() }),
    button(t('common.delete'), { icon: 'trash', size: 'sm', variant: 'danger-soft', onClick: () => bulkDelete() }),
  ];
  const bulkClear = button(t('common.clearSelection'), { icon: 'x', size: 'sm', variant: 'ghost', onClick: () => { sel.clear(); syncSelection(); } });
  const bulkBar = h('div', { class: 'bulk-bar', hidden: true, role: 'region', 'aria-label': t('common.bulkActions') },
    bulkCount, h('div', { class: 'bulk-actions' }, bulkBtns), bulkProgress, h('div', { class: 'toolbar-spacer' }), bulkClear);

  const region = h('div', { class: 'table-region' }, h('div', { class: 'loading-bar', 'aria-hidden': 'true' }), tableWrap(table));
  el.append(testJob, h('div', { class: 'kl-toolbar' }, search, statusSel, h('div', { class: 'toolbar-spacer' }), poolSum), card({
    flush: true, cls: 'kl-card',
    body: [bulkBar, region, footEl, pager],
  }));

  function syncSelection() {
    for (const id of [...sel]) if (!st.rows.some((k) => k.id === id)) sel.delete(id);
    bulkBar.hidden = !sel.size && !bulkRunning;
    bulkCount.textContent = t('common.selectedN', { n: sel.size });
    allBox.checked = !!st.rows.length && sel.size === st.rows.length;
    allBox.indeterminate = sel.size > 0 && sel.size < st.rows.length;
    tbody.querySelectorAll('tr[data-id]').forEach((tr) => {
      const on = sel.has(Number(tr.dataset.id));
      tr.classList.toggle('row-selected', on);
      const cb = tr.querySelector('.row-check'); if (cb) cb.checked = on;
    });
    [...bulkBtns, bulkClear].forEach((b) => { b.disabled = bulkRunning || !sel.size; });
    if (testJob.running()) bulkBtns[2].disabled = true;
  }

  function statusBadgeFor(k, { countdown = false } = {}) {
    if (k.status === 'active') return statusDot(t('status.active'), 'success');
    if (k.status === 'cooldown') {
      return statusDot([t('status.cooldown'), countdown && k.cooldown_until ? h('span', { class: 'kl-left' }, t('kl.leftPre'),
        h('span', { class: 'mono', 'data-countdown': k.cooldown_until, title: fmtDateTime(k.cooldown_until) }, countdownText(k.cooldown_until)), t('kl.leftPost')) : null], 'warning');
    }
    if (k.auto_disabled) return statusDot(t('status.autoDisabled'), 'danger');
    return statusDot(t('status.disabled'), 'neutral');
  }

  const restoreBtn = (k) => h('button', { type: 'button', class: 'btn btn-sm kl-restore', title: t('keys.restoreHint'), onclick: (e) => { e.stopPropagation(); busy(e.currentTarget, () => reset(k)); } }, t('keys.restore'));

  // Status dot + text; the reason stays in the hover hint and in the drawer (one line per row).
  function statusCell(k) {
    const rr = readableReason(k.status !== 'active' ? k.status_reason : k.last_error);
    return h('td', { class: 'cell-status m-full', title: k.enabled ? rr.full || null : null },
      h('div', { class: 'status-line' }, h('span', { class: 'hide-m' }, statusBadgeFor(k, { countdown: true })), k.auto_disabled ? restoreBtn(k) : null));
  }

  /** Last check: "正常 · 312 毫秒" / "无效（401）" with the time below; opens the full result. */
  function lastTestCell(k) {
    const r = k.last_test;
    if (!r) return h('td', { class: 'col-test m-meta' }, h('div', { class: 'kl-test is-none', title: t('keys.neverTested') }, icon('clock', 15), h('span', { class: 'kl-test-l' }, t('kt.untested'))));
    const code = (String(r.message || '').match(/\b([45]\d\d)\b/) || [])[1];
    const name = t(`test.${r.status}`) || r.status;
    const label = r.ok ? (r.latency_ms != null ? `${name} · ${fmtMs(r.latency_ms)}` : name) : code ? t('kl.withCode', { s: name, c: code }) : name;
    const v = r.ok ? 'ok' : r.status === 'rate_limited' ? 'warn' : 'bad';
    return h('td', { class: 'col-test m-meta' }, h('button', {
      type: 'button', class: `kl-test is-${v}`, title: testMessage(r) || null,
      onclick: (e) => { e.stopPropagation(); showTestResult({ ...r, key_name: k.name }); },
    }, icon(r.ok ? 'checkCircle' : v === 'warn' ? 'alert' : 'xCircle', 15),
    h('span', { class: 'kl-test-l' }, h('span', null, label), r.tested_at ? h('span', { class: 'kl-test-t', title: fmtDateTime(r.tested_at) }, listTime(r.tested_at)) : null)));
  }

  /** Row / drawer menu: view logs, edit | quick check, full check | restore, enable/disable, delete. */
  function keyMenu(k) {
    return [
      { label: t('kd.viewLogs'), icon: 'logs', onClick: () => ctx.navigate(`#/logs?key_id=${k.id}`) },
      { label: t('common.edit'), icon: 'edit', onClick: () => openForm(k) },
      '-',
      { label: descLabel(t('kt.quickCheck'), t('kt.quickDesc')), icon: 'zap', onClick: () => testOne(k, null, false) },
      { label: descLabel(t('kt.fullCheck'), t('kt.fullDesc'), true), icon: 'activity', onClick: () => testOne(k, null, true) },
      '-',
      k.auto_disabled || k.status !== 'active' ? { label: k.auto_disabled ? t('keys.restore') : t('keys.reset'), icon: 'reset', onClick: () => reset(k) } : null,
      { label: k.enabled ? t('keys.disable') : t('keys.enable'), icon: k.enabled ? 'xCircle' : 'checkCircle', onClick: () => setEnabled(k, !k.enabled) },
      { label: t('common.delete'), icon: 'trash', danger: true, onClick: () => remove(k) },
    ];
  }

  async function setEnabled(k, v) {
    try { await patch(`/api/v1/keys/${k.id}`, { enabled: v }); toast(v ? t('keys.enabled') : t('keys.disabled'), 'success', 2000); load(); drawer.refresh(); return true; }
    catch (e) { toastError(e); return false; }
  }

  // Inline number editor for priority / weight: saves on blur or Enter with
  // PATCH {field}, keeps the row where it is, ticks on success, restores the
  // old value and explains on failure. Esc cancels.
  function inlineNum(k, field, min, label) {
    const tick = h('span', { class: 'inline-ok', 'aria-hidden': 'true' }, icon('check', 13));
    const inp = h('input', { type: 'number', class: 'inline-num', step: 1, value: k[field] ?? (field === 'weight' ? 1 : 0), 'aria-label': `${label} · ${k.name || '#' + k.id}`, title: t('kl.editHint') });
    if (min != null) inp.min = String(min);
    let saving = false;
    const restore = () => { inp.value = String(k[field] ?? (field === 'weight' ? 1 : 0)); inp.classList.remove('invalid'); };
    const save = async () => {
      if (saving) return;
      const raw = inp.value.trim();
      const v = Number(raw);
      if (raw === '' || !Number.isInteger(v) || (min != null && v < min)) {
        restore(); toast(field === 'weight' ? t('keys.weightMin') : t('kl.intRequired'), 'warning'); return;
      }
      if (v === (k[field] ?? null)) return;
      saving = true; inp.disabled = true;
      try {
        const res = await patch(`/api/v1/keys/${k.id}`, { [field]: v });
        k[field] = res && res[field] != null ? res[field] : v;
        inp.value = String(k[field]);
        tick.classList.remove('show'); void tick.offsetWidth; tick.classList.add('show');
        if (drawer.id === k.id) drawer.refresh();
      } catch (e) {
        restore(); toastError(e);
      } finally {
        saving = false; inp.disabled = false;
      }
    };
    inp.addEventListener('click', (e) => e.stopPropagation());
    inp.addEventListener('keydown', (e) => {
      e.stopPropagation();
      if (e.key === 'Enter') { e.preventDefault(); inp.blur(); }
      else if (e.key === 'Escape') { restore(); inp.blur(); }
    });
    inp.addEventListener('blur', save);
    return h('span', { class: 'inline-edit' }, h('span', { class: 'inline-pen', 'aria-hidden': 'true' }, icon('edit', 12)), inp, tick,
      h('span', { class: 'inline-hint', 'aria-hidden': 'true' }, t('kl.editHint')));
  }

  function row(k) {
    // Requests / failures / cost over the last 24 hours (stats breakdown by key).
    const u = use24.get(k.id) || {};
    const req = u.requests || 0, err = u.errors || 0;
    const menu = menuButton(() => keyMenu(k));
    const check = h('input', { type: 'checkbox', class: 'row-check', checked: sel.has(k.id), 'aria-label': t('common.select'), onclick: (e) => e.stopPropagation(), onchange: (e) => { if (e.target.checked) sel.add(k.id); else sel.delete(k.id); syncSelection(); } });
    const hint = [k.name, k.notes, k.base_url && k.base_url !== 'https://api.x.ai' ? k.base_url : '', t('kd.openHint')].filter(Boolean).join('\n');
    const tr = h('tr', { 'data-id': k.id, class: `${k.auto_disabled ? 'row-err' : !k.enabled ? 'row-muted' : ''} ${sel.has(k.id) ? 'row-selected' : ''}` },
      h('td', { class: 'col-check m-check' }, check),
      h('td', { class: 'cell-keyname m-title' },
        h('div', { class: 'title-line' },
          h('button', { type: 'button', class: 'name-link name-text', title: hint, onclick: (e) => { e.stopPropagation(); drawer.open(k.id, { push: true }); } }, k.name || `#${k.id}`),
          h('span', { class: 'kl-mask mono' }, k.key_masked || '—'),
          h('span', { class: 'm-only' }, statusBadgeFor(k)))),
      statusCell(k),
      h('td', { class: 'num col-prio m-meta' }, inlineNum(k, 'priority', null, t('keys.priority'))),
      h('td', { class: 'num col-weight m-meta' }, inlineNum(k, 'weight', 1, t('keys.weight'))),
      h('td', { class: 'num col-req mono m-meta', title: `${t('kd.requests')}: ${fmtInt(req)}\n${t('kd.errors')}: ${fmtInt(err)}` },
        fmtInt(req), h('span', { class: 'kl-sep' }, ' / '), h('span', { class: err && err / Math.max(req, 1) >= 0.01 ? 'text-danger' : 'kl-dim' }, fmtInt(err))),
      h('td', { class: `num col-cost mono m-meta ${u.cost_usd ? '' : 'kl-dim'}` }, fmtUSD(u.cost_usd || 0)),
      lastTestCell(k),
      h('td', { class: 'col-actions m-menu' }, h('div', { class: 'row-actions' },
        h('button', { class: 'btn btn-ghost btn-icon btn-sm hide-m', title: t('kd.viewLogs'), 'aria-label': t('kd.viewLogs'), onclick: (e) => { e.stopPropagation(); ctx.navigate(`#/logs?key_id=${k.id}`); } }, icon('logs', 15)),
        h('button', { class: 'btn btn-ghost btn-icon btn-sm hide-m', title: t('kt.zapTip'), 'aria-label': t('kt.quickCheck'), onclick: (e) => { e.stopPropagation(); testOne(k, e.currentTarget, false); } }, icon('zap', 15)),
        menu)));
    return tapToMenu(tr, menu);
  }

  // Each load aborts the previous one; old rows stay (dimmed) until the new page arrives.
  const nextSignal = latestSignal(ctx.signal);
  async function load({ silent = false } = {}) {
    const signal = nextSignal();
    if (!silent && st.loaded) { setTableLoading(region, true); pager.setLoading(true); }
    try {
      const filtered = !!(st.q || st.status);
      const [res, b24, all] = await Promise.all([
        get('/api/v1/keys', { q: st.q, status: st.status, page: st.page, page_size: st.pageSize }, { signal }),
        get('/api/v1/stats/breakdown', { range: '24h', by: 'key' }, { signal }).catch(() => null),
        // Pool counts need every key; reuse the page when it already holds all of them.
        filtered || st.page > 1 ? get('/api/v1/keys', { page_size: 200 }, { signal }).catch(() => null) : null,
      ]);
      if (signal.aborted) return;
      if (b24) use24 = new Map((b24.data || []).map((r) => [Number(r.id), r]));
      paintSummary(all ? all.data || [] : res.total <= (res.data || []).length ? res.data || [] : null, all ? all.total : res.total);
      let rows = res.data || [];
      if (silent && rows.length === st.rows.length) {
        // Same keys as on screen: keep their current order, so an inline
        // priority edit does not make rows jump on the next refresh.
        const pos = new Map(st.rows.map((k, i) => [k.id, i]));
        if (rows.every((k) => pos.has(k.id))) rows = rows.slice().sort((x, y) => pos.get(x.id) - pos.get(y.id));
      }
      st.rows = rows; st.total = res.total || 0; st.totalKind = res.total_kind || 'exact'; st.loaded = true;
      if (!st.rows.length && st.page > 1 && st.total && st.totalKind === 'exact') { st.page = Math.ceil(st.total / st.pageSize); syncParams(); return load({ silent }); }
      tbody.replaceChildren(...(st.rows.length ? st.rows.map(row) : [tableEmptyRow(COLS, filtered ? { icon: 'search', title: t('common.noMatches'), action: button(t('kl.clearFilters'), { onClick: clearFilters }) } : { icon: 'key', title: t('keys.empty'), desc: t('keys.emptyDesc'), action: button(t('keys.add'), { icon: 'plus', variant: 'primary', onClick: () => openForm() }) })]));
      pager.update({ page: st.page, pageSize: st.pageSize, total: st.total, totalKind: st.totalKind, hasMore: st.rows.length >= st.pageSize, rowCount: st.rows.length, loading: false });
      // One page: a plain "共 14 个上游密钥" line; more: the pager.
      const onePage = st.page === 1 && st.totalKind === 'exact' && st.total <= st.pageSize;
      pager.hidden = onePage; footEl.hidden = !onePage || !st.total;
      footEl.textContent = filtered ? t('kl.matchN', { n: fmtInt(st.total) }) : t('kl.totalN', { n: fmtInt(st.total) });
      syncSelection();
    } catch (e) {
      if (e.name === 'AbortError' || signal.aborted) return;
      pager.setLoading(false);
      if (silent) return;
      toastError(e);
      tbody.replaceChildren(tableEmptyRow(COLS, { icon: 'alert', title: t('common.loadFailed'), desc: errorMessage(e) }));
    } finally {
      if (!signal.aborted) setTableLoading(region, false);
    }
  }

  function clearFilters() {
    st.q = ''; st.status = ''; st.page = 1; search.input.value = ''; statusSel.value = '';
    syncParams(true); load();
  }

  /** "● 可用 10  ● 冷却中 1  ● 自动禁用 2  ● 已禁用 1": the four always add up to the total. */
  function paintSummary(all, total) {
    if (!all || all.length < total) { poolSum.hidden = true; return; }
    const n = { active: 0, cooldown: 0, auto: 0, disabled: 0 };
    for (const k of all) n[k.status === 'active' ? 'active' : k.status === 'cooldown' ? 'cooldown' : k.auto_disabled ? 'auto' : 'disabled']++;
    const item = (key, label, v) => h('span', { class: `kl-sum-item status-dot is-${v}`, 'data-k': key }, h('span', null, label), h('span', { class: 'mono' }, fmtInt(n[key])));
    poolSum.hidden = false;
    poolSum.replaceChildren(
      item('active', t('status.active'), 'success'),
      item('cooldown', t('status.cooldown'), 'warning'),
      item('auto', t('status.autoDisabled'), 'danger'),
      item('disabled', t('status.disabled'), 'neutral'));
  }

  ctx.onParams((p) => {
    const next = stateFromParams(p);
    const listChanged = ['q', 'status', 'page', 'pageSize'].some((f) => next[f] !== st[f]);
    Object.assign(st, next);
    search.input.value = st.q; statusSel.value = st.status;
    if (listChanged) { sel.clear(); load(); }
    drawer.sync(p.open);
    if (p.new) { openForm(); ctx.setParams({ ...listParams(), open: drawer.id || '' }); }
  });

  // ---- bulk actions: one API call per key, three at a time, with live progress ----
  async function runBulk(label, fn) {
    const items = st.rows.filter((k) => sel.has(k.id));
    if (!items.length) return [];
    bulkRunning = true; syncSelection();
    bulkProgress.hidden = false;
    let done = 0, failed = 0;
    const out = [];
    const update = () => { bulkProgress.replaceChildren(spinner(13), h('span', null, `${label} ${done}/${items.length}${failed ? ` · ${t('common.failedN', { n: failed })}` : ''}`)); };
    update();
    const queue = items.slice();
    const worker = async () => {
      while (queue.length && ctx.alive()) {
        const k = queue.shift();
        try { out.push(await fn(k)); } catch (e) { failed++; out.push(null); console.warn('bulk', k.id, e); }
        done++; update();
      }
    };
    await Promise.all([worker(), worker(), worker()]);
    bulkRunning = false;
    bulkProgress.hidden = true;
    if (!ctx.alive()) return out;
    toast(failed ? t('common.bulkDoneFailed', { ok: items.length - failed, total: items.length, n: failed }) : t('common.bulkDone', { n: items.length }), failed ? 'warning' : 'success');
    sel.clear();
    await load();
    return out;
  }
  async function bulkToggle(enabled) {
    await runBulk(enabled ? t('common.enable') : t('common.disable'), (k) => patch(`/api/v1/keys/${k.id}`, { enabled }));
  }
  async function bulkDelete() {
    const ok = await confirmDialog({ title: t('keys.bulkDeleteTitle', { n: sel.size }), message: t('kd.bulkDeleteMsg'), confirmText: t('common.delete'), danger: true });
    if (ok) await runBulk(t('common.delete'), (k) => del(`/api/v1/keys/${k.id}`));
  }
  async function bulkTest() {
    const probeBox = checkbox({ checked: false, label: t('kt.fullCheckOpt'), hint: t('kt.fullCheckOptHint') });
    const ids = st.rows.filter((k) => sel.has(k.id)).map((k) => k.id);
    const ok = await confirmDialog({ title: t('kt.bulkTitle', { n: ids.length }), message: t('kt.testAllMsg'), confirmText: t('kt.quickCheck'), extra: probeBox });
    if (!ok || !ids.length) return;
    await startTest({ ids, probe: probeBox.input.checked });
    sel.clear(); syncSelection();
  }

  async function startTest(body) {
    if (testJob.running()) { toast(t('keys.testRunning'), 'info'); return; }
    try {
      const res = await post('/api/v1/keys/test', { ...body, async: true });
      testJob.track(asJob(res, 'test_keys'));
    } catch (e) { toastError(e); }
  }

  // probe=false is the quick check (GET /v1/api-key only, no cost); probe=true also sends one minimal request.
  async function testOne(k, btn, probe = false) {
    const run = async () => {
      try {
        const r = await post(`/api/v1/keys/${k.id}/test`, { probe });
        const status = t(`test.${r.status}`) || r.status;
        const why = r.ok ? '' : (testMessage(r) || readableReason(r.message).short);
        toast(why ? `${k.name}: ${status} — ${why}` : `${k.name}: ${status}`, r.ok ? 'success' : 'warning', r.ok ? 4200 : 7000);
        // Always title the dialog with the key's current name (the stored result may carry an old one).
        showTestResult({ ...r, key_name: k.name });
        load(); drawer.refresh();
      } catch (e) { toastError(e); }
    };
    if (btn) await busy(btn, run); else await run();
  }

  async function testAll(btn) {
    const ok = await confirmDialog({ title: t('kt.testAllTitle'), message: t('kt.testAllMsg'), confirmText: t('kt.testAll') });
    if (!ok) return;
    await busy(btn, () => startTest({ probe: false }));
    if (testJob.running()) btn.disabled = true;
  }

  async function reset(k) {
    try { await post(`/api/v1/keys/${k.id}/reset`, {}); toast(t('keys.resetDone', { name: k.name }), 'success'); drawer.refresh(); await load(); } catch (e) { toastError(e); }
  }

  async function remove(k) {
    const ok = await confirmDialog({ title: t('keys.deleteTitle'), message: t('kd.deleteMsg', { name: k.name || k.key_masked }), confirmText: t('common.delete'), danger: true });
    if (!ok) return;
    try { await del(`/api/v1/keys/${k.id}`); toast(t('common.deleted'), 'success'); if (drawer.id === k.id) drawer.close(); load(); } catch (e) { toastError(e); }
  }

  function openForm(k) {
    const edit = !!k;
    const f = {
      name: input({ value: k?.name || '', placeholder: 'key-1' }),
      key: passwordInput({ value: '', placeholder: edit ? t('keys.keyKeep') : 'xai-…', class: 'input mono', autocomplete: 'new-password', spellcheck: 'false', 'data-lpignore': 'true' }),
      base_url: input({ value: k?.base_url || '', placeholder: 'https://api.x.ai', class: 'input mono' }),
      priority: input({ type: 'number', value: k?.priority ?? 0, step: 1 }),
      weight: input({ type: 'number', value: k?.weight ?? 1, min: 1, step: 1 }),
      max_concurrency: input({ type: 'number', value: k?.max_concurrency ?? 0, min: 0, step: 1 }),
      rpm_limit: input({ type: 'number', value: k?.rpm_limit ?? 0, min: 0, step: 1 }),
      notes: h('textarea', { class: 'input', rows: 2, placeholder: t('common.notesPh') }, k?.notes || ''),
    };
    const enabled = switchEl({ checked: k ? k.enabled : true, label: t('common.enabled') });
    const formErr = h('div', { class: 'callout callout-danger field-full', role: 'alert', hidden: true }, icon('xCircle', 16), h('span', { class: 'break' }));
    const showErr = (e) => {
      formErr.lastChild.textContent = errorMessage(e);
      formErr.hidden = false;
      const onUrl = /base_url|upstream host|allowed hosts|private|insecure/i.test(e.message || '');
      f.base_url.classList.toggle('invalid', onUrl);
      if (onUrl) f.base_url.focus();
    };
    f.base_url.addEventListener('input', () => { f.base_url.classList.remove('invalid'); formErr.hidden = true; });
    const body = h('form', { class: 'form-grid', onsubmit: (e) => { e.preventDefault(); save(); } },
      formErr,
      field(t('common.name'), f.name),
      field(t('keys.baseUrl'), f.base_url, { hint: t('keys.baseUrlHint') }),
      field(t('keys.apiKey'), f.key, { required: !edit, full: true, hint: edit ? t('keys.keyKeepHint') : null }),
      field(t('keys.priority'), f.priority, { hint: t('keys.priorityHint') }),
      field(t('keys.weight'), f.weight, { hint: t('keys.weightHint') }),
      field(t('keys.maxConcurrency'), f.max_concurrency, { hint: t('common.zeroUnlimited') }),
      field(t('keys.rpmLimit'), f.rpm_limit, { hint: t('common.zeroUnlimited') }),
      field(t('common.notes'), f.notes, { full: true }),
      h('div', { class: 'field field-full' }, enabled),
      h('button', { type: 'submit', hidden: true }));
    const saveBtn = h('button', { class: 'btn btn-primary', onclick: () => save() }, h('span', null, edit ? t('common.save') : t('keys.add')));
    // P0-4: test right after adding a key or replacing its secret (quick check, no cost).
    const autoTest = checkbox({ checked: true, label: t('kt.autoTest') });
    autoTest.classList.add('footer-check');
    if (edit) {
      autoTest.hidden = true;
      f.key.input.addEventListener('input', () => { autoTest.hidden = !f.key.input.value.trim(); });
    }
    const m = openModal({ title: edit ? t('keys.editTitle', { name: k.name }) : t('keys.addTitle'), size: 'md', body, footer: [autoTest, h('button', { class: 'btn', onclick: () => m.close() }, t('common.cancel')), saveBtn] });
    async function save() {
      const num = (x) => (x.value === '' ? undefined : Number(x.value));
      const payload = {
        name: f.name.value.trim(), base_url: f.base_url.value.trim() || undefined,
        priority: num(f.priority), weight: num(f.weight), max_concurrency: num(f.max_concurrency), rpm_limit: num(f.rpm_limit),
        enabled: enabled.input.checked, notes: f.notes.value,
      };
      if (f.key.input.value.trim()) payload.key = f.key.input.value.trim();
      if (!edit && !payload.key) { f.key.input.focus(); f.key.input.classList.add('invalid'); toast(t('keys.keyRequired'), 'warning'); return; }
      if (payload.weight != null && payload.weight < 1) { toast(t('keys.weightMin'), 'warning'); f.weight.focus(); return; }
      let body = payload;
      if (edit) {
        body = {};
        for (const [kk, v] of Object.entries(payload)) if (v !== undefined && v !== k[kk]) body[kk] = v;
        if (!Object.keys(body).length) { m.close(); return; }
      }
      const runTest = autoTest.input.checked && (!edit || body.key !== undefined);
      await busy(saveBtn, async () => {
        let saved;
        try {
          saved = edit ? await patch(`/api/v1/keys/${k.id}`, body) : await post('/api/v1/keys', body);
        } catch (e) { if (e.status === 400) showErr(e); else toastError(e); return; }
        toast(edit ? t('common.saved') : t('keys.added'), 'success', runTest ? 2000 : 4200);
        m.close(); load(); drawer.refresh();
        const target = saved && saved.id ? saved : (edit ? { ...k, ...body } : null);
        if (runTest && target) testOne(target, null, false);
      });
    }
  }

  function openImport() {
    const ta = h('textarea', { class: 'input mono', rows: 9, placeholder: 'xai-xxxxxxxxxxxxxxxx\nbackup,xai-yyyyyyyyyyyyyyyy', spellcheck: 'false' });
    const lineCount = h('span', { class: 'field-hint' }, t('keys.importLines', { n: 0 }));
    ta.addEventListener('input', () => { lineCount.textContent = t('keys.importLines', { n: ta.value.split('\n').filter((l) => l.trim()).length }); });
    const baseUrl = input({ placeholder: 'https://api.x.ai', class: 'input mono' });
    const priority = input({ type: 'number', value: 0, step: 1 });
    const weight = input({ type: 'number', value: 1, min: 1, step: 1 });
    const prefix = input({ value: 'key', placeholder: 'key' });
    const testBox = checkbox({ checked: true, label: t('keys.importTest') });
    const probeBox = checkbox({ checked: false, label: t('kt.fullCheckOpt'), hint: t('kt.fullCheckOptHint') });
    const impErr = h('div', { class: 'callout callout-danger field-full', role: 'alert', hidden: true }, icon('xCircle', 16), h('span', { class: 'break' }));
    baseUrl.addEventListener('input', () => { baseUrl.classList.remove('invalid'); impErr.hidden = true; });
    const form = h('div', { class: 'form-grid' },
      impErr,
      field(t('keys.importKeys'), h('div', null, ta, lineCount), { full: true, hint: t('keys.importHint') }),
      field(t('keys.baseUrl'), baseUrl, { hint: t('keys.baseUrlHint') }),
      field(t('keys.namePrefix'), prefix),
      field(t('keys.priority'), priority),
      field(t('keys.weight'), weight),
      h('div', { class: 'field field-full checks' }, testBox, probeBox));
    const go = h('button', { class: 'btn btn-primary' }, icon('upload', 15), h('span', null, t('keys.import')));
    const cancel = h('button', { class: 'btn', onclick: () => m.close() }, t('common.cancel'));
    const m = openModal({ title: t('keys.bulkImport'), subtitle: t('keys.importSub'), size: 'lg', body: form, footer: [cancel, go] });
    go.addEventListener('click', () => busy(go, async () => {
      if (!ta.value.trim()) { ta.focus(); return; }
      try {
        const res = await post('/api/v1/keys/import', {
          keys: ta.value, base_url: baseUrl.value.trim() || undefined, priority: Number(priority.value) || 0, weight: Number(weight.value) || 1,
          name_prefix: prefix.value.trim() || undefined, test: testBox.input.checked, probe: probeBox.input.checked,
        });
        const created = res.created || [], skipped = res.skipped || [], tests = res.tests || [];
        const okN = tests.filter((x) => x.ok).length;
        m.setTitle(t('keys.importResult'));
        m.setBody(h('div', { class: 'import-result' },
          h('div', { class: 'stat-row' },
            h('div', { class: 'stat stat-success' }, h('div', { class: 'stat-v' }, String(created.length)), h('div', { class: 'stat-l' }, t('keys.created'))),
            h('div', { class: 'stat stat-warning' }, h('div', { class: 'stat-v' }, String(skipped.length)), h('div', { class: 'stat-l' }, t('keys.skipped'))),
            tests.length ? h('div', { class: 'stat' }, h('div', { class: 'stat-v' }, `${okN}/${tests.length}`), h('div', { class: 'stat-l' }, t('keys.testsPassed'))) : null),
          created.length ? [h('h4', { class: 'section-title' }, t('keys.created')), h('div', { class: 'chips' }, created.map((c) => h('span', { class: 'chip' }, h('span', { class: 'strong' }, c.name), ' ', h('span', { class: 'mono muted' }, c.key_masked))))] : null,
          skipped.length ? [h('h4', { class: 'section-title' }, t('keys.skipped')), tableWrap(h('table', { class: 'table table-compact' },
            h('thead', null, h('tr', null, h('th', { class: 'num' }, t('keys.line')), h('th', null, t('common.key')), h('th', null, t('keys.reason')))),
            h('tbody', null, skipped.map((s) => h('tr', null, h('td', { class: 'num mono' }, String(s.line)), h('td', { class: 'mono' }, s.key_masked || '—'), h('td', { class: 'muted' }, s.reason))))), { maxHeight: '220px' })] : null,
          tests.length ? [h('h4', { class: 'section-title' }, t('keys.testResults')), testResultsTable(tests, showTestResult)] : null));
        m.setFooter(h('button', { class: 'btn btn-primary', onclick: () => m.close() }, t('common.done')));
        load();
      } catch (e) {
        if (e.status === 400) {
          impErr.lastChild.textContent = errorMessage(e); impErr.hidden = false;
          if (/base_url|upstream host|allowed hosts|private|insecure/i.test(e.message || '')) { baseUrl.classList.add('invalid'); baseUrl.focus(); }
        } else toastError(e);
      }
    }));
  }

  // ---- detail drawer (frame 123-3982) ----
  function buildDrawer(id, d, signal) {
    const actionsEl = h('div', { class: 'detail-actions' });
    const reasonEl = h('div', { class: 'kd-reason', hidden: true });
    const testEl = h('div');
    const configEl = h('div');
    let k = st.rows.find((x) => x.id === id) || null;
    let autoEv; // undefined = not fetched yet, null = none
    const fetchAuto = () => lastAutoDisable(id, signal).then((e) => { autoEv = e; paintReason(); }).catch(() => { autoEv = null; paintReason(); });
    function paintReason() {
      if (!k) return;
      const rr = readableReason(k.status !== 'active' ? k.status_reason : '');
      if (k.auto_disabled) {
        const why = upstreamWhy((autoEv && autoEv.detail && autoEv.detail.reason) || k.status_reason);
        const text = autoEv ? t('kd.autoAt', { when: listTime(autoEv.created_at), why }) : t('kd.autoNoTime', { why });
        const reqBtn = autoEv && autoEv.request_id ? h('a', {
          href: '#/logs', class: 'link nowrap', onclick: async (e) => {
            e.preventDefault();
            const lid = await findRequestLog(autoEv.request_id, signal).catch(() => null);
            if (lid) ctx.navigate(`#/logs?range=all&open=${lid}`); else toast(t('kd.triggerGone'), 'info');
          },
        }, t('kd.viewTrigger')) : null;
        reasonEl.className = 'kd-reason callout-line is-danger';
        reasonEl.replaceChildren(icon('xCircle', 16), h('span', { class: 'kd-reason-t', title: rr.full || null }, text), reqBtn, restoreBtn(k));
        reasonEl.hidden = false;
      } else if (k.status === 'cooldown' && rr.short) {
        reasonEl.className = 'kd-reason callout-line is-warning';
        reasonEl.replaceChildren(icon('alert', 16), h('span', { class: 'kd-reason-t', title: rr.full }, rr.short));
        reasonEl.hidden = false;
      } else reasonEl.hidden = true;
    }
    const paint = () => {
      if (!k) return;
      d.setHead({ title: k.name || `#${k.id}`, badges: [h('span', { class: 'kd-status' }, statusBadgeFor(k, { countdown: true }))], sub: h('span', { class: 'mono' }, k.key_masked || '—') });
      actionsEl.replaceChildren(
        button(t('common.edit'), { icon: 'edit', size: 'sm', onClick: () => openForm(k) }),
        button(t('kt.quickCheck'), { icon: 'zap', size: 'sm', attrs: { title: t('kt.zapTip') }, onClick: (e) => testOne(k, e.currentTarget, false) }),
        button(t('kd.viewLogs'), { icon: 'logs', size: 'sm', onClick: () => ctx.navigate(`#/logs?key_id=${k.id}`) }),
        k.enabled
          ? button(t('keys.disable'), { icon: 'xCircle', size: 'sm', variant: 'danger-soft', onClick: (e) => busy(e.currentTarget, () => setEnabled(k, false)) })
          : button(t('keys.enable'), { icon: 'checkCircle', size: 'sm', onClick: (e) => busy(e.currentTarget, () => setEnabled(k, true)) }));
      if (k.auto_disabled && autoEv === undefined) fetchAuto();
      paintReason();
      const lt = k.last_test;
      if (lt) {
        const kind = t(lt.probe ? 'kd.checkKind.full' : 'kd.checkKind.quick');
        const meta = [kind, lt.tested_at ? fmtDateTime(lt.tested_at, { seconds: false }) : '', lt.latency_ms != null ? fmtMs(lt.latency_ms) : ''].filter(Boolean).join(' · ');
        const v = lt.ok ? 'ok' : lt.status === 'rate_limited' ? 'warn' : 'bad';
        testEl.replaceChildren(h('button', { type: 'button', class: `kd-test is-${v}`, title: `${testMessage(lt)}\n${t('kd.testDetails')}`, onclick: () => showTestResult({ ...lt, key_name: k.name }) },
          icon(lt.ok ? 'checkCircle' : v === 'warn' ? 'alert' : 'xCircle', 16),
          h('span', { class: 'kd-test-s' }, t(`test.${lt.status}`) || lt.status),
          h('span', { class: 'kd-test-m' }, meta)));
      } else {
        testEl.replaceChildren(h('div', { class: 'kd-test is-none' }, icon('clock', 16), h('span', { class: 'kd-test-s' }, t('keys.neverTested')),
          button(t('kd.testNow'), { icon: 'zap', size: 'sm', onClick: (e) => testOne(k, e.currentTarget, false) })));
      }
      const lim = (v) => (v ? fmtInt(v) : t('kd.noLimit'));
      configEl.replaceChildren(kv([
        [t('keys.baseUrl'), h('span', { class: 'mono break' }, k.base_url || 'https://api.x.ai')],
        [t('kd.prioWeight'), h('span', { class: 'mono' }, `${k.priority ?? 0} / ${k.weight ?? 1}`)],
        [t('keys.maxConcurrency'), h('span', { class: k.max_concurrency ? 'mono' : '' }, lim(k.max_concurrency))],
        [t('keys.rpmLimit'), h('span', { class: k.rpm_limit ? 'mono' : '' }, lim(k.rpm_limit))],
        [t('common.notes'), k.notes ? h('span', { class: 'break' }, k.notes) : null],
        [t('kd.created'), k.created_at ? h('span', { class: 'mono', title: fmtDateTime(k.created_at) }, fmtDateTime(k.created_at, { seconds: false })) : null],
        [t('kd.updated'), k.updated_at ? h('span', { class: 'mono', title: fmtDateTime(k.updated_at) }, fmtDateTime(k.updated_at, { seconds: false })) : null],
      ]));
    };
    const fetchKey = async () => {
      try {
        const was = k && k.auto_disabled;
        k = await get(`/api/v1/keys/${id}`, null, { signal });
        if (signal.aborted) return;
        if (k.auto_disabled && !was) autoEv = undefined;
        paint();
        if (!d.body.querySelector('.entity-body')) mountBody();
      } catch (e) {
        if (e.name === 'AbortError' || signal.aborted) return;
        if (e.status === 404) goneBody(d, { by: 'key', param: 'key_id', id, signal, navigate: ctx.navigate, message: t('kd.notFound') });
        else if (!d.body.querySelector('.entity-body')) d.setBody(h('div', { class: 'drawer-loading' }, icon('alert', 22), h('span', null, errorMessage(e))));
        else toastError(e);
      }
    };
    const mountBody = () => d.setBody(h('div', { class: 'entity-body kd-body' },
      h('div', { class: 'kd-top' }, reasonEl, actionsEl),
      section(t('kd.usage'), usageBlock('key', id, signal)),
      h('div', { class: 'entity-cols' }, section(t('kd.lastCheck'), testEl), section(t('kd.config'), configEl)),
      section(t('kd.recentEvents'), recentEventsBlock({ keyId: id, signal, navigate: ctx.navigate }), allEventsLink(id, ctx.navigate)),
      section(t('kd.recentFailures'), failuresBlock({ param: 'key_id', id, signal, navigate: ctx.navigate }), allLogsLink(`#/logs?key_id=${id}`, ctx.navigate))));
    if (k) { paint(); mountBody(); }
    fetchKey();
    return fetchKey;
  }

  if (ctx.params.new) openForm();
  syncParams();
  load();
  if (ctx.params.open) drawer.open(ctx.params.open);
  testJob.resume();
  // Background refresh, skipped while an inline priority / weight is being edited.
  const timer = setInterval(() => { if (!document.hidden && !document.querySelector('.overlay') && !(document.activeElement && document.activeElement.classList.contains('inline-num'))) load({ silent: true }); }, 15000);
  return () => clearInterval(timer);
}
