import { h, fmtBytes, fmtDuration, fmtDateTime, timeEl, fmtInt, toLocalInput } from '../util.js';
import { icon } from '../icons.js';
import { t } from '../i18n.js';
import { get, put, post, del } from '../api.js';
import {
  card, toast, toastError, openModal, confirmDialog, switchEl, field, input, select, button, busy, tagInput,
  kv, tableWrap, tableEmptyRow, skeletonRows, spinner, passwordInput, checkbox, errorMessage,
} from '../ui.js';
import { showSecret } from './tokens.js';

// Field schema mirrors components.schemas.Settings in openapi.yaml; min/max mirror
// internal/config/settings.go Validate() so the browser flags out-of-range values before saving.
const SECTIONS = [
  { id: 'routing', icon: 'layers', fields: [
    { k: 'upstream_base_url', type: 'text', mono: true },
    { k: 'selection_strategy', type: 'select', options: ['weighted', 'random', 'round_robin'] },
    { k: 'max_attempts', type: 'int', min: 1, max: 50 },
    { k: 'retry_status_codes', type: 'intlist', full: true },
    { k: 'retry_keywords', type: 'strlist', full: true },
    { k: 'disable_keywords', type: 'strlist', full: true },
    { k: 'cooldown_seconds', type: 'int', unit: 's', min: 1, max: 86400 },
    { k: 'breaker_threshold', type: 'int', min: 1, max: 100 },
    { k: 'breaker_seconds', type: 'int', unit: 's', min: 1, max: 86400 },
    { k: 'task_binding_ttl_hours', type: 'int', unit: 'h', min: 1, max: 8760 },
  ] },
  { id: 'timeouts', icon: 'timer', fields: [
    { k: 'request_timeout_seconds', type: 'int', unit: 's', min: 5, max: 7200 },
    { k: 'connect_timeout_seconds', type: 'int', unit: 's', min: 1, max: 300 },
    { k: 'stream_idle_timeout_seconds', type: 'int', unit: 's', min: 5, max: 7200 },
    { k: 'max_body_mb', type: 'int', unit: 'MB', min: 1, max: 1024 },
  ] },
  { id: 'logging', icon: 'logs', fields: [
    { k: 'log_enabled', type: 'bool' },
    { k: 'log_bodies', type: 'bool' },
    { k: 'log_body_max_kb', type: 'int', unit: 'KB', min: 1, max: 65536 },
    { k: 'ws_log_messages', type: 'int', min: 0, max: 10000 },
    { k: 'log_retention_days', type: 'int', unit: 'd', min: 0, max: 3650 },
  ] },
  { id: 'models', icon: 'cpu', fields: [
    { k: 'model_sync_interval_hours', type: 'int', unit: 'h', min: 0, max: 8760 },
    { k: 'key_test_model', type: 'text', mono: true, models: true },
    { k: 'unpriced_models', type: 'select', options: ['block_quota_tokens', 'allow'], def: 'block_quota_tokens', full: true },
    { k: 'inject_stream_usage', type: 'bool', full: true },
    { k: 'anthropic_stream_fix', type: 'bool', full: true },
  ] },
  { id: 'security', icon: 'shield', fields: [
    { k: 'upstream_allowed_hosts', type: 'strlist', full: true, mono: true },
    { k: 'allow_insecure_upstreams', type: 'bool', full: true },
    { k: 'allow_private_upstreams', type: 'bool', full: true, warn: true },
    { k: 'trusted_proxies', type: 'lines', full: true },
    { k: 'expose_debug', type: 'bool', full: true },
  ] },
];
const NAV = ['routing', 'timeouts', 'logging', 'models', 'security', 'account'];

