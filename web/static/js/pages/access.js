// 管理访问 (#/access): admin password + admin API tokens (moved here from Settings in v0.4.0).
import { h, timeEl, cred } from '../util.js';
import { icon } from '../icons.js';
import { t } from '../i18n.js';
import { get, put, post, del } from '../api.js';
import {
  card, toast, toastError, openModal, confirmDialog, field, input, button, busy,
  tableWrap, tableEmptyRow, skeletonRows, passwordInput, checkbox, errorMessage,
} from '../ui.js';
import { showSecret } from './tokens.js';

/** Change-password form, shared by this page, the account menu and the "initial password" banner. */
export function passwordForm({ onDone, autofocus = false, inline = false } = {}) {
  const oldPw = passwordInput({ autocomplete: 'current-password', autofocus });
  const newPw = passwordInput({ autocomplete: 'new-password', minlength: 8 });
  const newPw2 = passwordInput({ autocomplete: 'new-password' });
  const revoke = checkbox({ checked: false, label: t('settings.revokeAdminTokens'), hint: t('settings.revokeAdminTokensHint') });
  const btn = h('button', { class: 'btn btn-primary', type: 'submit' }, h('span', null, t('settings.changePassword')));
  const fields = [
    field(t('settings.oldPassword'), oldPw),
    field(t('settings.newPassword'), newPw, inline ? {} : { hint: t('settings.minChars') }),
    field(t('settings.confirmPassword'), newPw2),
  ];
  if (inline) newPw.input.placeholder = t('settings.minChars');
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
  // ---- password ----
  const pwCard = card({
    title: h('span', { class: 'card-title-ic' }, icon('lock', 16), t('settings.password')), subtitle: t('settings.passwordSub'),
    body: passwordForm({ inline: true, onDone: (b) => { ctx.refreshInfo(); if (b.revoke_admin_tokens) loadAdminTokens(); } }),
  });

  // ---- admin API tokens ----
  const atBody = h('tbody', null, skeletonRows(4, 2));
  const atCard = card({
    title: h('span', { class: 'card-title-ic' }, icon('key', 16), t('settings.adminTokens')), subtitle: [t('access.tokensIntro'), ' ', h('a', { class: 'link', href: '#/docs?tab=admin' }, t('access.viewReference'))], flush: true,
    actions: button(t('common.create'), { icon: 'plus', size: 'sm', variant: 'primary', onClick: createAdminToken }),
    body: [tableWrap(h('table', { class: 'table table-compact' },
      h('thead', null, h('tr', null, h('th', null, t('common.name')), h('th', null, t('common.lastUsed')), h('th', null, t('common.created')), h('th', { class: 'col-actions' }, ''))),
      atBody))],
  });
  el.append(h('div', { class: 'page-narrow page-stack' }, pwCard, atCard));

  async function loadAdminTokens() {
    try {
      const res = await get('/api/v1/admin-tokens', null, { signal: ctx.signal });
      const rows = res.data || [];
      atBody.replaceChildren(...(rows.length ? rows.map((a) => h('tr', null,
        h('td', null, cred(a.name, a.prefix || 'xra-', 'strong')),
        h('td', null, timeEl(a.last_used_at)),
        h('td', null, timeEl(a.created_at)),
        h('td', { class: 'col-actions' }, h('button', { class: 'btn btn-sm btn-danger-soft', onclick: () => revoke(a) }, t('settings.revoke'))))) : [tableEmptyRow(4, { icon: 'key', title: t('settings.noAdminTokens'), desc: t('settings.noAdminTokensDesc') })]));
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
}
