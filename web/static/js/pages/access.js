// 管理访问 (#/access): admin password + admin API tokens (moved here from Settings in v0.4.0).
import { h, timeEl, fmtDate, fmtDateTime, fmtInt } from '../util.js';
import { t } from '../i18n.js';
import { get, put, post, del } from '../api.js';
import {
  toast, toastError, openModal, confirmDialog, field, input, button, busy,
  tableEmptyRow, skeletonRows, passwordInput, checkbox, errorMessage,
} from '../ui.js';
import { showSecret } from './tokens.js';

/** Change-password form, shared by this page, the account menu and the "initial password" banner. */
export function passwordForm({ onDone, autofocus = false, inline = false, placeholders = false } = {}) {
  const oldPw = passwordInput({ autocomplete: 'current-password', autofocus });
  const newPw = passwordInput({ autocomplete: 'new-password', minlength: 8 });
  const newPw2 = passwordInput({ autocomplete: 'new-password' });
  const revoke = checkbox({ checked: false, label: t('settings.revokeAdminTokens'), hint: placeholders ? null : t('settings.revokeAdminTokensHint') });
  if (placeholders) revoke.title = t('access.revokeTip');
  const btn = h('button', { class: 'btn btn-primary', type: 'submit' }, h('span', null, t('settings.changePassword')));
  const fields = [
    field(t('settings.oldPassword'), oldPw),
    field(t('settings.newPassword'), newPw, inline || placeholders ? {} : { hint: t('settings.minChars') }),
    field(t('settings.confirmPassword'), newPw2),
  ];
  if (inline || placeholders) newPw.input.placeholder = t('access.minCharsPh');
  if (placeholders) newPw2.input.placeholder = t('access.againPh');
  const form = h('form', { class: 'stack', onsubmit: (e) => { e.preventDefault(); submit(); } },
    inline ? h('div', { class: 'inline-form' }, fields, h('div', { class: 'inline-form-btn' }, btn)) : fields,
    revoke,
    inline ? null : h('div', null, btn));
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

export function render(el, ctx) {
  el.classList.add('pg-access');
  const section = (title, desc, body) => h('section', { class: 'access-sec' },
    h('div', { class: 'access-sec-head' }, h('h2', null, title), ...[].concat(desc).filter(Boolean).map((d) => h('p', null, d))),
    h('div', { class: 'access-sec-body' }, body));

  // ---- password ----
  const lastLogin = h('p', { class: 'access-last', hidden: true });
  const pwSec = section(t('settings.password'), [t('settings.passwordSub')],
    h('div', { class: 'access-pw' }, passwordForm({ placeholders: true, onDone: (b) => { ctx.refreshInfo(); if (b.revoke_admin_tokens) loadAdminTokens(); } })));
  pwSec.querySelector('.access-sec-head').append(lastLogin);

  // ---- admin API tokens ----
  const atBody = h('tbody', null, skeletonRows(4, 2));
  const atCount = h('span', { class: 'access-count' });
  const atSec = section(t('settings.adminTokens'), [t('access.tokensIntro')], [
    h('div', { class: 'access-tokens-head' }, atCount, button(t('access.createToken'), { icon: 'plus', size: 'sm', onClick: createAdminToken })),
    h('table', { class: 'table access-table' },
      h('thead', null, h('tr', null, h('th', null, t('common.name')), h('th', { class: 'col-date' }, t('common.created')), h('th', { class: 'col-date' }, t('common.lastUsed')), h('th', { class: 'col-revoke' }, h('span', { class: 'sr-only' }, t('common.actions'))))),
      atBody),
    h('a', { class: 'access-ref', href: '#/docs?tab=admin' }, t('access.viewReference')),
  ]);
  el.append(h('div', { class: 'access-wrap' }, pwSec, atSec));

  async function loadLastLogin() {
    try {
      const r = await get('/api/v1/logs', { type: 'audit', action: 'auth.login', page_size: 1 }, { signal: ctx.signal });
      const e = (r.data || [])[0];
      if (!e) return;
      lastLogin.textContent = e.ip ? t('access.lastLoginIp', { at: fmtDateTime(e.created_at, { seconds: false }), ip: e.ip }) : t('access.lastLogin', { at: fmtDateTime(e.created_at, { seconds: false }) });
      lastLogin.hidden = false;
    } catch { /* optional line */ }
  }

  async function loadAdminTokens() {
    try {
      const res = await get('/api/v1/admin-tokens', null, { signal: ctx.signal });
      const rows = res.data || [];
      atCount.textContent = t('access.tokensN', { n: fmtInt(rows.length) });
      atBody.replaceChildren(...(rows.length ? rows.map((a) => h('tr', null,
        h('td', null, h('span', { class: 'access-name' }, a.name), h('span', { class: 'mono access-prefix' }, a.prefix || 'xra-')),
        h('td', { class: 'col-date', title: fmtDateTime(a.created_at) }, fmtDate(a.created_at)),
        h('td', { class: 'col-date' }, a.last_used_at ? timeEl(a.last_used_at) : h('span', { class: 'muted' }, t('access.neverUsed'))),
        h('td', { class: 'col-revoke' }, h('button', { class: 'btn-link-danger', type: 'button', onclick: () => revoke(a) }, t('settings.revoke'))))) : [tableEmptyRow(4, { icon: 'key', title: t('settings.noAdminTokens'), desc: t('settings.noAdminTokensDesc') })]));
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
  loadAdminTokens();
  loadLastLogin();
}