/** Change-password form, shared by the Settings page and the "initial password" banner dialog. */
export function passwordForm({ onDone, autofocus = false } = {}) {
  const oldPw = passwordInput({ autocomplete: 'current-password', autofocus });
  const newPw = passwordInput({ autocomplete: 'new-password', minlength: 8 });
  const newPw2 = passwordInput({ autocomplete: 'new-password' });
  const revoke = checkbox({ checked: false, label: t('settings.revokeAdminTokens'), hint: t('settings.revokeAdminTokensHint') });
  const btn = h('button', { class: 'btn btn-primary', type: 'submit' }, h('span', null, t('settings.changePassword')));
  const form = h('form', { class: 'stack', onsubmit: (e) => { e.preventDefault(); submit(); } },
    field(t('settings.oldPassword'), oldPw),
    field(t('settings.newPassword'), newPw, { hint: t('settings.minChars') }),
    field(t('settings.confirmPassword'), newPw2),
    revoke,
    h('div', null, btn));
  async function submit() {
    if (!oldPw.input.value) { oldPw.input.focus(); return; }
    if (newPw.input.value.length < 8) { toast(t('settings.minChars'), 'warning'); newPw.input.focus(); return; }
    if (newPw.input.value !== newPw2.input.value) { toast(t('settings.mismatch'), 'warning'); newPw2.input.focus(); return; }
    await busy(btn, async () => {
      try {
        const body = { old_password: oldPw.input.value, new_password: newPw.input.value };
        if (revoke.input.checked) body.revoke_admin_tokens = true;
        await put('/api/v1/auth/password', body, { noAuthRedirect: true });
        oldPw.input.value = newPw.input.value = newPw2.input.value = '';
        revoke.input.checked = false;
        toast(body.revoke_admin_tokens ? t('settings.passwordChangedRevoked') : t('settings.passwordChanged'), 'success');
        if (onDone) onDone(body);
      } catch (e) { toastError(e.status === 401 ? { message: t('settings.wrongPassword') } : e); }
    });
  }
  return form;
}

export function openPasswordDialog({ onDone } = {}) {
  const m = openModal({
    title: t('settings.password'), subtitle: t('settings.passwordSub'), size: 'sm',
    body: passwordForm({ autofocus: true, onDone: (b) => { m.close(); if (onDone) onDone(b); } }),
  });
  return m;
}

const eq = (a, b) => JSON.stringify(a ?? null) === JSON.stringify(b ?? null);

