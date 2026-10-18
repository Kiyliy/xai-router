// Shared formatting for the unified log (v0.4.1, pm-spec 9 and 10): audit / system event labels
// and one-sentence descriptions, readable request error reasons, type / level tags. Used by the
// logs page, the key drawer's "recent events" and the overview's "events needing attention".
import { h, fmtUSD, fmtBytes, fmtInt, fmtMs, fmtDateTime, readableReason } from './util.js';
import { t, hasT } from './i18n.js';
import { badge } from './ui.js';

const TICKS = 1e10;
/** Amount from ticks; 0 → "$0" (pm-spec 10.3). */
export const usdT = (ticks) => fmtUSD((Number(ticks) || 0) / TICKS);
/** Signed change "+$5.00" / "−$1.20" (full-width minus). */
export const usdDelta = (ticks) => { const v = Number(ticks) || 0; return `${v < 0 ? '−' : '+'}${usdT(Math.abs(v))}`; };

/** "token.quota_increase" → "增加额度"; unknown actions show as-is. */
export function actionLabel(action) {
  if (!action) return '—';
  const k = `ev.a.${action}`;
  return hasT(k) ? t(k) : action;
}
export function targetTypeLabel(tt) {
  const k = `ev.tt.${tt || 'none'}`;
  return hasT(k) ? t(k) : tt;
}
/** Who did it: password session, admin API token (by name), system, unauthenticated. */
export function actorLabel(e) {
  const a = e.actor || '';
  if (a === 'system') return t('ev.actor.system');
  if (a === 'password') return t('ev.actor.password');
  if (a === 'anonymous' || !a) return t('ev.actor.anonymous');
  if (a.startsWith('admin_token:')) return `${t('ev.actor.adminToken')} · ${e.actor_name || '#' + a.slice(12)}`;
  return e.actor_name || a;
}

export const LEVEL_VARIANT = { info: 'info', warn: 'warning', error: 'danger' };
export function levelDot(level) {
  const lv = level || 'info';
  return h('span', { class: `lv lv-${lv}`, title: t(`ev.level.${lv}`) }, h('span', { class: `dot dot-${LEVEL_VARIANT[lv] || 'neutral'}` }));
}
export function levelBadge(level) {
  const lv = level || 'info';
  return badge(t(`ev.level.${lv}`), LEVEL_VARIANT[lv] || 'neutral', { class: 'badge-dot' });
}
/** "请求" / "操作" / "系统" tag with the level dot. */
export function typeTag(type, level) {
  return h('span', { class: `type-tag type-${type}`, title: t(`ev.level.${level || 'info'}`) }, levelDot(level), h('span', null, t(`lg.type.${type}`)));
}

function fmtVal(v) {
  if (v == null || v === '') return t('ev.empty');
  if (Array.isArray(v)) return v.length ? v.join('、') : t('ev.empty');
  if (typeof v === 'object') return JSON.stringify(v);
  if (typeof v === 'boolean') return v ? t('ev.yes') : t('ev.no');
  return String(v);
}
export { fmtVal as fmtChangeValue };

/** Field label for a changed setting / object field. */
export function fieldLabel(type, f) {
  if (type === 'settings' && hasT(`settings.f.${f}`)) return t(`settings.f.${f}`);
  if (hasT(`ev.f.${f}`)) return t(`ev.f.${f}`);
  return f;
}

/** Object label: name (or #id), or the object type for singletons (settings, disk …). */
export function targetText(e) {
  if (!e.target_type) return targetTypeLabel('');
  if (['settings', 'logs', 'database', 'disk'].includes(e.target_type)) return targetTypeLabel(e.target_type);
  if (e.target_type === 'model') return e.target_name || e.target_id || targetTypeLabel('model');
  return e.target_name || (e.target_id ? `#${e.target_id}` : targetTypeLabel(e.target_type));
}
export function targetHint(e) {
  if (!e.target_hint) return '';
  return e.target_type === 'token' ? `${e.target_hint}…` : e.target_hint;
}

/** Why an upstream key was disabled / paused, in plain words ("xAI 返回 401（密钥无效或被封禁）"). */
export function upstreamWhy(reason) {
  const raw = String(reason || '');
  const m = /HTTP (\d{3})/.exec(raw);
  if (m) {
    const c = Number(m[1]);
    const k = c === 401 || c === 403 ? 'ev.why.auth' : c === 429 ? 'ev.why.rate' : c === 402 ? 'ev.why.credit' : c >= 500 ? 'ev.why.server' : 'ev.why.other';
    return t(k, { c });
  }
  if (/timeout|timed out/i.test(raw)) return t('ev.why.timeout');
  if (/blocked|disabled|credit|balance/i.test(raw)) return t('ev.why.blocked');
  return readableReason(raw).short || t('ev.why.unknown');
}

