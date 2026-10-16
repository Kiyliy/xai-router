import { h, fmtInt, fmtUSD, timeEl, debounce, fmtDateTime, toLocalInput, fromLocalInput } from '../util.js';
import { icon } from '../icons.js';
import { t } from '../i18n.js';
import { get, post, patch, del } from '../api.js';
import {
  card, toast, toastError, openModal, confirmDialog, menuButton, badge, switchEl, searchInput, pagination,
  skeletonRows, tableEmptyRow, tableWrap, field, input, checkbox, button, busy, progress, tagInput, codeBlock, copyButton, tabs,
  tapToMenu, spinner, errorMessage,
} from '../ui.js';

let modelCache = null;
async function modelIds() {
  if (modelCache) return modelCache;
  try {
    const res = await get('/api/v1/models');
    const ids = new Set();
    for (const m of res.data || []) { ids.add(m.id); (m.aliases || []).forEach((a) => ids.add(a)); }
    modelCache = [...ids].sort();
    setTimeout(() => { modelCache = null; }, 60000);
  } catch { modelCache = []; }
  return modelCache;
}

/** One-time secret display with usage snippets. */
export function showSecret({ title, secret, kind = 'client' }) {
  const base = `${location.origin}/v1`;
  const secretBox = h('div', { class: 'secret-box' }, h('code', { class: 'mono secret-value' }, secret), copyButton(secret, { small: false, cls: 'btn-primary', label: t('common.copy') }));
  let snippets = null;
  if (kind === 'client') {
    const curl = `curl ${base}/chat/completions \\\n  -H "Authorization: Bearer ${secret}" \\\n  -H "Content-Type: application/json" \\\n  -d '{"model": "grok-4.3", "messages": [{"role": "user", "content": "Hello"}]}'`;
    const py = `from openai import OpenAI\n\nclient = OpenAI(\n    base_url="${base}",\n    api_key="${secret}",\n)\n\nresp = client.chat.completions.create(\n    model="grok-4.3",\n    messages=[{"role": "user", "content": "Hello"}],\n)\nprint(resp.choices[0].message.content)`;
    const node = `import OpenAI from "openai";\n\nconst client = new OpenAI({\n  baseURL: "${base}",\n  apiKey: "${secret}",\n});\n\nconst resp = await client.chat.completions.create({\n  model: "grok-4.3",\n  messages: [{ role: "user", content: "Hello" }],\n});\nconsole.log(resp.choices[0].message.content);`;
    snippets = tabs([
      { id: 'py', label: 'Python', render: () => codeBlock(py) },
      { id: 'node', label: 'Node.js', render: () => codeBlock(node) },
      { id: 'curl', label: 'curl', render: () => codeBlock(curl) },
    ], { cls: 'tabs-sm' });
  } else {
    snippets = codeBlock(`curl ${location.origin}/api/v1/keys \\\n  -H "Authorization: Bearer ${secret}"`);
  }
  const m = openModal({
    title, size: 'lg', dismissible: false,
    body: h('div', { class: 'secret-modal' },
      h('div', { class: 'callout callout-warning' }, icon('alert', 16), h('span', null, t('tokens.secretOnce'))),
      secretBox,
      h('h4', { class: 'section-title' }, t('tokens.howToUse')),
      kind === 'client' ? h('p', { class: 'muted small' }, t('tokens.baseUrlIs'), ' ', h('code', { class: 'mono inline-code' }, base)) : null,
      snippets),
    footer: [h('button', { class: 'btn btn-primary', onclick: () => m.close() }, t('tokens.savedIt'))],
  });
}

