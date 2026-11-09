/**
 * Session teardown. Lifted out of the sidebar unchanged when the sidebar was
 * replaced by the top bar.
 */

import { post, TOKEN_KEY } from '../core/http.js';

/**
 * Sign the current admin out: tell the server to invalidate the session,
 * clear the locally stored credentials, and return to the login screen.
 * The server call is best effort; the local session is always cleared.
 */
export async function logout() {
  try {
    await post('/admin/api/auth/logout');
  } catch {
    // Clear the local session regardless of the server response.
  }
  localStorage.removeItem(TOKEN_KEY);
  localStorage.removeItem('of_user');
  // Hard navigation, not a route change: the whole app shell was built for a
  // session that no longer exists.
  window.location.href = '/login';
}
