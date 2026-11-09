/**
 * Fetch wrapper with JWT injection and error handling.
 *
 *   - get/post/put/del: throws on non-2xx. Pair with try/catch.
 *   - latest(fn): sequence guard for paginated/concurrent loaders.
 */

import { getLocale } from './i18n.js';

export const TOKEN_KEY = 'of_token';
export const SESSION_EXPIRED_KEY = 'of_session_expired';

let sessionEnding = false;

/**
 * The server rejected our token. Clear the session and reload onto /login: a
 * hard navigation, not a route change, because the shell (command palette,
 * event stream, connection banner) was built for a session that no longer
 * exists and would otherwise keep running over the login screen.
 */
function expireSession() {
  if (sessionEnding) return;
  sessionEnding = true;
  localStorage.removeItem(TOKEN_KEY);
  localStorage.removeItem('of_user');
  try { sessionStorage.setItem(SESSION_EXPIRED_KEY, '1'); } catch { /* storage blocked */ }
  window.location.href = '/login';
}

function getToken() {
  return localStorage.getItem(TOKEN_KEY);
}

/**
 * Names the interface language on every request. The server resolves the
 * assistant's reply language from it for users who follow the interface; it is
 * the only place the choice made in the browser reaches the server.
 * Must match replylang.Header in internal/replylang.
 */
export const LOCALE_HEADER = 'X-Of-Locale';

async function request(method, url, body, opts = {}) {
  const headers = { 'Content-Type': 'application/json', [LOCALE_HEADER]: getLocale(), ...(opts.headers || {}) };
  const token = getToken();
  if (token) headers['Authorization'] = `Bearer ${token}`;

  const fetchOpts = { method, headers };
  if (body !== undefined && method !== 'GET') {
    fetchOpts.body = JSON.stringify(body);
  }
  if (opts.signal) fetchOpts.signal = opts.signal;

  const res = await fetch(url, fetchOpts);

  // A 401 from the login call itself is a wrong password, not an expired session:
  // fall through so the server's own message ("invalid credentials") reaches the form.
  if (res.status === 401 && !url.includes('/auth/login')) {
    expireSession();
    const err = new Error('Unauthorized');
    err.status = 401;
    throw err;
  }

  const text = await res.text();
  let data;
  try { data = JSON.parse(text); } catch { data = text; }

  if (!res.ok) {
    const errMsg = data?.error || data?.message || `Request failed (${res.status})`;
    const err = new Error(errMsg);
    err.status = res.status;
    err.data = data;
    // The server stamps every response with X-Request-ID and logs it; carrying it
    // on the error lets the UI show a reference the user can quote in a bug report.
    err.requestId = res.headers.get('X-Request-Id') || '';
    throw err;
  }

  return data;
}

export function get(url, opts) { return request('GET', url, undefined, opts); }
export function post(url, body, opts) { return request('POST', url, body, opts); }
export function put(url, body, opts) { return request('PUT', url, body, opts); }
export function del(url, opts) { return request('DELETE', url, undefined, opts); }

/**
 * Fetch wrapper for non-JSON responses (streaming bodies, blob downloads, form
 * uploads): same JWT injection and 401 -> login handling as get/post, but
 * returns the raw Response so the caller can read .body / .blob() / .text()
 * itself. Throws on a non-2xx after extracting an { error }/{ message } body.
 * Use this instead of a bare fetch() so no call site re-implements auth.
 */
export async function rawFetch(url, opts = {}) {
  const headers = { [LOCALE_HEADER]: getLocale(), ...(opts.headers || {}) };
  const token = getToken();
  if (token) headers['Authorization'] = `Bearer ${token}`;

  const res = await fetch(url, { ...opts, headers });

  if (res.status === 401) {
    expireSession();
    const err = new Error('Unauthorized');
    err.status = 401;
    throw err;
  }
  if (!res.ok) {
    let msg = `Request failed (${res.status})`;
    try {
      const data = JSON.parse(await res.clone().text());
      msg = data?.error || data?.message || msg;
    } catch { /* non-JSON error body; keep the status message */ }
    const err = new Error(msg);
    err.status = res.status;
    err.requestId = res.headers.get('X-Request-Id') || '';
    throw err;
  }
  return res;
}

/**
 * Download a URL as a file via the authenticated rawFetch path, then trigger a
 * browser save. Centralizes the blob + temporary <a> dance, so views don't
 * hand-roll it with a bare fetch and miss 401 handling. A server-provided
 * Content-Disposition filename wins; `fallbackName` is used when there is none.
 */
export async function download(url, fallbackName) {
  const res = await rawFetch(url);
  const blob = await res.blob();
  const name = filenameFromDisposition(res.headers.get('Content-Disposition')) || fallbackName || '';
  const objUrl = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = objUrl;
  if (name) a.download = name;
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(objUrl);
}

// filenameFromDisposition pulls the filename out of a Content-Disposition header
// (e.g. attachment; filename="my-app.of"), or returns null.
function filenameFromDisposition(value) {
  if (!value) return null;
  const match = /filename="?([^"]+)"?/.exec(value);
  return match ? match[1] : null;
}

/**
 * Sequence guard for paginated/concurrent loaders: wraps an async fn so only
 * the most recent invocation's result is delivered. Resolves to
 * `{ stale: false, data }` or `{ stale: true }` when superseded.
 */
export function latest(fn) {
  let seq = 0;
  return async function (...args) {
    const my = ++seq;
    const data = await fn(...args);
    if (my !== seq) return { stale: true };
    return { stale: false, data };
  };
}
