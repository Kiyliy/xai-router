// Entry point: auth gate, app shell, hash router.
import { h, mount, relTime, fmtDuration, qsParse, qsBuild, fmtBytes } from './util.js';
import { icon, logo } from './icons.js';
import { t, getLang, setLang } from './i18n.js';
import { auth, onUnauthorized, get, post } from './api.js';
import { toastError, segmented, closeAllModals, closeMenus, busy, confirmDialog, passwordInput, errorMessage, openMenu } from './ui.js';
import { applyTheme, cycleTheme, getThemePref } from './theme.js';
import * as overview from './pages/overview.js';
import * as keys from './pages/keys.js';
import * as tokens from './pages/tokens.js';
import * as models from './pages/models.js';
import * as logs from './pages/logs.js';
import * as settings from './pages/settings.js';
import * as data from './pages/data.js';
import * as access from './pages/access.js';
import { openPasswordDialog } from './pages/access.js';
import * as docs from './pages/docs.js';
import { docsTab } from './pages/docs.js';

const ROUTES = { overview, keys, tokens, models, logs, settings, data, access, docs };

// Sidebar: "overview" on its own, then the gateway / system / docs groups (pm-spec 2.1).
// The two docs entries are tabs of one page; `tab` picks which one is active.
const NAV = [
  { route: 'overview', icon: 'overview' },
  { group: 'gateway' },
  { route: 'keys', icon: 'key' },
  { route: 'tokens', icon: 'ticket' },
  { route: 'models', icon: 'cpu' },
  { route: 'logs', icon: 'logs' },
  { group: 'system' },
  { route: 'settings', icon: 'settings' },
  { route: 'data', icon: 'database' },
  { route: 'access', icon: 'shield' },
  { group: 'docs' },
  { route: 'docs', tab: 'client', icon: 'book', label: 'nav.docsClient' },
  { route: 'docs', tab: 'admin', icon: 'braces', label: 'nav.docsAdmin' },
];

/** Page title key for a route (docs depends on its tab); the nav label uses the same key. */
function titleKey(name, params) {
  if (name === 'docs') return docsTab(params) === 'admin' ? 'nav.docsAdmin' : 'nav.docsClient';
  return `nav.${name}`;
}

// Old links (v0.3.x) -> new pages.
function redirectFor(name, params) {
  if (name === 'settings') {
    const sec = params.section;
    if (sec === 'account' || sec === 'password') return '#/access';
    if (sec === 'maintenance' || sec === 'storage' || sec === 'system') return '#/data';
  }
  if (name === 'docs' && params.tab !== 'client' && params.tab !== 'admin') return `#/docs?${qsBuild({ ...params, tab: 'client' })}`;
  return null;
}

const app = document.getElementById('app');
const state = { me: null, info: null, cleanup: null, controller: null, routeSeq: 0, shell: null, view: 'none', guard: null, lastHash: '', onParams: null, curName: '' };

