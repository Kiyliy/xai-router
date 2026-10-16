import { h, fmtInt, fmtUSD, fmtMs, fmtCompact, timeEl, debounce, fmtDateTime, fmtDuration, readableReason } from '../util.js';
import { icon } from '../icons.js';
import { t, hasT } from '../i18n.js';
import { get, post, patch, del } from '../api.js';
import {
  card, toast, toastError, openModal, confirmDialog, menuButton, badge, switchEl, searchInput, select, pagination,
  skeletonRows, tableEmptyRow, tableWrap, field, input, checkbox, button, busy, kv, spinner, passwordInput, tapToMenu, errorMessage,
} from '../ui.js';

const TEST_VARIANT = { ok: 'success', rate_limited: 'warning', invalid: 'danger', blocked: 'danger', no_credits: 'danger', error: 'danger' };

/** Localized one-line explanation of a test status; the server's English message stays available as a tooltip. */
export function testMessage(r) {
  if (!r) return '';
  return hasT(`test.msg.${r.status}`) ? t(`test.msg.${r.status}`) : (r.message || '');
}

export function testBadge(r) {
  if (!r) return h('span', { class: 'muted' }, t('keys.neverTested'));
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
        h('h4', null, t('keys.probe')),
        r.probe ? kv([
          [t('common.model'), h('span', { class: 'mono' }, r.probe.model)],
          [t('common.status'), r.probe.status ? badge(String(r.probe.status), r.probe.status < 300 ? 'success' : 'danger') : null],
          [t('common.latency'), fmtMs(r.probe.latency_ms)],
          [t('common.cost'), fmtUSD(r.probe.cost_usd)],
          r.probe.error ? [t('common.error'), h('span', { class: 'text-danger break' }, r.probe.error)] : null,
        ]) : h('div', { class: 'muted' }, t('keys.noProbe'))),
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

function testResultsTable(results, onOpen) {
  return tableWrap(h('table', { class: 'table' },
    h('thead', null, h('tr', null, h('th', null, t('common.key')), h('th', null, t('common.status')), h('th', null, t('common.message')), h('th', { class: 'num' }, t('common.latency')), h('th', { class: 'num' }, t('keys.probe')), h('th', { class: 'num' }, t('common.cost')))),
    h('tbody', null, results.length ? results.map((r) => h('tr', { class: 'clickable', onclick: () => onOpen(r) },
      h('td', null, h('span', { class: 'strong' }, r.key_name || `#${r.key_id}`)),
      h('td', null, testBadge(r)),
      h('td', { class: 'cell-wrap muted', title: r.message || '' }, testMessage(r)),
      h('td', { class: 'num mono' }, fmtMs(r.latency_ms)),
      h('td', { class: 'num mono' }, r.probe ? fmtMs(r.probe.latency_ms) : '—'),
      h('td', { class: 'num mono' }, r.probe ? fmtUSD(r.probe.cost_usd) : '—'))) : tableEmptyRow(6, {}))), { maxHeight: '50vh' });
}

