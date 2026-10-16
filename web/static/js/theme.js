// Theme: 'system' (follow prefers-color-scheme) | 'light' | 'dark', persisted in localStorage.
const KEY = 'xr.theme';
const mq = window.matchMedia('(prefers-color-scheme: dark)');

export function getThemePref() {
  try { const v = localStorage.getItem(KEY); if (v === 'light' || v === 'dark' || v === 'system') return v; } catch { /* ignore */ }
  return 'system';
}
export function effectiveTheme() {
  const p = getThemePref();
  return p === 'system' ? (mq.matches ? 'dark' : 'light') : p;
}
export function applyTheme() {
  const p = getThemePref();
  const root = document.documentElement;
  if (p === 'system') root.removeAttribute('data-theme'); else root.setAttribute('data-theme', p);
  root.style.colorScheme = effectiveTheme();
  document.dispatchEvent(new CustomEvent('xr:themechange', { detail: effectiveTheme() }));
}
export function setThemePref(v) {
  try { localStorage.setItem(KEY, v); } catch { /* ignore */ }
  applyTheme();
}
export function cycleTheme() {
  const order = ['system', 'light', 'dark'];
  setThemePref(order[(order.indexOf(getThemePref()) + 1) % order.length]);
}
mq.addEventListener('change', () => { if (getThemePref() === 'system') applyTheme(); });
