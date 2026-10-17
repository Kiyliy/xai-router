import { h, fmtInt, fmtUSD, timeEl, debounce, fmtDateTime, toLocalInput, fromLocalInput, fmtTotal, latestSignal } from '../util.js';
import { icon } from '../icons.js';
import { t } from '../i18n.js';
import { get, post, patch, del } from '../api.js';
import {
  card, toast, toastError, openModal, confirmDialog, menuButton, badge, switchEl, searchInput, pager as pagerEl, setTableLoading,
  skeletonRows, tableEmptyRow, tableWrap, field, input, checkbox, button, busy, progress, tagInput, codeBlock, copyButton, tabs,
  tapToMenu, spinner, errorMessage, segmented, kv,
} from '../ui.js';
import { drawerState, section, usageBlock, failuresBlock, allLogsLink, goneBody } from '../detail-drawer.js';

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

/**
 * Client usage snippets (Python / Node / curl, plus the Anthropic SDK when `anthropic`), shared by
 * the one-time secret dialog and the token drawer's integration tab.
 */
export function clientSnippets(secret, { anthropic = false } = {}) {
  const origin = location.origin;
  const base = `${origin}/v1`;
  const curl = `curl ${base}/chat/completions \\\n  -H "Authorization: Bearer ${secret}" \\\n  -H "Content-Type: application/json" \\\n  -d '{"model": "grok-4.3", "messages": [{"role": "user", "content": "Hello"}]}'`;
    const py = `from openai import OpenAI\n\nclient = OpenAI(\n    base_url="${base}",\n    api_key="${secret}",\n)\n\nresp = client.chat.completions.create(\n    model="grok-4.3",\n    messages=[{"role": "user", "content": "Hello"}],\n)\nprint(resp.choices[0].message.content)`;
    const node = `import OpenAI from "openai";\n\nconst client = new OpenAI({\n  baseURL: "${base}",\n  apiKey: "${secret}",\n});\n\nconst resp = await client.chat.completions.create({\n  model: "grok-4.3",\n  messages: [{ role: "user", content: "Hello" }],\n});\nconsole.log(resp.choices[0].message.content);`;
  const anth = `import anthropic\n\n# The Anthropic SDK appends /v1/messages itself, so use the bare origin.\nclient = anthropic.Anthropic(\n    base_url="${origin}",\n    api_key="${secret}",\n)\n\nmsg = client.messages.create(\n    model="grok-4.3",\n    max_tokens=1024,\n    messages=[{"role": "user", "content": "Hello"}],\n)\nprint(msg.content[0].text)`;
  return tabs([
    { id: 'py', label: 'Python', render: () => codeBlock(py) },
    { id: 'node', label: 'Node.js', render: () => codeBlock(node) },
    { id: 'curl', label: 'curl', render: () => codeBlock(curl) },
    anthropic ? { id: 'anth', label: 'Anthropic', render: () => codeBlock(anth) } : null,
  ].filter(Boolean), { cls: 'tabs-sm' });
}