export function render(el, ctx) {
  const st = { q: ctx.params.q || '', status: ctx.params.status || '', page: Number(ctx.params.page) || 1, pageSize: Number(localStorage.getItem('xr.keys.ps')) || 20, total: 0, rows: [] };
  const sel = new Set();
  let bulkRunning = false;
  const syncParams = () => ctx.setParams({ q: st.q, status: st.status, page: st.page > 1 ? st.page : '' });

  ctx.setActions(
    button(t('keys.testAll'), { icon: 'zap', onClick: (e) => testAll(e.currentTarget) }),
    button(t('keys.bulkImport'), { icon: 'upload', onClick: openImport }),
    button(t('keys.add'), { icon: 'plus', variant: 'primary', onClick: () => openForm() }),
  );

  const search = searchInput({ value: st.q, placeholder: t('keys.searchPh'), onInput: debounce((v) => { st.q = v.trim(); st.page = 1; syncParams(); load(); }, 300) });
  const statusSel = select([{ value: '', label: t('keys.allStatuses') }, { value: 'active', label: t('status.active') }, { value: 'cooldown', label: t('status.cooldown') }, { value: 'disabled', label: t('status.disabled') }], st.status,
    { onchange: (e) => { st.status = e.target.value; st.page = 1; syncParams(); load(); } });
  const countEl = h('span', { class: 'toolbar-count' });
  const COLS = 8;
  const allBox = h('input', { type: 'checkbox', class: 'row-check', 'aria-label': t('common.selectAll'), title: t('common.selectAll'), onchange: () => { if (allBox.checked) st.rows.forEach((k) => sel.add(k.id)); else sel.clear(); syncSelection(); } });
  const tbody = h('tbody', null, skeletonRows(COLS));
  const table = h('table', { class: 'table table-keys table-cards' },
    h('thead', null, h('tr', null,
      h('th', { class: 'col-check' }, allBox),
      h('th', { class: 'col-switch' }, t('common.enabled')), h('th', null, t('common.name')), h('th', null, t('common.status')),
      h('th', { title: `${t('keys.priorityHint')}\n${t('keys.maxConcurrency')} / ${t('keys.rpmLimit')}` }, t('keys.routing')),
      h('th', { class: 'num' }, t('keys.usage')),
      h('th', null, t('keys.lastTest')), h('th', { class: 'col-actions' }, h('span', { class: 'sr-only' }, t('common.actions'))))),
    tbody);
  const pager = h('div');

  // ---- bulk bar ----
  const bulkCount = h('span', { class: 'bulk-count' });
  const bulkProgress = h('span', { class: 'bulk-progress', hidden: true });
  const bulkBtns = [
    button(t('common.enable'), { icon: 'checkCircle', size: 'sm', onClick: () => bulkToggle(true) }),
    button(t('common.disable'), { icon: 'xCircle', size: 'sm', onClick: () => bulkToggle(false) }),
    button(t('keys.bulkTest'), { icon: 'zap', size: 'sm', onClick: () => bulkTest() }),
    button(t('common.delete'), { icon: 'trash', size: 'sm', variant: 'danger-soft', onClick: () => bulkDelete() }),
  ];
  const bulkClear = button(t('common.clearSelection'), { icon: 'x', size: 'sm', variant: 'ghost', onClick: () => { sel.clear(); syncSelection(); } });
  const bulkBar = h('div', { class: 'bulk-bar', hidden: true, role: 'region', 'aria-label': t('common.bulkActions') },
    bulkCount, h('div', { class: 'bulk-actions' }, bulkBtns), bulkProgress, h('div', { class: 'toolbar-spacer' }), bulkClear);

  el.append(card({
    flush: true,
    body: [h('div', { class: 'toolbar' }, search, statusSel, h('div', { class: 'toolbar-spacer' }), countEl), bulkBar, tableWrap(table), pager],
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
  }

  function statusBadgeFor(k) {
    if (k.status === 'active') return badge(t('status.active'), 'success', { class: 'badge-dot' });
    if (k.status === 'cooldown') return badge(t('status.cooldown'), 'warning', { class: 'badge-dot' });
    if (k.auto_disabled) return badge(t('status.autoDisabled'), 'danger', { class: 'badge-dot' });
    return badge(t('status.disabled'), 'neutral', { class: 'badge-dot' });
  }

  function statusCell(k) {
    const parts = [h('span', { class: 'hide-m' }, statusBadgeFor(k))];
    if (k.status === 'cooldown' && k.cooldown_until) parts.push(h('span', { class: 'countdown mono', 'data-countdown': k.cooldown_until, title: fmtDateTime(k.cooldown_until) }, countdownText(k.cooldown_until)));
    if (k.auto_disabled) {
      parts.push(h('button', { class: 'btn btn-sm btn-warning-soft btn-restore', title: t('keys.restoreHint'), onclick: (e) => { e.stopPropagation(); busy(e.currentTarget, () => reset(k)); } }, icon('reset', 13), h('span', null, t('keys.restore'))));
    }
    const rr = readableReason(k.status !== 'active' ? k.status_reason : '');
    const reason = rr.short ? h('div', { class: 'cell-sub reason', title: rr.full }, rr.short) : null;
    const le = readableReason(k.status === 'active' ? k.last_error : '');
    const lastErr = le.short ? h('div', { class: 'cell-sub reason text-warning', title: le.full }, le.short) : null;
    const empty = parts.length === 1 && !reason && !lastErr;
    return h('td', { class: `cell-status m-full ${empty ? 'm-hide' : ''}` }, h('div', { class: 'status-line' }, parts), reason, lastErr);
  }

  function row(k) {
    const errRate = k.requests ? k.errors / k.requests : 0;
    const lim = (v) => (v ? fmtInt(v) : '∞');
    const conflict = k.enabled && k.auto_disabled;
    const sw = switchEl({
      checked: k.enabled,
      title: conflict ? t('keys.autoDisabledSwitch', { reason: readableReason(k.status_reason).short || '—' }) : k.enabled ? t('keys.disable') : t('keys.enable'),
      onChange: async (v) => { try { await patch(`/api/v1/keys/${k.id}`, { enabled: v }); toast(v ? t('keys.enabled') : t('keys.disabled'), 'success', 2000); load(); } catch (e) { toastError(e); return false; } },
    });
    if (conflict) sw.classList.add('switch-warn');
    const menu = menuButton(() => [
      { label: t('common.edit'), icon: 'edit', onClick: () => openForm(k) },
      { label: t('keys.test'), icon: 'zap', onClick: () => testOne(k, null, true) },
      { label: t('keys.testNoProbe'), icon: 'zap', onClick: () => testOne(k, null, false) },
      { label: k.auto_disabled ? t('keys.restore') : t('keys.reset'), icon: 'reset', onClick: () => reset(k), disabled: k.status === 'active' && !k.auto_disabled },
      { label: k.enabled ? t('keys.disable') : t('keys.enable'), icon: k.enabled ? 'xCircle' : 'checkCircle', onClick: () => patch(`/api/v1/keys/${k.id}`, { enabled: !k.enabled }).then(() => { toast(!k.enabled ? t('keys.enabled') : t('keys.disabled'), 'success', 2000); load(); }, toastError) },
      '-',
      { label: t('common.delete'), icon: 'trash', danger: true, onClick: () => remove(k) },
    ]);
    const check = h('input', { type: 'checkbox', class: 'row-check', checked: sel.has(k.id), 'aria-label': t('common.select'), onclick: (e) => e.stopPropagation(), onchange: (e) => { if (e.target.checked) sel.add(k.id); else sel.delete(k.id); syncSelection(); } });
    const subline = k.notes ? h('div', { class: 'cell-sub truncate', title: k.notes }, k.notes) : (k.base_url && k.base_url !== 'https://api.x.ai' ? h('div', { class: 'cell-sub mono truncate', title: k.base_url }, k.base_url) : null);
    const tr = h('tr', { 'data-id': k.id, class: `${k.auto_disabled ? 'row-warn' : !k.enabled ? 'row-muted' : ''} ${sel.has(k.id) ? 'row-selected' : ''}` },
      h('td', { class: 'col-check m-check' }, check),
      h('td', { class: 'col-switch m-meta m-switch' }, sw),
      h('td', { class: 'cell-keyname m-title' },
        h('div', { class: 'title-line' }, h('span', { class: 'strong name-text', title: k.name || '' }, k.name || `#${k.id}`), h('span', { class: 'm-only' }, statusBadgeFor(k))),
        h('div', { class: 'cell-sub mono key-mask' }, k.key_masked || '—'),
        subline),
      statusCell(k),
      h('td', { class: 'mono m-meta', title: `${t('keys.priority')}: ${k.priority ?? 0} · ${t('keys.weight')}: ${k.weight ?? 1}\n${t('keys.maxConcurrency')}: ${lim(k.max_concurrency)}\n${t('keys.rpmLimit')}: ${lim(k.rpm_limit)}\n${t('keys.inflight')}: ${k.inflight || 0}` },
        h('div', { class: 'nowrap' }, `P${k.priority ?? 0}`, h('span', { class: 'muted' }, ` · W${k.weight ?? 1}`)),
        h('div', { class: `cell-sub nowrap ${k.inflight ? 'text-accent' : ''}` }, `${lim(k.max_concurrency)} / ${lim(k.rpm_limit)}`, k.inflight ? ` · ${t('keys.inflightN', { n: k.inflight })}` : '')),
      h('td', { class: 'num mono m-meta', title: `${t('ov.requests')}: ${fmtInt(k.requests)}\n${t('ov.errorsCap')}: ${fmtInt(k.errors)}\n${t('common.cost')}: ${fmtUSD(k.cost_usd)}\n${t('common.lastUsed')}: ${k.last_used_at ? fmtDateTime(k.last_used_at) : '—'}` },
        h('div', { class: 'nowrap' }, fmtCompact(k.requests), h('span', { class: 'muted' }, ` ${t('keys.reqShort')}`), k.errors ? h('span', { class: errRate > 0.1 ? 'text-danger' : 'muted' }, ` · ${fmtCompact(k.errors)} ${t('keys.errShort')}`) : null),
        h('div', { class: 'cell-sub nowrap' }, fmtUSD(k.cost_usd), ' · ', timeEl(k.last_used_at))),
      h('td', { class: 'm-meta' }, k.last_test
        ? h('button', { class: 'link-cell', onclick: (e) => { e.stopPropagation(); showTestResult({ ...k.last_test, key_name: k.name }); }, title: testMessage(k.last_test) }, testBadge(k.last_test), k.last_test.latency_ms != null ? h('span', { class: 'mono muted small' }, fmtMs(k.last_test.latency_ms)) : null)
        : h('span', { class: 'muted small nowrap' }, t('keys.neverTested'))),
      h('td', { class: 'col-actions m-menu' }, h('div', { class: 'row-actions' },
        h('button', { class: 'btn btn-ghost btn-icon btn-sm hide-m', title: t('keys.test'), 'aria-label': t('keys.test'), onclick: (e) => testOne(k, e.currentTarget) }, icon('zap', 15)),
        menu)));
    return tapToMenu(tr, menu);
  }

  let loadSeq = 0;
  async function load() {
    const seq = ++loadSeq;
    try {
      const res = await get('/api/v1/keys', { q: st.q, status: st.status, page: st.page, page_size: st.pageSize }, { signal: ctx.signal });
      if (seq !== loadSeq) return;
      st.rows = res.data || []; st.total = res.total || 0;
      if (!st.rows.length && st.page > 1 && st.total) { st.page = Math.ceil(st.total / st.pageSize); return load(); }
      countEl.textContent = t('common.totalN', { n: fmtInt(st.total) });
      tbody.replaceChildren(...(st.rows.length ? st.rows.map(row) : [tableEmptyRow(COLS, st.q || st.status ? { icon: 'search', title: t('common.noMatches') } : { icon: 'key', title: t('keys.empty'), desc: t('keys.emptyDesc'), action: button(t('keys.add'), { icon: 'plus', variant: 'primary', onClick: () => openForm() }) })]));
      pager.replaceChildren(pagination({ page: st.page, pageSize: st.pageSize, total: st.total, onChange: (p, ps) => { st.page = p; st.pageSize = ps; sel.clear(); try { localStorage.setItem('xr.keys.ps', ps); } catch { /* */ } syncParams(); load(); } }));
      syncSelection();
    } catch (e) {
      if (e.name === 'AbortError') return;
      toastError(e);
      tbody.replaceChildren(tableEmptyRow(COLS, { icon: 'alert', title: t('common.loadFailed'), desc: errorMessage(e) }));
    }
  }

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
    const ok = await confirmDialog({ title: t('keys.bulkDeleteTitle', { n: sel.size }), message: t('keys.bulkDeleteMsg'), confirmText: t('common.delete'), danger: true });
    if (ok) await runBulk(t('common.delete'), (k) => del(`/api/v1/keys/${k.id}`));
  }
  async function bulkTest() {
    const probeBox = checkbox({ checked: false, label: t('keys.probeLabel'), hint: t('keys.probeHint') });
    const ok = await confirmDialog({ title: t('keys.bulkTestTitle', { n: sel.size }), message: t('keys.testAllMsg'), confirmText: t('keys.bulkTest'), extra: probeBox });
    if (!ok) return;
    const probe = probeBox.input.checked;
    const results = (await runBulk(t('keys.bulkTest'), async (k) => ({ ...(await post(`/api/v1/keys/${k.id}/test`, { probe })), key_id: k.id, key_name: k.name }))).filter(Boolean);
    if (!results.length || !ctx.alive()) return;
    openModal({ title: t('keys.testAllResults'), subtitle: t('keys.testSummary', { ok: results.filter((r) => r.ok).length, total: results.length }), size: 'xl', body: testResultsTable(results, showTestResult) });
  }

  async function testOne(k, btn, probe = true) {
    const run = async () => {
      try {
        const r = await post(`/api/v1/keys/${k.id}/test`, { probe });
        toast(`${k.name}: ${t(`test.${r.status}`) || r.status}`, r.ok ? 'success' : 'warning');
        // Always title the dialog with the key's current name (the stored result may carry an old one).
        showTestResult({ ...r, key_name: k.name });
        load();
      } catch (e) { toastError(e); }
    };
    if (btn) await busy(btn, run); else await run();
  }

  async function testAll(btn) {
    const probeBox = checkbox({ checked: true, label: t('keys.probeLabel'), hint: t('keys.probeHint') });
    const ok = await confirmDialog({ title: t('keys.testAllTitle'), message: t('keys.testAllMsg'), confirmText: t('keys.testAll'), extra: probeBox });
    if (!ok) return;
    await busy(btn, async () => {
      try {
        const res = await post('/api/v1/keys/test', { probe: probeBox.input.checked });
        const results = res.results || [];
        const good = results.filter((r) => r.ok).length;
        openModal({
          title: t('keys.testAllResults'), subtitle: t('keys.testSummary', { ok: good, total: results.length }), size: 'xl',
          body: testResultsTable(results, showTestResult),
        });
        load();
      } catch (e) { toastError(e); }
    });
  }

  async function reset(k) {
    try { await post(`/api/v1/keys/${k.id}/reset`, {}); toast(t('keys.resetDone', { name: k.name }), 'success'); await load(); } catch (e) { toastError(e); }
  }

  async function remove(k) {
    const ok = await confirmDialog({ title: t('keys.deleteTitle'), message: t('keys.deleteMsg', { name: k.name || k.key_masked }), confirmText: t('common.delete'), danger: true });
    if (!ok) return;
    try { await del(`/api/v1/keys/${k.id}`); toast(t('common.deleted'), 'success'); load(); } catch (e) { toastError(e); }
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
    const m = openModal({ title: edit ? t('keys.editTitle', { name: k.name }) : t('keys.addTitle'), size: 'md', body, footer: [h('button', { class: 'btn', onclick: () => m.close() }, t('common.cancel')), saveBtn] });
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
      await busy(saveBtn, async () => {
        try {
          if (edit) await patch(`/api/v1/keys/${k.id}`, body); else await post('/api/v1/keys', body);
          toast(edit ? t('common.saved') : t('keys.added'), 'success');
          m.close(); load();
        } catch (e) { if (e.status === 400) showErr(e); else toastError(e); }
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
    const probeBox = checkbox({ checked: false, label: t('keys.probeLabel'), hint: t('keys.probeHint') });
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

  load();
  const timer = setInterval(() => { if (!document.hidden && !document.querySelector('.overlay')) load(); }, 15000);
  return () => clearInterval(timer);
}
