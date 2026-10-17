// Reusable UI primitives: toasts, modals/drawers, confirm, menus, pagination, inputs.
import { h, copyText, fmtTotal, fmtInt } from './util.js';
import { icon } from './icons.js';
import { t, hasT } from './i18n.js';

// ---------- toasts ----------
let toastRoot;
export function toast(message, type = 'info', timeout = 4200) {
  if (!toastRoot) { toastRoot = h('div', { class: 'toasts', role: 'status', 'aria-live': 'polite' }); document.body.appendChild(toastRoot); }
  const ic = { success: 'checkCircle', error: 'xCircle', warning: 'alert', info: 'info' }[type] || 'info';
  const el = h('div', { class: `toast toast-${type}` },
    icon(ic, 16, 'toast-icon'),
    h('div', { class: 'toast-msg' }, message),
    h('button', { class: 'toast-close', 'aria-label': t('common.close'), onclick: () => dismiss() }, icon('x', 14)));
  const dismiss = () => { el.classList.add('leaving'); setTimeout(() => el.remove(), 180); };
  toastRoot.appendChild(el);
  if (timeout) setTimeout(dismiss, timeout);
  return dismiss;
}
/**
 * Human message for an error. Known server `error.code`s are mapped to translated text
 * (with the server's detail interpolated where useful); unknown codes fall back to the
 * server message.
 */
export function errorMessage(e) {
  if (!e) return '';
  const raw = e.message ? String(e.message) : String(e);
  const code = e.code;
  if (code === 'invalid_settings') {
    const m = /^([a-z_]+) must be between (-?\d+) and (-?\d+)$/.exec(raw);
    if (m) return t('err.settingsRange', { field: hasT(`settings.f.${m[1]}`) ? t(`settings.f.${m[1]}`) : m[1], a: m[2], b: m[3] });
  }
  if (code && hasT(`err.${code}`)) return t(`err.${code}`, { detail: raw });
  if (code === 'network_error') return t('err.network_error', { detail: raw });
  return raw;
}
export function toastError(e) {
  if (e && e.name === 'AbortError') return;
  if (e && e.status === 401) return; // handled globally
  const msg = errorMessage(e);
  const mapped = e && e.code && hasT(`err.${e.code}`);
  toast(e && e.code && !mapped && e.code !== 'network_error' && !msg.includes(e.code) ? `${msg} (${e.code})` : msg, 'error', 6000);
}

export const isMobile = () => window.matchMedia('(max-width: 768px)').matches;

/** On phones, tapping anywhere on a card row opens its ⋯ menu (unless a control was tapped). */
export function tapToMenu(tr, menuBtn) {
  tr.addEventListener('click', (e) => {
    if (!isMobile() || !menuBtn) return;
    if (e.target.closest('button, a, input, label, select, textarea, summary, details')) return;
    menuBtn.click();
  });
  return tr;
}

/** Password input with a show/hide toggle. Returns the wrapper; `.input` is the <input>. */
export function passwordInput(attrs = {}) {
  const inp = h('input', { class: 'input', type: 'password', autocomplete: 'off', spellcheck: 'false', ...attrs });
  const btn = h('button', {
    type: 'button', class: 'pw-toggle', title: t('common.showSecret'), 'aria-label': t('common.showSecret'), 'aria-pressed': 'false', tabindex: '-1',
    onclick: () => {
      const show = inp.type === 'password';
      inp.type = show ? 'text' : 'password';
      btn.replaceChildren(icon(show ? 'eyeOff' : 'eye', 15));
      btn.setAttribute('aria-pressed', show ? 'true' : 'false');
      btn.title = show ? t('common.hideSecret') : t('common.showSecret');
      inp.focus();
    },
  }, icon('eye', 15));
  const wrap = h('div', { class: 'pw-wrap' }, inp, btn);
  wrap.input = inp;
  wrap.toggle = btn;
  return wrap;
}

// ---------- overlay stack (modals + drawers) ----------
const stack = [];
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') {
    const menuOpen = document.querySelector('.menu-pop');
    if (menuOpen) return; // menu handles its own Esc
    const top = stack[stack.length - 1];
    if (top) { e.preventDefault(); top.close(); }
  }
});

/**
 * openModal({ title, body, footer, size: sm|md|lg|xl, kind: 'modal'|'drawer', onClose, dismissible })
 * Returns { close, el, body, setFooter, setTitle }.
 */
