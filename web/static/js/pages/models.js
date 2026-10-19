import { h, fmtInt, fmtPrice, fmtUSD, timeEl, relTime, fmtCompact, debounce } from '../util.js';
import { icon } from '../icons.js';
import { t } from '../i18n.js';
import { get, post, patch, del } from '../api.js';
import {
  toast, toastError, openModal, confirmDialog, menuButton, switchEl, searchInput, segmented,
  skeletonRows, tableEmptyRow, tableWrap, field, input, select, checkbox, button, busy, tagInput, tapToMenu, errorMessage,
} from '../ui.js';

const KINDS = ['language', 'image', 'video', 'embedding'];

export function render(el, ctx) {
  const st = { kind: KINDS.includes(ctx.params.kind) ? ctx.params.kind : '', q: ctx.params.q || '', data: [], lastSync: null };
  ctx.setActions(
    button(t('models.sync'), { icon: 'refresh', onClick: openSync }),
    button(t('models.add'), { icon: 'plus', variant: 'primary', onClick: () => openForm() }),
  );

  el.classList.add('pg-models');
  const kindBox = h('div', { class: 'models-kinds' });
  const search = searchInput({ value: st.q, placeholder: t('models.searchPh'), width: '200px', onInput: debounce((v) => { st.q = v.trim().toLowerCase(); ctx.setParams({ kind: st.kind, q: st.q }); draw(); }, 150) });
  const syncInfo = h('div', { class: 'models-meta' });
  const COLS = 9;
  let selectedId = null;
  const tbody = h('tbody', null, skeletonRows(COLS));
  const th = (text, cls = '', tip = '') => h('th', { class: cls, title: tip || null }, text);
  const table = h('table', { class: 'table table-models table-cards' },
    h('thead', null, h('tr', null,
      th(t('common.model')), th(t('models.kind'), 'col-kind'),
      th(t('models.thInput'), 'num'), th(t('models.thCached'), 'num'), th(t('models.thOutput'), 'num'),
      th(t('models.thLongTiers'), 'col-long', t('models.longCtxHint')),
      th(t('models.source'), 'col-src', t('models.sourceTip')), th(t('models.thEnabled'), 'col-enabled', t('models.disabledHint')),
      h('th', { class: 'col-actions' }, h('span', { class: 'sr-only' }, t('common.actions'))))),
    tbody);
  el.append(h('div', { class: 'models-bar' }, syncInfo, h('div', { class: 'toolbar-spacer' }), search, kindBox), tableWrap(table));

  let syncHours = null;
  const metaItem = (label, ...value) => h('span', { class: 'models-meta-item' }, h('span', { class: 'l' }, label), h('span', { class: 'v' }, ...value));
  function renderSyncInfo() {
    const s = st.lastSync;
    const manual = st.data.filter((m) => m.source === 'manual').length;
    const items = [];
    if (!s) items.push(metaItem(t('models.lastSync'), t('models.neverSynced')));
    else if (s.ok) items.push(metaItem(t('models.lastSync'), timeEl(s.at), t('models.syncOkSuffix')));
    else items.push(h('span', { class: 'models-meta-item', title: s.error || '' }, h('span', { class: 'l' }, t('models.lastSync')), h('span', { class: 'v status-dot is-danger' }, h('span', null, timeEl(s.at), t('models.syncFailSuffix')))));
    if (syncHours != null) items.push(metaItem(t('models.autoSync'), syncHours > 0 ? t('models.everyNHours', { n: fmtInt(syncHours) }) : t('models.manualOnly')));
    if (manual) items.push(metaItem(t('models.manualEdits'), t('models.manualN', { n: fmtInt(manual) })));
    syncInfo.replaceChildren(...items);
  }
  function renderKinds() {
    const count = (k) => (k ? st.data.filter((m) => m.kind === k).length : st.data.length);
    const kinds = KINDS.filter((k) => count(k) > 0 || k === st.kind);
    kindBox.replaceChildren(segmented({
      options: [{ value: '', label: [t('common.all'), h('span', { class: 'seg-count' }, String(count('')))] },
        ...kinds.map((k) => ({ value: k, label: [t(`kind.${k}`), h('span', { class: 'seg-count' }, String(count(k)))] }))],
      value: st.kind,
      onChange: (v) => { st.kind = v; ctx.setParams({ kind: v, q: st.q }); draw(); },
    }));
  }

  const range = (vals, unit) => {
    const v = vals.filter((x) => x > 0);
    if (!v.length) return null;
    const lo = Math.min(...v), hi = Math.max(...v);
    return `${lo === hi ? fmtUSD(lo) : `${fmtUSD(lo)}–${fmtUSD(hi)}`} / ${unit}`;
  };
  const dash = () => h('td', { class: 'num muted m-hide' }, '—');
  const priceTd = (v, label) => (v > 0 ? h('td', { class: 'num mono m-meta', 'data-label': label }, fmtPrice(v)) : dash());
  // Long-context rule or the media tiers, as one short line.
  function longOrTiers(m) {
    if (m.kind === 'image' || m.kind === 'video') {
      const tiers = (m.kind === 'image' ? m.image_tiers : m.video_tiers) || [];
      if (!tiers.length) return h('td', { class: 'muted m-hide' }, '—');
      const quality = tiers.some((x) => x.quality);
      const res = [...new Set(tiers.map((x) => x.resolution))];
      return h('td', { class: 'm-meta nowrap', 'data-label': t('models.thLongTiers') }, quality ? t('models.tiersQR', { n: tiers.length }) : res.join(t('models.listSep')));
    }
    if (!(m.long_context_threshold > 0)) return h('td', { class: 'muted m-hide' }, '—');
    const n = fmtCompact(m.long_context_threshold);
    const ratio = (a, b) => (b > 0 ? Math.round((a / b) * 100) / 100 : 0);
    const ri = ratio(m.long_input_price, m.input_price), ro = ratio(m.long_output_price, m.output_price);
    const text = ri > 0 && ri === ro ? t('models.longX', { n, x: ri }) : t('models.longPrices', { n, i: fmtPrice(m.long_input_price), o: fmtPrice(m.long_output_price) });
    const tip = `${t('models.longTip', { n })}\n${t('models.input')}: ${fmtPrice(m.long_input_price)}\n${t('models.cached')}: ${fmtPrice(m.long_cached_input_price)}\n${t('models.output')}: ${fmtPrice(m.long_output_price)}`;
    return h('td', { class: 'nowrap m-meta', 'data-label': t('models.longShort'), title: tip }, text);
  }

  function row(m) {
    const media = m.kind === 'image' || m.kind === 'video';
    const disabled = m.enabled === false;
    const menu = menuButton(() => [
      media ? { label: t('models.editTiers'), icon: 'layers', onClick: () => openTiers(m) } : null,
      { label: t('common.edit'), icon: 'edit', onClick: () => openForm(m) },
      { label: t('models.viewLogs'), icon: 'logs', onClick: () => ctx.navigate(`#/logs?model=${encodeURIComponent(m.id)}`) },
      '-',
      { label: t('common.delete'), icon: 'trash', danger: true, onClick: () => remove(m) },
    ].filter(Boolean));
    const unitImg = t('models.perImageU'), unitSec = t('models.perSecU');
    const vt = m.video_tiers || [];
    const mediaPrice = m.kind === 'image' ? range([m.image_price, ...(m.image_tiers || []).map((x) => x.price)], unitImg)
      : m.kind === 'video' ? range(vt.length ? vt.map((x) => x.price_per_second) : [m.video_price_per_second], unitSec) : null;
    const aliases = m.aliases || [];
    const nameTip = [m.id, aliases.length ? `${t('models.aliases')}: ${aliases.join(', ')}` : '', m.context_length ? `${t('models.context')}: ${fmtCompact(m.context_length)}` : ''].filter(Boolean).join('\n');
    let tr;
    const toggle = switchEl({
      checked: !disabled, title: t('models.disabledHint'),
      onChange: (on) => patch(`/api/v1/models/${encodeURIComponent(m.id)}`, { enabled: on }).then(() => {
        toast(on ? t('models.enabledToast', { id: m.id }) : t('models.disabledToast', { id: m.id }), 'success');
        m.enabled = on; tr.classList.toggle('row-muted', !on);
      }, (e) => { toastError(e); return false; }),
    });
    tr = h('tr', { class: `${disabled ? 'row-muted' : ''} ${selectedId === m.id ? 'is-selected' : ''}`, 'data-id': m.id },
      h('td', { class: 'cell-model m-title' },
        h('span', { class: 'mono model-name', title: nameTip }, m.id),
        aliases.length ? h('span', { class: 'model-alias', title: aliases.join('\n') }, t('models.aliasesN', { n: aliases.length })) : null),
      h('td', { class: 'col-kind m-hide' }, t(`kind.${m.kind}`) || m.kind),
      media ? h('td', { class: 'num mono m-meta nowrap', 'data-label': t('models.price') }, mediaPrice || '—') : priceTd(m.input_price, t('models.input')),
      media ? dash() : priceTd(m.cached_input_price, t('models.cached')),
      media ? dash() : priceTd(m.output_price, t('models.output')),
      longOrTiers(m),
      h('td', { class: 'col-src m-badge' }, h('span', { class: `src-tag src-${m.source || 'builtin'}`, title: m.synced_at ? `${t('models.synced')}: ${relTime(m.synced_at)}` : null }, t(`source.${m.source}`) || m.source || '—')),
      h('td', { class: 'col-enabled' }, toggle),
      h('td', { class: 'col-actions m-menu' }, h('div', { class: 'row-actions' }, menu)));
    if (media) tr.addEventListener('dblclick', () => openTiers(m));
    return tapToMenu(tr, menu);
  }

  function draw() {
    renderKinds();
    let rows = st.data;
    if (st.kind) rows = rows.filter((m) => m.kind === st.kind);
    if (st.q) rows = rows.filter((m) => m.id.toLowerCase().includes(st.q) || (m.aliases || []).some((a) => a.toLowerCase().includes(st.q)));
    if (!rows.length) {
      tbody.replaceChildren(tableEmptyRow(COLS, st.data.length ? { icon: 'search', title: t('common.noMatches') } : { icon: 'cpu', title: t('models.empty'), desc: t('models.emptyDesc'), action: button(t('models.sync'), { icon: 'refresh', variant: 'primary', onClick: openSync }) }));
      return;
    }
    const order = (k) => { const i = KINDS.indexOf(k); return i < 0 ? KINDS.length : i; };
    rows = rows.slice().sort((a, b) => order(a.kind) - order(b.kind) || a.id.localeCompare(b.id));
    tbody.replaceChildren(...rows.map(row));
  }

  async function load() {
    try {
      const [res, set] = await Promise.all([
        get('/api/v1/models', null, { signal: ctx.signal }),
        syncHours == null ? get('/api/v1/settings', null, { signal: ctx.signal }).catch(() => null) : null,
      ]);
      if (set && set.model_sync_interval_hours != null) syncHours = set.model_sync_interval_hours;
      st.data = (res.data || []).slice();
      st.lastSync = res.last_sync || null;
      renderSyncInfo();
      draw();
    } catch (e) {
      if (e.name === 'AbortError') return;
      toastError(e);
      tbody.replaceChildren(tableEmptyRow(COLS, { icon: 'alert', title: t('common.loadFailed'), desc: errorMessage(e) }));
    }
  }

  // Resolution tier editor (frame 125-12826): a drawer per image or video model.
  function openTiers(m) {
    const video = m.kind === 'video';
    const priceKey = video ? 'price_per_second' : 'price';
    const unit = video ? t('models.perSecU') : t('models.perImageU');
    const norm = (r) => { const x = String(r || '').trim().toLowerCase(); return video && /^\d+$/.test(x) ? `${x}p` : x; };
    const isDefault = (r) => norm(r) === (video ? '480p' : '1k');
    const rows = ((video ? m.video_tiers : m.image_tiers) || []).map((x) => ({ ...x }));
    const withQuality = !video && rows.some((x) => x.quality);
    const money = (value, onInput, u, attrs = {}) => h('div', { class: 'input-affix tier-money' },
      h('span', { class: 'affix' }, '$'),
      input({ type: 'number', step: 'any', min: 0, value: value ?? '', class: 'input mono', oninput: (e) => onInput(e.target.value === '' ? null : Number(e.target.value)), ...attrs }),
      h('span', { class: 'tier-unit' }, `/ ${u}`));
    const defTag = (r) => (isDefault(r) ? h('span', { class: 'tag-outline' }, t('models.defaultTier')) : '');
    const list = h('div', { class: 'tier-list' });
    const note = h('div', { class: 'tier-note' });
    function paintNote() {
      const valid = rows.filter((x) => String(x.resolution || '').trim() && x[priceKey] != null);
      const top = valid.slice().sort((a, b) => b[priceKey] - a[priceKey])[0];
      note.replaceChildren(icon('info', 14), h('span', null, t('models.tierRule', { d: video ? '480p' : '1k', top: top ? top.resolution : '—' })));
    }
    function paint() {
      list.replaceChildren(...rows.map((tier, i) => {
        const def = h('span', { class: 'tier-def' }, defTag(tier.resolution));
        return h('div', { class: `tier-line ${withQuality ? 'has-q' : ''}` },
          withQuality ? input({ value: tier.quality || '', class: 'input tier-q', placeholder: t('models.qualityOpt'), 'aria-label': t('models.quality'), oninput: (e) => { tier.quality = e.target.value; } }) : null,
          input({ value: tier.resolution || '', class: 'input mono tier-res', placeholder: video ? '720p' : '2k', 'aria-label': t('models.resolution'), oninput: (e) => { tier.resolution = e.target.value; def.replaceChildren(defTag(tier.resolution)); paintNote(); } }),
          money(tier[priceKey], (v) => { tier[priceKey] = v; paintNote(); }, unit, { 'aria-label': t('models.price') }),
          def,
          h('button', { type: 'button', class: 'btn btn-ghost btn-icon btn-sm', 'aria-label': t('common.remove'), title: t('common.remove'), onclick: () => { rows.splice(i, 1); paint(); } }, icon('trash', 14)));
      }),
      h('button', { type: 'button', class: 'btn btn-ghost btn-sm tier-add', onclick: () => { rows.push(video ? { resolution: '', price_per_second: null } : { quality: '', resolution: '', price: null }); paint(); const r = list.querySelectorAll('.tier-res'); r[r.length - 1]?.focus(); } }, icon('plus', 14), h('span', null, t('models.addTier'))));
      paintNote();
    }
    paint();
    const media = { input_image_price: m.input_image_price || 0, input_video_price_per_second: m.input_video_price_per_second || 0 };
    const inLine = (label, v, set, u) => h('div', { class: 'tier-line tier-line-in' }, h('span', { class: 'tier-in-l' }, label), money(v || null, (x) => set(x ?? 0), u, { placeholder: '0' }), h('span', { class: 'tier-def' }), h('span'));
    const inputs = [inLine(t('models.inImage'), media.input_image_price, (x) => { media.input_image_price = x; }, t('models.perImageU'))];
    if (video) inputs.push(inLine(t('models.inVideo'), media.input_video_price_per_second, (x) => { media.input_video_price_per_second = x; }, t('models.perSecU')));
    const saveBtn = h('button', { class: 'btn btn-primary', type: 'button', onclick: () => save() }, h('span', null, t('common.save')));
    const mark = (on) => tbody.querySelectorAll('tr[data-id]').forEach((x) => x.classList.toggle('is-selected', on && x.dataset.id === m.id));
    selectedId = m.id; mark(true);
    const src = m.source === 'manual' ? h('span', { class: 'src-tag src-manual' }, t('models.manualKept')) : h('span', { class: `src-tag src-${m.source || 'builtin'}` }, t(`source.${m.source}`) || '');
    const dr = openModal({
      kind: 'drawer', size: 'md',
      title: h('span', { class: 'tier-title' }, h('span', { class: 'mono' }, m.id), src),
      subtitle: video ? t('models.tierSubVideo') : t('models.tierSubImage'),
      body: h('div', { class: 'tier-editor' },
        h('div', { class: 'tier-sec' }, t('models.tiersSection')), list,
        h('div', { class: 'tier-sec' }, t('models.inputMedia')), ...inputs,
        note),
      footer: [h('button', { class: 'btn', type: 'button', onclick: () => dr.close() }, t('common.cancel')), saveBtn],
      onClose: () => { selectedId = null; mark(false); },
    });
    dr.el.classList.add('tier-drawer');
    async function save() {
      const clean = rows.filter((x) => String(x.resolution || '').trim() && x[priceKey] != null).map((x) => ({ ...x, resolution: String(x.resolution).trim() }));
      const body = { [video ? 'video_tiers' : 'image_tiers']: clean, input_image_price: media.input_image_price };
      if (video) body.input_video_price_per_second = media.input_video_price_per_second;
      await busy(saveBtn, async () => {
        try { await patch(`/api/v1/models/${encodeURIComponent(m.id)}`, body); toast(t('common.saved'), 'success'); dr.close(); load(); } catch (e) { toastError(e); }
      });
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
