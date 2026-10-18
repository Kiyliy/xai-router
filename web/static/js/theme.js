// Theme: 'dark' (default, v0.5 design) | 'light' | 'system' (follow prefers-color-scheme), persisted in localStorage.
// <html data-theme> always carries the effective theme; style.css defines dark on :root and light on [data-theme="light"].
const KEY = 'xr.theme';
const mq = window.matchMedia('(prefers-color-scheme: dark)');

export function getThemePref() {
  try { const v = localStorage.getItem(KEY); if (v === 'light' || v === 'dark' || v === 'system') return v; } catch { /* ignore */ }
  return 'dark';
}
export function effectiveTheme() {
  const p = getThemePref();
  return p === 'system' ? (mq.matches ? 'dark' : 'light') : p;
}
export function applyTheme() {
  const root = document.documentElement;
  const eff = effectiveTheme();
  root.setAttribute('data-theme', eff);
  root.style.colorScheme = eff;
  document.dispatchEvent(new CustomEvent('xr:themechange', { detail: eff }));
}
export function setThemePref(v) {
  try { localStorage.setItem(KEY, v); } catch { /* ignore */ }
  applyTheme();
}
export function cycleTheme() {
  const order = ['dark', 'light', 'system'];
  setThemePref(order[(order.indexOf(getThemePref()) + 1) % order.length]);
}
mq.addEventListener('change', () => { if (getThemePref() === 'system') applyTheme(); });