// ---------- routing ----------
function parseHash() {
  const raw = location.hash.replace(/^#/, '') || '/overview';
  const [p, q] = raw.split('?');
  const name = p.replace(/^\/+/, '').split('/')[0] || 'overview';
  return { name, params: qsParse(q) };
}

function teardownPage() {
  closeMenus();
  state.guard = null;
  state.onParams = null;
  state.curName = '';
  if (state.controller) state.controller.abort();
  state.controller = null;
  if (typeof state.cleanup === 'function') { try { state.cleanup(); } catch (e) { console.error(e); } }
  state.cleanup = null;
}

function route() {
  if (state.view !== 'shell') return;
  let { name, params } = parseHash();
  if (!ROUTES[name]) { location.replace('#/overview'); return; }
  const redirect = redirectFor(name, params);
  if (redirect) {
    history.replaceState(null, '', redirect);
    ({ name, params } = parseHash());
  }
  state.lastHash = location.hash;
  refreshBanners();
  // Same page, new query (back/forward, or a link to other filters): let the page apply the
  // params in place so the current rows stay visible instead of re-rendering from scratch.
  if (state.onParams && state.curName === name) {
    state.shell.shell.classList.remove('sidebar-open');
    try { state.onParams(params); return; } catch (e) { console.error(e); }
  }
  const seq = ++state.routeSeq;
  teardownPage();
  closeAllModals();
  const sh = state.shell;
  markNav(name, params);
  setCrumbs([]);
  sh.title.textContent = t(titleKey(name, params));
  document.title = `${t(titleKey(name, params))} · xai-router`;
  sh.actions.replaceChildren();
  sh.shell.classList.remove('sidebar-open');
  const pageEl = h('div', { class: `page page-${name}` });
  sh.content.replaceChildren(pageEl);
  sh.main.scrollTop = 0;
  const controller = new AbortController();
  state.controller = controller;
  const ctx = {
    params,
    signal: controller.signal,
    alive: () => seq === state.routeSeq,
    setActions: (...nodes) => sh.actions.replaceChildren(...nodes.flat().filter(Boolean)),
    // Breadcrumbs between "xai-router" and the page title, e.g. [{ label: '日志', href: '#/logs' }];
    // pass `title` to replace the last crumb (the page title) as well.
    setCrumbs: (items, title) => { if (seq === state.routeSeq) { setCrumbs(items); if (title != null) sh.title.textContent = title; } },
    navigate: (hash) => { location.hash = hash; },
    // push: true adds a history entry (page / filter changes) so back/forward restores the view;
    // otherwise the current entry is replaced (typing, drawer state).
    setParams: (obj, { push = false } = {}) => {
      if (seq !== state.routeSeq || parseHash().name !== name) return;
      const qs = qsBuild(obj);
      const hash = `#/${name}${qs ? '?' + qs : ''}`;
      if (hash === location.hash) return;
      if (push) history.pushState(null, '', hash); else history.replaceState(null, '', hash);
      state.lastHash = location.hash;
    },
    // Register a handler that applies new params in place (see route()).
    onParams: (fn) => { if (seq === state.routeSeq) state.onParams = fn; },
    // A page can register a "has unsaved changes" check; in-app navigation then asks first.
    setGuard: (fn) => { if (seq === state.routeSeq) state.guard = fn; },
    info: () => state.info,
    refreshInfo: loadInfo,
    // Pages that already fetched /stats/overview pass its `keys` block to update the banner.
    reportKeys: (k) => setKeyHealth(k),
  };
  state.curName = name;
  let r;
  try { r = ROUTES[name].render(pageEl, ctx); } catch (e) { console.error(e); toastError(e); }
  if (r && typeof r.then === 'function') {
    r.then((c) => { if (seq === state.routeSeq) state.cleanup = c; else if (typeof c === 'function') c(); })
      .catch((e) => { if (e.name !== 'AbortError') { console.error(e); toastError(e); } });
  } else state.cleanup = r;
}

// ---------- shell ----------
function themeIcon() { return { system: 'monitor', light: 'sun', dark: 'moon' }[getThemePref()]; }

function prefsButtons(compact = false) {
  const themeBtn = h('button', {
    class: 'btn btn-ghost btn-icon', title: t(`theme.${getThemePref()}`), 'aria-label': t('theme.toggle'),
    onclick: () => { cycleTheme(); themeBtn.replaceChildren(icon(themeIcon(), 17)); themeBtn.title = t(`theme.${getThemePref()}`); },
  }, icon(themeIcon(), 17));
  const langBtn = h('button', {
    class: `btn btn-ghost ${compact ? 'btn-icon' : 'btn-lang'}`, title: t('lang.toggle'), 'aria-label': t('lang.toggle'),
    onclick: () => { setLang(getLang() === 'zh' ? 'en' : 'zh'); rerender(); },
  }, icon('languages', 17), compact ? null : h('span', null, getLang() === 'zh' ? 'EN' : '中文'));
  return [themeBtn, langBtn];
}

function renderShell() {
  state.view = 'shell';
  const nav = h('nav', { class: 'nav' },
    NAV.map((it) => (it.group
      ? h('div', { class: 'nav-group-label', role: 'presentation' }, t(`nav.group.${it.group}`))
      : h('a', { class: 'nav-item', href: it.tab ? `#/${it.route}?tab=${it.tab}` : `#/${it.route}`, 'data-route': it.route, 'data-tab': it.tab || '' },
        icon(it.icon, 17), h('span', null, t(it.label || `nav.${it.route}`))))));
  const sidebar = h('aside', { class: 'sidebar' },
    h('div', { class: 'brand' }, logo(24), h('div', { class: 'brand-text' }, h('div', { class: 'brand-name' }, 'xai-router'), h('div', { class: 'brand-sub' }, t('app.tagline')))),
    nav,
    // Theme and language live in the account menu (v0.5 design: the sidebar foot is just the account).
    h('div', { class: 'sidebar-foot' }, accountButton()));
  const title = h('h1', { class: 'topbar-title crumb-current' });
  const crumbsMid = h('span', { class: 'crumbs-mid', style: { display: 'contents' } });
  const crumbs = h('nav', { class: 'crumbs', 'aria-label': 'breadcrumb' },
    h('a', { class: 'crumb crumb-root', href: '#/overview' }, 'xai-router'),
    h('span', { class: 'crumb-sep', 'aria-hidden': 'true' }),
    crumbsMid, title);
  const actions = h('div', { class: 'topbar-actions' });
  const banner = h('div', { class: 'banner-slot' });
  const content = h('div', { class: 'content' });
  const shell = h('div', { class: 'shell' });
  const main = h('main', { class: 'main' },
    banner,
    h('header', { class: 'topbar' },
      h('button', { class: 'btn btn-ghost btn-icon hamburger', 'aria-label': t('nav.menu'), onclick: () => shell.classList.toggle('sidebar-open') }, icon('menu', 20)),
      h('div', { class: 'topbar-heading' }, crumbs),
      actions),
    content);
  mount(shell, sidebar, h('div', { class: 'sidebar-backdrop', onclick: () => shell.classList.remove('sidebar-open') }), main);
  mount(app, shell);
  state.shell = { shell, nav, title, crumbsMid, actions, content, banner, main };
  const topbar = main.querySelector('.topbar');
  const setTopVar = () => document.documentElement.style.setProperty('--topbar-real', `${topbar.offsetHeight}px`);
  if (state.topRO) state.topRO.disconnect();
  state.topRO = new ResizeObserver(setTopVar);
  state.topRO.observe(topbar);
  setTopVar();
  renderBanner();
}

/** Middle breadcrumbs: [{ label, href? }] rendered as "xai-router / a / b / <title>". */
function setCrumbs(items) {
  const mid = state.shell && state.shell.crumbsMid;
  if (!mid) return;
  mid.replaceChildren(...(items || []).flatMap((it) => [
    it.href ? h('a', { class: 'crumb', href: it.href }, it.label) : h('span', { class: 'crumb' }, it.label),
    h('span', { class: 'crumb-sep', 'aria-hidden': 'true' }),
  ]));
}

function markNav(name, params) {
  const tab = name === 'docs' ? docsTab(params) : '';
  state.shell.nav.querySelectorAll('.nav-item').forEach((a) => a.classList.toggle('active', a.dataset.route === name && (a.dataset.tab || '') === tab));
}

// ---------- account menu (sidebar foot) ----------
function identity() {
  const me = state.me || {};
  const isToken = me.type === 'admin_token';
  return {
    isToken,
    name: isToken ? (me.name || t('auth.adminToken')) : t('account.admin'),
    sub: isToken ? t('auth.adminToken') : t('auth.session'),
    label: isToken ? `${t('auth.adminToken')} · ${me.name || ''}` : t('auth.session'),
  };
}
function accountButton() {
  const id = identity();
  const btn = h('button', {
    class: 'me-btn', type: 'button', 'aria-haspopup': 'menu', 'aria-expanded': 'false', title: id.label,
    // mousedown outside the menu already closes it; remember that so this click does not reopen it.
    onmousedown: () => { btn._wasOpen = btn.getAttribute('aria-expanded') === 'true'; },
    onclick: (e) => {
      e.stopPropagation();
      if (btn._wasOpen) { btn._wasOpen = false; closeMenus(); return; }
      openAccountMenu(btn);
    },
  },
  h('span', { class: 'me-avatar' }, icon(id.isToken ? 'key' : 'lock', 13)),
  h('span', { class: 'me-text' }, h('span', { class: 'me-name' }, id.isToken ? id.label : id.name), h('span', { class: 'me-sub' }, id.isToken ? t('account.viaToken') : id.sub)),
  icon('chevronDown', 15));
  return btn;
}
function openAccountMenu(btn) {
  const id = identity();
  openMenu(btn, [
    !id.isToken ? { icon: 'lock', label: t('account.changePassword'), onClick: () => openPasswordDialog({ onDone: () => loadInfo() }) } : null,
    { icon: 'shield', label: t('nav.access'), onClick: () => { location.hash = '#/access'; } },
    '-',
    // Cycles dark -> light -> follow system; the label shows the current choice.
    { icon: themeIcon(), label: t(`theme.${getThemePref()}`), onClick: () => cycleTheme() },
    { icon: 'languages', label: getLang() === 'zh' ? 'English' : '中文', onClick: () => { setLang(getLang() === 'zh' ? 'en' : 'zh'); rerender(); } },
    '-',
    { icon: 'logout', label: t('auth.logout'), danger: true, onClick: logout },
  ]);
  const pop = document.querySelector('.menu-pop');
  if (!pop) return;
  pop.classList.add('menu-account');
  pop.prepend(h('div', { class: 'menu-head' }, h('strong', null, id.isToken ? id.label : id.name), id.isToken ? t('account.viaToken') : id.sub), h('div', { class: 'menu-sep' }));
  // Open upward, aligned with the sidebar's inner edge.
  const r = btn.getBoundingClientRect();
  pop.style.left = `${Math.max(8, r.left)}px`;
  pop.style.top = `${Math.max(8, r.top - pop.offsetHeight - 6)}px`;
  btn.setAttribute('aria-expanded', 'true');
  const obs = new MutationObserver(() => { if (!pop.isConnected) { btn.setAttribute('aria-expanded', 'false'); obs.disconnect(); } });
  obs.observe(document.body, { childList: true });
}

// ---------- banners ----------
function renderBanner() {
  const slot = state.shell && state.shell.banner;
  if (!slot) return;
  const nodes = [];
  // Red "no usable upstream key" banner first: every request fails while it shows (pm-spec P1-2).
  const k = state.keyHealth;
  if (k && k.total > 0 && !k.active) {
    nodes.push(h('div', { class: 'banner banner-danger', role: 'alert' },
      icon('xCircle', 16),
      h('span', { class: 'banner-text' }, h('strong', null, t('banner.noKeys')), ' ', t('banner.noKeysDesc')),
      h('a', { class: 'btn btn-sm', href: '#/keys' }, t('banner.noKeysBtn'))));
  }
  // Data disk almost full (system/info disk_warning), between the no-keys and password banners.
  const i = state.info;
  if (i && i.disk_warning) {
    const pct = typeof i.disk_used_percent === 'number' ? `${Math.floor(i.disk_used_percent)}%` : '—';
    nodes.push(h('div', { class: 'banner banner-warning banner-disk', role: 'alert' },
      icon('database', 16),
      h('span', { class: 'banner-text' }, t('banner.disk', { p: pct, free: fmtBytes(i.disk_free_bytes) })),
      h('a', { class: 'btn btn-sm', href: '#/data' }, t('banner.diskBtn'))));
  }
  if (state.info && state.info.needs_password_change) {
    nodes.push(h('div', { class: 'banner banner-warning', role: 'alert' },
      icon('alert', 16),
      h('span', { class: 'banner-text' }, t('banner.changePassword')),
      h('button', { class: 'btn btn-sm', type: 'button', onclick: () => openPasswordDialog({ onDone: () => loadInfo() }) }, t('banner.changePasswordBtn'))));
  }
  slot.replaceChildren(...nodes);
}

function setKeyHealth(k) {
  if (!k || typeof k !== 'object') return;
  const prev = state.keyHealth;
  state.keyHealth = { total: k.total || 0, active: k.active || 0 };
  if (!prev || prev.total !== state.keyHealth.total || prev.active !== state.keyHealth.active) renderBanner();
}
let healthBusy = false;
// keys.total / keys.active do not depend on the range; 1h is the cheapest query.
async function checkKeyHealth() {
  if (healthBusy || state.view !== 'shell' || !auth.token) return;
  healthBusy = true;
  try { const o = await get('/api/v1/stats/overview', { range: '1h' }); setKeyHealth(o && o.keys); }
  catch (e) { if (e.status !== 401) console.warn('stats/overview', e); }
  finally { healthBusy = false; }
}
// The disk banner rides along: system/info is refreshed with the key health (route change, every 60 s).
function refreshBanners() {
  checkKeyHealth();
  if (state.view === 'shell' && auth.token) loadInfo();
}
setInterval(() => { if (!document.hidden) refreshBanners(); }, 60000);
document.addEventListener('visibilitychange', () => { if (!document.hidden) refreshBanners(); });

// Concurrent callers (route change + a page asking for fresh info) share one request.
let infoReq = null;
function loadInfo() {
  if (infoReq) return infoReq;
  infoReq = (async () => {
    try { state.info = await get('/api/v1/system/info'); renderBanner(); } catch (e) { if (e.status !== 401) console.warn('system/info', e); }
    finally { infoReq = null; }
    return state.info;
  })();
  return infoReq;
}

function rerender() {
  document.documentElement.lang = getLang() === 'zh' ? 'zh-CN' : 'en';
  if (state.view === 'shell') { teardownPage(); renderShell(); route(); }
  else if (state.view === 'login') showLogin();
}

async function logout() {
  try { await post('/api/v1/auth/logout', {}, { noAuthRedirect: true }); } catch { /* ignore */ }
  auth.clear();
  showLogin();
}

// ---------- login ----------
function showLogin(message) {
  teardownPage();
  closeAllModals();
  state.view = 'login';
  state.shell = null;
  document.title = `${t('auth.signIn')} · xai-router`;
  let mode = 'password';
  const err = h('div', { class: 'login-error', role: 'alert', hidden: !message }, message || '');
  const pw = passwordInput({ class: 'input input-lg', autocomplete: 'current-password', placeholder: t('auth.passwordPlaceholder'), autofocus: true, required: true });
  const inp = pw.input;
  inp.addEventListener('input', () => { err.hidden = true; });
  const label = h('label', { class: 'field-label' }, t('auth.password'));
  const hint = h('div', { class: 'field-hint' }, '');
  const submit = h('button', { class: 'btn btn-primary btn-lg btn-block', type: 'submit' }, h('span', null, t('auth.signIn')));
  const setMode = (m) => {
    mode = m;
    err.hidden = true;
    label.textContent = m === 'password' ? t('auth.password') : t('auth.adminToken');
    inp.placeholder = m === 'password' ? t('auth.passwordPlaceholder') : 'xra-…';
    inp.type = m === 'password' ? 'password' : 'text';
    pw.toggle.hidden = m !== 'password';
    pw.toggle.replaceChildren(icon('eye', 15));
    inp.autocomplete = m === 'password' ? 'current-password' : 'off';
    inp.classList.toggle('mono', m === 'token');
    hint.textContent = m === 'token' ? t('auth.tokenHint') : '';
    inp.value = ''; inp.focus();
  };
  const form = h('form', {
    class: 'login-form',
    onsubmit: async (e) => {
      e.preventDefault();
      const v = inp.value.trim();
      if (!v) { inp.focus(); return; }
      err.hidden = true;
      await busy(submit, async () => {
        try {
          if (mode === 'password') {
            const res = await post('/api/v1/auth/login', { password: inp.value }, { noAuthRedirect: true, token: '' });
            auth.set(res.token);
          } else {
            await get('/api/v1/auth/me', null, { noAuthRedirect: true, token: v });
            auth.set(v);
          }
          await boot();
        } catch (ex) {
          err.textContent = ex.status === 401 ? (mode === 'password' ? t('auth.badPassword') : t('auth.badToken')) : ex.status === 429 ? t('auth.tooMany') : errorMessage(ex);
          err.hidden = false;
          inp.select();
        }
      });
    },
  },
  segmented({ options: [{ value: 'password', label: t('auth.password'), icon: 'lock' }, { value: 'token', label: t('auth.adminToken'), icon: 'key' }], value: 'password', onChange: setMode, size: 'seg-block' }),
  h('div', { class: 'field' }, label, pw, hint),
  err,
  submit);
  mount(app, h('div', { class: 'login' },
    h('div', { class: 'login-bg' }),
    h('div', { class: 'login-card' },
      h('div', { class: 'login-brand' }, logo(40), h('div', null, h('div', { class: 'login-title' }, 'xai-router'), h('div', { class: 'login-sub' }, t('auth.consoleTitle')))),
      form),
    h('div', { class: 'login-tools' }, prefsButtons())));
  setTimeout(() => inp.focus(), 30);
}

// ---------- boot ----------
async function boot() {
  applyTheme();
  document.documentElement.lang = getLang() === 'zh' ? 'zh-CN' : 'en';
  if (!auth.token) { showLogin(); return; }
  try {
    state.me = await get('/api/v1/auth/me');
  } catch (e) {
    if (e.status === 401) return; // handler shows login
    mount(app, h('div', { class: 'fatal' },
      h('div', { class: 'fatal-card' }, icon('alert', 28), h('h2', null, t('app.unreachable')), h('p', { class: 'muted' }, errorMessage(e)),
        h('button', { class: 'btn btn-primary', onclick: boot }, icon('refresh', 15), h('span', null, t('common.retry'))))));
    return;
  }
  renderShell();
  route(); // also loads system/info for the banners
}

onUnauthorized(() => { if (state.view !== 'login') showLogin(t('auth.sessionExpired')); });
let guardBypass = false;
async function onHashChange() {
  if (state.view === 'shell' && !guardBypass && state.guard && location.hash !== state.lastHash) {
    let dirty = false;
    try { dirty = state.guard(); } catch { dirty = false; }
    if (dirty) {
      const target = location.hash;
      history.replaceState(null, '', state.lastHash || '#/overview');
      const ok = await confirmDialog({ title: t('guard.title'), message: t('guard.msg'), confirmText: t('guard.leave'), danger: true });
      if (!ok) return;
      guardBypass = true;
      location.hash = target;
      return;
    }
  }
  guardBypass = false;
  route();
}
window.addEventListener('hashchange', onHashChange);

// live relative times + countdowns
setInterval(() => {
  const now = Date.now();
  document.querySelectorAll('[data-countdown]').forEach((el) => {
    const left = (new Date(el.dataset.countdown).getTime() - now) / 1000;
    el.textContent = left > 0 ? fmtDuration(left) : t('keys.cooldownEnding');
  });
}, 1000);
setInterval(() => {
  document.querySelectorAll('[data-rel]').forEach((el) => { el.textContent = relTime(el.dataset.rel); });
}, 30000);

boot();
