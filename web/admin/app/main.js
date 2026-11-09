import './core/i18n.js';
import { t } from './core/i18n.js';
import * as router from './core/router.js';
import * as theme from './core/theme.js';
import * as state from './core/state.js';
import { SSEClient } from './core/sse.js';
import { get, TOKEN_KEY } from './core/http.js';
import { bumpPending, setPending, setPendingFromList, totalPending, watchPending } from './core/questions.js';
import { render, clear, h } from './core/dom.js';
import { wireBrainBus, wireToolEvents, wireReconnectBackfill } from './core/engine-bus.js';
import { addRecent, setActiveProject } from './ui/project-switcher.js';
import { attachConnectionBanner } from './ui/connection-banner.js';
import { setPhaseBadge } from './ui/phase-badge.js';
import { setPending as setPendingTabIndicator } from './ui/tab-indicator.js';

import { createLayout } from './ui/layout.js';
import { createAuthShell, authCard } from './ui/auth-shell.js';
import { mountCommandPalette } from './ui/command-palette.js';
import { renderLogin } from './views/login.js';
import { renderSetup } from './views/setup/index.js';
import * as toast from './ui/toast.js';
import { renderHome } from './views/home.js';
import { renderProjects } from './views/projects.js';
import { renderProjectDetail } from './views/project/index.js';
import { renderLLMProviders, renderImageProviders, renderStockProviders, renderSearchProviders } from './views/global/providers.js';
import { renderServiceProviders } from './views/global/service-providers.js';
import { renderSettings } from './views/global/settings.js';
import { renderUsage } from './views/global/usage.js';
import { renderUsers } from './views/global/users.js';
import { renderAudit } from './views/global/audit.js';
import { renderQuestions } from './views/global/questions.js';
import { renderMessagingApps } from './views/global/messaging-apps.js';

let sse = null;
let layout = null;

function renderConnectionError(app) {
  const { card } = authCard({
    compact: true,
    title: t('connection.title'),
    body: [
      h('p', { className: 'auth-card__lede' }, t('connection.body')),
    ],
    footer: [
      h('button', { className: 'btn btn--primary', onClick: () => boot() }, t('common.retry')),
    ],
  });
  render(app, createAuthShell({ children: [card], tagline: false }).root);
}

// showSetupOrLogin routes an unauthenticated (or newly-invalidated) session to
// the right first screen: the setup wizard when the instance still needs
// configuring, the connection-error screen when the server is unreachable, or
// the login form otherwise. Shared by the no-token and rejected-token paths in
// boot() so the two stay in lockstep.
async function showSetupOrLogin(app) {
  try {
    const check = await get('/admin/api/setup/check');
    if (check.needs_setup) { renderSetup(app, { needsSetupToken: !!check.needs_setup_token }); return; }
  } catch {
    renderConnectionError(app);
    return;
  }
  renderLogin(app);
}

async function boot() {
  theme.init();

  const app = document.getElementById('app');
  const token = localStorage.getItem(TOKEN_KEY);

  if (!token) {
    await showSetupOrLogin(app);
    return;
  }

  try {
    const status = await get('/admin/api/system/status');
    state.set('systemStatus', status);
  } catch (err) {
    // Only a rejected token ends the session. A 5xx, a timeout or a dropped
    // network is a connection problem: keep the token so Retry can recover.
    if (err && err.status === 401) {
      localStorage.removeItem(TOKEN_KEY);
      await showSetupOrLogin(app);
    } else {
      renderConnectionError(app);
    }
    return;
  }

  try {
    const projects = await get('/admin/api/projects');
    state.set('projects', projects);
  } catch {
    state.set('projects', []);
  }

  state.set('projectActivity', {});

  layout = createLayout();
  render(app, layout.root);

  // Ctrl/Cmd-K over the projects, the menu's routes and a few actions.
  // Mounted once per boot, after the shell exists, and outlives every view.
  mountCommandPalette();

  registerRoutes(layout.main);

  window.addEventListener('routechange', trackActiveProject);
  trackActiveProject();

  // The composer is the front door, but it gets an explicit name rather than
  // living at the bare root. replaceState so back doesn't bounce off /.
  if (router.currentPath() === '/') {
    window.history.replaceState({}, '', '/new');
  }
  router.interceptRouteLinks();
  router.start();

  connectSSE();
}

function trackActiveProject() {
  const projectId = router.getActiveProjectId();
  if (projectId) {
    setActiveProject(projectId);
    addRecent(projectId);
  }
}

function registerRoutes(mainContent) {
  router.register('/new', () => renderHome(mainContent));

  router.register('/login', () => {
    if (localStorage.getItem(TOKEN_KEY)) { router.navigate('/new'); return; }
    const app = document.getElementById('app');
    clear(app);
    renderLogin(app);
  });

  router.register('/projects', () => renderProjects(mainContent));
  router.register('/projects/:id/:tab', (params) => renderProjectDetail(mainContent, params));
  router.register('/projects/:id', (params) => router.navigate(`/projects/${params.id}/chat`));
  router.register('/providers', () => router.navigate('/providers/llm'));
  router.register('/providers/llm', () => renderLLMProviders(mainContent));
  router.register('/providers/image', () => renderImageProviders(mainContent));
  router.register('/providers/stock', () => renderStockProviders(mainContent));
  router.register('/providers/search', () => renderSearchProviders(mainContent));
  router.register('/messaging-apps', () => renderMessagingApps(mainContent));
  router.register('/service-providers', () => renderServiceProviders(mainContent));
  router.register('/settings', () => renderSettings(mainContent));
  router.register('/usage', () => renderUsage(mainContent));
  router.register('/users', () => renderUsers(mainContent));
  router.register('/audit', () => renderAudit(mainContent));
  router.register('/questions', () => renderQuestions(mainContent));

  router.setNotFound(() => {
    // Put a real path in the address bar rather than leaving the unknown one
    // sitting next to correct-looking content. replaceState rather than a
    // navigate, so Back doesn't land on the bad path and bounce straight here
    // again.
    window.history.replaceState({}, '', '/new');
    return renderHome(mainContent);
  });
}