export function openModal({ title, subtitle, body, footer, size = 'md', kind = 'modal', onClose, dismissible = true, headerExtra } = {}) {
  const prevFocus = document.activeElement;
  const titleEl = h('div', { class: 'modal-title' }, title || '');
  const subEl = subtitle ? h('div', { class: 'modal-subtitle' }, subtitle) : null;
  const bodyEl = h('div', { class: 'modal-body' }, body);
  const footEl = h('div', { class: 'modal-footer' }, footer);
  if (!footer) footEl.hidden = true;
  const panel = h('div', { class: `${kind}-panel ${kind}-${size}`, role: 'dialog', 'aria-modal': 'true' },
    h('div', { class: 'modal-header' },
      h('div', { class: 'modal-heading' }, titleEl, subEl),
      h('div', { class: 'modal-header-actions' }, headerExtra,
        h('button', { class: 'btn btn-ghost btn-icon', 'aria-label': t('common.close'), title: t('common.close') + ' (Esc)', onclick: () => api.close() }, icon('x', 18)))),
    bodyEl, footEl);
  const backdrop = h('div', { class: `overlay overlay-${kind}`, onmousedown: (e) => { if (e.target === backdrop && dismissible) api.close(); } }, panel);
  document.body.appendChild(backdrop);
  document.body.classList.add('no-scroll');
  requestAnimationFrame(() => backdrop.classList.add('open'));
  let closed = false;
  const api = {
    el: panel, body: bodyEl, dismissible,
    close() {
      if (closed) return; closed = true;
      const i = stack.indexOf(api); if (i >= 0) stack.splice(i, 1);
      backdrop.classList.remove('open'); backdrop.classList.add('closing');
      setTimeout(() => backdrop.remove(), 200);
      if (!stack.length) document.body.classList.remove('no-scroll');
      if (onClose) onClose();
      if (prevFocus && prevFocus.focus) try { prevFocus.focus({ preventScroll: true }); } catch { /* ignore */ }
    },
    setFooter(...nodes) { footEl.replaceChildren(...nodes.flat().filter(Boolean)); footEl.hidden = !nodes.flat().filter(Boolean).length; },
    setTitle(text) { titleEl.textContent = text; },
    setBody(...nodes) { bodyEl.replaceChildren(...nodes.flat().filter(Boolean)); },
  };
  stack.push(api);
  setTimeout(() => {
    const f = panel.querySelector('[autofocus], .modal-body input:not([type=checkbox]):not([disabled]), .modal-body textarea, .modal-body select');
    if (f) f.focus(); else panel.focus?.();
  }, 30);
  return api;
}

export function confirmDialog({ title, message, confirmText, danger = false, extra } = {}) {
  return new Promise((resolve) => {
    let result = false;
    const ok = h('button', { class: `btn ${danger ? 'btn-danger' : 'btn-primary'}`, onclick: () => { result = true; m.close(); } }, confirmText || t('common.confirm'));
    const m = openModal({
      title: title || t('common.areYouSure'), size: 'sm',
      body: h('div', { class: 'confirm-body' }, message ? h('p', null, message) : null, extra),
      footer: [h('button', { class: 'btn', onclick: () => m.close() }, t('common.cancel')), ok],
      onClose: () => resolve(result),
    });
    setTimeout(() => ok.focus(), 40);
  });
}

// ---------- popover menu ----------
export function openMenu(anchor, items) {
  closeMenus();
  const pop = h('div', { class: 'menu-pop', role: 'menu' },
    items.filter(Boolean).map((it) => it === '-' ? h('div', { class: 'menu-sep' }) :
      h('button', {
        class: `menu-item ${it.danger ? 'danger' : ''}`, role: 'menuitem', disabled: it.disabled,
        onclick: (e) => { e.stopPropagation(); closeMenus(); it.onClick && it.onClick(); },
      }, it.icon ? icon(it.icon, 15) : null, h('span', null, it.label))));
  document.body.appendChild(pop);
  const r = anchor.getBoundingClientRect();
  const pw = pop.offsetWidth, ph = pop.offsetHeight;
  let left = r.right - pw, top = r.bottom + 4;
  if (left < 8) left = 8;
  if (top + ph > window.innerHeight - 8) top = Math.max(8, r.top - ph - 4);
  pop.style.left = `${left}px`; pop.style.top = `${top}px`;
  const onDoc = (e) => { if (!pop.contains(e.target)) closeMenus(); };
  const onKey = (e) => { if (e.key === 'Escape') { e.stopPropagation(); closeMenus(); } };
  setTimeout(() => {
    document.addEventListener('mousedown', onDoc);
    document.addEventListener('keydown', onKey, true);
    window.addEventListener('scroll', closeMenus, { once: true, capture: true });
  });
  pop._cleanup = () => { document.removeEventListener('mousedown', onDoc); document.removeEventListener('keydown', onKey, true); };
  pop.querySelector('.menu-item')?.focus();
}
export function closeMenus() {
  document.querySelectorAll('.menu-pop').forEach((p) => { p._cleanup && p._cleanup(); p.remove(); });
}
export function menuButton(items, label) {
  const b = h('button', {
    class: 'btn btn-ghost btn-icon btn-sm', 'aria-label': label || t('common.actions'), title: label || t('common.actions'),
    onclick: (e) => { e.stopPropagation(); openMenu(b, typeof items === 'function' ? items() : items); },
  }, icon('more', 16));
  return b;
}

