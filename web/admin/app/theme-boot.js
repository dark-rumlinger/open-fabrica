/**
 * Pre-paint theme bootstrap.
 *
 * Loaded as a CLASSIC script (no type="module", no defer) from the <head>
 * before the stylesheets, so it is parser-blocking and runs before any of them
 * applies. app/main.js is a module and therefore deferred - it runs after first
 * paint, so theme.init() alone would flash the default palette at anyone who
 * picked the other one.
 *
 * It is a separate file rather than inline because the admin panel is served
 * under `script-src 'self'` with no 'unsafe-inline'; a hash would work too, but
 * it would break silently on any edit to the script.
 *
 * With no saved choice it applies the dark default. Keep the storage key and default in
 * step with THEME_KEY and DEFAULT_THEME in app/core/theme.js.
 */
(function () {
  var t = null;
  try {
    t = localStorage.getItem('of_theme');
  } catch (e) {
    // Storage blocked (private window, cookies off): no saved choice.
  }
  if (!t) {
    t = 'dark';
  }
  document.documentElement.setAttribute('data-theme', t);
})();
