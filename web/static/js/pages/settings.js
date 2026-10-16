import { h, fmtBytes, fmtDuration, fmtDateTime, timeEl, fmtInt, toLocalInput } from '../util.js';
import { icon } from '../icons.js';
import { t } from '../i18n.js';
import { get, put, post, del } from '../api.js';
import {
  card, toast, toastError, openModal, confirmDialog, switchEl, field, input, select, button, busy, tagInput,
  kv, tableWrap, tableEmptyRow, skeletonRows, spinner,
} from '../ui.js';
import { showSecret } from './tokens.js';

// Field schema mirrors components.schemas.Settings in openapi.yaml.
const SECTIONS = [
  { id: 'routing', icon: 'layers', fields: [
    { k: 'upstream_base_url', type: 'text', mono: true },
    { k: 'selection_strategy', type: 'select', options: ['weighted', 'random', 'round_robin'] },
    { k: 'max_attempts', type: 'int', min: 1 },
    { k: 'retry_status_codes', type: 'intlist', full: true },
    { k: 'retry_keywords', type: 'strlist', full: true },
    { k: 'disable_keywords', type: 'strlist', full: true },
    { k: 'cooldown_seconds', type: 'int', unit: 's', min: 0 },
    { k: 'breaker_threshold', type: 'int', min: 0 },
    { k: 'breaker_seconds', type: 'int', unit: 's', min: 0 },
    { k: 'task_binding_ttl_hours', type: 'int', unit: 'h', min: 0 },
  ] },
  { id: 'timeouts', icon: 'timer', fields: [
    { k: 'request_timeout_seconds', type: 'int', unit: 's', min: 0 },
    { k: 'connect_timeout_seconds', type: 'int', unit: 's', min: 0 },
    { k: 'stream_idle_timeout_seconds', type: 'int', unit: 's', min: 0 },
    { k: 'max_body_mb', type: 'int', unit: 'MB', min: 1 },
  ] },
  { id: 'logging', icon: 'logs', fields: [
    { k: 'log_enabled', type: 'bool' },
    { k: 'log_bodies', type: 'bool' },
    { k: 'log_body_max_kb', type: 'int', unit: 'KB', min: 0 },
    { k: 'log_retention_days', type: 'int', unit: 'd', min: 0 },
  ] },
  { id: 'models', icon: 'cpu', fields: [
    { k: 'model_sync_interval_hours', type: 'int', unit: 'h', min: 0 },
    { k: 'key_test_model', type: 'text', mono: true, models: true },
    { k: 'inject_stream_usage', type: 'bool', full: true },
    { k: 'anthropic_stream_fix', type: 'bool', full: true },
  ] },
  { id: 'security', icon: 'shield', fields: [
    { k: 'trusted_proxies', type: 'lines', full: true },
    { k: 'expose_debug', type: 'bool', full: true },
  ] },
];

const eq = (a, b) => JSON.stringify(a ?? null) === JSON.stringify(b ?? null);