// ---------- small components ----------
export function badge(text, variant = 'neutral', attrs = {}) {
  return h('span', { ...attrs, class: `badge badge-${variant} ${attrs.class || ''}` }, text);
}
export function dot(variant) { return h('span', { class: `dot dot-${variant}` }); }

export function switchEl({ checked = false, onChange, disabled = false, label, title } = {}) {
  const input = h('input', { type: 'checkbox', role: 'switch', checked, disabled });
  const el = h('label', { class: 'switch', title, onclick: (e) => e.stopPropagation() }, input, h('span', { class: 'switch-track' }, h('span', { class: 'switch-thumb' })), label ? h('span', { class: 'switch-label' }, label) : null);
  input.addEventListener('change', async () => {
    if (!onChange) return;
    input.disabled = true;
    try { const r = await onChange(input.checked); if (r === false) input.checked = !input.checked; }
    catch { input.checked = !input.checked; }
    finally { input.disabled = disabled; }
  });
  el.input = input;
  return el;
}

export function checkbox({ checked = false, label, onChange, hint } = {}) {
  const input = h('input', { type: 'checkbox', checked, onchange: () => onChange && onChange(input.checked) });
  const el = h('label', { class: 'checkbox' }, input, h('span', null, label, hint ? h('span', { class: 'hint-inline' }, hint) : null));
  el.input = input;
  return el;
}

export function segmented({ options, value, onChange, size = '' }) {
  const el = h('div', { class: `segmented ${size}`, role: 'tablist' });
  const render = () => {
    el.replaceChildren(...options.map((o) => h('button', {
      class: `seg-btn ${o.value === value ? 'active' : ''}`, role: 'tab', 'aria-selected': o.value === value ? 'true' : 'false', type: 'button',
      onclick: () => { if (o.value === value) return; value = o.value; render(); onChange && onChange(value); },
    }, o.icon ? icon(o.icon, 14) : null, o.label)));
  };
  render();
  el.setValue = (v) => { value = v; render(); };
  return el;
}

export function field(label, control, { hint, required, full, id } = {}) {
  const lid = id || (control && control.id) || null;
  return h('div', { class: `field ${full ? 'field-full' : ''}` },
    label ? h('label', { class: 'field-label', for: lid }, label, required ? h('span', { class: 'req' }, '*') : null) : null,
    control,
    hint ? h('div', { class: 'field-hint' }, hint) : null);
}

export function input(attrs = {}) { return h('input', { class: 'input', type: 'text', ...attrs }); }
export function select(options, value, attrs = {}) {
  return h('select', { class: 'input select', ...attrs },
    options.map((o) => {
      const opt = typeof o === 'object' ? o : { value: o, label: o };
      return h('option', { value: opt.value, selected: String(opt.value) === String(value ?? '') }, opt.label);
    }));
}

export function searchInput({ value = '', placeholder, onInput, width } = {}) {
  const inp = h('input', { class: 'input', type: 'search', value, placeholder: placeholder || t('common.search'), oninput: (e) => onInput && onInput(e.target.value) });
  const el = h('div', { class: 'search-input', style: width ? { width } : null }, icon('search', 15), inp);
  el.input = inp;
  return el;
}

export function spinner(size = 16) { return h('span', { class: 'spinner', style: { width: `${size}px`, height: `${size}px` } }); }

