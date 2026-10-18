// DOM + formatting helpers. All DOM construction goes through h()/s(), which only ever
// set text via text nodes / setAttribute, so server data can never become markup.
import { getLang, t } from './i18n.js';

const PROPS = new Set(['value', 'checked', 'disabled', 'selected', 'indeterminate', 'readOnly', 'multiple', 'open']);

function applyAttrs(el, attrs, isSvg) {
  if (!attrs) return;
  for (const [k, v] of Object.entries(attrs)) {
    if (v == null || v === false) continue;
    if (k === 'innerHTML' || k === 'outerHTML') throw new Error('h(): innerHTML is not allowed');
    if (k === 'class') isSvg ? el.setAttribute('class', v) : (el.className = v);
    else if (k === 'style' && typeof v === 'object') Object.assign(el.style, v);
    else if (k === 'dataset') Object.assign(el.dataset, v);
    else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2).toLowerCase(), v);
    else if (k === 'ref' && typeof v === 'function') v(el);
    else if (!isSvg && PROPS.has(k)) el[k] = v;
    else el.setAttribute(k, v === true ? '' : String(v));
  }
}

function append(el, children) {
  for (const c of children) {
    if (c == null || c === false || c === true) continue;
    if (Array.isArray(c)) append(el, c);
    else if (c instanceof Node) el.appendChild(c);
    else el.appendChild(document.createTextNode(String(c)));
  }
}

/** Create an HTML element. Children may be strings (always text), nodes or arrays. */
export function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  applyAttrs(el, attrs, false);
  append(el, children);
  return el;
}

/** Create an SVG element. */
export function s(tag, attrs, ...children) {
  const el = document.createElementNS('http://www.w3.org/2000/svg', tag);
  applyAttrs(el, attrs, true);
  append(el, children);
  return el;
}

export function clear(el) { while (el.firstChild) el.removeChild(el.firstChild); return el; }
export function mount(el, ...children) { clear(el); append(el, children); return el; }

// ---------- formatting ----------
const locale = () => (getLang() === 'zh' ? 'zh-CN' : 'en-US');
const isNum = (v) => typeof v === 'number' && Number.isFinite(v);

export function fmtInt(n) { return isNum(n) ? Math.round(n).toLocaleString(locale()) : '—'; }

export function fmtCompact(n) {
  if (!isNum(n)) return '—';
  if (getLang() === 'zh') {
    // 中文界面：小于 1 万写完整数字，1 万起用“万”（1 位小数），1 亿起用“亿”（2 位小数）。
    const a = Math.abs(n);
    // 例：8,612；1.2 万；5,210 万；1.86 亿（整数位加千分位，“万”的 .0 省略）。
    if (a < 1e4) return Number.isInteger(n) ? fmtInt(n) : String(Math.round(n * 100) / 100);
    if (a < 1e8) {
      const v = Math.round(n / 1e3) / 10;
      return `${v.toLocaleString('en-US', { maximumFractionDigits: 1 })} 万`;
    }
    return `${(n / 1e8).toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 })} 亿`;
  }
  if (Math.abs(n) < 1000) return String(Math.round(n * 100) / 100);
  return new Intl.NumberFormat(locale(), { notation: 'compact', maximumFractionDigits: 1 }).format(n);
}

/**
 * USD (pm-spec 10.3): $1,234.50 · $0.0056 · $0.00045 · $0 · −$1.20 (minus sign U+2212).
 * Below $1: the first non-zero digit plus one more ($0.0017), at most 6 decimals.
 * { precise: true } keeps up to 6 decimals, for itemized cost tables (pm-spec 10.8 batch 3 #76).
 */
export function fmtUSD(v, { zero = '$0', precise = false } = {}) {
  if (!isNum(v)) return '—';
  if (v === 0) return zero;
  const a = Math.abs(v), sign = v < 0 ? '−' : '';
  if (a < 0.000001) return '<$0.000001';
  let str;
  if (a >= 1000) str = a.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
  else if (a >= 1) str = a.toFixed(2);
  else {
    const digits = Math.min(6, Math.max(2, (precise ? 5 : 1) - Math.floor(Math.log10(a))));
    str = a.toFixed(digits).replace(/(\.\d*?[1-9])0+$/, '$1').replace(/\.0+$/, '');
    if (!str.includes('.')) str += '.00';
    else if (str.split('.')[1].length < 2) str += '0';
  }
  return `${sign}$${str}`;
}