export function render(el, ctx) {
  const st = { q: ctx.params.q || '', page: Number(ctx.params.page) || 1, pageSize: Number(localStorage.getItem('xr.tokens.ps')) || 20, total: 0 };
  const syncParams = () => ctx.setParams({ q: st.q, page: st.page > 1 ? st.page : '' });
  ctx.setActions(button(t('tokens.create'), { icon: 'plus', variant: 'primary', onClick: () => openForm() }));

  const search = searchInput({ value: st.q, placeholder: t('tokens.searchPh'), onInput: debounce((v) => { st.q = v.trim(); st.page = 1; syncParams(); load(); }, 300) });
  const countEl = h('span', { class: 'toolbar-count' });
  const COLS = 7;
  const tbody = h('tbody', null, skeletonRows(COLS));
  const table = h('table', { class: 'table table-tokens table-cards' },
    h('thead', null, h('tr', null,
      h('th', { class: 'col-switch' }, t('common.enabled')), h('th', null, t('common.name')),
      h('th', { class: 'col-quota' }, t('tokens.quota')), h('th', null, t('tokens.expiry')), h('th', null, t('tokens.models')),
      h('th', { class: 'num' }, t('keys.usage')), h('th', { class: 'col-actions' }, h('span', { class: 'sr-only' }, t('common.actions'))))),
    tbody);
  let knownNames = [];
  const pager = h('div');
  el.append(card({ flush: true, body: [h('div', { class: 'toolbar' }, search, h('div', { class: 'toolbar-spacer' }), countEl), tableWrap(table), pager] }));

  function quotaCell(tk) {
    const unlimited = !tk.quota_usd;
    return h('td', { class: 'col-quota m-full' },
      h('div', { class: 'quota-line' },
        h('span', { class: 'mono' }, fmtUSD(tk.used_usd)),
        h('span', { class: 'muted' }, ' / '),
        h('span', { class: 'mono muted' }, unlimited ? t('tokens.unlimited') : fmtUSD(tk.quota_usd))),
      unlimited ? null : progress(tk.used_usd / tk.quota_usd),
      unlimited ? null : h('div', { class: 'cell-sub' }, t('tokens.remaining', { v: fmtUSD(tk.remaining_usd ?? Math.max(0, tk.quota_usd - tk.used_usd)) })));
  }

  function modelsCell(list) {
    if (!list || !list.length) return h('td', { class: 'm-meta' }, h('span', { class: 'muted small nowrap' }, t('tokens.allModels')));
    const shown = list.slice(0, 3);
    return h('td', { class: 'm-full' }, h('div', { class: 'chips chips-tight' },
      shown.map((m) => h('span', { class: 'chip chip-sm mono' }, m)),
      list.length > 3 ? h('span', { class: 'chip chip-sm', 'data-tip': list.slice(3).join(', ') }, `+${list.length - 3}`) : null));
  }

  function statusBadge(tk) {
    if (tk.expired) return badge(t('tokens.expired'), 'danger', { class: 'badge-dot' });
    if (!tk.enabled) return badge(t('common.disabled'), 'neutral', { class: 'badge-dot' });
    if (tk.quota_usd && (tk.remaining_usd ?? tk.quota_usd - tk.used_usd) <= 0) return badge(t('tokens.exhausted'), 'warning', { class: 'badge-dot' });
    return badge(t('status.active'), 'success', { class: 'badge-dot' });
  }

  function row(tk) {
    const expiring = tk.expires_at && !tk.expired && new Date(tk.expires_at).getTime() - Date.now() < 7 * 86400e3;
    const dup = knownNames.filter((n) => n === tk.name).length > 1;
    const menu = menuButton([
      { label: t('common.edit'), icon: 'edit', onClick: () => openForm(tk) },
      { label: t('tokens.regenerate'), icon: 'refresh', onClick: () => regenerate(tk) },
      { label: t('tokens.viewLogs'), icon: 'logs', onClick: () => ctx.navigate(`#/logs?token_id=${tk.id}`) },
      '-',
      { label: t('common.delete'), icon: 'trash', danger: true, onClick: () => remove(tk) },
    ]);
    const tr = h('tr', { class: !tk.enabled || tk.expired ? 'row-muted' : '' },
      h('td', { class: 'col-switch m-meta m-switch' }, switchEl({ checked: tk.enabled, onChange: async (v) => { try { await patch(`/api/v1/tokens/${tk.id}`, { enabled: v }); toast(v ? t('tokens.enabledToast', { name: tk.name }) : t('tokens.disabledToast', { name: tk.name }), 'success', 2000); load(); } catch (e) { toastError(e); return false; } } })),
      h('td', { class: 'cell-tokname m-title' },
        h('div', { class: 'title-line' },
          h('span', { class: 'strong name-text', title: tk.name || '' }, tk.name || `#${tk.id}`),
          dup ? h('span', { class: 'dup-flag', title: t('tokens.dupInList') }, icon('alert', 13)) : null,
          h('span', { class: 'm-only' }, statusBadge(tk))),
        h('div', { class: 'cell-sub mono key-mask' }, `${tk.prefix || ''}…`),
        tk.notes ? h('div', { class: 'cell-sub truncate', title: tk.notes }, tk.notes) : null),
      quotaCell(tk),
      h('td', { class: 'nowrap m-meta' }, !tk.expires_at ? h('span', { class: 'muted small' }, t('tokens.never'))
        : tk.expired ? h('div', null, badge(t('tokens.expired'), 'danger'), h('div', { class: 'cell-sub', title: fmtDateTime(tk.expires_at) }, fmtDateTime(tk.expires_at, { seconds: false })))
          : h('div', null, h('span', { title: fmtDateTime(tk.expires_at), class: expiring ? 'text-warning' : '' }, fmtDateTime(tk.expires_at, { seconds: false })), h('div', { class: 'cell-sub' }, timeEl(tk.expires_at)))),
      modelsCell(tk.allowed_models),
      h('td', { class: 'num m-meta' }, h('div', { class: 'mono nowrap' }, fmtInt(tk.requests), h('span', { class: 'muted' }, ` ${t('keys.reqShort')}`)), h('div', { class: 'cell-sub nowrap' }, timeEl(tk.last_used_at))),
      h('td', { class: 'col-actions m-menu' }, h('div', { class: 'row-actions' },
        h('button', { class: 'btn btn-ghost btn-icon btn-sm hide-m', title: t('tokens.viewLogs'), 'aria-label': t('tokens.viewLogs'), onclick: () => ctx.navigate(`#/logs?token_id=${tk.id}`) }, icon('logs', 15)),
        menu)));
    return tapToMenu(tr, menu);
  }

  let loadSeq = 0;
  async function load() {
    const seq = ++loadSeq;
    try {
      const res = await get('/api/v1/tokens', { q: st.q, page: st.page, page_size: st.pageSize }, { signal: ctx.signal });
      if (seq !== loadSeq) return;
      const rows = res.data || []; st.total = res.total || 0;
      knownNames = rows.map((x) => x.name);
      countEl.textContent = t('common.totalN', { n: fmtInt(st.total) });
      tbody.replaceChildren(...(rows.length ? rows.map(row) : [tableEmptyRow(COLS, st.q ? { icon: 'search', title: t('common.noMatches') } : { icon: 'ticket', title: t('tokens.empty'), desc: t('tokens.emptyDesc'), action: button(t('tokens.create'), { icon: 'plus', variant: 'primary', onClick: () => openForm() }) })]));
      pager.replaceChildren(pagination({ page: st.page, pageSize: st.pageSize, total: st.total, onChange: (p, ps) => { st.page = p; st.pageSize = ps; try { localStorage.setItem('xr.tokens.ps', ps); } catch { /* */ } syncParams(); load(); } }));
    } catch (e) {
      if (e.name === 'AbortError') return;
      toastError(e);
      tbody.replaceChildren(tableEmptyRow(COLS, { icon: 'alert', title: t('common.loadFailed'), desc: errorMessage(e) }));
    }
  }

  async function regenerate(tk) {
    const ok = await confirmDialog({ title: t('tokens.regenTitle'), message: t('tokens.regenMsg', { name: tk.name }), confirmText: t('tokens.regenerate'), danger: true });
    if (!ok) return;
    try { const r = await post(`/api/v1/tokens/${tk.id}/regenerate`, {}); showSecret({ title: t('tokens.newSecret', { name: tk.name }), secret: r.token }); load(); } catch (e) { toastError(e); }
  }

  async function remove(tk) {
    const ok = await confirmDialog({ title: t('tokens.deleteTitle'), message: t('tokens.deleteMsg', { name: tk.name }), confirmText: t('common.delete'), danger: true });
    if (!ok) return;
    try { await del(`/api/v1/tokens/${tk.id}`); toast(t('common.deleted'), 'success'); load(); } catch (e) { toastError(e); }
  }

  function openForm(tk) {
    const edit = !!tk;
    const name = input({ value: tk?.name || '', placeholder: 'my-app', maxlength: 100 });
    const dupWarn = h('div', { class: 'field-warn', hidden: true }, icon('alert', 13), h('span'));
    let allNames = knownNames.slice();
    const checkDup = () => {
      const v = name.value.trim();
      const clash = v && allNames.some((n) => n === v) && !(edit && v === tk.name);
      dupWarn.hidden = !clash;
      if (clash) dupWarn.lastChild.textContent = t('tokens.dupName', { name: v });
      return clash;
    };
    name.addEventListener('input', checkDup);
    const quota = input({ type: 'number', value: tk?.quota_usd ?? 0, min: 0, step: 'any' });
    const never = checkbox({ checked: !tk?.expires_at, label: t('tokens.neverExpires') });
    const expires = input({ type: 'datetime-local', value: toLocalInput(tk?.expires_at), disabled: !tk?.expires_at });
    never.input.addEventListener('change', () => {
      expires.disabled = never.input.checked;
      if (!never.input.checked && !expires.value) expires.value = toLocalInput(new Date(Date.now() + 30 * 86400e3).toISOString());
    });
    const presets = h('div', { class: 'preset-row' }, [7, 30, 90, 365].map((d) => h('button', { type: 'button', class: 'chip chip-btn', onclick: () => { never.input.checked = false; expires.disabled = false; expires.value = toLocalInput(new Date(Date.now() + d * 86400e3).toISOString()); } }, t('tokens.inDays', { n: d }))));
    const models = tagInput({ values: tk?.allowed_models || [], suggestions: modelCache || [], placeholder: t('tokens.modelsPh') });
    const modelsHint = h('div', { class: 'field-hint' }, modelCache ? t('tokens.modelsHint') : [spinner(11), ' ', t('tokens.loadingModels')]);
    const notes = h('textarea', { class: 'input', rows: 2, placeholder: t('common.notesPh') }, tk?.notes || '');
    const enabled = switchEl({ checked: tk ? tk.enabled : true, label: t('common.enabled') });
    const resetUsage = edit ? checkbox({ checked: false, label: t('tokens.resetUsage'), hint: t('tokens.resetUsageHint', { v: fmtUSD(tk.used_usd) }) }) : null;
    const body = h('form', { class: 'form-grid', onsubmit: (e) => { e.preventDefault(); save(); } },
      h('div', { class: 'field' }, h('label', { class: 'field-label' }, t('common.name'), h('span', { class: 'req' }, '*')), name, dupWarn),
      field(t('tokens.quotaUsd'), h('div', { class: 'input-affix' }, h('span', { class: 'affix' }, '$'), quota), { hint: t('tokens.quotaHint') }),
      field(t('tokens.expiry'), h('div', { class: 'stack-sm' }, h('div', { class: 'row-sm' }, expires, never), presets), { full: true }),
      h('div', { class: 'field field-full' }, h('label', { class: 'field-label' }, t('tokens.allowedModels')), models, modelsHint),
      field(t('common.notes'), notes, { full: true }),
      h('div', { class: 'field field-full checks' }, enabled, resetUsage),
      h('button', { type: 'submit', hidden: true }));
    const saveBtn = h('button', { class: 'btn btn-primary', onclick: () => save() }, h('span', null, edit ? t('common.save') : t('tokens.create')));
    const m = openModal({ title: edit ? t('tokens.editTitle', { name: tk.name }) : t('tokens.createTitle'), size: 'md', body, footer: [h('button', { class: 'btn', onclick: () => m.close() }, t('common.cancel')), saveBtn] });
    // Fill model suggestions and the full name list in the background; the dialog is already usable.
    if (!modelCache) modelIds().then((ids) => { models.setSuggestions(ids); modelsHint.replaceChildren(t('tokens.modelsHint')); });
    get('/api/v1/tokens', { page_size: 1000 }).then((r) => { allNames = (r.data || []).map((x) => x.name); checkDup(); }).catch(() => {});
    async function save() {
      if (!name.value.trim()) { name.focus(); name.classList.add('invalid'); return; }
      if (checkDup() && !(await confirmDialog({ title: t('tokens.dupTitle'), message: t('tokens.dupConfirm', { name: name.value.trim() }), confirmText: t('tokens.dupContinue') }))) { name.focus(); return; }
      const q = Number(quota.value || 0);
      if (!(q >= 0)) { quota.focus(); return; }
      const payload = {
        name: name.value.trim(), quota_usd: q, enabled: enabled.input.checked, notes: notes.value,
        allowed_models: models.getValues(),
        expires_at: never.input.checked ? null : fromLocalInput(expires.value),
      };
      let body = payload;
      if (edit) {
        body = {};
        for (const [k, v] of Object.entries(payload)) {
          const old = tk[k];
          if (k === 'allowed_models') { if (JSON.stringify(v) !== JSON.stringify(old || [])) body[k] = v; }
          else if (k === 'expires_at') { if ((v ? new Date(v).getTime() : null) !== (old ? Math.floor(new Date(old).getTime() / 60000) * 60000 : null)) body[k] = v; }
          else if (v !== old) body[k] = v;
        }
        if (resetUsage.input.checked) body.reset_usage = true;
        if (!Object.keys(body).length) { m.close(); return; }
      }
      await busy(saveBtn, async () => {
        try {
          if (edit) { await patch(`/api/v1/tokens/${tk.id}`, body); toast(t('common.saved'), 'success'); m.close(); }
          else { const r = await post('/api/v1/tokens', body); m.close(); showSecret({ title: t('tokens.created', { name: r.name }), secret: r.token }); }
          load();
        } catch (e) { toastError(e); }
      });
    }
  }

  load();
}