/** One-time secret display with usage snippets. */
export function showSecret({ title, secret, kind = 'client' }) {
  const base = `${location.origin}/v1`;
  const secretBox = h('div', { class: 'secret-box' }, h('code', { class: 'mono secret-value' }, secret), copyButton(secret, { small: false, cls: 'btn-primary', label: t('common.copy') }));
  let snippets = null;
  if (kind === 'client') {
    snippets = clientSnippets(secret);
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

const PAGE_SIZES = [10, 20, 50, 100, 200];
const DEF_PS = 20;
const STATUSES = ['', 'enabled', 'disabled', 'expired', 'exhausted'];
function stateFromParams(p) {
  let saved = 0;
  if (!Object.keys(p).length) try { saved = Number(localStorage.getItem('xr.tokens.ps')); } catch { /* ignore */ }
  const ps = Number(p.page_size) || saved || DEF_PS;
  return {
    q: p.q || '', status: STATUSES.includes(p.status) ? p.status : '',
    page: Math.max(1, Math.floor(Number(p.page)) || 1), pageSize: PAGE_SIZES.includes(ps) ? ps : DEF_PS,
  };
}

function isExhausted(tk) { return !!tk.quota_usd && (tk.remaining_usd ?? tk.quota_usd - tk.used_usd) <= 0; }

function statusBadge(tk) {
  if (tk.expired) return badge(t('tokens.expired'), 'danger', { class: 'badge-dot' });
  if (!tk.enabled) return badge(t('common.disabled'), 'neutral', { class: 'badge-dot' });
  if (isExhausted(tk)) return badge(t('tokens.exhausted'), 'warning', { class: 'badge-dot' });
  return badge(t('status.active'), 'success', { class: 'badge-dot' });
}

export function render(el, ctx) {
  const st = { ...stateFromParams(ctx.params), total: 0, totalKind: 'exact', rows: [], loaded: false };
  const sel = new Set();
  let bulkRunning = false;
  const listParams = () => ({ q: st.q, status: st.status, page: st.page > 1 ? st.page : '', page_size: st.pageSize !== DEF_PS ? st.pageSize : '' });
  const syncParams = (push = false) => ctx.setParams({ ...listParams(), open: drawer.id || '' }, { push });
  // Token detail drawer, URL #/tokens?open={id}.
  const drawer = drawerState({ ctx, getParams: listParams, build: buildDrawer });
  ctx.setActions(
    button(t('tl.guide'), { icon: 'book', onClick: () => ctx.navigate('#/docs?tab=client') }),
    button(t('tokens.create'), { icon: 'plus', variant: 'primary', onClick: () => openForm() }));

  const search = searchInput({ value: st.q, placeholder: t('tokens.searchPh'), onInput: debounce((v) => { st.q = v.trim(); st.page = 1; syncParams(); load(); }, 300) });
  const statusSeg = segmented({
    size: 'seg-sm', value: st.status,
    options: STATUSES.map((v) => ({ value: v, label: t(`tl.status.${v || 'all'}`) })),
    onChange: (v) => { st.status = v; st.page = 1; sel.clear(); syncParams(true); load(); },
  });
  statusSeg.classList.add('status-filter');
  statusSeg.setAttribute('aria-label', t('tl.statusFilter'));
  const countEl = h('span', { class: 'toolbar-count' });
  const COLS = 8;
  const allBox = h('input', { type: 'checkbox', class: 'row-check', 'aria-label': t('common.selectAll'), title: t('common.selectAll'), onchange: () => { if (allBox.checked) st.rows.forEach((x) => sel.add(x.id)); else sel.clear(); syncSelection(); } });
  const tbody = h('tbody', null, skeletonRows(COLS));
  const table = h('table', { class: 'table table-tokens table-cards' },
    h('thead', null, h('tr', null,
      h('th', { class: 'col-check' }, allBox),
      h('th', { class: 'col-switch' }, t('common.enabled')), h('th', null, t('common.name')),
      h('th', { class: 'col-quota' }, t('tl.quotaCol')), h('th', null, t('tokens.expiry')), h('th', null, t('tokens.models')),
      h('th', { class: 'num' }, t('keys.usage')), h('th', { class: 'col-actions' }, h('span', { class: 'sr-only' }, t('common.actions'))))),
    tbody);
  let knownNames = [];
  const pager = pagerEl({
    sizes: PAGE_SIZES,
    onChange: (p, ps) => {
      if (ps !== st.pageSize) try { localStorage.setItem('xr.tokens.ps', ps); } catch { /* */ }
      st.page = p; st.pageSize = ps; sel.clear(); syncParams(true); load();
    },
  });

  // ---- bulk bar (P1-3): sequential PATCH / DELETE per token ----
  const bulkCount = h('span', { class: 'bulk-count' });
  const bulkProgress = h('span', { class: 'bulk-progress', hidden: true });
  const bulkBtns = [
    button(t('common.enable'), { icon: 'checkCircle', size: 'sm', onClick: () => bulkToggle(true) }),
    button(t('common.disable'), { icon: 'xCircle', size: 'sm', onClick: () => bulkToggle(false) }),
    button(t('common.delete'), { icon: 'trash', size: 'sm', variant: 'danger-soft', onClick: () => bulkDelete() }),
  ];
  const bulkClear = button(t('common.clearSelection'), { icon: 'x', size: 'sm', variant: 'ghost', onClick: () => { sel.clear(); syncSelection(); } });
  const bulkBar = h('div', { class: 'bulk-bar', hidden: true, role: 'region', 'aria-label': t('common.bulkActions') },
    bulkCount, h('div', { class: 'bulk-actions' }, bulkBtns), bulkProgress, h('div', { class: 'toolbar-spacer' }), bulkClear);

  const region = h('div', { class: 'table-region' }, h('div', { class: 'loading-bar', 'aria-hidden': 'true' }), tableWrap(table));
  el.append(card({ flush: true, body: [h('div', { class: 'toolbar' }, search, statusSeg, h('div', { class: 'toolbar-spacer' }), countEl), bulkBar, region, pager] }));

  function syncSelection() {
    for (const id of [...sel]) if (!st.rows.some((x) => x.id === id)) sel.delete(id);
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

  async function setEnabled(tk, v) {
    try {
      await patch(`/api/v1/tokens/${tk.id}`, { enabled: v });
      toast(v ? t('tokens.enabledToast', { name: tk.name }) : t('tokens.disabledToast', { name: tk.name }), 'success', 2000);
      load(); drawer.refresh();
      return true;
    } catch (e) { toastError(e); return false; }
  }

  function tokenMenu(tk) {
    return [
      { label: t('tokens.viewLogs'), icon: 'logs', onClick: () => ctx.navigate(`#/logs?token_id=${tk.id}`) },
      { label: t('common.edit'), icon: 'edit', onClick: () => openForm(tk) },
      { label: t('td.regenerate'), icon: 'refresh', onClick: () => regenerate(tk) },
      '-',
      { label: t('common.delete'), icon: 'trash', danger: true, onClick: () => remove(tk) },
    ];
  }

  function row(tk) {
    const expiring = tk.expires_at && !tk.expired && new Date(tk.expires_at).getTime() - Date.now() < 7 * 86400e3;
    const dup = knownNames.filter((n) => n === tk.name).length > 1;
    const menu = menuButton(() => tokenMenu(tk));
    const check = h('input', { type: 'checkbox', class: 'row-check', checked: sel.has(tk.id), 'aria-label': t('common.select'), onclick: (e) => e.stopPropagation(), onchange: (e) => { if (e.target.checked) sel.add(tk.id); else sel.delete(tk.id); syncSelection(); } });
    const tr = h('tr', { 'data-id': tk.id, class: `${!tk.enabled || tk.expired ? 'row-muted' : ''} ${sel.has(tk.id) ? 'row-selected' : ''}` },
      h('td', { class: 'col-check m-check' }, check),
      h('td', { class: 'col-switch m-meta m-switch' }, switchEl({ checked: tk.enabled, onChange: (v) => setEnabled(tk, v) })),
      h('td', { class: 'cell-tokname m-title' },
        h('div', { class: 'title-line' },
          h('button', { type: 'button', class: 'name-link strong name-text', title: `${tk.name || ''}\n${t('td.openHint')}`, onclick: (e) => { e.stopPropagation(); drawer.open(tk.id, { push: true }); } }, tk.name || `#${tk.id}`),
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
        h('button', { class: 'btn btn-ghost btn-icon btn-sm hide-m', title: t('tokens.viewLogs'), 'aria-label': t('tokens.viewLogs'), onclick: (e) => { e.stopPropagation(); ctx.navigate(`#/logs?token_id=${tk.id}`); } }, icon('logs', 15)),
        menu)));
    return tapToMenu(tr, menu);
  }

  // Each load aborts the previous one; old rows stay (dimmed) until the new page arrives.
  const nextSignal = latestSignal(ctx.signal);
  async function load() {
    const signal = nextSignal();
    if (st.loaded) { setTableLoading(region, true); pager.setLoading(true); }
    try {
      const res = await get('/api/v1/tokens', { q: st.q, status: st.status, page: st.page, page_size: st.pageSize }, { signal });
      if (signal.aborted) return;
      const rows = res.data || []; st.rows = rows; st.total = res.total || 0; st.totalKind = res.total_kind || 'exact'; st.loaded = true;
      if (!rows.length && st.page > 1 && st.total && st.totalKind === 'exact') { st.page = Math.ceil(st.total / st.pageSize); syncParams(); return load(); }
      knownNames = rows.map((x) => x.name);
      countEl.textContent = t('common.totalN', { n: fmtTotal(st.total, st.totalKind) });
      tbody.replaceChildren(...(rows.length ? rows.map(row) : [tableEmptyRow(COLS, st.q || st.status ? { icon: 'search', title: t('common.noMatches') } : { icon: 'ticket', title: t('tokens.empty'), desc: t('tokens.emptyDesc'), action: button(t('tokens.create'), { icon: 'plus', variant: 'primary', onClick: () => openForm() }) })]));
      pager.update({ page: st.page, pageSize: st.pageSize, total: st.total, totalKind: st.totalKind, hasMore: rows.length >= st.pageSize, rowCount: rows.length, loading: false });
      syncSelection();
    } catch (e) {
      if (e.name === 'AbortError' || signal.aborted) return;
      pager.setLoading(false);
      toastError(e);
      tbody.replaceChildren(tableEmptyRow(COLS, { icon: 'alert', title: t('common.loadFailed'), desc: errorMessage(e) }));
    } finally {
      if (!signal.aborted) setTableLoading(region, false);
    }
  }

  ctx.onParams((p) => {
    const next = stateFromParams(p);
    const listChanged = ['q', 'status', 'page', 'pageSize'].some((f) => next[f] !== st[f]);
    Object.assign(st, next);
    search.input.value = st.q; statusSeg.setValue(st.status);
    if (listChanged) { sel.clear(); load(); }
    drawer.sync(p.open);
    if (p.new) { openForm(); ctx.setParams({ ...listParams(), open: drawer.id || '' }); }
  });

  async function runBulk(label, items, fn) {
    if (!items.length) return;
    bulkRunning = true; syncSelection();
    bulkProgress.hidden = false;
    let done = 0, failed = 0;
    const update = () => { bulkProgress.replaceChildren(spinner(13), h('span', null, `${label} ${done}/${items.length}${failed ? ` · ${t('common.failedN', { n: failed })}` : ''}`)); };
    update();
    for (const tk of items) {
      if (!ctx.alive()) break;
      try { await fn(tk); } catch (e) { failed++; console.warn('bulk', tk.id, e); }
      done++; update();
    }
    bulkRunning = false;
    bulkProgress.hidden = true;
    if (!ctx.alive()) return;
    toast(failed ? t('common.bulkDoneFailed', { ok: items.length - failed, total: items.length, n: failed }) : t('common.bulkDone', { n: items.length }), failed ? 'warning' : 'success');
    sel.clear();
    await load();
  }
  const selected = () => st.rows.filter((x) => sel.has(x.id));
  async function bulkToggle(enabled) {
    await runBulk(enabled ? t('common.enable') : t('common.disable'), selected(), (tk) => patch(`/api/v1/tokens/${tk.id}`, { enabled }));
  }
  async function bulkDelete() {
    const items = selected();
    if (!items.length) return;
    const names = items.slice(0, 10).map((x) => h('li', null, h('span', { class: 'strong' }, x.name || `#${x.id}`), ' ', h('span', { class: 'mono muted small' }, `${x.prefix || ''}…`)));
    const extra = h('div', { class: 'bulk-names' }, h('ul', null, names), items.length > 10 ? h('div', { class: 'muted small' }, t('tl.andMore', { n: items.length, m: items.length - 10 })) : null);
    const ok = await confirmDialog({ title: t('tl.bulkDeleteTitle', { n: items.length }), message: t('tl.bulkDeleteMsg'), confirmText: t('common.delete'), danger: true, extra });
    if (ok) await runBulk(t('common.delete'), items, (tk) => del(`/api/v1/tokens/${tk.id}`));
  }

  async function regenerate(tk) {
    const ok = await confirmDialog({ title: t('td.regenTitle'), message: t('td.regenMsg', { name: tk.name }), confirmText: t('td.regenerate'), danger: true });
    if (!ok) return;
    try { const r = await post(`/api/v1/tokens/${tk.id}/regenerate`, {}); showSecret({ title: t('td.newSecret', { name: tk.name }), secret: r.token }); load(); drawer.refresh(); } catch (e) { toastError(e); }
  }

  async function remove(tk) {
    const ok = await confirmDialog({ title: t('tokens.deleteTitle'), message: t('td.deleteMsg', { name: tk.name }), confirmText: t('common.delete'), danger: true });
    if (!ok) return;
    try { await del(`/api/v1/tokens/${tk.id}`); toast(t('common.deleted'), 'success'); if (drawer.id === tk.id) drawer.close(); load(); } catch (e) { toastError(e); }
  }

  // ---- detail drawer (P1-4) ----
  function buildDrawer(id, d, signal) {
    const actionsEl = h('div', { class: 'detail-actions' });
    const quotaEl = h('div');
    let tk = st.rows.find((x) => x.id === id) || null;
    const paint = () => {
      if (!tk) return;
      const models = tk.allowed_models && tk.allowed_models.length ? tk.allowed_models.join(', ') : t('tokens.allModels');
      d.setHead({
        title: tk.name || `#${tk.id}`,
        badges: [statusBadge(tk), switchEl({ checked: tk.enabled, title: tk.enabled ? t('common.disable') : t('common.enable'), onChange: (v) => setEnabled(tk, v) })],
        sub: [
          h('span', { class: 'mono' }, `${tk.prefix || ''}…`),
          !tk.expires_at ? h('span', null, t('tokens.never'))
            : h('span', { title: fmtDateTime(tk.expires_at), class: tk.expired ? 'text-danger' : '' }, tk.expired ? t('td.expiredAt', { when: fmtDateTime(tk.expires_at, { seconds: false }) }) : t('td.expiresAt', { when: fmtDateTime(tk.expires_at, { seconds: false }) })),
          h('span', { class: 'entity-sub-models', title: models }, models),
        ],
      });
      actionsEl.replaceChildren(
        button(t('common.edit'), { icon: 'edit', size: 'sm', onClick: () => openForm(tk) }),
        button(t('td.regenerate'), { icon: 'refresh', size: 'sm', onClick: () => regenerate(tk) }),
        button(t('tokens.viewLogs'), { icon: 'logs', size: 'sm', onClick: () => ctx.navigate(`#/logs?token_id=${tk.id}`) }));
      const unlimited = !tk.quota_usd;
      const ratio = unlimited ? 0 : tk.used_usd / tk.quota_usd;
      const remaining = unlimited ? null : (tk.remaining_usd ?? Math.max(0, tk.quota_usd - tk.used_usd));
      quotaEl.replaceChildren(h('div', { class: 'detail-block quota-block' },
        h('div', { class: 'section-label' }, h('span', null, t('td.quota'))),
        h('div', { class: 'quota-figures' },
          h('span', { class: 'big', title: t('td.quotaUsed') }, fmtUSD(tk.used_usd)),
          h('span', { class: 'of' }, '/ ', unlimited ? t('tokens.unlimited') : fmtUSD(tk.quota_usd)),
          h('span', { class: 'left' }, unlimited ? t('td.noQuota') : t('tokens.remaining', { v: fmtUSD(remaining) }))),
        unlimited ? null : progress(ratio, ratio >= 1 ? 'danger' : ratio >= 0.8 ? 'warning' : 'accent'),
        kv([
          [t('td.quotaUsed'), h('span', null, h('span', { class: 'num-inline' }, fmtUSD(tk.used_usd)),
            tk.used_reset_at ? h('span', { class: 'muted small', title: fmtDateTime(tk.used_reset_at) }, ` · ${t('td.sinceReset', { date: fmtDateTime(tk.used_reset_at, { seconds: false }) })}`) : null)],
          tk.spent_usd != null ? [h('span', { title: t('td.spentHint') }, t('td.spent')), h('span', { class: 'num-inline', title: t('td.spentHint') }, fmtUSD(tk.spent_usd))] : null,
          [t('tokens.expiry'), !tk.expires_at ? t('tokens.never') : h('span', { class: tk.expired ? 'text-danger' : '' }, fmtDateTime(tk.expires_at, { seconds: false }), ' · ', timeEl(tk.expires_at))],
          [t('tokens.allowedModels'), tk.allowed_models && tk.allowed_models.length ? h('div', { class: 'chips chips-tight' }, tk.allowed_models.map((m) => h('span', { class: 'chip chip-sm mono' }, m))) : t('tokens.allModels')],
          tk.notes ? [t('common.notes'), h('span', { class: 'break' }, tk.notes)] : null,
        ])));
    };
    const overview = () => h('div', { class: 'entity-body' },
      quotaEl,
      section(t('kd.usage'), usageBlock('token', id, signal)),
      section(t('kd.recentFailures'), failuresBlock({ param: 'token_id', id, signal, navigate: ctx.navigate }), allLogsLink(`#/logs?token_id=${id}`, ctx.navigate)));
    const examples = () => h('div', { class: 'entity-body' },
      h('p', { class: 'muted small' }, t('tokens.baseUrlIs'), ' ', h('code', { class: 'mono inline-code' }, `${location.origin}/v1`)),
      clientSnippets('sk-xr-YOUR_TOKEN', { anthropic: true }),
      h('div', { class: 'field-hint' }, t('td.secretNote')));
    const mountBody = () => {
      let ov = null;
      d.setBody(h('div', { class: 'entity-body' }, actionsEl,
        tabs([
          { id: 'overview', label: t('td.tabOverview'), render: () => (ov = ov || overview()) },
          { id: 'examples', label: t('td.tabExamples'), render: examples },
        ], { cls: 'tabs-sm dd-tabs' })));
    };
    const fetchToken = async () => {
      try {
        tk = await get(`/api/v1/tokens/${id}`, null, { signal });
        if (signal.aborted) return;
        paint();
        if (!d.body.querySelector('.dd-tabs')) mountBody();
      } catch (e) {
        if (e.name === 'AbortError' || signal.aborted) return;
        if (e.status === 404) goneBody(d, { by: 'token', param: 'token_id', id, signal, navigate: ctx.navigate, message: t('td.notFound') });
        else if (!d.body.querySelector('.dd-tabs')) d.setBody(h('div', { class: 'drawer-loading' }, icon('alert', 22), h('span', null, errorMessage(e))));
        else toastError(e);
      }
    };
    if (tk) { paint(); mountBody(); }
    fetchToken();
    return fetchToken;
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
          load(); drawer.refresh();
        } catch (e) { toastError(e); }
      });
    }
  }

  if (ctx.params.new) openForm();
  syncParams();
  load();
  if (ctx.params.open) drawer.open(ctx.params.open);
}