/** Price per 1M tokens etc. Empty when not applicable. */
export function fmtPrice(v) { return isNum(v) && v > 0 ? fmtUSD(v, { precise: true }) : "—"; }

export function fmtMs(ms) {
  if (!isNum(ms)) return '—';
  if (getLang() === 'zh') {
    if (ms < 1000) return `${Math.round(ms)} 毫秒`;
    if (ms < 60000) return `${(ms / 1000).toFixed(1)} 秒`;
    return `${Math.floor(ms / 60000)} 分 ${Math.round((ms % 60000) / 1000)} 秒`;
  }
  if (ms < 1000) return `${Math.round(ms)} ms`;
  if (ms < 60000) return `${(ms / 1000).toFixed(1)} s`;
  const m = Math.floor(ms / 60000), sec = Math.round((ms % 60000) / 1000);
  return `${m}m ${sec}s`;
}

export function fmtPct(v, digits = 1) {
  if (!isNum(v)) return '—';
  const p = v * 100;
  if (p === 100 || p === 0) return `${p}%`;
  // ≥ 99% (success rates): 2 decimals, rounded down so 99.996% never shows as 100.00%.
  if (p >= 99 && p < 100) return `${(Math.floor(p * 100) / 100).toFixed(2)}%`;
  return `${p.toFixed(digits)}%`;
}

export function fmtBytes(b) {
  if (!isNum(b)) return '—';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0; while (b >= 1024 && i < u.length - 1) { b /= 1024; i++; }
  return `${b.toFixed(i === 0 ? 0 : 1)} ${u[i]}`;
}

export function fmtDuration(sec) {
  if (!isNum(sec)) return '—';
  sec = Math.max(0, Math.floor(sec));
  const d = Math.floor(sec / 86400), hh = Math.floor((sec % 86400) / 3600), mm = Math.floor((sec % 3600) / 60), ss = sec % 60;
  if (getLang() === 'zh') {
    // 例：41 秒；1 分 42 秒；3 小时 5 分；2 天 4 小时
    if (d) return `${d} 天 ${hh} 小时`;
    if (hh) return `${hh} 小时 ${mm} 分`;
    if (mm) return `${mm} 分 ${ss} 秒`;
    return `${ss} 秒`;
  }
  if (d) return `${d}d ${hh}h`;
  if (hh) return `${hh}h ${mm}m`;
  if (mm) return `${mm}m ${ss}s`;
  return `${ss}s`;
}

const pad = (n) => String(n).padStart(2, '0');
/**
 * Detail timestamp: 2026-10-18 14:02:33. With { seconds: false } this is the list form
 * (pm-spec 10.3): zh “10月18日 14:02” (year added when it is not this year), en “Oct 18, 14:02”.
 */
