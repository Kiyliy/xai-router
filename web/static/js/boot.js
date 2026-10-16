// Apply the saved theme before first paint to avoid a flash. Loaded as a
// classic (blocking) script from <head>.
(function () {
  try {
    var v = localStorage.getItem('xr.theme');
    if (v === 'light' || v === 'dark') document.documentElement.setAttribute('data-theme', v);
  } catch (e) {}
})();
