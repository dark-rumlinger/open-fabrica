/**
 * The project's chat tab, behind /projects/:id/chat: a stats bar across the
 * top and the conversation filling the rest, full width.
 *
 * This is the screen; chat.js next door is the conversation panel it drops in
 * underneath the stats.
 */

import { h, render, clear } from '../../core/dom.js';
import { t, tn, intlTag } from '../../core/i18n.js';
import { get } from '../../core/http.js';
import { icon } from '../../ui/icon.js';
import * as state from '../../core/state.js';
import * as toast from '../../ui/toast.js';
import { createScope } from '../../core/lifecycle.js';
import { renderProjectChat } from './chat.js';
import { isForProject } from '../../core/utils.js';
import { humanizeError } from '../../ui/errors.js';
import { setStatValue } from '../../ui/helpers.js';
import { describePhase } from '../../ui/phase-state.js';

export function renderProjectChatTab(container, projectId) {
  // One lifecycle scope owns every teardown (SSE watchers via state.select, the
  // chat cleanup, and the debounce timer), replacing the hand-tracked
  // unwatchers[] array so this view uses the same convention as the rest.
  const scope = createScope();
  let chatCleanup = null;

  // Stats bar (top ~25%)
  const statsBar = h('div', { className: 'project-chat-stats' });

  // Chat panel (bottom ~75%)
  const chatPanel = h('div', { className: 'project-chat-panel' });

  const layout = h('div', { className: 'project-chat-layout' }, [
    statsBar,
    chatPanel,
  ]);

  render(container, layout);

  // Render chat
  chatCleanup = renderProjectChat(chatPanel, projectId);
  scope.add(() => { if (chatCleanup) chatCleanup(); });

  // Refs for targeted DOM updates
  let pagesValueEl = null;
  let phaseValueEl = null;
  let phaseSubEl = null;
  let tokensValueEl = null;
  let tokensSubEl = null;
  let modelValueEl = null;
  let visitorsValueEl = null;
  let visitorsSubEl = null;

  // Debounced summary refresh (tokens, pages, visitors)
  let summaryTimer = null;
  function debouncedRefreshSummary() {
    if (summaryTimer) clearTimeout(summaryTimer);
    summaryTimer = setTimeout(() => refreshSummary(projectId), 3000);
  }
  scope.add(() => { if (summaryTimer) clearTimeout(summaryTimer); });

  // Load initial stats
  loadStats(statsBar, projectId);

  // --- Real-time SSE watchers ---

  // Any tool execution -> debounced summary refresh (tokens, pages, visitors)
  state.select('toolExecuted', (evt) => {
    if (!isForProject(evt, projectId)) return;
    debouncedRefreshSummary();
  }, scope);

  // Stage changes (e.g. plan->build) -> refresh so plan-phase tokens show up
  state.select('brainStageChange', (evt) => {
    if (!isForProject(evt, projectId)) return;
    debouncedRefreshSummary();
  }, scope);

  // Project updates (e.g. auto-rename) -> refresh so their token cost shows up
  state.select('projectUpdated', (evt) => {
    if (!isForProject(evt, projectId)) return;
    debouncedRefreshSummary();
  }, scope);

  function paintPhase(phase) {
    const view = describePhase(phase);
    setStatValue(phaseValueEl, view.label);
    if (phaseSubEl) phaseSubEl.textContent = view.sentence;
  }

  // Phase changes (building -> needs you -> live, paused), including the engine
  // starting or stopping under a building project.
  state.select('projectPhaseChanged', (data) => {
    if (!isForProject(data, projectId)) return;
    paintPhase(data.phase);
  }, scope);

  async function loadStats(bar, sid) {
    clear(bar);

    try {
      const [summary, project] = await Promise.all([
        get(`/admin/api/projects/${sid}/summary`),
        get(`/admin/api/projects/${sid}`),
      ]);

      clear(bar);
      const grid = h('div', { className: 'project-chat-stats-grid' });

      // 1. Phase: one answer to "where is my project", with the reason.
      const phaseView = describePhase(project.phase);
      const phaseCard = createStatCard(t('sp.status'), phaseView.label, phaseView.sentence, 'brain');
      phaseValueEl = phaseCard.valueEl;
      phaseSubEl = phaseCard.subEl;
      grid.appendChild(phaseCard.el);

      // 2. Tokens
      const tokensCard = createStatCard(
        t('ct.tokensUsed'),
        summary.total_tokens.toLocaleString(intlTag()),
        tn('ct.actions', summary.brain_actions),
        'zap',
      );
      tokensValueEl = tokensCard.valueEl;
      tokensSubEl = tokensCard.subEl;
      grid.appendChild(tokensCard.el);

      // 3. Model, the project's configured model (what the engine runs).
      const modelCard = createStatCard(
        t('models.modelLabel'),
        summary.model || '-',
        t('ct.inUse'),
        'layers',
      );
      modelCard.el.classList.add('stat-card--model');
      modelValueEl = modelCard.valueEl;
      grid.appendChild(modelCard.el);

      // 4. Pages
      const pagesCard = createStatCard(t('tabs.pages'), String(summary.pages_count), t('ct.published'), 'file-text');
      pagesValueEl = pagesCard.valueEl;
      grid.appendChild(pagesCard.el);

      // 5. Visitors (24h)
      const viewsCard = createStatCard(
        t('ct.visitors24'),
        String(summary.unique_visitors),
        tn('ct.pageViews', summary.page_views),
        'activity',
      );
      visitorsValueEl = viewsCard.valueEl;
      visitorsSubEl = viewsCard.subEl;
      grid.appendChild(viewsCard.el);

      bar.appendChild(grid);
    } catch (err) {
      clear(bar);
      bar.appendChild(h('div', { className: 'project-chat-stats-error' }, t('ct.statsFailed')));
      toast.error(t('ct.statsError', { error: humanizeError(err.message) }));
    }
  }

  function createStatCard(label, value, sub, iconName, extraChildren) {
    const valueEl = h('div', { className: 'stat-card__value' }, value);
    const subEl = h('div', { className: 'stat-card__sub' }, sub);

    const headerRow = h('div', { className: 'flex items-center justify-between' }, [
      h('span', { className: 'stat-card__label' }, label),
      h('span', { innerHTML: icon(iconName), className: 'text-tertiary' }),
    ]);

    const children = [headerRow, valueEl, subEl];
    if (extraChildren) children.push(...extraChildren);

    const el = h('div', { className: 'stat-card stat-card--compact' }, children);
    return { el, valueEl, subEl };
  }

  async function refreshSummary(sid) {
    try {
      const summary = await get(`/admin/api/projects/${sid}/summary`);
      setStatValue(tokensValueEl, summary.total_tokens.toLocaleString(intlTag()));
      if (tokensSubEl) tokensSubEl.textContent = tn('ct.actions', summary.brain_actions);
      if (summary.model) setStatValue(modelValueEl, summary.model);
      setStatValue(pagesValueEl, String(summary.pages_count));
      setStatValue(visitorsValueEl, String(summary.unique_visitors));
      if (visitorsSubEl) visitorsSubEl.textContent = tn('ct.pageViews', summary.page_views);
    } catch (_) { /* silent */ }
  }

  return () => scope.dispose();
}
