// Shared pieces of the upstream key / client token detail drawers (v0.4.0):
// drawer shell, URL-synced open state, 24h/7d/30d usage, recent failed requests,
// and the "name · prefix" credential label.
import { h, fmtInt, fmtUSD, fmtCompact, fmtDateTime, fmtTimeShort, statusClass } from './util.js';
import { icon } from './icons.js';
import { t } from './i18n.js';
import { get } from './api.js';
import { openModal, badge, spinner, errorMessage } from './ui.js';

export const USAGE_RANGES = ['24h', '7d', '30d'];

/** "name · prefix" label so same-named credentials can be told apart. */
export function cred(name, prefix, { link = false, title } = {}) {
  return h('span', { class: `cred ${link ? 'cred-link' : ''}`, title: title || [name, prefix].filter(Boolean).join(' · ') || null },
    h('span', { class: 'cred-name' }, name || '—'),
    prefix ? h('span', { class: 'cred-prefix' }, prefix) : null);
}
/** Plain-text form for <option> labels. */
export function credText(name, prefix) { return prefix ? `${name || '—'} · ${prefix}` : (name || '—'); }

/** Right-side drawer styled like the log detail drawer. */
export function openDetailDrawer({ onClose }) {
  const d = openModal({
    kind: 'drawer', size: 'lg', title: '',
    body: h('div', { class: 'drawer-loading' }, spinner(22), h('span', { class: 'muted' }, t('common.loading'))),
    onClose,
  });
  d.el.classList.add('detail-drawer');
  /** Header: title + badges on the first line, a "·"-separated meta line under it. */
  d.setHead = ({ title, badges = [], sub = [] }) => {
    const heading = d.el.querySelector('.modal-heading');
    heading.replaceChildren(
      h('div', { class: 'entity-head' }, h('span', { class: 'entity-title modal-title' }, title), ...badges.filter(Boolean)),
      sub.filter(Boolean).length ? h('div', { class: 'entity-sub' }, sub.filter(Boolean).flatMap((x, i) => (i ? [h('span', { 'aria-hidden': 'true' }, '·'), x] : [x]))) : '');
  };
  return d;
}

/**
 * Keeps a page's `open` URL parameter and its drawer in sync.
 * open(id, { push }) adds a history entry when opened by a click (so Back closes it);
 * Esc / × goes back over that entry, or replaces the URL without `open` after a reload.
 * build(id, drawer, entry) renders the drawer and returns a refresh function.
 */
export function drawerState({ ctx, getParams, build }) {
  let cur = null;
  const api = {
    get id() { return cur ? cur.id : null; },
    open(id, { push = false } = {}) {
      id = Number(id);
      if (!id) return;
      if (cur && cur.id === id) { if (cur.refresh) cur.refresh(); return; }
      if (cur) api.close({ silent: true });
      const entry = { id, pushed: push, drawer: null, refresh: null, silent: false, ctl: new AbortController() };
      cur = entry;
      ctx.setParams({ ...getParams(), open: id }, { push });
      if (push) history.replaceState({ xrDrawer: id }, '', location.hash);
      ctx.signal.addEventListener('abort', () => entry.ctl.abort(), { once: true });
      entry.drawer = openDetailDrawer({
        onClose: () => {
          entry.ctl.abort();
          if (cur !== entry) return;
          cur = null;
          if (entry.silent || !ctx.alive()) return;
          if (entry.pushed && history.state && history.state.xrDrawer === id) history.back();
          else ctx.setParams(getParams());
        },
      });
      entry.refresh = build(id, entry.drawer, entry.ctl.signal);
    },
    close({ silent = false } = {}) {
      if (!cur) return;
      cur.silent = silent;
      cur.drawer.close();
    },
    /** Apply the `open` value of a URL change (back / forward / link). */
    sync(openParam) {
      const id = Number(openParam) || null;
      if (id && (!cur || cur.id !== id)) api.open(id);
      else if (!id && cur) api.close({ silent: true });
    },
    refresh() { if (cur && cur.refresh) cur.refresh(); },
  };
  return api;
}

/** Titled block inside a drawer; `extra` sits at the right of the label (e.g. a link). */
export function section(title, body, extra) {
  return h('section', { class: 'entity-section' },
    h('div', { class: 'section-label' }, h('span', null, title), extra || null),
    body);
}

function loadingLine() { return h('div', { class: 'mini-empty' }, spinner(14), h('span', null, t('common.loading'))); }
function failLine(e) { return h('div', { class: 'mini-empty mini-error' }, icon('alert', 15), h('span', null, `${t('common.loadFailed')} · ${errorMessage(e)}`)); }

/**
 * 24h / 7d / 30d usage of one key or token from GET /api/v1/stats/breakdown?by=…&range=…
 * (the overview breakdown numbers; a key/token without a row there counts as 0).
 */