export function emptyState({ icon: ic = 'inbox', title, desc, action } = {}) {
  return h('div', { class: 'empty' }, h('div', { class: 'empty-icon' }, icon(ic, 22)), h('div', { class: 'empty-title' }, title || t('common.noData')), desc ? h('div', { class: 'empty-desc' }, desc) : null, action || null);
}

export function skeletonRows(cols, rows = 6) {
  return Array.from({ length: rows }, () => h('tr', { class: 'skeleton-row' }, Array.from({ length: cols }, (_, i) => h('td', null, h('span', { class: 'skeleton', style: { width: `${40 + ((i * 37 + rows * 13) % 50)}%` } })))));
}

export function tableEmptyRow(cols, opts) { return h('tr', { class: 'empty-row' }, h('td', { colspan: cols }, emptyState(opts))); }

export function copyButton(getText, { label, small = true, iconOnly = false, cls = '' } = {}) {
  const b = h('button', {
    class: `btn ${small ? 'btn-sm' : ''} ${iconOnly ? 'btn-ghost btn-icon' : ''} ${cls}`, type: 'button', title: label || t('common.copy'),
    onclick: async (e) => {
      e.stopPropagation();
      const text = typeof getText === 'function' ? await getText() : getText;
      if (text == null) return;
      const ok = await copyText(String(text));
      if (ok) {
        b.classList.add('copied');
        const prev = [...b.childNodes];
        b.replaceChildren(...[icon('check', 14), iconOnly ? null : h('span', null, t('common.copied'))].filter(Boolean));
        setTimeout(() => { b.classList.remove('copied'); b.replaceChildren(...prev); }, 1400);
      } else toast(t('common.copyFailed'), 'error');
    },
  }, icon('copy', 14), iconOnly ? null : h('span', null, label || t('common.copy')));
  return b;
}

export function codeBlock(text, { lang, copy = true } = {}) {
  return h('div', { class: 'code-block' },
    copy ? h('div', { class: 'code-actions' }, copyButton(text, { iconOnly: true })) : null,
    h('pre', { class: `code ${lang ? 'lang-' + lang : ''}` }, h('code', null, text)));
}

/**
 * Stateful pager. pager({ onChange(page, pageSize), sizes }) returns an element with
 * .update({ page, pageSize, total, totalKind, hasMore, loading }) so controls (and focus)
 * survive page changes. totalKind: exact | approx (estimate) | at_least (lower bound).
 * For estimates "last" jumps to the estimated last page; for lower bounds it is disabled
 * and "next" follows `hasMore` (the current page was full).
 */
