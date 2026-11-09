/* Open Fabrica auth bootstrap.
   PLATFORM-MANAGED: the platform owns this file and your pages load it at runtime
   - do not edit, recreate, or delete it. Import the helpers by specifier:
     import { requireAuth, apiFetch, logout } from 'auth';
   Never fetch /me or write your own guard functions. See the auth_contract guide. */

// Drain a token from the URL fragment (magic-link / OAuth return), then load the
// signed-in user ONCE. This runs at module evaluation; the module evaluates a
// single time, so authReady is one in-flight /me promise shared by every importer.
var _m = location.hash.match(/[#&]auth_token=([^&]+)/);
if (_m) {
  try { localStorage.setItem('auth_token', decodeURIComponent(_m[1])); } catch (e) {}
  history.replaceState(null, '', location.pathname + location.search);
}
var _token = null;
try { _token = localStorage.getItem('auth_token'); } catch (e) {}

// user is a LIVE-BINDING export: null until /me resolves, then the signed-in user
// (or null). Importers read the current value; never gate on a synchronous read at
// module-eval time - await authReady / requireAuth() instead.
export let user = null;

export const authReady = _token
  ? fetch('/api/__AUTH_PATH__/me', { headers: { 'Authorization': 'Bearer ' + _token } })
      .then(function (r) {
        // A rejected token (401/403) is authoritatively dead: drop it here so
        // it cannot trip a presence-only guard and bounce. Any OTHER non-OK
        // (a transient 5xx) and a network error (caught below) must NOT log the
        // user out - keep the token and resolve to null for this load.
        if (r.status === 401 || r.status === 403) { try { localStorage.removeItem('auth_token'); } catch (e) {} return null; }
        if (!r.ok) { return null; }
        return r.json();
      })
      .then(function (u) { user = u || null; return user; })
      .catch(function () { return null; })
  : Promise.resolve(null);

export function requireRole(role) {
  return authReady.then(function (u) {
    // admin passes every role gate, exactly as the server's required_role check does.
    if (u && (!role || u.role === role || u.role === 'admin')) return u;
    // Unauthenticated -> /login. Authenticated but wrong role -> home, never
    // /login: bouncing a logged-in user to /login and back is the flash loop.
    var dest = u ? '/' : '/login';
    if (location.pathname !== dest) location.href = dest;
    return null;
  });
}

export function requireAuth() { return requireRole(null); }

export function redirectIfLoggedIn(target) {
  // Gate on the validated user, not mere token presence: a stale token was
  // already cleared by the bootstrap above, so user is null and the visitor
  // stays on /login to sign in fresh. Reuses the in-flight /me promise.
  return authReady.then(function (u) {
    if (u) location.href = target || '/';
    return u;
  });
}

export function apiFetch(path, options) {
  options = options || {};
  var t = null;
  try { t = localStorage.getItem('auth_token'); } catch (e) {}
  var headers = Object.assign({}, options.headers || {});
  if (t) headers['Authorization'] = 'Bearer ' + t;
  // Serialize into a LOCAL, never back into options: the caller may reuse the
  // same options object, and writing the string back would make the second call
  // see a non-object body, skip Content-Type, and POST JSON the server reads as
  // something else.
  // JSON-encode only a plain object or array. FormData, Blob/File, URLSearchParams and
  // binary buffers pass through untouched so the browser sends them as they are.
  var body = options.body;
  if (body && typeof body === 'object' &&
      (Array.isArray(body) || Object.prototype.toString.call(body) === '[object Object]')) {
    headers['Content-Type'] = 'application/json';
    body = JSON.stringify(body);
  }
  var url = path.indexOf('/') === 0 ? path : '/api/' + path;
  return fetch(url, Object.assign({}, options, { headers: headers, body: body })).then(function (r) {
    if (r.status === 401) {
      try { localStorage.removeItem('auth_token'); } catch (e) {}
      user = null;
      if (location.pathname !== '/login') location.href = '/login';
    }
    return r;
  });
}

export function logout() {
  try { localStorage.removeItem('auth_token'); } catch (e) {}
  user = null;
  if (location.pathname !== '/login') location.href = '/login';
}

export default {
  get user() { return user; },
  authReady: authReady,
  requireRole: requireRole,
  requireAuth: requireAuth,
  redirectIfLoggedIn: redirectIfLoggedIn,
  apiFetch: apiFetch,
  logout: logout
};
