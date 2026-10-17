// Entry point: auth gate, app shell, hash router.
import { h, mount, relTime, fmtDuration, qsParse, qsBuild } from './util.js';
import { icon, logo } from './icons.js';
import { t, getLang, setLang } from './i18n.js';
import { auth, onUnauthorized, get, post } from './api.js';
import { toastError, segmented, closeAllModals, closeMenus, busy, confirmDialog, passwordInput, errorMessage } from './ui.js';
import { applyTheme, cycleTheme, getThemePref } from './theme.js';
import * as overview from './pages/overview.js';
import * as keys from './pages/keys.js';
import * as tokens from './pages/tokens.js';
import * as models from './pages/models.js';
import * as logs from './pages/logs.js';
import * as settings from './pages/settings.js';
import { openPasswordDialog } from './pages/settings.js';
import * as docs from './pages/docs.js';

const ROUTES = {
  overview: { page: overview, icon: 'overview' },
  keys: { page: keys, icon: 'key' },
  tokens: { page: tokens, icon: 'ticket' },
  models: { page: models, icon: 'cpu' },
  logs: { page: logs, icon: 'logs' },
  settings: { page: settings, icon: 'settings' },
  docs: { page: docs, icon: 'book' },
};

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
  state.lastHash = location.hash;
  const { name, params } = parseHash();
  if (!ROUTES[name]) { location.replace('#/overview'); return; }
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
  sh.nav.querySelectorAll('.nav-item').forEach((a) => a.classList.toggle('active', a.dataset.route === name));
  sh.title.textContent = t(`nav.${name}`);
  sh.subtitle.textContent = t(`page.${name}.subtitle`);
  document.title = `${t(`nav.${name}`)} · xai-router`;
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
  };
  state.curName = name;
  let r;
  try { r = ROUTES[name].page.render(pageEl, ctx); } catch (e) { console.error(e); toastError(e); }
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
    Object.entries(ROUTES).map(([name, r]) => [
      name === 'settings' ? h('div', { class: 'nav-sep' }) : null,
      h('a', { class: 'nav-item', href: `#/${name}`, 'data-route': name }, icon(r.icon, 17), h('span', null, t(`nav.${name}`))),
    ]));
  const meLabel = state.me ? (state.me.type === 'admin_token' ? `${t('auth.adminToken')} · ${state.me.name || ''}` : t('auth.session')) : '';
  const logoutBtn = h('button', { class: 'btn btn-ghost btn-icon', title: t('auth.logout'), 'aria-label': t('auth.logout'), onclick: logout }, icon('logout', 17));
  const sidebar = h('aside', { class: 'sidebar' },
    h('div', { class: 'brand' }, logo(28), h('div', { class: 'brand-text' }, h('div', { class: 'brand-name' }, 'xai-router'), h('div', { class: 'brand-sub' }, t('app.tagline')))),
    nav,
    h('div', { class: 'sidebar-foot' },
      h('div', { class: 'me' }, h('span', { class: 'me-dot' }), h('span', { class: 'me-label', title: meLabel }, meLabel)),
      h('div', { class: 'sidebar-tools' }, prefsButtons(), logoutBtn)));
  const title = h('h1', { class: 'topbar-title' });
  const subtitle = h('p', { class: 'topbar-subtitle' });
  const actions = h('div', { class: 'topbar-actions' });
  const banner = h('div', { class: 'banner-slot' });
  const content = h('div', { class: 'content' });
  const shell = h('div', { class: 'shell' });
  const main = h('main', { class: 'main' },
    banner,
    h('header', { class: 'topbar' },
      h('button', { class: 'btn btn-ghost btn-icon hamburger', 'aria-label': t('nav.menu'), onclick: () => shell.classList.toggle('sidebar-open') }, icon('menu', 20)),
      h('div', { class: 'topbar-heading' }, title, subtitle),
      actions),
    content);
  mount(shell, sidebar, h('div', { class: 'sidebar-backdrop', onclick: () => shell.classList.remove('sidebar-open') }), main);
  mount(app, shell);
  state.shell = { shell, nav, title, subtitle, actions, content, banner, main };
  const topbar = main.querySelector('.topbar');
  const setTopVar = () => document.documentElement.style.setProperty('--topbar-real', `${topbar.offsetHeight}px`);
  if (state.topRO) state.topRO.disconnect();
  state.topRO = new ResizeObserver(setTopVar);
  state.topRO.observe(topbar);
  setTopVar();
  renderBanner();
}

function renderBanner() {
  const slot = state.shell && state.shell.banner;
  if (!slot) return;
  if (state.info && state.info.needs_password_change) {
    mount(slot, h('div', { class: 'banner banner-warning', role: 'alert' },
      icon('alert', 16),
      h('span', { class: 'banner-text' }, t('banner.changePassword')),
      h('button', { class: 'btn btn-sm', type: 'button', onclick: () => openPasswordDialog({ onDone: () => loadInfo() }) }, t('banner.changePasswordBtn'))));
  } else slot.replaceChildren();
}

async function loadInfo() {
  try { state.info = await get('/api/v1/system/info'); renderBanner(); } catch (e) { if (e.status !== 401) console.warn('system/info', e); }
  return state.info;
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
  route();
  loadInfo();
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
