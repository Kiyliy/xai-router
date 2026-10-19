import { h, fmtInt, fmtPrice, fmtUSD, timeEl, fmtCompact, debounce } from '../util.js';
import { icon } from '../icons.js';
import { t } from '../i18n.js';
import { get, post, patch, del } from '../api.js';
import {
  card, toast, toastError, openModal, confirmDialog, menuButton, badge, switchEl, searchInput, segmented,
  skeletonRows, tableEmptyRow, tableWrap, field, input, select, checkbox, button, busy, tagInput, tapToMenu, errorMessage,
} from '../ui.js';

const KINDS = ['language', 'image', 'video', 'embedding'];
const SOURCE_VARIANT = { sync: 'info', manual: 'accent', builtin: 'neutral' };

export function render(el, ctx) {
  const st = { kind: KINDS.includes(ctx.params.kind) ? ctx.params.kind : '', q: ctx.params.q || '', data: [], lastSync: null };
  ctx.setActions(
    button(t('models.sync'), { icon: 'refresh', onClick: openSync }),
    button(t('models.add'), { icon: 'plus', variant: 'primary', onClick: () => openForm() }),
  );

  const kindSel = segmented({
    options: [{ value: '', label: t('common.all') }, ...KINDS.map((k) => ({ value: k, label: t(`kind.${k}`) }))], value: st.kind,
    onChange: (v) => { st.kind = v; ctx.setParams({ kind: v, q: st.q }); draw(); },
  });
  const search = searchInput({ value: st.q, placeholder: t('models.searchPh'), onInput: debounce((v) => { st.q = v.trim().toLowerCase(); ctx.setParams({ kind: st.kind, q: st.q }); draw(); }, 150) });
  const syncInfo = h('div', { class: 'sync-info' });
  let COLS = 10;
  let showCtx = false;
  const tbody = h('tbody', null, skeletonRows(COLS));
  const ctxTh = h('th', { class: 'num col-ctx' }, t('models.context'));
  const table = h('table', { class: 'table table-models table-cards' },
    h('thead', null, h('tr', null,
      h('th', null, t('common.model')), ctxTh,
      h('th', { class: 'num' }, t('models.thInput')), h('th', { class: 'num' }, t('models.thCached')), h('th', { class: 'num' }, t('models.thOutput')),
      h('th', { class: 'num', title: t('models.longCtxHint') }, t('models.longCtx')), h('th', { class: 'num' }, t('models.image')), h('th', { class: 'num' }, t('models.video')),
      h('th', null, t('models.source')), h('th', { class: 'col-actions' }, h('span', { class: 'sr-only' }, t('common.actions'))))),
    tbody);
  el.append(card({ flush: true, body: [h('div', { class: 'toolbar' }, kindSel, search, h('div', { class: 'toolbar-spacer' }), syncInfo), tableWrap(table)] }));

  function renderSyncInfo() {
    const s = st.lastSync;
    if (!s) { syncInfo.replaceChildren(h('span', { class: 'muted small' }, t('models.neverSynced'))); return; }
    syncInfo.replaceChildren(h('span', { class: 'small muted sync-pill', title: s.error || '' },
      s.ok ? icon('checkCircle', 14, 'text-success') : icon('xCircle', 14, 'text-danger'),
      h('span', { class: 'sync-text' }, t('models.lastSync'), ' ', timeEl(s.at), s.ok ? ` · ${t('models.syncCounts', { a: (s.added || []).length, u: (s.updated || []).length })}` : ` · ${s.error || t('common.error')}`)));
  }

  function aliasesEl(m) {
    const a = m.aliases || [];
    if (!a.length) return null;
    if (a.length <= 2) return h('div', { class: 'chips chips-tight' }, a.map((x) => h('span', { class: 'chip chip-sm mono' }, x)));
    const det = h('details', { class: 'aliases' },
      h('summary', null, t('models.aliasesN', { n: a.length })),
      h('div', { class: 'chips chips-tight' }, a.map((x) => h('span', { class: 'chip chip-sm mono' }, x))));
    return det;
  }

  function priceCell(v, sub, label) {
    return h('td', { class: `num mono ${v > 0 ? 'm-meta' : 'm-hide'}`, 'data-label': label }, fmtPrice(v), sub ? h('div', { class: 'cell-sub' }, sub) : null);
  }

  // Media input prices (per input image, per second of input video), as a cell sub-line.
  function inputSub(m) {
    const parts = [];
    if (m.input_image_price > 0) parts.push(`${t('models.inImg')} ${fmtUSD(m.input_image_price)}${t('models.perImage')}`);
    if (m.input_video_price_per_second > 0) parts.push(`${t('models.inVid')} ${fmtUSD(m.input_video_price_per_second)}${t('models.perSec')}`);
    return parts.length ? h('div', { class: 'cell-sub nowrap' }, parts.join(' · ')) : null;
  }

  function row(m) {
    const long = m.long_context_threshold > 0
      ? h('td', { class: 'num mono m-meta', 'data-label': t('models.longShort'), title: `${t('models.input')}: ${fmtPrice(m.long_input_price)}\n${t('models.cached')}: ${fmtPrice(m.long_cached_input_price)}\n${t('models.output')}: ${fmtPrice(m.long_output_price)}` },
        h('div', { class: 'nowrap' }, `${fmtPrice(m.long_input_price)} / ${fmtPrice(m.long_output_price)}`), h('div', { class: 'cell-sub nowrap' }, `> ${fmtCompact(m.long_context_threshold)} ${t('models.tok')}`))
      : h('td', { class: 'num muted m-hide' }, '—');
    const tiers = m.image_tiers || [];
    const vtiers = m.video_tiers || [];
    const img = m.image_price > 0 || tiers.length
      ? h('td', { class: 'num mono m-meta', 'data-label': t('models.image') }, m.image_price > 0 ? `${fmtUSD(m.image_price)}${t('models.perImage')}` : '—',
        tiers.length ? h('div', { class: 'cell-sub', 'data-tip': tiers.map((x) => `${x.quality || '*'} ${x.resolution || ''}: ${fmtUSD(x.price)}`).join(' · ') }, t('models.tiersN', { n: tiers.length })) : null,
        m.kind === 'image' ? inputSub(m) : null)
      : h('td', { class: 'num muted m-hide' }, '—');
    // Only mention the image-input price when it differs from the text input price.
    const imgIn = m.image_input_price > 0 && m.image_input_price !== m.input_price ? `${t('models.imgIn')} ${fmtPrice(m.image_input_price)}` : null;
    const disabled = m.enabled === false;
    const menu = menuButton(() => [
      { label: t('common.edit'), icon: 'edit', onClick: () => openForm(m) },
      { label: disabled ? t('common.enable') : t('models.disableBlock'), icon: disabled ? 'checkCircle' : 'xCircle', onClick: () => patch(`/api/v1/models/${encodeURIComponent(m.id)}`, { enabled: disabled }).then(() => { toast(disabled ? t('models.enabledToast', { id: m.id }) : t('models.disabledToast', { id: m.id }), 'success'); load(); }, toastError) },
      { label: t('models.viewLogs'), icon: 'logs', onClick: () => ctx.navigate(`#/logs?model=${encodeURIComponent(m.id)}`) },
      '-',
      { label: t('common.delete'), icon: 'trash', danger: true, onClick: () => remove(m) },
    ]);
    const tr = h('tr', { class: disabled ? 'row-muted' : '' },
      h('td', { class: 'cell-model m-title' },
        h('div', { class: 'model-id' }, h('span', { class: 'mono strong model-name', title: m.id }, m.id), disabled ? badge(t('common.disabled'), 'neutral', { title: t('models.disabledHint') }) : null),
        aliasesEl(m)),
      showCtx ? h('td', { class: `num mono ${m.context_length ? 'm-meta' : 'm-hide'}`, 'data-label': t('models.context') }, m.context_length ? fmtCompact(m.context_length) : '—') : null,
      priceCell(m.input_price, imgIn, t('models.input')),
      priceCell(m.cached_input_price, null, t('models.cached')),
      priceCell(m.output_price, null, t('models.output')),
      long, img,
      h('td', { class: `num mono ${m.video_price_per_second > 0 ? 'm-meta' : 'm-hide'}`, 'data-label': t('models.video') }, m.video_price_per_second > 0 ? `${fmtUSD(m.video_price_per_second)}${t('models.perSec')}` : h('span', { class: 'muted' }, '—'),
        vtiers.length ? h('div', { class: 'cell-sub', 'data-tip': vtiers.map((x) => `${x.resolution}: ${fmtUSD(x.price_per_second)}${t('models.perSec')}`).join(' · ') }, t('models.tiersN', { n: vtiers.length })) : null,
        m.kind === 'video' ? inputSub(m) : null),
      h('td', { class: 'm-badge' }, badge(t(`source.${m.source}`) || m.source || '—', SOURCE_VARIANT[m.source] || 'neutral'), m.synced_at ? h('div', { class: 'cell-sub nowrap hide-m' }, timeEl(m.synced_at)) : null),
      h('td', { class: 'col-actions m-menu' }, h('div', { class: 'row-actions' },
        h('button', { class: 'btn btn-ghost btn-icon btn-sm hide-m', title: t('common.edit'), 'aria-label': t('common.edit'), onclick: () => openForm(m) }, icon('edit', 15)),
        menu)));
    return tapToMenu(tr, menu);
  }

  function draw() {
    // The context column only earns its width when at least one model reports a context length.
    showCtx = st.data.some((m) => m.context_length > 0);
    ctxTh.hidden = !showCtx;
    COLS = showCtx ? 10 : 9;
    let rows = st.data;
    if (st.kind) rows = rows.filter((m) => m.kind === st.kind);
    if (st.q) rows = rows.filter((m) => m.id.toLowerCase().includes(st.q) || (m.aliases || []).some((a) => a.toLowerCase().includes(st.q)));
    if (!rows.length) {
      tbody.replaceChildren(tableEmptyRow(COLS, st.data.length ? { icon: 'search', title: t('common.noMatches') } : { icon: 'cpu', title: t('models.empty'), desc: t('models.emptyDesc'), action: button(t('models.sync'), { icon: 'refresh', variant: 'primary', onClick: openSync }) }));
      return;
    }
    const out = [];
    for (const kind of KINDS) {
      const group = rows.filter((m) => m.kind === kind);
      if (!group.length) continue;
      if (!st.kind) out.push(h('tr', { class: 'group-row' }, h('td', { colspan: COLS }, h('span', { class: `kind-tag kind-${kind}` }, t(`kind.${kind}`)), h('span', { class: 'muted small' }, ` ${group.length}`))));
      group.forEach((m) => out.push(row(m)));
    }
    const other = rows.filter((m) => !KINDS.includes(m.kind));
    other.forEach((m) => out.push(row(m)));
    tbody.replaceChildren(...out);
  }

  async function load() {
    try {
      const res = await get('/api/v1/models', null, { signal: ctx.signal });
      st.data = (res.data || []).slice().sort((a, b) => a.id.localeCompare(b.id));
      st.lastSync = res.last_sync || null;
      renderSyncInfo();
      draw();
    } catch (e) {
      if (e.name === 'AbortError') return;
      toastError(e);
      tbody.replaceChildren(tableEmptyRow(COLS, { icon: 'alert', title: t('common.loadFailed'), desc: errorMessage(e) }));
    }
  }

  function openSync() {
    const overwrite = checkbox({ checked: false, label: t('models.overwriteManual'), hint: t('models.overwriteHint') });
    const go = h('button', { class: 'btn btn-primary' }, icon('refresh', 15), h('span', null, t('models.syncStart')));
    const m = openModal({
      title: t('models.syncTitle'), size: 'md',
      body: h('div', { class: 'stack' }, h('p', { class: 'muted' }, t('models.syncDesc')), overwrite),
      footer: [h('button', { class: 'btn', onclick: () => m.close() }, t('common.cancel')), go],
    });
    go.addEventListener('click', () => busy(go, async () => {
      try {
        const r = await post('/api/v1/models/sync', { overwrite_manual: overwrite.input.checked });
        const list = (title, items, variant) => items && items.length ? [h('h4', { class: 'section-title' }, title, h('span', { class: 'count-pill' }, String(items.length))), h('div', { class: 'chips' }, items.map((x) => h('span', { class: `chip mono chip-${variant}` }, x)))] : null;
        m.setTitle(t('models.syncResult'));
        m.setBody(h('div', { class: 'stack' },
          r.ok === false ? h('div', { class: 'callout callout-danger' }, icon('xCircle', 16), h('span', null, r.error || t('common.error'))) : h('div', { class: 'callout callout-success' }, icon('checkCircle', 16), h('span', null, t('models.syncOk', { total: r.total ?? '—' }))),
          h('div', { class: 'stat-row' },
            h('div', { class: 'stat stat-success' }, h('div', { class: 'stat-v' }, String((r.added || []).length)), h('div', { class: 'stat-l' }, t('models.added'))),
            h('div', { class: 'stat' }, h('div', { class: 'stat-v' }, String((r.updated || []).length)), h('div', { class: 'stat-l' }, t('models.updated'))),
            h('div', { class: 'stat' }, h('div', { class: 'stat-v' }, String(r.total ?? '—')), h('div', { class: 'stat-l' }, t('models.total')))),
          list(t('models.added'), r.added, 'success'),
          list(t('models.updated'), r.updated, 'info')));
        m.setFooter(h('button', { class: 'btn btn-primary', onclick: () => m.close() }, t('common.done')));
        load();
      } catch (e) { toastError(e); }
    }));
  }

  async function remove(m) {
    const ok = await confirmDialog({ title: t('models.deleteTitle'), message: t('models.deleteMsg', { id: m.id }), confirmText: t('common.delete'), danger: true });
    if (!ok) return;
    try { await del(`/api/v1/models/${encodeURIComponent(m.id)}`); toast(t('common.deleted'), 'success'); load(); } catch (e) { toastError(e); }
  }

  function openForm(m) {
    const edit = !!m;
    const num = (v, attrs = {}) => input({ type: 'number', value: v ?? '', step: 'any', min: 0, placeholder: '0', ...attrs });
    const f = {
      id: input({ value: m?.id || '', class: 'input mono', placeholder: 'grok-4.3', disabled: edit }),
      kind: select(KINDS.map((k) => ({ value: k, label: t(`kind.${k}`) })), m?.kind || 'language'),
      context_length: num(m?.context_length, { step: 1 }),
      input_price: num(m?.input_price), cached_input_price: num(m?.cached_input_price), image_input_price: num(m?.image_input_price), output_price: num(m?.output_price),
      long_context_threshold: num(m?.long_context_threshold, { step: 1 }),
      long_input_price: num(m?.long_input_price), long_cached_input_price: num(m?.long_cached_input_price), long_output_price: num(m?.long_output_price),
      image_price: num(m?.image_price), video_price_per_second: num(m?.video_price_per_second),
      input_image_price: num(m?.input_image_price), input_video_price_per_second: num(m?.input_video_price_per_second),
    };
    const aliases = tagInput({ values: m?.aliases || [], placeholder: t('models.aliasPh') });
    const enabled = switchEl({ checked: m ? m.enabled !== false : true, label: t('common.enabled') });
    // Tier editors: image tiers (quality, resolution, $ per image) and video
    // tiers (resolution, $ per second).
    const tierEditor = (rows, withQuality, priceKey) => {
      const box = h('div', { class: 'tiers' });
      const draw = () => {
        box.replaceChildren(
          ...rows.map((tier, i) => h('div', { class: `tier-row${withQuality ? '' : ' tier-row-2'}` },
            withQuality ? input({ value: tier.quality || '', placeholder: t('models.qualityOpt'), 'aria-label': t('models.quality'), oninput: (e) => { tier.quality = e.target.value; } }) : null,
            input({ value: tier.resolution || '', placeholder: withQuality ? '1k / 1.5k / 2k' : '480p / 720p / 1080p', 'aria-label': t('models.resolution'), oninput: (e) => { tier.resolution = e.target.value; } }),
            h('div', { class: 'input-affix' }, h('span', { class: 'affix' }, '$'), input({ type: 'number', step: 'any', min: 0, value: tier[priceKey] ?? '', 'aria-label': withQuality ? t('models.imagePrice') : t('models.videoPrice'), oninput: (e) => { tier[priceKey] = e.target.value === '' ? null : Number(e.target.value); } })),
            h('button', { type: 'button', class: 'btn btn-ghost btn-icon btn-sm', 'aria-label': t('common.remove'), onclick: () => { rows.splice(i, 1); draw(); } }, icon('trash', 14)))),
          h('button', { type: 'button', class: 'btn btn-sm', onclick: () => { rows.push(withQuality ? { quality: '', resolution: '', price: null } : { resolution: '', price_per_second: null }); draw(); } }, icon('plus', 14), h('span', null, t('models.addTier'))));
      };
      draw();
      return box;
    };
    const tiers = (m?.image_tiers || []).map((x) => ({ ...x }));
    const vtiers = (m?.video_tiers || []).map((x) => ({ ...x }));
    const tiersBox = tierEditor(tiers, true, 'price');
    const vtiersBox = tierEditor(vtiers, false, 'price_per_second');
    const usd = (x) => h('div', { class: 'input-affix' }, h('span', { class: 'affix' }, '$'), x);
    const sectionTitle = (txt) => h('div', { class: 'form-section field-full' }, txt);
    const body = h('form', { class: 'form-grid form-grid-4', onsubmit: (e) => { e.preventDefault(); save(); } },
      field(t('models.id'), f.id, { required: true }),
      field(t('models.kind'), f.kind),
      field(t('models.context'), f.context_length),
      h('div', { class: 'field field-switch', title: t('models.disabledHint') }, enabled),
      h('div', { class: 'field-hint field-full model-enable-hint' }, icon('info', 13), ' ', t('models.disabledHint')),
      field(t('models.aliases'), aliases, { full: true }),
      sectionTitle(t('models.tokenPrices')),
      field(t('models.input'), usd(f.input_price)),
      field(t('models.cached'), usd(f.cached_input_price)),
      field(t('models.output'), usd(f.output_price)),
      field(t('models.imageInput'), usd(f.image_input_price)),
      sectionTitle(t('models.longCtxPrices')),
      field(t('models.threshold'), f.long_context_threshold, { hint: t('models.thresholdHint') }),
      field(t('models.input'), usd(f.long_input_price)),
      field(t('models.cached'), usd(f.long_cached_input_price)),
      field(t('models.output'), usd(f.long_output_price)),
      sectionTitle(t('models.mediaPrices')),
      field(t('models.imagePrice'), usd(f.image_price)),
      field(t('models.videoPrice'), usd(f.video_price_per_second)),
      field(t('models.inputImagePrice'), usd(f.input_image_price), { hint: t('models.inputImageHint') }),
      field(t('models.inputVideoPrice'), usd(f.input_video_price_per_second), { hint: t('models.inputVideoHint') }),
      field(t('models.imageTiers'), tiersBox, { full: true, hint: t('models.imageTiersHint') }),
      field(t('models.videoTiers'), vtiersBox, { full: true, hint: t('models.videoTiersHint') }),
      h('button', { type: 'submit', hidden: true }));
    const saveBtn = h('button', { class: 'btn btn-primary', onclick: () => save() }, h('span', null, edit ? t('common.save') : t('models.add')));
    const md = openModal({ title: edit ? t('models.editTitle', { id: m.id }) : t('models.addTitle'), subtitle: t('models.pricesPer1M'), size: 'lg', body, footer: [h('button', { class: 'btn', onclick: () => md.close() }, t('common.cancel')), saveBtn] });
    async function save() {
      const id = f.id.value.trim();
      if (!id) { f.id.focus(); f.id.classList.add('invalid'); return; }
      const n = (x) => (x.value === '' ? 0 : Number(x.value));
      const payload = { kind: f.kind.value, aliases: aliases.getValues(), enabled: enabled.input.checked, image_tiers: tiers.filter((x) => x.price != null && x.price !== ''), video_tiers: vtiers.filter((x) => x.price_per_second != null && x.price_per_second !== '') };
      for (const k of ['context_length', 'input_price', 'cached_input_price', 'image_input_price', 'output_price', 'long_context_threshold', 'long_input_price', 'long_cached_input_price', 'long_output_price', 'image_price', 'video_price_per_second', 'input_image_price', 'input_video_price_per_second']) payload[k] = n(f[k]);
      let body = payload;
      if (edit) {
        body = {};
        for (const [k, v] of Object.entries(payload)) {
          const old = m[k];
          if (Array.isArray(v)) { if (JSON.stringify(v) !== JSON.stringify(old || [])) body[k] = v; }
          else if (v !== (old ?? (typeof v === 'number' ? 0 : old))) body[k] = v;
        }
        if (!Object.keys(body).length) { md.close(); return; }
      } else body = { id, ...payload };
      await busy(saveBtn, async () => {
        try {
          if (edit) await patch(`/api/v1/models/${encodeURIComponent(m.id)}`, body); else await post('/api/v1/models', body);
          toast(t('common.saved'), 'success'); md.close(); load();
        } catch (e) { toastError(e); }
      });
    }
  }

  load();
}