export function render(el, ctx) {
  let orig = null;
  const controls = {};
  const formCol = h('div', { class: 'settings-main' });
  const sideCol = h('div', { class: 'settings-side' });
  const navLinks = NAV.map((id) => h('a', {
    class: 'snav-item', href: `#/settings?section=${id}`, 'data-sec': id,
    onclick: (e) => { e.preventDefault(); jump(id); },
  }, t(id === 'account' ? 'settings.s.account' : `settings.s.${id}`)));
  const snav = h('nav', { class: 'settings-nav', 'aria-label': t('settings.sections') }, navLinks);
  el.append(snav, h('div', { class: 'settings-layout' }, formCol, sideCol));
  let spyLock = 0;
  function markNav(id) { navLinks.forEach((a) => a.classList.toggle('active', a.dataset.sec === id)); const cur = navLinks.find((a) => a.dataset.sec === id); if (cur && cur.scrollIntoView && snav.scrollWidth > snav.clientWidth) snav.scrollLeft = cur.offsetLeft - 16; }
  function jump(id) {
    const target = el.querySelector(`#sec-${id}`);
    if (!target) return;
    markNav(id); spyLock = Date.now() + 900;
    target.scrollIntoView({ behavior: 'smooth', block: 'start' });
    target.classList.remove('flash'); void target.offsetWidth; target.classList.add('flash');
    ctx.setParams({ section: id });
  }
  const onScroll = () => {
    if (Date.now() < spyLock) return;
    const line = (parseFloat(getComputedStyle(document.documentElement).getPropertyValue('--topbar-real')) || 64) + snav.offsetHeight + 40;
    let best = null, bestTop = -Infinity;
    // With the two-column layout the account card sits beside the form; it never "owns" the scroll position.
    const sideBySide = sideCol.getBoundingClientRect().left > formCol.getBoundingClientRect().left + 10;
    for (const id of NAV) {
      if (id === 'account' && sideBySide) continue;
      const n = el.querySelector(`#sec-${id}`); if (!n) continue;
      const top = n.getBoundingClientRect().top;
      if (top <= line && top > bestTop) { best = id; bestTop = top; }
    }
    if (window.innerHeight + window.scrollY >= document.documentElement.scrollHeight - 4 && best !== 'account') best = 'security';
    markNav(best || 'routing');
  };
  window.addEventListener('scroll', onScroll, { passive: true });
  ctx.setGuard(() => Object.keys(diff()).length > 0);

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
      if (!eq(v, orig[f.k] ?? f.def ?? (f.type.endsWith('list') || f.type === 'lines' ? [] : f.type === 'bool' ? false : ''))) out[f.k] = v;
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
      case 'int': c = input({ type: 'number', step: 1, min: f.min, max: f.max, inputmode: 'numeric' }); break;
      case 'bool': c = switchEl({ checked: false }); break;
      case 'intlist': c = tagInput({ placeholder: t('settings.addCode'), validate: (v) => /^\d{3}$/.test(v) }); break;
      case 'strlist': c = tagInput({ placeholder: f.mono ? t('settings.addHost') : t('settings.addKeyword'), mono: !!f.mono }); break;
      case 'lines': c = h('textarea', { class: 'input mono textarea-lines', rows: 7, placeholder: '10.0.0.0/8\n172.16.0.0/12\n127.0.0.1/32', spellcheck: 'false' }); break;
      default: c = input({ class: `input ${f.mono ? 'mono' : ''}`, list: f.models ? 'dl-settings-models' : null, placeholder: f.models ? t('settings.cheapestModel') : '' });
    }
    controls[f.k] = c;
    let node = c;
    if (f.unit) node = h('div', { class: 'input-affix input-affix-end' }, c, h('span', { class: 'affix' }, f.unit));
    if (f.type === 'bool') {
      return h('div', { class: `field field-bool ${f.full ? 'field-full' : ''} ${f.warn ? 'field-bool-warn' : ''}` },
        h('div', { class: 'field-bool-text' }, h('div', { class: 'field-label' }, t(`settings.f.${f.k}`)),
          h('div', { class: 'field-hint' }, f.warn ? icon('alert', 12) : null, f.warn ? ' ' : null, t(`settings.h.${f.k}`))), c);
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
        case 'select': c.value = v ?? f.def ?? ''; break;
        default: c.value = v ?? '';
      }
    }
    refreshDirty();
  }

  const dlModels = h('datalist', { id: 'dl-settings-models' });
  const form = h('form', { class: 'settings-form', onsubmit: (e) => { e.preventDefault(); save(); }, oninput: scheduleDirty, onchange: scheduleDirty, onclick: scheduleDirty, onkeyup: scheduleDirty },
    SECTIONS.map((sec) => {
      const c = card({
        title: h('span', { class: 'card-title-ic' }, icon(sec.icon, 16), t(`settings.s.${sec.id}`)), subtitle: t(`settings.sd.${sec.id}`),
        body: h('div', { class: 'form-grid' }, sec.fields.map((f) => control(f))), cls: 'settings-anchor',
      });
      c.id = `sec-${sec.id}`;
      return c;
    }),
    dlModels, saveBar);
  const loadingEl = h('div', { class: 'card card-loading' }, spinner(20), h('span', { class: 'muted' }, t('common.loading')));
  formCol.append(loadingEl);

  function rangeError() {
    for (const sec of SECTIONS) for (const f of sec.fields) {
      if (f.type !== 'int') continue;
      const c = controls[f.k];
      const v = c.value === '' ? null : Number(c.value);
      const bad = v != null && (!Number.isInteger(v) || (f.min != null && v < f.min) || (f.max != null && v > f.max));
      c.classList.toggle('invalid', bad);
      if (bad) return { f, c };
    }
    return null;
  }
  async function save() {
    const d = diff();
    if (!Object.keys(d).length) return;
    const bad = rangeError();
    if (bad) {
      toast(t('err.settingsRange', { field: t(`settings.f.${bad.f.k}`), a: bad.f.min, b: bad.f.max }), 'warning');
      bad.c.scrollIntoView({ behavior: 'smooth', block: 'center' }); bad.c.focus({ preventScroll: true });
      return;
    }
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

  // ---- side: account (password) ----
  const pwCard = card({
    title: h('span', { class: 'card-title-ic' }, icon('lock', 16), t('settings.password')), subtitle: t('settings.passwordSub'),
    body: passwordForm({ onDone: (b) => { ctx.refreshInfo(); if (b.revoke_admin_tokens) loadAdminTokens(); } }), cls: 'settings-anchor',
  });
  pwCard.id = 'sec-account';
  sideCol.append(pwCard);

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
    } catch (e) { if (e.name !== 'AbortError') { toastError(e); atBody.replaceChildren(tableEmptyRow(4, { icon: 'alert', title: t('common.loadFailed'), desc: errorMessage(e) })); } }
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
      loadingEl.replaceChildren(icon('alert', 18), h('span', null, errorMessage(e)));
    }
  })();
  loadInfo();
  loadAdminTokens();
  const wanted = ctx.params.section === 'password' ? 'account' : ctx.params.section;
  if (wanted && NAV.includes(wanted)) {
    // Sections render once settings load; wait for the target to exist.
    const tryJump = (n) => { if (el.querySelector(`#sec-${wanted}`)) jump(wanted); else if (n > 0) setTimeout(() => tryJump(n - 1), 120); };
    setTimeout(() => tryJump(20), 60);
  } else markNav('routing');

  const beforeUnload = (e) => { if (Object.keys(diff()).length) { e.preventDefault(); e.returnValue = ''; } };
  window.addEventListener('beforeunload', beforeUnload);
  return () => { window.removeEventListener('beforeunload', beforeUnload); window.removeEventListener('scroll', onScroll); };
}
