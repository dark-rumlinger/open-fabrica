/**
 * Project detail shell: header, the tab bar, and whichever panel the route
 * names - the chat or one of the other project panels.
 */

import { h, render, clear } from '../../core/dom.js';
import { t } from '../../core/i18n.js';
import { createScope } from '../../core/lifecycle.js';
import { get, post } from '../../core/http.js';
import { navigate } from '../../core/router.js';
import * as toast from '../../ui/toast.js';
import * as state from '../../core/state.js';
import { setPanelSwitcher } from '../../ui/chat/cards/index.js';
import { formatPublicUrl, emptyState } from '../../ui/helpers.js';
import { createHostingChip } from '../../ui/hosting-status.js';
import { setPhaseBadge } from '../../ui/phase-badge.js';
import { phaseAction } from '../../ui/phase-state.js';
import { createProjectTabs, splitPanelId } from '../../ui/project-tabs.js';
import { setProjectContext } from '../../ui/topbar.js';
import { humanizeError } from '../../ui/errors.js';

import { renderProjectChatTab } from './chat-tab.js';
import { renderProjectPages } from './pages.js';
import { renderProjectTables } from './tables.js';
import { renderProjectEndpoints } from './endpoints.js';
import { renderProjectFiles } from './files.js';
import { renderProjectTasks } from './tasks.js';
import { renderProjectProviders } from './providers.js';
import { renderProjectSettings } from './settings.js';
import { renderProjectBuilds } from './builds.js';
import { renderProjectQuestions } from './questions.js';
import { renderProjectWebhooks } from './webhooks.js';
import { renderProjectSecrets } from './secrets.js';
import { renderProjectPayments } from './payments.js';
import { renderProjectMemory } from './memory.js';
import { renderProjectLayouts } from './layouts.js';
import { renderProjectAnalytics } from './analytics.js';
import { renderProjectDiagnostics } from './diagnostics.js';

// The active project-detail lifecycle. One scope owns the header watchers, the
// home chat, and the context-panel cleanup, replacing three hand-tracked
// holders. Re-rendering (tab switch, start/stop) disposes the previous scope
// and opens a fresh one; navigating away disposes it through the controller the
// router holds, which fixes the watchers/chat/panel leaking on exit.
let detailScope = null;

export function renderProjectDetail(container, params) {
  if (detailScope) detailScope.dispose();
  detailScope = createScope();
  mountProjectDetail(detailScope, container, params);
  return {
    destroy() {
      if (detailScope) {
        detailScope.dispose();
        detailScope = null;
      }
    },
  };
}

