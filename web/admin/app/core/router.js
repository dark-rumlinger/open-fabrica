/**
 * History-based SPA router with path parameter support.
 *
 * Real paths, no #fragment: the admin server already serves index.html for any
 * path it has no file for (registerAdminSPA in server/admin/routes.go), so a
 * cold load of /projects/<id>/pages works. That also means every asset in
 * index.html must be referenced root-absolutely - a relative href would resolve
 * against the current path and get the fallback HTML back.
 *
 * pushState does not fire popstate, so resolve() dispatches a `routechange`
 * window event for the shell pieces that re-render on navigation.
 *
 * Note: this ties the panel to being mounted at the origin root. Hash routing
 * would survive being reverse-proxied under a sub-path; this does not, without
 * a base path.
 */

const routes = [];
let notFoundHandler = null;
let activeController = null;

function destroyActive() {
  if (activeController && typeof activeController.destroy === 'function') {
    try { activeController.destroy(); }
    catch (e) { console.error('[router] view destroy threw', e); }
  }
  activeController = null;
}

/**
 * Register a route pattern with a handler function.
 * Supports :param style path parameters, e.g. /projects/:id
 */
export function register(path, handler) {
  const paramNames = [];
  const pattern = path.replace(/:([^/]+)/g, (_, name) => {
    paramNames.push(name);
    return '([^/]+)';
  });
  const regex = new RegExp(`^${pattern}$`);
  routes.push({ path, regex, paramNames, handler });
}

/**
 * Set a handler for unmatched routes.
 */
export function setNotFound(handler) {
  notFoundHandler = handler;
}

/**
 * Navigate to a path programmatically, adding a history entry.
 * Navigating to where you already are is a no-op, so a repeated click does not
 * stack duplicate entries for the back button to chew through.
 */
export function navigate(path) {
  if (path === currentPath()) return;
  window.history.pushState({}, '', path);
  resolve();
}

/**
 * Get the current path.
 */
export function currentPath() {
  return window.location.pathname || '/';
}

/**
 * Resolve the current path, invoke the matching handler, then tell the shell.
 *
 * The swap is wrapped in a view transition where the browser has one, which
 * cross-fades the old screen into the new instead of snapping. runRoute() and
 * the routechange event keep their order inside the callback, so the only
 * difference downstream is that both happen a frame later. Async mounts
 * resolve after the transition captures, which is intended: the skeleton a
 * view renders first is the state being transitioned to.
 */
function resolve() {
  const swap = () => {
    runRoute();
    window.dispatchEvent(new CustomEvent('routechange'));
  };

  const reduce = typeof window.matchMedia === 'function'
    && window.matchMedia('(prefers-reduced-motion: reduce)').matches;

  if (reduce || typeof document.startViewTransition !== 'function') {
    swap();
    return;
  }

  try {
    // .finished rejects when a transition is superseded by a faster click.
    document.startViewTransition(swap).finished.catch(() => {});
  } catch {
    swap();
  }
}

function runRoute() {
  const path = currentPath();

  for (const route of routes) {
    const match = path.match(route.regex);
    if (match) {
      const params = {};
      let valid = true;
      route.paramNames.forEach((name, i) => {
        const val = decodeURIComponent(match[i + 1]);
        // Validate numeric params (id, projectId, etc.)
        if (/id$/i.test(name) && !/^\d+$/.test(val)) {
          valid = false;
        }
        params[name] = val;
      });
      if (!valid) {
        runNotFound();
        return;
      }
      destroyActive();
      adopt(route.handler(params));
      return;
    }
  }

  runNotFound();
}

function runNotFound() {
  destroyActive();
  // The fallback mounts a real view, so its controller is adopted like any
  // other - otherwise navigating away from it leaks the view's scope.
  if (notFoundHandler) adopt(notFoundHandler());
}

function adopt(result) {
  if (result && typeof result.destroy === 'function') {
    activeController = result;
  }
}

/**
 * Start listening for history navigation (back/forward).
 */
export function start() {
  window.addEventListener('popstate', resolve);
  resolve();
}

// First segments the shell serves; anything else (uploads, API paths) is a
// plain link and keeps the browser's default.
const APP_ROUTE_RE = /^\/(new|projects|providers|messaging-apps|service-providers|settings|usage|users|questions)(\/|$)/;

/**
 * Map a link href to a router path, or null when it is not an in-app link.
 * Chat messages stored before the move to real paths carry the hash form
 * (/#/projects/1/settings), which this router never resolves; those are
 * rewritten here, and the old Share link lands on Sharing rather than General.
 */
export function appPath(href) {
  if (typeof href !== 'string') return null;
  const legacy = href.startsWith('/#/');
  const path = legacy ? href.slice(2) : href;
  if (!APP_ROUTE_RE.test(path)) return null;
  return legacy && /^\/projects\/\d+\/settings$/.test(path) ? `${path}-sharing` : path;
}

/**
 * Turn clicks on links marked data-route into router navigations, so in-app
 * links from chat content switch views without a reload or a new tab. Modified
 * clicks (new tab, new window) keep the browser's default.
 */
export function interceptRouteLinks() {
  document.addEventListener('click', (e) => {
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    const link = e.target instanceof Element ? e.target.closest('a[data-route]') : null;
    const path = link ? appPath(link.getAttribute('href')) : null;
    if (!path) return;
    e.preventDefault();
    navigate(path);
  });
}

/**
 * Extract the active project ID from the current path.
 * Returns null if not on a project route.
 */
export function getActiveProjectId() {
  const match = currentPath().match(/^\/projects\/(\d+)/);
  return match ? parseInt(match[1], 10) : null;
}