export function pager({ onChange, sizes = [10, 20, 50, 100, 200] } = {}) {
  let st = { page: 1, pageSize: sizes[0], total: 0, totalKind: 'exact', hasMore: false, loading: false, rowCount: null };
  const lastPage = () => Math.max(1, Math.ceil((st.total || 0) / st.pageSize));
  const canNext = () => st.page < lastPage() || (st.totalKind !== 'exact' && st.hasMore);
  const go = (p) => {
    p = Math.floor(Number(p));
    if (!Number.isFinite(p) || p < 1) p = 1;
    if (st.totalKind === 'exact') p = Math.min(p, lastPage());
    if (p === st.page) { paint(); return; }
    onChange(p, st.pageSize);
  };
  const info = h('div', { class: 'page-info' });
  const sizeSel = select(sizes.map((s) => ({ value: s, label: t('common.perPage', { n: s }) })), st.pageSize, {
    class: 'input select select-sm', 'aria-label': t('common.pageSize'), onchange: (e) => onChange(1, Number(e.target.value)),
  });
  const btn = (ic, label, fn) => h('button', { class: 'page-btn', type: 'button', 'aria-label': label, title: label, onclick: fn }, icon(ic, 15));
  const firstB = btn('chevronsLeft', t('common.firstPage'), () => go(1));
  const prevB = btn('chevronLeft', t('common.prev'), () => go(st.page - 1));
  const nextB = btn('chevronRight', t('common.next'), () => go(st.page + 1));
  const lastB = btn('chevronsRight', t('common.lastPage'), () => go(lastPage()));
  const nums = h('div', { class: 'page-nums' });
  const jump = h('input', {
    class: 'input page-jump', type: 'number', min: 1, inputmode: 'numeric', 'aria-label': t('common.goToPage'), title: t('common.goToPage'),
    onkeydown: (e) => { if (e.key === 'Enter') { e.preventDefault(); go(jump.value); } },
    onchange: () => go(jump.value),
  });
  const jumpWrap = h('label', { class: 'page-jump-wrap' }, h('span', null, t('common.goTo')), jump);
  const el = h('div', { class: 'pagination' }, info,
    h('div', { class: 'page-controls' }, sizeSel, firstB, prevB, nums, nextB, lastB, jumpWrap));
  function paint() {
    const { page, pageSize, total, totalKind, loading } = st;
    const pages = lastPage();
    const exact = totalKind === 'exact';
    const rows = st.rowCount != null ? st.rowCount : Math.max(0, Math.min(pageSize, (total || 0) - (page - 1) * pageSize));
    const from = rows ? (page - 1) * pageSize + 1 : 0, to = rows ? (page - 1) * pageSize + rows : 0;
    info.replaceChildren(
      ...(loading ? [h('span', { class: 'page-loading' }, spinner(12))] : []),
      h('span', null, t(exact ? 'common.showing' : 'common.showingEst', { from: fmtInt(from), to: fmtInt(to), total: fmtTotal(total || 0, totalKind) })));
    sizeSel.value = String(pageSize);
    prevB.disabled = firstB.disabled = page <= 1;
    nextB.disabled = !canNext();
    lastB.disabled = totalKind === 'at_least' || page === pages;
    lastB.title = totalKind === 'approx' ? t('common.lastPageEst') : t('common.lastPage');
    const list = [];
    const add = (p) => list.push(h('button', { class: `page-btn ${p === page ? 'active' : ''}`, type: 'button', 'aria-current': p === page ? 'page' : null, onclick: () => go(p) },
      (totalKind === 'approx' && p === pages && p !== page ? '~' : '') + fmtInt(p)));
    const gap = () => list.push(h('span', { class: 'page-gap' }, '…'));
    if (totalKind !== 'at_least' && page > pages) {
      // Past the estimated end: 1 … ~last … current
      add(1); if (pages > 2) gap(); if (pages > 1) add(pages); gap(); add(page);
    } else {
      const lo = Math.max(1, page - 1), hi = totalKind === 'at_least' ? page + (canNext() ? 1 : 0) : Math.min(pages, page + 1);
      if (lo > 1) { add(1); if (lo > 2) gap(); }
      for (let p = lo; p <= hi; p++) add(p);
      if (totalKind !== 'at_least' && hi < pages) { if (hi < pages - 1) gap(); add(pages); }
      else if (totalKind === 'at_least' && canNext()) gap();
    }
    nums.replaceChildren(...list);
    if (document.activeElement !== jump) jump.value = '';
    jump.placeholder = String(page);
    if (exact) jump.max = String(pages); else jump.removeAttribute('max');
    jumpWrap.hidden = pages <= 1 && !canNext() && page === 1;
  }
  el.update = (o) => { st = { ...st, ...o }; paint(); };
  el.setLoading = (on) => { st.loading = !!on; paint(); };
  paint();
  return el;
}


/** Back-compat wrapper: a one-shot pager. */
export function pagination(o) { const p = pager({ onChange: o.onChange, sizes: o.sizes }); p.update(o); return p; }

/** Toggle the "loading, previous rows still shown" state on a table region. */
export function setTableLoading(region, on) {
  if (!region) return;
  clearTimeout(region._loadT);
  if (on) region._loadT = setTimeout(() => region.classList.add('is-loading'), 120);
  else region.classList.remove('is-loading');
}

/** Tag input with optional suggestions (datalist). Returns element with .getValues(). */
export function tagInput({ values = [], placeholder, suggestions = [], validate, mono = true } = {}) {
  let tags = [...values];
  const listId = `dl-${Math.random().toString(36).slice(2)}`;
  const inp = h('input', { class: 'tag-entry', placeholder: placeholder || t('common.addTag'), list: listId });
  const wrap = h('div', { class: `tag-input ${mono ? 'mono' : ''}`, onclick: () => inp.focus() });
  const dl = h('datalist', { id: listId }, suggestions.map((s) => h('option', { value: s })));
  const render = () => {
    wrap.replaceChildren(
      ...tags.map((tg, i) => h('span', { class: 'tag' }, tg, h('button', { type: 'button', class: 'tag-x', 'aria-label': t('common.remove'), onclick: (e) => { e.stopPropagation(); tags.splice(i, 1); render(); } }, icon('x', 11)))),
      ...[inp, dl].filter(Boolean));
  };
  const commit = () => {
    const parts = inp.value.split(/[,\n]/).map((x) => x.trim()).filter(Boolean);
    for (const p of parts) {
      if (validate && !validate(p)) { toast(t('common.invalidValue', { v: p }), 'warning'); continue; }
      if (!tags.includes(p)) tags.push(p);
    }
    inp.value = ''; render(); inp.focus();
  };
  inp.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' || e.key === ',' || e.key === 'Tab') { if (inp.value.trim()) { e.preventDefault(); commit(); } }
    else if (e.key === 'Backspace' && !inp.value && tags.length) { tags.pop(); render(); }
  });
  inp.addEventListener('change', () => { if (suggestions.includes(inp.value.trim())) commit(); });
  inp.addEventListener('blur', () => { if (inp.value.trim()) commit(); });
  render();
  wrap.getValues = () => { if (inp.value.trim()) commit(); return [...tags]; };
  wrap.setValues = (v) => { tags = [...v]; render(); };
  wrap.setSuggestions = (list) => { suggestions = list || []; dl.replaceChildren(...suggestions.map((s) => h('option', { value: s }))); };
  wrap.peek = () => [...tags];
  return wrap;
}