async function mountProjectDetail(scope, container, params) {
  const projectId = parseInt(params.id, 10);
  const panel = params.tab || 'chat';

  // Load project data
  let project;
  try {
    project = await get(`/admin/api/projects/${projectId}`);
  } catch (err) {
    // A newer mount owns the page now; a stale failure must not toast or yank
    // the user away from where they are.
    if (scope.disposed) return;
    toast.error(t('pd.loadFailed', { error: humanizeError(err.message) }));
    navigate('/projects');
    return;
  }
  if (scope.disposed) return;

  // --- Project context, shown in the top bar ---
  // No name and no running dot: the project switcher carries both. One phase
  // badge and the one action that moves the project forward. Whether visitors
  // can reach the site is the hosting chip's and the Visitors card's job, and
  // the engine itself is an Advanced setting.
  let phase = project.phase;
  const phaseHost = h('span', { className: 'project-context__phase' });
  const actionBtn = h('button', { className: 'btn btn--sm btn--primary', onClick: () => runAction() });

  function paintPhase() {
    setPhaseBadge(phaseHost, phase);
    const action = phaseAction(phase);
    actionBtn.textContent = action.label;
    actionBtn.disabled = false;
    actionBtn.className = `btn btn--sm ${action.id === 'pause' ? 'btn--secondary' : 'btn--primary'}`;
    actionBtn.title = action.id === 'pause' && action.interrupt
      ? t('pd.pauseTip')
      : '';
  }
  paintPhase();

  async function setMode(mode) {
    await post(`/admin/api/engine/${projectId}/mode`, { mode });
  }

  async function runAction() {
    const action = phaseAction(phase);
    actionBtn.disabled = true;
    try {
      if (action.id === 'answer') {
        navigate(`/projects/${projectId}/questions`);
      } else if (action.id === 'start') {
        await post(`/admin/api/engine/${projectId}/start`);
        toast.success(t('eng.started'));
      } else if (action.id === 'resume') {
        await setMode(action.to);
        toast.success(t('pd.resumed'));
      } else if (action.interrupt) {
        try {
          await post(`/admin/api/engine/${projectId}/interrupt`, {});
          actionBtn.textContent = t('pd.pausing');
          toast.info(t('pd.pausingSafe'));
          // The phase event repaints the button. This covers a stop that lands
          // without one, so the button cannot stay stuck disabled.
          scope.timeout(() => paintPhase(), 60000);
          return;
        } catch {
          // Nothing was in flight after all, so pause the mode directly.
          await setMode('paused');
          toast.success(t('pd.paused'));
        }
      } else {
        await setMode('paused');
        toast.success(t('pd.paused'));
      }
    } catch (err) {
      toast.error(humanizeError(err.message));
    }
    actionBtn.disabled = false;
  }

  const context = h('div', { className: 'project-context' }, [
    (() => {
      const { url, label } = formatPublicUrl(project, state.get('systemStatus'));
      // No domain means no address to open; a '#' link would only spawn a blank tab.
      if (url === '#') return h('span', { className: 'project-context__url text-secondary' }, label);
      return h('a', {
        href: url,
        target: '_blank',
        rel: 'noopener',
        className: 'link project-context__url',
        title: t('pd.view'),
      }, label);
    })(),
    ...(project.domain ? [createHostingChip(projectId, { scope })] : []),
    h('div', { className: 'project-context__controls' }, [
      phaseHost,
      // Start, Pause and Resume are admin-only routes; a non-admin would only
      // ever get a 403 from this button.
      ...(state.isAdmin() ? [actionBtn] : []),
    ]),
  ]);
  setProjectContext(context);
  scope.add(() => setProjectContext(null));

  const tabs = createProjectTabs({ projectId, activePanel: panel, scope });

  // Register panel switcher for chat card links
  setPanelSwitcher((panelName) => {
    navigate(`/projects/${projectId}/${panelName}`);
  });

  if (panel === 'chat') {
    // --- Chat: stats bar + conversation, full width ---
    const chatContainer = h('div', { className: 'project-chat-container' });
    const wrapper = h('div', { className: 'flex-col', style: { height: '100%', overflow: 'hidden' } }, [
      tabs.element,
      h('div', { className: 'project-body' }, [chatContainer]),
    ]);
    render(container, wrapper);
    scope.add(renderProjectChatTab(chatContainer, projectId));
  } else {
    // --- Every other tab: full-width panel ---
    const contentPanel = h('div', { className: 'project-content-full' });
    const wrapper = h('div', { className: 'flex-col', style: { height: '100%', overflow: 'hidden' } }, [
      tabs.element,
      h('div', { className: 'project-body' }, [
        ...(tabs.rail ? [tabs.rail] : []),
        contentPanel,
      ]),
    ]);
    render(container, wrapper);

    renderContextPanel(scope, contentPanel, panel, projectId, project);
  }

  // The phase comes from the server: it is derived from the mode, the run and
  // the open questions, so this only repaints what the server announces. That
  // includes the engine starting or stopping, which decides whether a building
  // project is stalled.
  state.select('projectPhaseChanged', (data) => {
    if (!data || data.project_id !== projectId) return;
    project.mode = data.mode;
    project.phase = data.phase;
    phase = data.phase;
    paintPhase();
  }, scope);
}

function renderContextPanel(scope, container, panel, projectId, project) {
  clear(container);

  // `section` is the part after the dash in ids like settings-advanced; the
  // panels that have sections open on it, the rest ignore it.
  const { panel: base, section } = splitPanelId(panel);

  const panels = {
    pages: (c, id) => renderProjectPages(c, id),
    tables: (c, id) => renderProjectTables(c, id),
    endpoints: (c, id) => renderProjectEndpoints(c, id, project),
    files: (c, id) => renderProjectFiles(c, id),
    tasks: (c, id, sec) => renderProjectTasks(c, id, sec),
    questions: (c, id) => renderProjectQuestions(c, id),
    providers: (c, id) => renderProjectProviders(c, id),
    webhooks: (c, id, sec) => renderProjectWebhooks(c, id, sec, project),
    builds: (c, id) => renderProjectBuilds(c, id),
    secrets: (c, id) => renderProjectSecrets(c, id),
    payments: (c, id) => renderProjectPayments(c, id),
    memory: (c, id) => renderProjectMemory(c, id),
    layouts: (c, id, sec) => renderProjectLayouts(c, id, sec),
    analytics: (c, id) => renderProjectAnalytics(c, id, project),
    diagnostics: (c, id) => renderProjectDiagnostics(c, id),
    settings: (c, id, sec) => renderProjectSettings(c, id, project, sec),
  };

  const renderFn = panels[base];
  if (!renderFn) {
    // An unknown tab is a stale or mistyped link, not the Pages panel; say so and
    // offer the way back rather than silently showing Pages as if it had worked.
    container.appendChild(emptyState({
      iconName: 'alert-triangle',
      title: t('errors.notFound'),
      action: { label: t('common.back'), onClick: () => navigate(`/projects/${projectId}`) },
    }));
    return;
  }
  const cleanup = renderFn(container, projectId, section);

  // cleanup may be a Promise (from async render fns) that resolves to a function.
  // The scope runs it on dispose, or immediately if we've already navigated away.
  if (cleanup && typeof cleanup.then === 'function') {
    cleanup.then(fn => { if (typeof fn === 'function') scope.add(fn); }).catch(() => {
      // The panel reports its own load failure; only the cleanup hook is lost.
    });
  } else if (typeof cleanup === 'function') {
    scope.add(cleanup);
  }
}