function changesText(e) {
  const ch = (e.detail || {}).changes;
  if (!ch || typeof ch !== 'object') return '';
  const parts = Object.entries(ch).map(([f, c]) => `${fieldLabel(e.target_type, f)} ${fmtVal(c && c.before)} → ${fmtVal(c && c.after)}`);
  return parts.slice(0, 3).join('，') + (parts.length > 3 ? t('ev.moreFields', { n: parts.length }) : '');
}

const LOGIN_REASONS = { 'wrong password': 'ev.login.wrong', 'empty password': 'ev.login.empty', 'locked out after too many failed attempts': 'ev.login.locked' };
const QUOTA_REASON_CODES = { pre_v0_4_1: 'ev.q.codePre', reset_folded: 'ev.q.codeReset' };
function quotaReason(d) {
  if (d.reason_code && QUOTA_REASON_CODES[d.reason_code]) return t(QUOTA_REASON_CODES[d.reason_code]);
  return d.reason || '';
}

function quotaSentence(e, name) {
  const d = e.detail || {};
  const act = (e.action || '').replace('token.quota_', '');
  const r = quotaReason(d);
  const why = r ? t('ev.q.reason', { r }) : '';
  const p = { name, a: usdT(d.before_ticks), b: usdT(d.after_ticks), d: usdDelta(d.amount_ticks), used: usdT(d.used_ticks), left: usdT((d.after_ticks || 0) - (d.used_ticks || 0)) };
  switch (act) {
    case 'initial': return d.unlimited ? t('ev.q.initialUnlimited', p) : t('ev.q.initial', p);
    case 'increase': return t('ev.q.increase', p) + why;
    case 'decrease': return t('ev.q.decrease', p) + why;
    case 'set_limited': return t('ev.q.setLimited', p) + why;
    case 'set_unlimited': return t('ev.q.setUnlimited', p) + why;
    case 'migrate': return (d.unlimited ? t('ev.q.migrateUnlimited', p) : t('ev.q.migrate', p)) + why;
    default: return actionLabel(e.action) + why;
  }
}

/**
 * One sentence per event: what happened + why + the effect (pm-spec 10.4), e.g.
 * "上游密钥 prod-07 已自动禁用：xAI 返回 401（密钥无效或被封禁）。请检查后恢复或删除。"
 */
export function eventSentence(e) {
  const d = e.detail || {};
  const a = e.action || '';
  const name = targetText(e);
  const tt = targetTypeLabel(e.target_type);
  const res = d.result && typeof d.result === 'object' ? d.result : {};
  if (a.startsWith('token.quota_')) return quotaSentence(e, name);
  switch (a) {
    case 'key.auto_disable': return t('ev.s.autoDisable', { name, why: upstreamWhy(d.reason) });
    case 'key.cooldown': return /429|rate/i.test(d.reason || '') ? t('ev.s.cooldownRate', { name, n: d.seconds ?? '—' }) : t('ev.s.cooldown', { name, n: d.seconds ?? '—', why: upstreamWhy(d.reason) });
    case 'key.circuit_open': { const m = /after (\d+) consecutive/.exec(d.reason || ''); return t('ev.s.circuit', { name, k: m ? m[1] : '—', n: d.seconds ?? '—' }); }
    case 'model.sync':
      if (res.ok === false || d.error) return t(e.type === 'system' ? 'ev.s.syncFailAuto' : 'ev.s.syncFail', { why: readableReason(res.error || d.error || '').short });
      return t(e.type === 'system' ? 'ev.s.syncAuto' : 'ev.s.sync', { a: (res.added || []).length, u: (res.updated || []).length });
    case 'disk.low': return t('ev.s.disk', { p: Math.floor(d.used_percent || 0), f: fmtBytes(d.free_bytes || 0) });
    case 'logs.retention_start': return t('ev.s.retStart', { n: d.retention_days ?? '—' });
    case 'logs.retention_end': return d.error ? t('ev.s.retFail', { why: d.error }) : t('ev.s.retEnd', { n: fmtInt(d.deleted ?? 0), d: d.retention_days ?? '—', ms: fmtMs(d.duration_ms || 0) });
    case 'logs.purge': {
      const before = d.request && d.request.before ? fmtDateTime(d.request.before, { seconds: false }) : '—';
      return d.error ? t('ev.s.purgeFail', { why: d.error }) : t('ev.s.purge', { before, n: fmtInt(res.deleted ?? 0), f: fmtBytes(res.freed_bytes || 0) });
    }
    case 'system.compact': return d.error ? t('ev.s.compactFail', { why: d.error }) : t('ev.s.compact', { f: fmtBytes(res.freed_bytes || 0) });
    case 'system.start': return t('ev.s.start', { v: d.version || '—', s: d.schema_version ?? '—' });
    case 'system.migrate': return t('ev.s.migrate', { a: d.from_version ?? '—', b: d.to_version ?? '—' });
    case 'auth.login': return t('ev.s.login');
    case 'auth.login_failed': return t('ev.s.loginFailed', { why: LOGIN_REASONS[d.reason] ? t(LOGIN_REASONS[d.reason]) : (d.reason || '—') });
    case 'auth.logout': return t('ev.s.logout');
    case 'auth.password_change': return t('ev.s.password') + (d.request && d.request.revoke_admin_tokens ? t('ev.s.passwordRevoked') : '');
    case 'key.import': return t('ev.s.import', { a: res.created ?? 0, s: res.skipped ?? 0 });
    case 'key.test': return t('ev.s.test', { name, r: res.ok ? t('ev.testOk') : (hasT(`test.${res.status}`) ? t(`test.${res.status}`) : res.status || '—') });
    case 'key.test_many': return d.error ? t('ev.s.testManyFail', { why: d.error }) : t('ev.s.testMany', { n: res.total ?? 0, ok: (res.by_status || {}).ok ?? 0 });
    case 'token.regenerate': return t('ev.s.regenerate', { name });
    case 'settings.update': return t('ev.s.settings', { c: changesText(e) || '—' });
    default: break;
  }
  const verb = a.slice(a.lastIndexOf('.') + 1);
  if (verb === 'update') { const c = changesText(e); return c ? t('ev.s.updateWith', { tt, name, c }) : t('ev.s.update', { tt, name }); }
  if (['create', 'enable', 'disable', 'delete', 'restore', 'revoke'].includes(verb)) return t(`ev.s.${verb}`, { tt, name });
  return `${actionLabel(a)}${e.target_type ? ` · ${name}` : ''}`;
}

