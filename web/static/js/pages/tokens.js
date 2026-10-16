import { h, fmtInt, fmtUSD, timeEl, debounce, fmtDateTime, toLocalInput, fromLocalInput } from '../util.js';
import { icon } from '../icons.js';
import { t } from '../i18n.js';
import { get, post, patch, del } from '../api.js';
import {
  card, toast, toastError, openModal, confirmDialog, menuButton, badge, switchEl, searchInput, pagination,
  skeletonRows, tableEmptyRow, tableWrap, field, input, checkbox, button, busy, progress, tagInput, codeBlock, copyButton, tabs,
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
  const COLS = 9;
  const tbody = h('tbody', null, skeletonRows(COLS));
  const table = h('table', { class: 'table' },
    h('thead', null, h('tr', null,
      h('th', null, t('common.name')), h('th', null, t('tokens.prefix')), h('th', { class: 'col-switch' }, t('common.enabled')),
      h('th', { class: 'col-quota' }, t('tokens.quota')), h('th', null, t('tokens.expiry')), h('th', null, t('tokens.models')),
      h('th', { class: 'num' }, t('ov.requests')), h('th', null, t('common.lastUsed')), h('th', { class: 'col-actions' }, ''))),
    tbody);
  const pager = h('div');
  el.append(card({ flush: true, body: [h('div', { class: 'toolbar' }, search, h('div', { class: 'toolbar-spacer' }), countEl), tableWrap(table), pager] }));

  function quotaCell(tk) {
    const unlimited = !tk.quota_usd;
    return h('td', { class: 'col-quota' },
      h('div', { class: 'quota-line' },
        h('span', { class: 'mono' }, fmtUSD(tk.used_usd)),
        h('span', { class: 'muted' }, ' / '),
        h('span', { class: 'mono muted' }, unlimited ? t('tokens.unlimited') : fmtUSD(tk.quota_usd))),
      unlimited ? null : progress(tk.used_usd / tk.quota_usd),
      unlimited ? null : h('div', { class: 'cell-sub' }, t('tokens.remaining', { v: fmtUSD(tk.remaining_usd ?? Math.max(0, tk.quota_usd - tk.used_usd)) })));
  }

  function modelsCell(list) {
    if (!list || !list.length) return h('td', null, h('span', { class: 'muted small' }, t('tokens.allModels')));
    const shown = list.slice(0, 3);
    return h('td', null, h('div', { class: 'chips chips-tight' },
      shown.map((m) => h('span', { class: 'chip chip-sm mono' }, m)),
      list.length > 3 ? h('span', { class: 'chip chip-sm', 'data-tip': list.slice(3).join(', ') }, `+${list.length - 3}`) : null));
  }

  function row(tk) {
    const expiring = tk.expires_at && !tk.expired && new Date(tk.expires_at).getTime() - Date.now() < 7 * 86400e3;
    return h('tr', { class: !tk.enabled || tk.expired ? 'row-muted' : '' },
      h('td', null, h('div', { class: 'strong' }, tk.name || `#${tk.id}`), tk.notes ? h('div', { class: 'cell-sub truncate', title: tk.notes }, tk.notes) : null),
      h('td', null, h('span', { class: 'mono key-mask' }, `${tk.prefix || ''}…`)),
      h('td', { class: 'col-switch' }, switchEl({ checked: tk.enabled, onChange: async (v) => { try { await patch(`/api/v1/tokens/${tk.id}`, { enabled: v }); load(); } catch (e) { toastError(e); return false; } } })),
      quotaCell(tk),
      h('td', { class: 'nowrap' }, !tk.expires_at ? h('span', { class: 'muted small' }, t('tokens.never'))
        : tk.expired ? h('div', null, badge(t('tokens.expired'), 'danger'), h('div', { class: 'cell-sub', title: fmtDateTime(tk.expires_at) }, fmtDateTime(tk.expires_at, { seconds: false })))
          : h('div', null, h('span', { title: fmtDateTime(tk.expires_at), class: expiring ? 'text-warning' : '' }, fmtDateTime(tk.expires_at, { seconds: false })), h('div', { class: 'cell-sub' }, timeEl(tk.expires_at)))),
      modelsCell(tk.allowed_models),
      h('td', { class: 'num mono' }, fmtInt(tk.requests)),
      h('td', null, timeEl(tk.last_used_at)),
      h('td', { class: 'col-actions' }, h('div', { class: 'row-actions' },
        h('button', { class: 'btn btn-ghost btn-icon btn-sm', title: t('tokens.viewLogs'), 'aria-label': t('tokens.viewLogs'), onclick: () => ctx.navigate(`#/logs?token_id=${tk.id}`) }, icon('logs', 15)),
        menuButton([
          { label: t('common.edit'), icon: 'edit', onClick: () => openForm(tk) },
          { label: t('tokens.regenerate'), icon: 'refresh', onClick: () => regenerate(tk) },
          { label: t('tokens.viewLogs'), icon: 'logs', onClick: () => ctx.navigate(`#/logs?token_id=${tk.id}`) },
          '-',
          { label: t('common.delete'), icon: 'trash', danger: true, onClick: () => remove(tk) },
        ]))));
  }

  let loadSeq = 0;
  async function load() {
    const seq = ++loadSeq;
    try {
      const res = await get('/api/v1/tokens', { q: st.q, page: st.page, page_size: st.pageSize }, { signal: ctx.signal });
      if (seq !== loadSeq) return;
      const rows = res.data || []; st.total = res.total || 0;
      countEl.textContent = t('common.totalN', { n: fmtInt(st.total) });
      tbody.replaceChildren(...(rows.length ? rows.map(row) : [tableEmptyRow(COLS, st.q ? { icon: 'search', title: t('common.noMatches') } : { icon: 'ticket', title: t('tokens.empty'), desc: t('tokens.emptyDesc'), action: button(t('tokens.create'), { icon: 'plus', variant: 'primary', onClick: () => openForm() }) })]));
      pager.replaceChildren(pagination({ page: st.page, pageSize: st.pageSize, total: st.total, onChange: (p, ps) => { st.page = p; st.pageSize = ps; try { localStorage.setItem('xr.tokens.ps', ps); } catch { /* */ } syncParams(); load(); } }));
    } catch (e) {
      if (e.name === 'AbortError') return;
      toastError(e);
      tbody.replaceChildren(tableEmptyRow(COLS, { icon: 'alert', title: t('common.loadFailed'), desc: e.message }));
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

  async function openForm(tk) {
    const edit = !!tk;
    const suggestions = await modelIds();
    const name = input({ value: tk?.name || '', placeholder: 'my-app' });
    const quota = input({ type: 'number', value: tk?.quota_usd ?? 0, min: 0, step: 'any' });
    const never = checkbox({ checked: !tk?.expires_at, label: t('tokens.neverExpires') });
    const expires = input({ type: 'datetime-local', value: toLocalInput(tk?.expires_at), disabled: !tk?.expires_at });
    never.input.addEventListener('change', () => {
      expires.disabled = never.input.checked;
      if (!never.input.checked && !expires.value) expires.value = toLocalInput(new Date(Date.now() + 30 * 86400e3).toISOString());
    });
    const presets = h('div', { class: 'preset-row' }, [7, 30, 90, 365].map((d) => h('button', { type: 'button', class: 'chip chip-btn', onclick: () => { never.input.checked = false; expires.disabled = false; expires.value = toLocalInput(new Date(Date.now() + d * 86400e3).toISOString()); } }, t('tokens.inDays', { n: d }))));
    const models = tagInput({ values: tk?.allowed_models || [], suggestions, placeholder: t('tokens.modelsPh') });
    const notes = h('textarea', { class: 'input', rows: 2, placeholder: t('common.notesPh') }, tk?.notes || '');
    const enabled = switchEl({ checked: tk ? tk.enabled : true, label: t('common.enabled') });
    const resetUsage = edit ? checkbox({ checked: false, label: t('tokens.resetUsage'), hint: t('tokens.resetUsageHint', { v: fmtUSD(tk.used_usd) }) }) : null;
    const body = h('form', { class: 'form-grid', onsubmit: (e) => { e.preventDefault(); save(); } },
      field(t('common.name'), name, { required: true }),
      field(t('tokens.quotaUsd'), h('div', { class: 'input-affix' }, h('span', { class: 'affix' }, '$'), quota), { hint: t('tokens.quotaHint') }),
      field(t('tokens.expiry'), h('div', { class: 'stack-sm' }, h('div', { class: 'row-sm' }, expires, never), presets), { full: true }),
      field(t('tokens.allowedModels'), models, { full: true, hint: t('tokens.modelsHint') }),
      field(t('common.notes'), notes, { full: true }),
      h('div', { class: 'field field-full checks' }, enabled, resetUsage),
      h('button', { type: 'submit', hidden: true }));
    const saveBtn = h('button', { class: 'btn btn-primary', onclick: () => save() }, h('span', null, edit ? t('common.save') : t('tokens.create')));
    const m = openModal({ title: edit ? t('tokens.editTitle', { name: tk.name }) : t('tokens.createTitle'), size: 'md', body, footer: [h('button', { class: 'btn', onclick: () => m.close() }, t('common.cancel')), saveBtn] });
    async function save() {
      if (!name.value.trim()) { name.focus(); name.classList.add('invalid'); return; }
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