export function usageBlock(by, id, signal, onRow) {
  const box = h('div', { class: 'usage-grid' }, USAGE_RANGES.map((r) => h('div', { class: 'usage-col' },
    h('div', { class: 'usage-range' }, t(`kd.range.${r}`)), h('div', { class: 'usage-cost' }, h('span', { class: 'skeleton dd-skel' })))));
  Promise.all(USAGE_RANGES.map((range) => get('/api/v1/stats/breakdown', { range, by }, { signal })
    .then((res) => (res.data || []).find((r) => String(r.id) === String(id)) || {})))
    .then((rows) => {
      if (onRow) { const named = rows.find((r) => r.name); if (named) onRow(named); }
      box.replaceChildren(...rows.map((r, i) => h('div', { class: 'usage-col', 'data-range': USAGE_RANGES[i] },
        h('div', { class: 'usage-range' }, t(`kd.range.${USAGE_RANGES[i]}`)),
        h('div', { class: 'usage-cost', title: t('kd.cost') }, fmtUSD(r.cost_usd || 0)),
        h('dl', { class: 'usage-rows' },
          h('dt', null, t('kd.requests')), h('dd', { 'data-k': 'requests' }, fmtInt(r.requests || 0)),
          h('dt', null, t('kd.errors')), h('dd', { class: r.errors ? 'bad' : '', 'data-k': 'errors' }, fmtInt(r.errors || 0)),
          h('dt', null, t('kd.tokens')), h('dd', { title: fmtInt(r.total_tokens || 0), 'data-k': 'tokens' }, fmtCompact(r.total_tokens || 0))))));
    })
    .catch((e) => { if (e.name !== 'AbortError') box.replaceChildren(h('div', { class: 'dd-span' }, failLine(e))); });
  return box;
}

/**
 * Last 5 failed requests where `param` (key_id | token_id) = id; each row opens that log's
 * detail on the request log page.
 */
export function failuresBlock({ param, id, signal, navigate }) {
  const box = h('div', { class: 'mini-list' }, loadingLine());
  get('/api/v1/logs', { [param]: id, status: 'error', page_size: 5 }, { signal })
    .then((res) => {
      const rows = (res.data || []).slice(0, 5);
      if (!rows.length) { box.replaceChildren(h('div', { class: 'mini-empty' }, icon('checkCircle', 15), h('span', null, t('kd.noFailures')))); return; }
      box.replaceChildren(...rows.map((r) => h('button', {
        type: 'button', class: 'mini-row', title: t('kd.openLog'),
        onclick: () => navigate(`#/logs?${param}=${id}&status=error&range=all&open=${r.id}`),
      },
      badge(r.status ? String(r.status) : 'ERR', statusClass(r.status), { class: 'badge-status mono' }),
      h('span', { class: 'when', title: fmtDateTime(r.created_at) }, sameDay(r.created_at) ? fmtTimeShort(r.created_at) : fmtDateTime(r.created_at, { seconds: false }).slice(5)),
      h('span', { class: 'what' },
        h('span', { class: 'mono' }, r.path || '', r.model ? h('span', { class: 'muted' }, ` · ${r.model}`) : null),
        r.error ? h('span', { class: 'err', title: r.error }, r.error) : null),
      icon('chevronRight', 14))));
    })
    .catch((e) => { if (e.name !== 'AbortError') box.replaceChildren(failLine(e)); });
  return box;
}

function sameDay(iso) { const d = new Date(iso), n = new Date(); return d.toDateString() === n.toDateString(); }

/**
 * Drawer body for a key / token that no longer exists (soft-deleted): its history is kept, so
 * still show the name from the breakdown rows (marked "(deleted)"), usage and failures.
 */
export function goneBody(d, { by, param, id, signal, navigate, message }) {
  d.setHead({ title: `#${id}`, badges: [badge(t('kd.deleted'), 'neutral')] });
  const onRow = (r) => d.setHead({
    title: r.deleted ? `${r.name} ${t('kd.deletedMark')}` : r.name,
    badges: r.deleted ? [] : [badge(t('kd.deleted'), 'neutral')],
    sub: [r.hint ? h('span', { class: 'mono' }, by === 'token' ? `${r.hint}…` : r.hint) : null],
  });
  d.setBody(h('div', { class: 'entity-body' },
    h('div', { class: 'callout callout-warning' }, icon('alert', 16), h('span', null, message)),
    section(t('kd.usage'), usageBlock(by, id, signal, onRow)),
    section(t('kd.recentFailures'), failuresBlock({ param, id, signal, navigate }), allLogsLink(`#/logs?${param}=${id}`, navigate))));
}

/** "View all logs →" link for a section label. */
export function allLogsLink(hash, navigate) {
  return h('a', { href: hash, class: 'link', onclick: (e) => { e.preventDefault(); navigate(hash); } }, t('kd.allLogs'));
}
