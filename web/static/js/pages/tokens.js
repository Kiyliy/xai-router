import { h, fmtInt, fmtUSD, timeEl, debounce, fmtDateTime, toLocalInput, fromLocalInput, fmtTotal, latestSignal } from '../util.js';
import { icon } from '../icons.js';
import { t, hasT } from '../i18n.js';
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

const isUnlimited = (tk) => (tk.unlimited != null ? !!tk.unlimited : !tk.quota_usd);
function isExhausted(tk) { return tk.exhausted != null ? !!tk.exhausted : !isUnlimited(tk) && (tk.remaining_usd ?? tk.quota_usd - tk.used_usd) <= 0; }
const remainingOf = (tk) => (isUnlimited(tk) ? null : tk.remaining_usd ?? Math.max(0, tk.quota_usd - tk.used_usd));
// Money in ticks (1 USD = 1e10) so the quota arithmetic is exact.
const TPU = 1e10;
const toTicks = (usd) => Math.round((Number(usd) || 0) * TPU);
const fromTicks = (ticks) => ticks / TPU;
const usd2 = (v) => fmtUSD(v);

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
    const unlimited = isUnlimited(tk);
    return h('td', { class: 'col-quota m-full' },
      // Without a quota there is nothing to compare against: show what has been used.
      unlimited
        ? h('div', { class: 'quota-line' },
          h('span', { class: 'mono' }, fmtUSD(tk.used_usd)),
          h('span', { class: 'muted' }, ' · '), h('span', { class: 'muted' }, t('td.used')))
        : h('div', { class: 'quota-line' },
          h('span', { class: 'mono' }, fmtUSD(tk.used_usd)),
          h('span', { class: 'muted' }, ' / '),
          h('span', { class: 'mono muted' }, fmtUSD(tk.quota_usd))),
      unlimited ? h('div', { class: 'cell-sub' }, t('tl.noQuota')) : null,
      unlimited ? null : progress(tk.quota_usd ? tk.used_usd / tk.quota_usd : 1),
      unlimited ? null : h('div', { class: `cell-sub ${isExhausted(tk) ? 'text-warning' : ''}` }, isExhausted(tk) ? t('tokens.exhausted') : t('tokens.remaining', { v: fmtUSD(remainingOf(tk)) })));
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
      { label: t('q.adjust'), icon: 'dollar', onClick: () => openAdjust(tk) },
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
    if (p.adjust && p.open) pendingAdjust = Number(p.open);
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
        button(t('q.adjust'), { icon: 'dollar', size: 'sm', onClick: () => openAdjust(tk) }),
        button(t('td.regenerate'), { icon: 'refresh', size: 'sm', onClick: () => regenerate(tk) }),
        button(t('tokens.viewLogs'), { icon: 'logs', size: 'sm', onClick: () => ctx.navigate(`#/logs?token_id=${tk.id}`) }));
      const unlimited = isUnlimited(tk);
      const ratio = unlimited ? 0 : tk.quota_usd ? tk.used_usd / tk.quota_usd : 1;
      const remaining = remainingOf(tk);
      quotaEl.replaceChildren(h('div', { class: 'detail-block quota-block' },
        h('div', { class: 'section-label' }, h('span', null, t('td.quota')),
          button(t('q.adjust'), { icon: 'dollar', size: 'sm', variant: 'ghost', onClick: () => openAdjust(tk) })),
        h('div', { class: 'quota-figures' },
          h('span', { class: 'big', title: t('td.used') }, fmtUSD(tk.used_usd)),
          h('span', { class: 'of' }, '/ ', unlimited ? t('tokens.unlimitedState') : fmtUSD(tk.quota_usd)),
          h('span', { class: `left ${isExhausted(tk) ? 'text-warning' : ''}` }, unlimited ? t('td.noQuota') : isExhausted(tk) ? t('q.exhaustedLine') : t('tokens.remaining', { v: fmtUSD(remaining) }))),
        unlimited ? null : progress(ratio, ratio >= 1 ? 'danger' : ratio >= 0.8 ? 'warning' : 'accent'),
        kv([
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
    let qh = null;
    const mountBody = () => {
      let ov = null;
      d.setBody(h('div', { class: 'entity-body' }, actionsEl,
        tabs([
          { id: 'overview', label: t('td.tabOverview'), render: () => (ov = ov || overview()) },
          { id: 'quota', label: t('q.history'), render: () => (qh = quotaHistory(id, signal)) },
          { id: 'examples', label: t('td.tabExamples'), render: examples },
        ], { cls: 'tabs-sm dd-tabs' })));
    };
    const fetchToken = async () => {
      try {
        tk = await get(`/api/v1/tokens/${id}`, null, { signal });
        if (signal.aborted) return;
        paint();
        if (!d.body.querySelector('.dd-tabs')) mountBody();
        else if (qh && qh.isConnected && qh.reload) qh.reload();
        if (pendingAdjust === id) { pendingAdjust = null; openAdjust(tk); }
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

  // ---- quota adjustments (pm-spec 8) ----
  let pendingAdjust = null;
  /** Adjust-quota dialog: increase / decrease a limited quota, or switch between limited and unlimited. */
  function openAdjust(tk0) {
    let tk = tk0;
    const startUnl = isUnlimited(tk);
    let mode = startUnl ? 'unlimited' : 'limited';
    let dir = 'increase';
    const amount = input({ type: 'text', inputmode: 'decimal', placeholder: '5.00', class: 'input mono', autocomplete: 'off' });
    const amountErr = h('div', { class: 'field-error', hidden: true });
    const reason = h('textarea', { class: 'input', rows: 2, maxlength: 200, placeholder: t('q.reasonPh') });
    const reasonCount = h('span', { class: 'muted small' }, '0 / 200');
    const reasonErr = h('div', { class: 'field-error', hidden: true });
    const preview = h('div', { class: 'q-preview', 'aria-live': 'polite' });
    const maxBtn = h('button', { type: 'button', class: 'link-btn', hidden: true });
    const errBox = h('div', { class: 'callout callout-danger', hidden: true });
    const amountLabel = h('label', { class: 'field-label' });
    const modeSeg = segmented({ size: 'seg-sm', value: mode, options: [{ value: 'limited', label: t('q.limited') }, { value: 'unlimited', label: t('q.unlimited') }], onChange: (v) => { mode = v; paint(); } });
    const dirSeg = segmented({ size: 'seg-sm', value: dir, options: [{ value: 'increase', label: t('q.increase') }, { value: 'decrease', label: t('q.decrease') }], onChange: (v) => { dir = v; paint(); } });
    const dirRow = h('div', { class: 'field' }, h('label', { class: 'field-label' }, t('q.direction')), dirSeg);
    const amountRow = h('div', { class: 'field' }, amountLabel, h('div', { class: 'input-affix' }, h('span', { class: 'affix' }, '$'), amount), amountErr, maxBtn);
    const note = h('div', { class: 'callout callout-warning', hidden: true }, icon('alert', 16), h('span'));
    const nowV = h('span', { class: 'q-now-v' });
    const body = h('form', { class: 'stack q-form', onsubmit: (e) => { e.preventDefault(); submit(); } },
      h('div', { class: 'q-now' }, h('span', { class: 'muted' }, t('q.now')), nowV),
      h('div', { class: 'field' }, h('label', { class: 'field-label' }, t('q.state')), modeSeg),
      dirRow, amountRow, note,
      h('div', { class: 'field' }, h('label', { class: 'field-label' }, t('q.reason'), h('span', { class: 'req' }, '*')), reason,
        h('div', { class: 'row-sm q-reason-foot' }, reasonErr, h('span', { class: 'toolbar-spacer' }), reasonCount)),
      preview, errBox, h('button', { type: 'submit', hidden: true }));
    const okBtn = h('button', { class: 'btn btn-primary', type: 'button', onclick: () => submit() }, h('span'));
    const m = openModal({ title: t('q.title', { name: tk.name || `#${tk.id}` }), size: 'sm', body, footer: [h('button', { class: 'btn', type: 'button', onclick: () => m.close() }, t('common.cancel')), okBtn] });
    const usedT = () => toTicks(tk.used_usd), quotaT = () => toTicks(tk.quota_usd);
    // Largest decrease with at most 4 decimals that keeps quota >= used.
    const maxDecT = () => Math.floor(Math.max(0, quotaT() - usedT()) / 1e6) * 1e6;
    const action = () => (startUnl ? (mode === 'limited' ? 'set_limited' : null) : (mode === 'unlimited' ? 'set_unlimited' : dir));
    const parseAmt = () => { const v = amount.value.trim(); return /^\d+(\.\d{1,4})?$/.test(v) ? toTicks(Number(v)) : null; };
    function validate(show) {
      const a = action();
      let err = '';
      if (a === 'increase' || a === 'decrease' || a === 'set_limited') {
        const v = parseAmt();
        if (amount.value.trim() === '') err = show ? t('q.errAmountReq') : '';
        else if (v == null) err = t('q.errAmountFmt');
        else if (a !== 'set_limited' && v <= 0) err = t('q.errAmountPos');
        else if (a === 'decrease' && quotaT() - v < usedT()) err = t('q.errMaxDec', { max: usd2(fromTicks(maxDecT())), used: usd2(tk.used_usd) });
      }
      amountErr.textContent = err; amountErr.hidden = !err; amount.classList.toggle('invalid', !!err);
      const r = reason.value.trim();
      const rErr = show && !r ? t('q.errReason') : '';
      reasonErr.textContent = rErr; reasonErr.hidden = !rErr; reason.classList.toggle('invalid', !!rErr);
      return !err && !!a && !!r && (a === 'set_unlimited' || parseAmt() != null);
    }
    function paint() {
      const a = action();
      nowV.textContent = startUnl ? t('q.roUnlimited', { used: usd2(tk.used_usd) }) : t('q.ro', { q: usd2(tk.quota_usd), used: usd2(tk.used_usd), left: usd2(remainingOf(tk)) });
      dirRow.hidden = a !== 'increase' && a !== 'decrease';
      amountRow.hidden = !(a === 'increase' || a === 'decrease' || a === 'set_limited');
      amountLabel.replaceChildren(a === 'set_limited' ? t('q.balance') : t('q.amount'), h('span', { class: 'req' }, '*'));
      amount.placeholder = a === 'set_limited' ? '0.00' : '5.00';
      maxBtn.hidden = a !== 'decrease' || maxDecT() <= 0;
      maxBtn.textContent = t('q.toUsed', { v: usd2(fromTicks(maxDecT())) });
      note.hidden = a !== 'set_unlimited' && a !== null;
      note.lastChild.textContent = a === 'set_unlimited' ? t('q.unlimitedWarn') : t('q.alreadyUnlimited');
      // Live preview: quota and remaining before → after.
      const v = parseAmt();
      let pv = '';
      if ((a === 'increase' || a === 'decrease') && v != null && v > 0) {
        const after = a === 'increase' ? quotaT() + v : quotaT() - v;
        if (after >= usedT()) {
          pv = t('q.preview', { a: usd2(tk.quota_usd), b: usd2(fromTicks(after)), ra: usd2(fromTicks(Math.max(0, quotaT() - usedT()))), rb: usd2(fromTicks(Math.max(0, after - usedT()))) });
          if (after === usedT()) pv += t('q.willExhaust');
        }
      } else if (a === 'set_limited' && v != null) {
        pv = t('q.previewLimited', { used: usd2(tk.used_usd), x: usd2(fromTicks(v)), q: usd2(fromTicks(usedT() + v)) });
      } else if (a === 'set_unlimited') pv = t('q.previewUnlimited', { q: usd2(tk.quota_usd) });
      preview.textContent = pv; preview.hidden = !pv;
      const vs = v != null && v > 0 ? usd2(fromTicks(v)) : '';
      const lbl = a === 'increase' ? (vs ? t('q.btnIncreaseN', { v: vs }) : t('q.increase'))
        : a === 'decrease' ? (vs ? t('q.btnDecreaseN', { v: vs }) : t('q.decrease'))
          : a === 'set_limited' ? t('q.btnLimited') : a === 'set_unlimited' ? t('q.btnUnlimited') : t('q.adjust');
      okBtn.lastChild.textContent = lbl;
      okBtn.disabled = !a;
      const danger = a === 'decrease' || a === 'set_unlimited';
      okBtn.classList.toggle('btn-danger', danger);
      okBtn.classList.toggle('btn-primary', !danger);
      validate(false);
    }
    amount.addEventListener('input', paint);
    reason.addEventListener('input', () => { reasonCount.textContent = `${[...reason.value].length} / 200`; validate(false); });
    maxBtn.addEventListener('click', () => { amount.value = String(fromTicks(maxDecT())); paint(); });
    async function submit() {
      errBox.hidden = true;
      if (!validate(true)) { (amountErr.hidden ? reason : amount).focus(); return; }
      const a = action();
      if (a === 'set_unlimited') {
        const ok = await confirmDialog({ title: t('q.confirmUnlimitedTitle', { name: tk.name }), message: t('q.confirmUnlimitedMsg', { q: usd2(tk.quota_usd) }), confirmText: t('q.btnUnlimited'), danger: true });
        if (!ok) return;
      }
      const payload = { action: a, reason: reason.value.trim() };
      if (a !== 'set_unlimited') payload.amount_usd = Number(amount.value.trim());
      await busy(okBtn, async () => {
        try {
          const res = await post(`/api/v1/tokens/${tk.id}/quota`, payload);
          const rec = res.record || {};
          m.close();
          const msg = a === 'increase' ? t('q.toastIncrease', { v: usd2(payload.amount_usd), a: usd2(rec.before_usd), b: usd2(rec.after_usd) })
            : a === 'decrease' ? t('q.toastDecrease', { v: usd2(payload.amount_usd), a: usd2(rec.before_usd), b: usd2(rec.after_usd) })
              : a === 'set_limited' ? t('q.toastLimited', { q: usd2(rec.after_usd) }) : t('q.toastUnlimited');
          toast(msg, 'success');
          load(); drawer.refresh();
        } catch (e) {
          errBox.replaceChildren(icon('xCircle', 16), h('span', null, errorMessage(e)));
          errBox.hidden = false;
        }
      });
    }
    paint();
    setTimeout(() => (amountRow.hidden ? reason : amount).focus(), 40);
    // Work from fresh numbers (usage moves while the dialog is open).
    get(`/api/v1/tokens/${tk.id}`).then((fresh) => { if (fresh && m.el.isConnected) { tk = fresh; paint(); } }).catch(() => {});
  }

  /** "Quota history" tab: every record, newest first; the amounts add up to the current quota. */
  function quotaHistory(id, signal) {
    const box = h('div', { class: 'entity-body q-history' }, h('div', { class: 'mini-empty' }, spinner(14), h('span', null, t('common.loading'))));
    const actionText = (r) => (hasT(`q.act.${r.action}`) ? t(`q.act.${r.action}`) : r.action);
    const amt = (r) => {
      if (r.action === 'set_unlimited' || (r.unlimited && !r.amount_usd)) return h('span', { class: 'muted' }, '—');
      const v = r.amount_usd || 0;
      return h('span', { class: v > 0 ? 'text-success' : v < 0 ? 'text-danger' : '' }, `${v < 0 ? '−' : '+'}${usd2(Math.abs(v))}`);
    };
    const side = (unl, v) => (unl ? t('tokens.unlimitedState') : usd2(v));
    const actor = (r) => (r.actor === 'system' ? t('q.actor.system') : r.actor === 'password' ? t('q.actor.password')
      : r.actor && r.actor.startsWith('admin_token:') ? `${t('q.actor.adminToken')} · ${r.actor_name || ''}` : (r.actor_name || r.actor || '—'));
    const reasonOf = (r) => (r.reason_code === 'pre_v0_4_1' ? t('q.code.pre') : r.reason_code === 'reset_folded' ? t('q.code.reset') : r.reason || '');
    let rows = [], total = 0, granted = null;
    async function fetchAll() {
      rows = []; total = 0;
      for (let page = 1; page <= 50; page++) {
        const res = await get(`/api/v1/tokens/${id}/quota-history`, { page, page_size: 200 }, { signal });
        rows.push(...(res.data || [])); total = res.total || rows.length; granted = res.granted_total_usd;
        if (rows.length >= total || !(res.data || []).length) break;
      }
    }
    async function render() {
      try {
        await fetchAll();
        if (signal.aborted) return;
        const tok = await get(`/api/v1/tokens/${id}`, null, { signal }).catch(() => null);
        const cur = tok ? tok.quota_usd : null;
        const equal = cur != null && granted != null && toTicks(cur) === toTicks(granted);
        const heads = [t('q.h.time'), t('q.h.action'), t('q.h.amount'), t('q.h.beforeAfter'), t('q.h.used'), t('q.h.actor'), t('q.h.reason')];
        box.replaceChildren(
          rows.length ? h('div', { class: 'table-wrap q-table-wrap' }, h('table', { class: 'table q-table' },
            h('thead', null, h('tr', null, heads.map((x, i) => h('th', { class: i === 2 || i === 4 ? 'num' : null }, x)))),
            h('tbody', null, rows.map((r) => h('tr', null,
              h('td', { class: 'nowrap', title: fmtDateTime(r.created_at) }, fmtDateTime(r.created_at, { seconds: false })),
              h('td', { class: 'nowrap' }, actionText(r)),
              h('td', { class: 'num mono nowrap' }, amt(r)),
              h('td', { class: 'mono nowrap' }, r.action === 'initial' ? side(r.unlimited, r.after_usd) : `${side(r.before_unlimited, r.before_usd)} → ${side(r.unlimited, r.after_usd)}`),
              h('td', { class: 'num mono nowrap' }, usd2(r.used_usd)),
              h('td', null, h('div', null, actor(r)), r.ip ? h('div', { class: 'cell-sub mono' }, r.ip) : null),
              h('td', { class: 'q-reason' }, reasonOf(r) || h('span', { class: 'muted' }, '—'))))))) : h('div', { class: 'mini-empty' }, t('q.noRecords')),
          h('div', { class: `q-total ${equal ? '' : 'text-warning'}` }, icon(equal ? 'checkCircle' : 'alert', 14),
            h('span', null, tok && isUnlimited(tok) ? t('q.totalUnlimited', { g: usd2(granted || 0) }) : t(equal ? 'q.total' : 'q.totalMismatch', { g: usd2(granted || 0), q: cur != null ? usd2(cur) : '—' }))),
          h('div', { class: 'row-sm' }, h('span', { class: 'toolbar-spacer' }),
            h('a', { class: 'link', href: `#/logs?type=audit&token_id=${id}&range=all` }, t('q.viewInLogs'))));
      } catch (e) {
        if (e.name === 'AbortError' || signal.aborted) return;
        box.replaceChildren(h('div', { class: 'mini-empty mini-error' }, icon('alert', 15), h('span', null, `${t('common.loadFailed')} · ${errorMessage(e)}`)));
      }
    }
    box.reload = render;
    render();
    return box;
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
    // Create: the first quota (limited, $0 allowed) or unlimited. Edit: read-only, changed with "Adjust quota".
    let quotaMode = 'unlimited';
    const quota = input({ type: 'text', inputmode: 'decimal', placeholder: '10.00', class: 'input mono' });
    const quotaErr = h('div', { class: 'field-error', hidden: true });
    const quotaWrap = h('div', { class: 'input-affix', hidden: true }, h('span', { class: 'affix' }, '$'), quota);
    const quotaSeg = segmented({ size: 'seg-sm', value: quotaMode, options: [{ value: 'limited', label: t('q.limited') }, { value: 'unlimited', label: t('q.unlimited') }],
      onChange: (v) => { quotaMode = v; quotaWrap.hidden = v !== 'limited'; quotaErr.hidden = true; if (v === 'limited') quota.focus(); } });
    const quotaField = edit
      ? field(t('td.quota'), h('div', { class: 'quota-ro' },
        h('span', null, isUnlimited(tk) ? t('q.roUnlimited', { used: usd2(tk.used_usd) }) : t('q.ro', { q: usd2(tk.quota_usd), used: usd2(tk.used_usd), left: usd2(remainingOf(tk)) })),
        button(t('q.adjust'), { icon: 'dollar', size: 'sm', onClick: () => openAdjust(tk) })), { full: true })
      : field(t('td.quota'), h('div', { class: 'stack-sm' }, quotaSeg, quotaWrap, quotaErr), { hint: t('q.createHint'), full: true });
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
    const body = h('form', { class: 'form-grid', onsubmit: (e) => { e.preventDefault(); save(); } },
      h('div', { class: 'field' }, h('label', { class: 'field-label' }, t('common.name'), h('span', { class: 'req' }, '*')), name, dupWarn),
      quotaField,
      field(t('tokens.expiry'), h('div', { class: 'stack-sm' }, h('div', { class: 'row-sm' }, expires, never), presets), { full: true }),
      h('div', { class: 'field field-full' }, h('label', { class: 'field-label' }, t('tokens.allowedModels')), models, modelsHint),
      field(t('common.notes'), notes, { full: true }),
      h('div', { class: 'field field-full checks' }, enabled),
      h('button', { type: 'submit', hidden: true }));
    const saveBtn = h('button', { class: 'btn btn-primary', onclick: () => save() }, h('span', null, edit ? t('common.save') : t('tokens.create')));
    const m = openModal({ title: edit ? t('tokens.editTitle', { name: tk.name }) : t('tokens.createTitle'), size: 'md', body, footer: [h('button', { class: 'btn', onclick: () => m.close() }, t('common.cancel')), saveBtn] });
    // Fill model suggestions and the full name list in the background; the dialog is already usable.
    if (!modelCache) modelIds().then((ids) => { models.setSuggestions(ids); modelsHint.replaceChildren(t('tokens.modelsHint')); });
    get('/api/v1/tokens', { page_size: 1000 }).then((r) => { allNames = (r.data || []).map((x) => x.name); checkDup(); }).catch(() => {});
    async function save() {
      if (!name.value.trim()) { name.focus(); name.classList.add('invalid'); return; }
      if (checkDup() && !(await confirmDialog({ title: t('tokens.dupTitle'), message: t('tokens.dupConfirm', { name: name.value.trim() }), confirmText: t('tokens.dupContinue') }))) { name.focus(); return; }
      let q;
      if (!edit && quotaMode === 'limited') {
        const v = quota.value.trim();
        if (!/^\d+(\.\d{1,4})?$/.test(v)) { quotaErr.textContent = t('q.errAmountCreate'); quotaErr.hidden = false; quota.classList.add('invalid'); quota.focus(); return; }
        q = Number(v);
      }
      const payload = {
        name: name.value.trim(), enabled: enabled.input.checked, notes: notes.value,
        allowed_models: models.getValues(),
        expires_at: never.input.checked ? null : fromLocalInput(expires.value),
      };
      let body = payload;
      if (!edit && q != null) body = { ...payload, quota_usd: q };
      if (edit) {
        body = {};
        for (const [k, v] of Object.entries(payload)) {
          const old = tk[k];
          if (k === 'allowed_models') { if (JSON.stringify(v) !== JSON.stringify(old || [])) body[k] = v; }
          else if (k === 'expires_at') { if ((v ? new Date(v).getTime() : null) !== (old ? Math.floor(new Date(old).getTime() / 60000) * 60000 : null)) body[k] = v; }
          else if (v !== old) body[k] = v;
        }
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
  if (ctx.params.open) {
    if (ctx.params.adjust) pendingAdjust = Number(ctx.params.open);
    drawer.open(ctx.params.open);
  }
}