export function fmtDateTime(iso, { seconds = true } = {}) {
  if (!iso) return '—';
  const d = new Date(iso);
  if (isNaN(d)) return String(iso);
  if (!seconds) return fmtDateList(d);
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}
const EN_MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
/** List date only: zh “9月12日” / “2027年1月1日”, en “Sep 12” / “Jan 1, 2027”. */
export function fmtDate(iso) {
  if (!iso) return '—';
  const d = iso instanceof Date ? iso : new Date(iso);
  if (isNaN(d)) return String(iso);
  const otherYear = d.getFullYear() !== new Date().getFullYear();
  if (getLang() === 'zh') return `${otherYear ? d.getFullYear() + '年' : ''}${d.getMonth() + 1}月${d.getDate()}日`;
  return `${EN_MONTHS[d.getMonth()]} ${d.getDate()}${otherYear ? ', ' + d.getFullYear() : ''}`;
}
/** List date and time: zh “10月18日 14:02”, en “Oct 18, 14:02”. */
export function fmtDateList(iso) {
  if (!iso) return '—';
  const d = iso instanceof Date ? iso : new Date(iso);
  if (isNaN(d)) return String(iso);
  const hm = `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  return getLang() === 'zh' ? `${fmtDate(d)} ${hm}` : `${fmtDate(d)}, ${hm}`;
}
export function fmtTimeShort(iso) {
  const d = new Date(iso);
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

export function relTime(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  if (isNaN(d)) return String(iso);
  const diff = (d.getTime() - Date.now()) / 1000;
  const a = Math.abs(diff);
  // Past (pm-spec 10.3): 刚刚 / N 分钟前 / N 小时前, older than a day → the absolute list time.
  if (diff <= 5) {
    const zh = getLang() === 'zh';
    if (a < 60) return zh ? '刚刚' : 'just now';
    if (a < 3600) { const n = Math.floor(a / 60); return zh ? `${n} 分钟前` : `${n} min ago`; }
    if (a < 86400) { const n = Math.floor(a / 3600); return zh ? `${n} 小时前` : `${n} hr ago`; }
    return fmtDateList(d);
  }
  const rtf = new Intl.RelativeTimeFormat(locale(), { numeric: 'auto' });
  const fmt = (n, unit) => { const out = rtf.format(n, unit); return getLang() === 'zh' ? out.replace(/(\d)([^\d\s])/, '$1 $2') : out; };
  if (a < 45) return fmt(Math.round(diff), 'second');
  if (a < 2700) return fmt(Math.round(diff / 60), 'minute');
  if (a < 79200) return fmt(Math.round(diff / 3600), 'hour');
  if (a < 2592000) return fmt(Math.round(diff / 86400), 'day');
  if (a < 31536000) return fmt(Math.round(diff / 2592000), 'month');
  return fmt(Math.round(diff / 31536000), 'year');
}

/** <time> showing relative time with the absolute timestamp on hover. */
export function timeEl(iso, cls = '') {
  if (!iso) return h('span', { class: 'muted' }, '—');
  return h('time', { class: `rel-time ${cls}`, datetime: iso, title: fmtDateTime(iso), 'data-rel': iso }, relTime(iso));
}

/** Convert ISO → value for <input type=datetime-local> (local time). */
export function toLocalInput(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  if (isNaN(d)) return '';
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}
export function fromLocalInput(v) { return v ? new Date(v).toISOString() : null; }

// ---------- misc ----------
export function debounce(fn, ms = 300) {
  let t; return (...a) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms); };
}

export async function copyText(text) {
  try {
    if (navigator.clipboard && window.isSecureContext) { await navigator.clipboard.writeText(text); return true; }
  } catch { /* fall through */ }
  const ta = h('textarea', { style: { position: 'fixed', top: '-1000px', opacity: '0' } });
  ta.value = text;
  document.body.appendChild(ta); ta.select();
  let ok = false;
  try { ok = document.execCommand('copy'); } catch { ok = false; }
  ta.remove();
  return ok;
}

/** Trigger a client-side download of `text` (no server round-trip). */
export function downloadText(filename, text, mime = 'application/json') {
  const url = URL.createObjectURL(new Blob([text], { type: `${mime};charset=utf-8` }));
  const a = h('a', { href: url, download: filename, style: { display: 'none' } });
  document.body.appendChild(a);
  a.click();
  setTimeout(() => { a.remove(); URL.revokeObjectURL(url); }, 1000);
}

/**
 * Make an upstream status reason readable: "HTTP 400: {"code":"x","error":"Bad key"}" → "HTTP 400: Bad key".
 * Returns { short, full }.
 */
export function readableReason(text) {
  const full = text == null ? '' : String(text);
  const i = full.indexOf('{');
  if (i < 0) return { short: full, full };
  let j = full.lastIndexOf('}');
  let obj = null;
  while (j > i && !obj) { try { obj = JSON.parse(full.slice(i, j + 1)); } catch { j = full.lastIndexOf('}', j - 1); } }
  if (!obj || typeof obj !== 'object') return { short: full, full };
  const pick = (o) => {
    if (!o || typeof o !== 'object') return null;
    for (const k of ['error', 'message', 'detail', 'msg']) {
      const v = o[k];
      if (typeof v === 'string' && v.trim()) return v.trim();
      if (v && typeof v === 'object') { const inner = pick(v); if (inner) return inner; }
    }
    return null;
  };
  const msg = pick(obj);
  if (!msg) return { short: full, full };
  const prefix = full.slice(0, i).replace(/[\s:–-]+$/, '').trim();
  return { short: prefix ? `${prefix}: ${msg}` : msg, full };
}

export function tryParseJSON(str) {
  if (typeof str !== 'string') return { ok: false };
  const t = str.trim();
  if (!t || (t[0] !== '{' && t[0] !== '[')) return { ok: false };
  try { return { ok: true, value: JSON.parse(t) }; } catch { return { ok: false }; }
}

/** Build a DOM fragment of syntax-highlighted pretty JSON (text nodes only). */
export function highlightJSON(value) {
  const text = JSON.stringify(value, null, 2);
  const frag = document.createDocumentFragment();
  if (text.length > 600000) { frag.appendChild(document.createTextNode(text)); return frag; }
  const re = /("(?:\\u[a-fA-F0-9]{4}|\\[^u]|[^\\"])*")(\s*:)?|\b(true|false)\b|\bnull\b|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?/g;
  let last = 0, m;
  while ((m = re.exec(text))) {
    if (m.index > last) frag.appendChild(document.createTextNode(text.slice(last, m.index)));
    let cls;
    if (m[1]) cls = m[2] ? 'j-key' : 'j-str';
    else if (m[3]) cls = 'j-bool';
    else if (m[0] === 'null') cls = 'j-null';
    else cls = 'j-num';
    if (m[1] && m[2]) {
      frag.appendChild(h('span', { class: cls }, m[1]));
      frag.appendChild(document.createTextNode(m[2]));
    } else frag.appendChild(h('span', { class: cls }, m[0]));
    last = re.lastIndex;
  }
  if (last < text.length) frag.appendChild(document.createTextNode(text.slice(last)));
  return frag;
}

export function statusClass(code) {
  if (!code) return 'neutral';
  if (code < 300) return 'success';
  if (code < 400) return 'info';
  if (code === 429 || code === 499) return 'warning';
  if (code < 500) return 'warning';
  return 'danger';
}

export function qsParse(str) {
  const o = {};
  new URLSearchParams(str || '').forEach((v, k) => { o[k] = v; });
  return o;
}
export function qsBuild(obj) {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(obj || {})) if (v !== '' && v != null && v !== false) p.set(k, v);
  return p.toString();
}

/**
 * Human total for a list response. kind: exact (thousands separators), approx (compact,
 * "about 3.0 million" / "约 300 万"), at_least ("1,000+" / "至少 1,000").
 */
export function fmtCompactLong(n) {
  if (!isNum(n)) return '—';
  if (Math.abs(n) < 10000) return fmtInt(n);
  const zh = getLang() === 'zh';
  const opts = { notation: 'compact', compactDisplay: 'long' };
  if (zh) opts.maximumSignificantDigits = 2;
  else if (Math.abs(n) >= 1e6) { opts.minimumFractionDigits = 1; opts.maximumFractionDigits = 1; }
  else opts.maximumSignificantDigits = 3;
  let s = new Intl.NumberFormat(locale(), opts).format(n);
  if (zh) s = s.replace(/(\d)([^\d.,\s])/, '$1 $2');
  return s;
}
export function fmtTotal(total, kind = 'exact') {
  const zh = getLang() === 'zh';
  if (!total) return fmtInt(0);
  if (kind === 'approx') return zh ? `约 ${fmtCompactLong(total)}` : `about ${fmtCompactLong(total)}`;
  if (kind === 'at_least') return zh ? `至少 ${fmtInt(total)}` : `${fmtInt(total)}+`;
  return fmtInt(total);
}

/** UTF-8 byte length of a string (used for size labels). */
export function utf8Bytes(str) {
  if (!str) return 0;
  try { return new TextEncoder().encode(str).length; } catch { return str.length; }
}

/**
 * Request sequencer: each call aborts the previous request's signal and returns a fresh one,
 * also aborted when `parent` (the route's signal) aborts.
 */
export function latestSignal(parent) {
  let ctl = null;
  if (parent) parent.addEventListener('abort', () => { if (ctl) ctl.abort(); }, { once: true });
  const next = () => {
    if (ctl) ctl.abort();
    ctl = new AbortController();
    if (parent && parent.aborted) ctl.abort();
    return ctl.signal;
  };
  next.abort = () => { if (ctl) ctl.abort(); ctl = null; };
  return next;
}

/**
 * Credential label "name · prefix" (.cred in style.css), e.g. e2e · sk-xr-nZja, so same-named
 * upstream keys / client tokens / admin API tokens can be told apart. `cls` is added to the name.
 */
export function cred(name, prefix, cls = '') {
  return h('span', { class: 'cred', title: prefix ? `${name} · ${prefix}` : name },
    h('span', { class: `cred-name ${cls}`.trim() }, name),
    prefix ? h('span', { class: 'cred-prefix' }, prefix) : null);
}
