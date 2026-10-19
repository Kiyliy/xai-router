// Shared pieces of the upstream key / client token detail drawers (v0.4.0):
// drawer shell, URL-synced open state, 24h/7d/30d usage, recent failed requests,
// and the "name · prefix" credential label.
import { h, fmtInt, fmtUSD, fmtCompact, fmtDateTime, fmtTimeShort, fmtMs, relTime } from './util.js';
import { icon } from './icons.js';
import { t, hasT } from './i18n.js';
import { get } from './api.js';
import { openModal, badge, statusCode, spinner, errorMessage } from './ui.js';
import { eventSentence, levelDot, actionLabel, actorLabel, upstreamWhy, fieldLabel, fmtChangeValue, requestReason } from './events.js';

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
  /**
   * Header: title + badges on the first line, at most one identifier (mask or prefix) under it
   * (pm-spec 10.8 C55: everything else lives in the drawer's configuration / summary block).
   * `aside` sits left of the close button (e.g. the token's "已启用" switch).
   */
  d.setHead = ({ title, badges = [], sub, aside } = {}) => {
    const heading = d.el.querySelector('.modal-heading');
    const one = Array.isArray(sub) ? sub.filter(Boolean)[0] : sub;
    heading.replaceChildren(
      h('div', { class: 'entity-head' }, h('span', { class: 'entity-title modal-title' }, title), ...badges.filter(Boolean)),
      one ? h('div', { class: 'entity-sub' }, one) : '');
    const acts = d.el.querySelector('.modal-header-actions');
    const old = acts.querySelector('.entity-aside');
    if (old) old.remove();
    if (aside) acts.prepend(h('div', { class: 'entity-aside' }, aside));
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
export function usageBlock(by, id, signal, onRow, { joined = false } = {}) {
  const box = h('div', { class: `usage-grid ${joined ? 'usage-joined' : ''}` }, USAGE_RANGES.map((r) => h('div', { class: 'usage-col' },
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
      box.replaceChildren(...rows.map((r) => {
        const why = requestReason(r);
        const sub = [r.model, why && why.text].filter(Boolean).join(t('kd.listSep'));
        return h('button', {
          type: 'button', class: 'mini-row kd-fail', title: t('kd.openLog'),
          onclick: () => navigate(`#/logs?${param}=${id}&status=error&range=all&open=${r.id}`),
        },
        statusCode(r.status),
        h('span', { class: 'when mono', title: fmtDateTime(r.created_at) }, failTime(r.created_at)),
        h('span', { class: 'what' },
          h('span', { class: 'mono' }, r.method ? `${r.method} ` : '', r.path || ''),
          sub ? h('span', { class: 'err', title: (why && why.raw) || sub }, sub) : null),
        icon('chevronRight', 14));
      }));
    })
    .catch((e) => { if (e.name !== 'AbortError') box.replaceChildren(failLine(e)); });
  return box;
}


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

/**
 * Short time for list rows (pm-spec 10.3): "刚刚" / "N 分钟前" / "N 小时前" within 24 hours,
 * otherwise "10月18日 14:02" (year added when it differs).
 */
export function listTime(iso) {
  const d = new Date(iso);
  if (isNaN(d)) return '—';
  return d.getTime() <= Date.now() + 5000 ? relTime(d) : fmtDateTime(d, { seconds: false });
}

/**
 * Last 5 operation / system records about an upstream key (auto-disable, edits, checks …), one
 * sentence each; a row opens that record on the logs page.
 */
export function recentEventsBlock({ keyId, signal, navigate }) {
  const box = h('div', { class: 'mini-list ev-mini' }, loadingLine());
  const base = `#/logs?type=audit,system&key_id=${keyId}&range=all`;
  get('/api/v1/logs', { type: 'audit,system', key_id: keyId, page_size: 5 }, { signal })
    .then((res) => {
      const rows = (res.data || []).slice(0, 5);
      if (!rows.length) { box.replaceChildren(h('div', { class: 'mini-empty' }, icon('checkCircle', 15), h('span', null, t('kd.noEvents')))); return; }
      box.replaceChildren(...rows.map((e) => {
        const line = keyEventLine(e);
        return h('button', { type: 'button', class: 'mini-row kd-ev', title: `${eventSentence(e)}\n${t('kd.openEvent')}`, onclick: () => navigate(`${base}&event=${e.id}`) },
          levelDot(e.level),
          h('span', { class: `type-tag-pill type-${e.type}` }, t(`lg.type.${e.type}`)),
          h('span', { class: 'kd-ev-act' }, keyEventLabel(e.action)),
          h('span', { class: 'what' }, h('span', { class: 'ev-line' }, line)),
          h('span', { class: 'when', title: fmtDateTime(e.created_at) }, listTime(e.created_at)));
      }));
    })
    .catch((e) => { if (e.name !== 'AbortError') box.replaceChildren(failLine(e)); });
  return box;
}

/** Within 24 hours "12:20:03", older "10月16日 10:02". */
function failTime(iso) {
  const d = new Date(iso);
  if (isNaN(d)) return '—';
  return Date.now() - d.getTime() < 86400e3 ? fmtTimeShort(d) : fmtDateTime(d, { seconds: false });
}

/** Short action name for the key drawer ("自动禁用", "编辑", "开始冷却" …). */
export function keyEventLabel(action) {
  const k = `kd.ev.${action}`;
  return hasT(k) ? t(k) : actionLabel(action);
}

/**
 * The event without the key's name, which the drawer already shows:
 * "xAI 返回 401（密钥无效或被封禁），已自动禁用"; "权重 20 → 10（管理员）"; "被 xAI 限流，冷却 60 秒".
 */
export function keyEventLine(e) {
  const d = e.detail || {};
  const who = e.type === 'audit' ? t('kd.byWho', { who: e.actor === 'password' ? t('kd.admin') : actorLabel(e) }) : '';
  switch (e.action) {
    case 'key.auto_disable': return t('kd.evAutoDisable', { why: upstreamWhy(d.reason) });
    case 'key.cooldown': return /429|rate/i.test(d.reason || '') ? t('kd.evRate', { n: d.seconds ?? '—' }) : t('kd.evPause', { why: upstreamWhy(d.reason), n: d.seconds ?? '—' });
    case 'key.update': {
      const ch = d.changes && typeof d.changes === 'object' ? Object.entries(d.changes) : [];
      if (!ch.length) break;
      const parts = ch.slice(0, 3).map(([f, c]) => `${fieldLabel(e.target_type, f)} ${fmtChangeValue(c && c.before)} → ${fmtChangeValue(c && c.after)}`);
      return parts.join(t('kd.listSep')) + who;
    }
    case 'key.test': {
      const res = d.result && typeof d.result === 'object' ? d.result : {};
      if (res.ok) return (res.latency_ms != null ? t('kd.evTestOkMs', { ms: fmtMs(res.latency_ms) }) : t('kd.evTestOk')) + who;
      if (res.status) return t('kd.evTestBad', { r: hasT(`test.${res.status}`) ? t(`test.${res.status}`) : res.status }) + who;
      break;
    }
    default: break;
  }
  return eventSentence(e);
}

/** The newest auto-disable record of a key (time, reason, the request that caused it), or null. */
export async function lastAutoDisable(keyId, signal) {
  const res = await get('/api/v1/logs', { type: 'system', key_id: keyId, q: 'key.auto_disable', page_size: 5 }, { signal });
  return (res.data || []).find((e) => e.action === 'key.auto_disable') || null;
}

/** "View all →" for the key drawer's recent events: the mixed operation / system list for this key. */
export function allEventsLink(keyId, navigate) {
  const hash = `#/logs?type=audit,system&key_id=${keyId}&range=all`;
  return h('a', { href: hash, class: 'link', onclick: (e) => { e.preventDefault(); navigate(hash); } }, t('kd.allEvents'));
}