// ---------- readable request error reasons (pm-spec 9.5) ----------
/**
 * { text, raw, key, link?: { label, hash } } for a request row; null for a plain success.
 * The raw error text stays visible in the detail drawer.
 */
export function requestReason(r) {
  const raw = r.error || '';
  const s = r.status || 0;
  const k = (key, link) => ({ text: t(`rr.${key}`), raw, link, key });
  if (/no upstream API keys are available|no_upstream_keys/i.test(raw)) return k('noKeys', { label: t('rr.noKeysLink'), hash: '#/keys' });
  if (s === 429 && /quota/i.test(raw) && !/^HTTP/.test(raw)) return k('quota', r.token_id ? { label: t('rr.quotaLink'), hash: `#/tokens?open=${r.token_id}&adjust=1` } : null);
  if (/not allowed to use model/i.test(raw)) return k('modelNotAllowed');
  if (/is disabled on this gateway/i.test(raw)) return k('modelDisabled');
  if (/has no known price/i.test(raw)) return k('modelNotPriced');
  if (s === 401 && /missing API key|invalid API key/i.test(raw)) return k('tokenInvalid');
  if (/this API key is disabled/i.test(raw)) return k('tokenDisabled');
  if (/this API key has expired/i.test(raw)) return k('tokenExpired');
  if (/model .* does not exist/i.test(raw)) return k('modelNotFound');
  if (s === 499) return k('clientGone');
  if (/at their concurrency or rate limit/i.test(raw)) return k('keysBusy');
  if (/stream idle timeout/i.test(raw)) return k('idleTimeout');
  if (/timeout|timed out|deadline exceeded/i.test(raw)) return k('timeout');
  if (s === 401 || s === 403) return k('upstreamAuth');
  if (s === 429) return k('upstreamRate');
  if (s >= 500) return k('upstream5xx');
  if (!s && raw) return k('network');
  if (raw) return { text: raw.length > 120 ? `${raw.slice(0, 120)}…` : raw, raw, key: 'raw' };
  if (s >= 400) return { text: `HTTP ${s}`, raw: '', key: 'raw' };
  return null;
}

/** The four billing categories of a request (pm-spec 9.4). cacheWrite null = not reported. */
export function tokenCats(r) {
  const prompt = r.prompt_tokens || 0, cached = r.cached_tokens || 0, img = r.image_tokens || 0;
  return {
    input: Math.max(0, prompt - cached - img),
    image: img,
    cacheWrite: r.cache_write_tokens == null ? null : r.cache_write_tokens,
    cacheRead: cached,
    output: (r.completion_tokens || 0) + (r.reasoning_tokens || 0),
    reasoning: r.reasoning_tokens || 0,
    // The Messages API reports no reasoning breakdown.
    reasoningKnown: !/\/v1\/messages$/.test(r.path || ''),
  };
}
