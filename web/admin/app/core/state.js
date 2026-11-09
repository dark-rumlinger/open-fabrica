/**
 * Lightweight reactive store with watchers.
 */

import { t } from './i18n.js';

const store = {};
const watchers = {};

/**
 * Get a value from the store.
 */
export function get(key) {
  return store[key];
}

/**
 * Set a value in the store and notify watchers.
 */
export function set(key, value) {
  const old = store[key];
  store[key] = value;

  if (watchers[key]) {
    for (const cb of watchers[key]) {
      cb(value, old);
    }
  }
}

/**
 * Watch for changes to a key. Returns an unwatch function.
 */
export function watch(key, callback) {
  if (!watchers[key]) {
    watchers[key] = [];
  }
  watchers[key].push(callback);

  return () => {
    watchers[key] = watchers[key].filter(cb => cb !== callback);
  };
}

/**
 * Watch a key and auto-unwatch when the given scope is disposed.
 * Drop-in replacement for `watch(...)` inside views with a lifecycle scope.
 */
export function select(key, callback, scope) {
  const off = watch(key, callback);
  if (scope && typeof scope.add === 'function') scope.add(off);
  return off;
}

/**
 * Returns the logged-in user object, or an empty object when not signed in
 * or the cached payload is unreadable.
 */
function currentUser() {
  try {
    return JSON.parse(localStorage.getItem('of_user') || '{}');
  } catch {
    return {};
  }
}

/**
 * Returns a friendly first name for the logged-in user, or an empty string
 * when no name is available. Splits display_name on whitespace so greetings
 * stay short ("Hi Ada" rather than "Hi Ada Lovelace").
 */
export function displayName() {
  const u = currentUser();
  const raw = (u.display_name || u.username || '').trim();
  if (!raw) return '';
  return raw.split(/\s+/)[0];
}

/**
 * Returns a time-of-day greeting ("Good morning" / "Good afternoon" /
 * "Good evening") based on the local clock. Used to open the chat warmly.
 */
export function greeting() {
  const hr = new Date().getHours();
  if (hr < 12) return t('greeting.morning');
  if (hr < 18) return t('greeting.afternoon');
  return t('greeting.evening');
}

/**
 * The signed-in user's id, or null when unknown. Lets lists mark "you" and
 * keep people from deleting or demoting themselves.
 */
export function currentUserId() {
  const id = currentUser().id;
  return typeof id === 'number' ? id : null;
}

/**
 * Returns true if the current user has the admin role.
 */
export function isAdmin() {
  return currentUser().role === 'admin';
}
