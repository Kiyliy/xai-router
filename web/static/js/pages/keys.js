import { h, fmtInt, fmtUSD, fmtMs, fmtCompact, timeEl, debounce, fmtDateTime, fmtDuration } from '../util.js';
import { icon } from '../icons.js';
import { t } from '../i18n.js';
import { get, post, patch, del } from '../api.js';
import {
  card, toast, toastError, openModal, confirmDialog, menuButton, badge, switchEl, searchInput, select, pagination,
  skeletonRows, tableEmptyRow, tableWrap, field, input, checkbox, button, busy, kv, spinner,
} from '../ui.js';

const TEST_VARIANT = { ok: 'success', rate_limited: 'warning', invalid: 'danger', blocked: 'danger', no_credits: 'danger', error: 'danger' };

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
        r.message ? h('div', { class: 'test-hero-msg' }, r.message) : null),
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
      h('td', { class: 'cell-wrap muted' }, r.message || ''),
      h('td', { class: 'num mono' }, fmtMs(r.latency_ms)),
      h('td', { class: 'num mono' }, r.probe ? fmtMs(r.probe.latency_ms) : '—'),
      h('td', { class: 'num mono' }, r.probe ? fmtUSD(r.probe.cost_usd) : '—'))) : tableEmptyRow(6, {}))), { maxHeight: '50vh' });
}