/** Tabs: [{ id, label, render: () => Node, count }] */
export function tabs(items, { active, onChange, cls = '' } = {}) {
  let cur = active || items[0]?.id;
  const head = h('div', { class: `tabs-head ${cls}`, role: 'tablist' });
  const body = h('div', { class: 'tabs-body' });
  const el = h('div', { class: 'tabs' }, head, body);
  const render = () => {
    head.replaceChildren(...items.map((it) => h('button', {
      class: `tab ${it.id === cur ? 'active' : ''}`, role: 'tab', type: 'button', 'aria-selected': it.id === cur ? 'true' : 'false',
      onclick: () => { cur = it.id; render(); onChange && onChange(cur); },
    }, it.label, it.count != null ? h('span', { class: 'tab-count' }, String(it.count)) : null)));
    const it = items.find((x) => x.id === cur) || items[0];
    body.replaceChildren(it ? it.render() : '');
  };
  render();
  el.head = head;
  return el;
}

/** Wrap table in a horizontally/vertically scrolling card region (sticky header). */
export function tableWrap(table, { maxHeight } = {}) {
  return h('div', { class: 'table-wrap', style: maxHeight ? { maxHeight } : null }, table);
}

export function kv(rows) {
  return h('dl', { class: 'kv' }, rows.filter(Boolean).map(([k, v]) => [h('dt', null, k), h('dd', null, v == null || v === '' ? h('span', { class: 'muted' }, '—') : v)]));
}

export function progress(ratio, variant) {
  const r = Math.max(0, Math.min(1, ratio || 0));
  const v = variant || (r >= 0.9 ? 'danger' : r >= 0.7 ? 'warning' : 'accent');
  return h('div', { class: `progress progress-${v}` }, h('div', { class: 'progress-bar', style: { width: `${(r * 100).toFixed(1)}%` } }));
}

export function button(label, { icon: ic, variant = '', size = '', onClick, attrs = {} } = {}) {
  return h('button', { class: `btn ${variant ? 'btn-' + variant : ''} ${size ? 'btn-' + size : ''}`, type: 'button', onclick: onClick, ...attrs }, ic ? icon(ic, 15) : null, label ? h('span', null, label) : null);
}

/** Run an async action while showing a busy state on a button. */
export async function busy(btn, fn) {
  if (btn.disabled) return;
  btn.disabled = true; btn.classList.add('is-busy');
  const sp = spinner(14); btn.prepend(sp);
  try { return await fn(); }
  finally { btn.disabled = false; btn.classList.remove('is-busy'); sp.remove(); }
}

export function closeAllModals() { [...stack].reverse().forEach((m) => m.close()); closeMenus(); }

/** Card with header (title, optional subtitle, right-side actions) and body. */
export function card({ title, subtitle, actions, body, cls = '', bodyCls = '', flush = false } = {}) {
  const head = title || actions ? h('div', { class: 'card-head' },
    h('div', { class: 'card-heading' }, title ? h('h3', { class: 'card-title' }, title) : null, subtitle ? h('div', { class: 'card-subtitle' }, subtitle) : null),
    actions ? h('div', { class: 'card-actions' }, actions) : null) : null;
  const b = h('div', { class: `card-body ${flush ? 'flush' : ''} ${bodyCls}` }, body);
  const el = h('section', { class: `card ${cls}` }, head, b);
  el.body = b;
  return el;
}