// animateProjectRename draws attention to a name the engine just generated.
// refreshProjects() re-renders the switcher with the new text anyway; this only
// replays the highlight so the change is noticed rather than silently swapped.
// Scoped to the currently-viewed project.
function animateProjectRename(data) {
  const name = data?.payload?.name;
  const projectId = data?.project_id;
  if (!name || projectId == null) return;
  if (String(projectId) !== String(router.getActiveProjectId())) return;

  const el = document.querySelector('.project-switcher__btn-name');
  if (!el) return;
  el.textContent = name;
  el.classList.remove('is-renamed');
  void el.offsetWidth; // force reflow so the animation restarts
  el.classList.add('is-renamed');
}

function connectSSE() {
  if (sse) sse.disconnect();
  sse = new SSEClient();

  // A dropped stream means missed events: on every reopen after the first,
  // refetch what the events would have kept current.
  let streamOpened = false;
  sse.onConnectionState((connState) => {
    if (connState !== 'open') return;
    if (streamOpened) {
      refreshProjects();
      refreshStatus();
      loadPendingQuestions();
    }
    streamOpened = true;
  });

  sse.on('project.created', refreshProjects);
  sse.on('project.updated', (data) => {
    refreshProjects();
    animateProjectRename(data);
    state.set('projectUpdated', { project_id: data.project_id, timestamp: Date.now() });
  });
  sse.on('project.deleted', refreshProjects);

  sse.on('engine.started', (data) => {
    refreshStatus();
    refreshProjectPhase(data.project_id);
  });
  sse.on('engine.stopped', (data) => {
    refreshStatus();
    refreshProjectPhase(data.project_id);
  });

  sse.on('question.asked', (data) => {
    if (!data?.payload) return;
    state.set('questionAsked', { project_id: data.project_id, ...(data.payload || {}) });
    bumpPending(data.project_id, 1);
    const projectId = data.project_id;
    toast.show(t('questions.needInput'), 'warning', 10000, () => {
      router.navigate(`/projects/${projectId}/chat`);
    });
  });

  sse.on('question.answered', (data) => {
    if (!data?.payload) return;
    state.set('questionAnswered', { project_id: data.project_id, ...(data.payload || {}) });
    // One answer does not mean the batch is done: take the server's count.
    const projectId = data.project_id;
    get(`/admin/api/projects/${projectId}/questions?status=pending`).then(rows => {
      if (Array.isArray(rows)) setPending(projectId, rows.length);
    }).catch(() => bumpPending(projectId, -1));
  });

  // Initial pending-question count (SSE only carries deltas).
  loadPendingQuestions();

  // Browser-tab indicator: total pending questions across all projects.
  watchPending(() => setPendingTabIndicator(totalPending()));

  // Engine + tool fanout into state keys. Backfill is scoped to the currently
  // active project so a reconnect doesn't replay other projects' events into
  // shared state keys.
  wireBrainBus(sse);
  wireToolEvents(sse);
  wireReconnectBackfill(sse, () => router.getActiveProjectId());

  if (layout?.root) attachConnectionBanner(layout.root, sse);

  watchPhases();

  sse.connect('/admin/api/events/stream');
}

// Keeps every phase display in step. The stored project list feeds the project
// switcher and the command palette; the projects page patches its phase cell in
// place, keyed on the dataset attr views/projects.js sets, rather than
// re-rendering. The home screen does not list projects, so /projects is the
// only page that selector can hit.
let phaseWatched = false;
function watchPhases() {
  if (phaseWatched) return;
  phaseWatched = true;
  state.watch('projectPhaseChanged', (data) => {
    if (!data) return;
    const projects = state.get('projects');
    if (Array.isArray(projects)) {
      state.set('projects', projects.map(p => (p.id === data.project_id ? { ...p, mode: data.mode, phase: data.phase } : p)));
    }
    if (router.currentPath() !== '/projects') return;
    const cell = document.querySelector(`[data-project-phase="${data.project_id}"]`);
    if (cell) setPhaseBadge(cell, data.phase, { withReason: true });
  });
}

// The engine starting or stopping changes whether a building project is
// stalled, and only the server knows, so the phase is fetched and announced the
// same way a phase event is.
async function refreshProjectPhase(projectId) {
  try {
    const project = await get(`/admin/api/projects/${projectId}`);
    state.set('projectPhaseChanged', { project_id: projectId, mode: project.mode, phase: project.phase });
  } catch {}
}

function loadPendingQuestions() {
  return get('/admin/api/questions?status=pending').then(questions => {
    if (Array.isArray(questions)) setPendingFromList(questions);
  }).catch(() => {});
}

async function refreshProjects() {
  try {
    const projects = await get('/admin/api/projects');
    state.set('projects', projects);
  } catch {}
}

async function refreshStatus() {
  try {
    const status = await get('/admin/api/system/status');
    state.set('systemStatus', status);
  } catch {}
}

if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', boot);
} else {
  boot();
}