export function render(el, ctx) {
  const st = { q: ctx.params.q || '', status: ctx.params.status || '', page: Number(ctx.params.page) || 1, pageSize: Number(localStorage.getItem('xr.keys.ps')) || 20, total: 0, rows: [] };
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
  const COLS = 11;
  const tbody = h('tbody', null, skeletonRows(COLS));
  const table = h('table', { class: 'table table-keys' },
    h('thead', null, h('tr', null,
      h('th', { class: 'col-switch' }, t('common.enabled')), h('th', null, t('common.name')), h('th', null, t('common.key')), h('th', null, t('common.status')),
      h('th', { class: 'num', title: t('keys.priorityHint') }, t('keys.prioWeight')), h('th', { class: 'num' }, t('keys.limits')), h('th', { class: 'num' }, t('keys.reqErr')),
      h('th', { class: 'num' }, t('common.cost')), h('th', null, t('common.lastUsed')), h('th', null, t('keys.lastTest')), h('th', { class: 'col-actions' }, ''))),
    tbody);
  const pager = h('div');
  el.append(card({
    flush: true,
    body: [h('div', { class: 'toolbar' }, search, statusSel, h('div', { class: 'toolbar-spacer' }), countEl), tableWrap(table), pager],
  }));

  function statusCell(k) {
    const parts = [];
    if (k.status === 'active') parts.push(badge(t('status.active'), 'success', { class: 'badge-dot' }));
    else if (k.status === 'cooldown') {
      parts.push(badge(t('status.cooldown'), 'warning', { class: 'badge-dot' }));
      if (k.cooldown_until) parts.push(h('span', { class: 'countdown mono', 'data-countdown': k.cooldown_until, title: fmtDateTime(k.cooldown_until) }, countdownText(k.cooldown_until)));
    } else if (k.auto_disabled) parts.push(badge(t('status.autoDisabled'), 'danger', { class: 'badge-dot', 'data-tip': k.status_reason || '' }));
    else parts.push(badge(t('status.disabled'), 'neutral', { class: 'badge-dot', 'data-tip': k.status_reason || null }));
    const reason = k.status !== 'active' && k.status_reason ? h('div', { class: 'cell-sub truncate', title: k.status_reason }, k.status_reason) : null;
    const lastErr = k.status === 'active' && k.last_error ? h('div', { class: 'cell-sub truncate text-warning', title: k.last_error }, k.last_error) : null;
    return h('td', { class: 'cell-status' }, h('div', { class: 'status-line' }, parts), reason, lastErr);
  }

  function row(k) {
    const errRate = k.requests ? k.errors / k.requests : 0;
    const lim = (v) => (v ? fmtInt(v) : '∞');
    return h('tr', { class: k.auto_disabled ? 'row-danger' : !k.enabled ? 'row-muted' : '' },
      h('td', { class: 'col-switch' }, switchEl({ checked: k.enabled, title: k.enabled ? t('keys.disable') : t('keys.enable'), onChange: async (v) => { try { await patch(`/api/v1/keys/${k.id}`, { enabled: v }); toast(v ? t('keys.enabled') : t('keys.disabled'), 'success', 2000); load(); } catch (e) { toastError(e); return false; } } })),
      h('td', null, h('div', { class: 'strong' }, k.name || `#${k.id}`), k.notes ? h('div', { class: 'cell-sub truncate', title: k.notes }, k.notes) : (k.base_url && k.base_url !== 'https://api.x.ai' ? h('div', { class: 'cell-sub mono truncate' }, k.base_url) : null)),
      h('td', null, h('span', { class: 'mono key-mask' }, k.key_masked || '—')),
      statusCell(k),
      h('td', { class: 'num mono' }, `P${k.priority ?? 0}`, h('span', { class: 'muted' }, ` · W${k.weight ?? 1}`)),
      h('td', { class: 'num mono', title: `${t('keys.maxConcurrency')}: ${lim(k.max_concurrency)}\n${t('keys.rpmLimit')}: ${lim(k.rpm_limit)}\n${t('keys.inflight')}: ${k.inflight || 0}` },
        h('div', null, `${lim(k.max_concurrency)} / ${lim(k.rpm_limit)}`), h('div', { class: `cell-sub ${k.inflight ? 'text-accent' : ''}` }, t('keys.inflightN', { n: k.inflight || 0 }))),
      h('td', { class: 'num mono' }, h('div', null, fmtCompact(k.requests)), h('div', { class: `cell-sub ${errRate > 0.1 ? 'text-danger' : ''}` }, `${fmtCompact(k.errors)} ${t('ov.errors')}`)),
      h('td', { class: 'num mono' }, fmtUSD(k.cost_usd)),
      h('td', null, timeEl(k.last_used_at)),
      h('td', null, k.last_test ? h('button', { class: 'link-cell', onclick: () => showTestResult(k.last_test), title: k.last_test.message || '' }, testBadge(k.last_test), k.last_test.latency_ms != null ? h('span', { class: 'mono muted small' }, fmtMs(k.last_test.latency_ms)) : null) : h('span', { class: 'muted small' }, t('keys.neverTested'))),
      h('td', { class: 'col-actions' }, h('div', { class: 'row-actions' },
        h('button', { class: 'btn btn-ghost btn-icon btn-sm', title: t('keys.test'), 'aria-label': t('keys.test'), onclick: (e) => testOne(k, e.currentTarget) }, icon('zap', 15)),
        menuButton([
          { label: t('common.edit'), icon: 'edit', onClick: () => openForm(k) },
          { label: t('keys.testNoProbe'), icon: 'zap', onClick: () => testOne(k, null, false) },
          { label: t('keys.reset'), icon: 'reset', onClick: () => reset(k), disabled: k.status === 'active' && !k.auto_disabled },
          { label: k.enabled ? t('keys.disable') : t('keys.enable'), icon: k.enabled ? 'xCircle' : 'checkCircle', onClick: () => patch(`/api/v1/keys/${k.id}`, { enabled: !k.enabled }).then(load, toastError) },
          '-',
          { label: t('common.delete'), icon: 'trash', danger: true, onClick: () => remove(k) },
        ]))));
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
      pager.replaceChildren(pagination({ page: st.page, pageSize: st.pageSize, total: st.total, onChange: (p, ps) => { st.page = p; st.pageSize = ps; try { localStorage.setItem('xr.keys.ps', ps); } catch { /* */ } syncParams(); load(); } }));
    } catch (e) {
      if (e.name === 'AbortError') return;
      toastError(e);
      tbody.replaceChildren(tableEmptyRow(COLS, { icon: 'alert', title: t('common.loadFailed'), desc: e.message }));
    }
  }

  async function testOne(k, btn, probe = true) {
    const run = async () => {
      try {
        const r = await post(`/api/v1/keys/${k.id}/test`, { probe });
        toast(`${k.name}: ${t(`test.${r.status}`) || r.status}`, r.ok ? 'success' : 'warning');
        showTestResult({ key_name: k.name, ...r });
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
    try { await post(`/api/v1/keys/${k.id}/reset`, {}); toast(t('keys.resetDone', { name: k.name }), 'success'); load(); } catch (e) { toastError(e); }
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
      key: input({ value: '', placeholder: edit ? t('keys.keyKeep') : 'xai-…', class: 'input mono', autocomplete: 'off', spellcheck: 'false' }),
      base_url: input({ value: k?.base_url || '', placeholder: 'https://api.x.ai', class: 'input mono' }),
      priority: input({ type: 'number', value: k?.priority ?? 0, step: 1 }),
      weight: input({ type: 'number', value: k?.weight ?? 1, min: 1, step: 1 }),
      max_concurrency: input({ type: 'number', value: k?.max_concurrency ?? 0, min: 0, step: 1 }),
      rpm_limit: input({ type: 'number', value: k?.rpm_limit ?? 0, min: 0, step: 1 }),
      notes: h('textarea', { class: 'input', rows: 2, placeholder: t('common.notesPh') }, k?.notes || ''),
    };
    const enabled = switchEl({ checked: k ? k.enabled : true, label: t('common.enabled') });
    const body = h('form', { class: 'form-grid', onsubmit: (e) => { e.preventDefault(); save(); } },
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
      if (f.key.value.trim()) payload.key = f.key.value.trim();
      if (!edit && !payload.key) { f.key.focus(); f.key.classList.add('invalid'); toast(t('keys.keyRequired'), 'warning'); return; }
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
        } catch (e) { toastError(e); }
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
    const form = h('div', { class: 'form-grid' },
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
      } catch (e) { toastError(e); }
    }));
  }

  load();
  const timer = setInterval(() => { if (!document.hidden && !document.querySelector('.overlay')) load(); }, 15000);
  return () => clearInterval(timer);
}
