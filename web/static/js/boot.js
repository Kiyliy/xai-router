// Apply the saved theme before first paint to avoid a flash. Loaded as a
// classic (blocking) script from <head>. Dark is the default (see theme.js).
(function () {
  var t = 'dark';
  try {
    var v = localStorage.getItem('xr.theme');
    if (v === 'light') t = 'light';
    else if (v === 'system' && window.matchMedia && !window.matchMedia('(prefers-color-scheme: dark)').matches) t = 'light';
  } catch (e) {}
  document.documentElement.setAttribute('data-theme', t);
})();