export function render(el, ctx) {
  let orig = null;
  const controls = {};
  const formCol = h('div', { class: 'settings-main' });
  const sideCol = h('div', { class: 'settings-side' });
  el.append(h('div', { class: 'settings-layout' }, formCol, sideCol));

  // ---- save bar ----
  const dirtyText = h('span');
  const saveBtn = h('button', { class: 'btn btn-primary', onclick: () => save() }, h('span', null, t('common.saveChanges')));
  const discardBtn = h('button', { class: 'btn', onclick: () => fill(orig) }, t('common.discard'));
  const saveBar = h('div', { class: 'save-bar', hidden: true }, icon('info', 16), dirtyText, h('div', { class: 'toolbar-spacer' }), discardBtn, saveBtn);

  function readValue(f) {
    const c = controls[f.k];
    if (!c) return undefined;
    switch (f.type) {
      case 'int': return c.value === '' ? null : Number(c.value);
      case 'bool': return c.input.checked;
      case 'intlist': return c.peek().map(Number).filter((n) => Number.isFinite(n));
      case 'strlist': return c.peek();
      case 'lines': return c.value.split('\n').map((x) => x.trim()).filter(Boolean);
      default: return c.value.trim();
    }
  }
  function diff() {
    const out = {};
    if (!orig) return out;
    for (const s of SECTIONS) for (const f of s.fields) {
      const v = readValue(f);
      if (v === undefined || v === null && f.type === 'int') continue;
      if (!eq(v, orig[f.k] ?? (f.type.endsWith('list') || f.type === 'lines' ? [] : f.type === 'bool' ? false : ''))) out[f.k] = v;
    }
    return out;
  }
  function refreshDirty() {
    const d = diff();
    const n = Object.keys(d).length;
    saveBar.hidden = !n;
    dirtyText.textContent = t('settings.unsaved', { n });
    for (const s of SECTIONS) for (const f of s.fields) {
      const wrap = controls[f.k] && controls[f.k].closest('.field');
      if (wrap) wrap.classList.toggle('field-dirty', f.k in d);
    }
  }
  let rdT;
  const scheduleDirty = () => { clearTimeout(rdT); rdT = setTimeout(refreshDirty, 60); };

  function control(f, modelList) {
    let c;
    switch (f.type) {
      case 'select': c = select(f.options.map((o) => ({ value: o, label: t(`settings.opt.${o}`) })), ''); break;
      case 'int': c = input({ type: 'number', step: 1, min: f.min }); break;
      case 'bool': c = switchEl({ checked: false }); break;
      case 'intlist': c = tagInput({ placeholder: t('settings.addCode'), validate: (v) => /^\d{3}$/.test(v) }); break;
      case 'strlist': c = tagInput({ placeholder: t('settings.addKeyword'), mono: false }); break;
      case 'lines': c = h('textarea', { class: 'input mono', rows: 4, placeholder: '10.0.0.0/8\n172.16.0.0/12\n127.0.0.1/32', spellcheck: 'false' }); break;
      default: c = input({ class: `input ${f.mono ? 'mono' : ''}`, list: f.models ? 'dl-settings-models' : null, placeholder: f.models ? t('settings.cheapestModel') : '' });
    }
    controls[f.k] = c;
    let node = c;
    if (f.unit) node = h('div', { class: 'input-affix input-affix-end' }, c, h('span', { class: 'affix' }, f.unit));
    if (f.type === 'bool') {
      return h('div', { class: `field field-bool ${f.full ? 'field-full' : ''}` },
        h('div', { class: 'field-bool-text' }, h('div', { class: 'field-label' }, t(`settings.f.${f.k}`)), h('div', { class: 'field-hint' }, t(`settings.h.${f.k}`))), c);
    }
    return field(t(`settings.f.${f.k}`), node, { hint: t(`settings.h.${f.k}`), full: f.full });
  }

  function fill(s) {
    if (!s) return;
    for (const sec of SECTIONS) for (const f of sec.fields) {
      const c = controls[f.k]; const v = s[f.k];
      switch (f.type) {
        case 'bool': c.input.checked = !!v; break;
        case 'intlist': case 'strlist': c.setValues((v || []).map(String)); break;
        case 'lines': c.value = (v || []).join('\n'); break;
        case 'int': c.value = v ?? ''; break;
        default: c.value = v ?? '';
      }
    }
    refreshDirty();
  }

  const dlModels = h('datalist', { id: 'dl-settings-models' });
  const form = h('form', { class: 'settings-form', onsubmit: (e) => { e.preventDefault(); save(); }, oninput: scheduleDirty, onchange: scheduleDirty, onclick: scheduleDirty, onkeyup: scheduleDirty },
    SECTIONS.map((sec) => card({
      title: h('span', { class: 'card-title-ic' }, icon(sec.icon, 16), t(`settings.s.${sec.id}`)), subtitle: t(`settings.sd.${sec.id}`),
      body: h('div', { class: 'form-grid' }, sec.fields.map((f) => control(f))),
    })),
    dlModels, saveBar);
  const loadingEl = h('div', { class: 'card card-loading' }, spinner(20), h('span', { class: 'muted' }, t('common.loading')));
  formCol.append(loadingEl);

  async function save() {
    const d = diff();
    if (!Object.keys(d).length) return;
    await busy(saveBtn, async () => {
      try {
        const res = await put('/api/v1/settings', d);
        orig = res && typeof res === 'object' ? res : { ...orig, ...d };
        fill(orig);
        toast(t('settings.saved'), 'success');
      } catch (e) { toastError(e); }
    });
  }

  // ---- side: system info ----
  const infoBody = h('div', null, h('div', { class: 'skeleton', style: { width: '80%' } }));
  sideCol.append(card({ title: h('span', { class: 'card-title-ic' }, icon('server', 16), t('settings.system')), body: infoBody, actions: button('', { icon: 'refresh', variant: 'ghost', size: 'icon', attrs: { title: t('common.refresh') }, onClick: loadInfo }) }));
  async function loadInfo() {
    try {
      const i = await ctx.refreshInfo();
      if (!i) return;
      infoBody.replaceChildren(kv([
        [t('settings.version'), h('span', { class: 'mono' }, i.version || '—')],
        [t('settings.commit'), i.commit ? h('span', { class: 'mono' }, i.commit.slice(0, 12)) : null],
        [t('settings.buildDate'), i.build_date ? h('span', { class: 'mono' }, i.build_date) : null],
        [t('settings.goVersion'), h('span', { class: 'mono' }, i.go_version || '—')],
        [t('settings.startedAt'), i.started_at ? h('span', { title: fmtDateTime(i.started_at) }, fmtDateTime(i.started_at, { seconds: false })) : null],
        [t('settings.uptime'), fmtDuration(i.uptime_seconds)],
        [t('settings.dbSize'), fmtBytes(i.db_size_bytes)],
        [t('settings.logQueue'), fmtInt(i.log_queue_length)],
        [t('settings.logDropped'), h('span', { class: i.log_dropped ? 'text-warning' : '' }, fmtInt(i.log_dropped))],
      ]));
    } catch (e) { toastError(e); }
  }

  // ---- side: password ----
  const oldPw = input({ type: 'password', autocomplete: 'current-password' });
  const newPw = input({ type: 'password', autocomplete: 'new-password', minlength: 8 });
  const newPw2 = input({ type: 'password', autocomplete: 'new-password' });
  const pwBtn = h('button', { class: 'btn btn-primary', type: 'submit' }, h('span', null, t('settings.changePassword')));
  const pwCard = card({
    title: h('span', { class: 'card-title-ic' }, icon('lock', 16), t('settings.password')), subtitle: t('settings.passwordSub'),
    body: h('form', { class: 'stack', onsubmit: (e) => { e.preventDefault(); changePw(); } },
      field(t('settings.oldPassword'), oldPw),
      field(t('settings.newPassword'), newPw, { hint: t('settings.minChars') }),
      field(t('settings.confirmPassword'), newPw2),
      h('div', null, pwBtn)),
  });
  pwCard.id = 'password';
  sideCol.append(pwCard);
  async function changePw() {
    if (newPw.value.length < 8) { toast(t('settings.minChars'), 'warning'); newPw.focus(); return; }
    if (newPw.value !== newPw2.value) { toast(t('settings.mismatch'), 'warning'); newPw2.focus(); return; }
    await busy(pwBtn, async () => {
      try {
        await put('/api/v1/auth/password', { old_password: oldPw.value, new_password: newPw.value }, { noAuthRedirect: true });
        oldPw.value = newPw.value = newPw2.value = '';
        toast(t('settings.passwordChanged'), 'success');
        ctx.refreshInfo();
      } catch (e) { toastError(e.status === 401 ? { message: t('settings.wrongPassword') } : e); }
    });
  }

  // ---- side: admin tokens ----
  const atBody = h('tbody', null, skeletonRows(4, 2));
  sideCol.append(card({
    title: h('span', { class: 'card-title-ic' }, icon('key', 16), t('settings.adminTokens')), subtitle: t('settings.adminTokensSub'), flush: true,
    actions: button(t('common.create'), { icon: 'plus', size: 'sm', onClick: createAdminToken }),
    body: tableWrap(h('table', { class: 'table table-compact' },
      h('thead', null, h('tr', null, h('th', null, t('common.name')), h('th', null, t('common.lastUsed')), h('th', null, t('common.created')), h('th', { class: 'col-actions' }, ''))),
      atBody)),
  }));
  async function loadAdminTokens() {
    try {
      const res = await get('/api/v1/admin-tokens', null, { signal: ctx.signal });
      const rows = res.data || [];
      atBody.replaceChildren(...(rows.length ? rows.map((a) => h('tr', null,
        h('td', null, h('div', { class: 'strong' }, a.name), h('div', { class: 'cell-sub mono' }, `${a.prefix || ''}…`)),
        h('td', null, timeEl(a.last_used_at)),
        h('td', null, timeEl(a.created_at)),
        h('td', { class: 'col-actions' }, h('button', { class: 'btn btn-ghost btn-sm btn-danger-ghost', onclick: () => revoke(a) }, t('settings.revoke'))))) : [tableEmptyRow(4, { icon: 'key', title: t('settings.noAdminTokens'), desc: t('settings.noAdminTokensDesc') })]));
    } catch (e) { if (e.name !== 'AbortError') toastError(e); }
  }
  function createAdminToken() {
    const name = input({ placeholder: 'ci-script', autofocus: true });
    const go = h('button', { class: 'btn btn-primary', type: 'submit' }, h('span', null, t('common.create')));
    const m = openModal({
      title: t('settings.newAdminToken'), size: 'sm',
      body: h('form', { class: 'stack', onsubmit: async (e) => {
        e.preventDefault();
        if (!name.value.trim()) { name.focus(); return; }
        await busy(go, async () => {
          try { const r = await post('/api/v1/admin-tokens', { name: name.value.trim() }); m.close(); showSecret({ title: t('settings.adminTokenCreated', { name: r.name }), secret: r.token, kind: 'admin' }); loadAdminTokens(); }
          catch (err) { toastError(err); }
        });
      } }, field(t('common.name'), name), h('div', { class: 'modal-inline-actions' }, h('button', { type: 'button', class: 'btn', onclick: () => m.close() }, t('common.cancel')), go)),
    });
  }
  async function revoke(a) {
    const ok = await confirmDialog({ title: t('settings.revokeTitle'), message: t('settings.revokeMsg', { name: a.name }), confirmText: t('settings.revoke'), danger: true });
    if (!ok) return;
    try { await del(`/api/v1/admin-tokens/${a.id}`); toast(t('settings.revoked'), 'success'); loadAdminTokens(); } catch (e) { toastError(e); }
  }

  // ---- side: maintenance ----
  const before = input({ type: 'datetime-local', value: toLocalInput(new Date(Date.now() - 30 * 86400e3).toISOString()) });
  const purgeBtn = h('button', { class: 'btn btn-danger-soft', onclick: () => purge() }, icon('trash', 15), h('span', null, t('settings.purge')));
  sideCol.append(card({
    title: h('span', { class: 'card-title-ic' }, icon('database', 16), t('settings.maintenance')), subtitle: t('settings.maintenanceSub'),
    body: h('div', { class: 'stack' }, field(t('settings.deleteBefore'), before), h('div', null, purgeBtn)),
  }));
  async function purge() {
    if (!before.value) return;
    const iso = new Date(before.value).toISOString();
    const ok = await confirmDialog({ title: t('settings.purgeTitle'), message: t('settings.purgeMsg', { d: fmtDateTime(iso, { seconds: false }) }), confirmText: t('settings.purge'), danger: true });
    if (!ok) return;
    await busy(purgeBtn, async () => {
      try { const r = await del('/api/v1/logs', { before: iso }); toast(t('settings.purged', { n: fmtInt(r?.deleted ?? 0) }), 'success'); } catch (e) { toastError(e); }
    });
  }

  // ---- load ----
  (async () => {
    try {
      const [s, ms] = await Promise.all([get('/api/v1/settings', null, { signal: ctx.signal }), get('/api/v1/models', { kind: 'language' }, { signal: ctx.signal }).catch(() => ({ data: [] }))]);
      orig = s || {};
      dlModels.replaceChildren(...(ms.data || []).map((m) => h('option', { value: m.id })));
      loadingEl.replaceWith(form);
      fill(orig);
    } catch (e) {
      if (e.name === 'AbortError') return;
      toastError(e);
      loadingEl.replaceChildren(icon('alert', 18), h('span', null, e.message));
    }
  })();
  loadInfo();
  loadAdminTokens();
  if (ctx.params.section) setTimeout(() => {
    const target = el.querySelector(`#${CSS.escape(ctx.params.section)}`);
    if (target) { target.scrollIntoView({ behavior: 'smooth', block: 'start' }); target.classList.add('flash'); const f = target.querySelector('input'); if (f) setTimeout(() => f.focus({ preventScroll: true }), 400); }
  }, 150);

  const beforeUnload = (e) => { if (Object.keys(diff()).length) { e.preventDefault(); e.returnValue = ''; } };
  window.addEventListener('beforeunload', beforeUnload);
  return () => window.removeEventListener('beforeunload', beforeUnload);
}
