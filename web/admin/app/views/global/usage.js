/**
 * Token usage display.
 */

import { h, clear } from '../../core/dom.js';
import { t, tn, intlTag } from '../../core/i18n.js';
import { get } from '../../core/http.js';
import { defineView } from '../../core/view.js';
import { emptyState, errorState } from '../../ui/helpers.js';
import { pageHeader, pageBody } from '../../ui/card.js';
import { humanizeError } from '../../ui/errors.js';
import { mapWithConcurrency } from '../../core/utils.js';

export const renderUsage = defineView(async (ctx, container) => {
  clear(container);

  container.appendChild(pageHeader({ title: t('nav.usage') }));
  const body = pageBody();
  container.appendChild(body);

  async function load() {
    clear(body);
    const statsContainer = h('div', { className: 'stat-cards' });
    const logsContainer = h('div');
    body.appendChild(statsContainer);
    body.appendChild(logsContainer);

    let status, projects;
    try {
      [status, projects] = await Promise.all([
        get('/admin/api/system/status'),
        get('/admin/api/projects'),
      ]);
    } catch (err) {
      clear(body);
      body.appendChild(errorState({
        message: t('usage.loadFailed', { error: humanizeError(err.message) }),
        retry: load,
        reference: err.requestId,
      }));
      return;
    }
    if (ctx.scope.disposed) return;

    statsContainer.appendChild(statCard(t('usage.activeEngines'), String(status.brain_running), tn('usage.ofProjects', status.project_count)));
    statsContainer.appendChild(statCard(t('usage.providers'), String(status.provider_count), t('usage.connected')));
    statsContainer.appendChild(statCard(t('usage.uptime'), status.uptime, t('usage.version', { version: status.version || '-' })));

    if (projects.length === 0) {
      logsContainer.appendChild(emptyState({
        iconName: 'activity',
        title: t('usage.empty.title'),
        description: t('usage.empty.description'),
      }));
      return;
    }

    logsContainer.appendChild(h('h3', { className: 'mb-4 mt-6' }, t('usage.byProject')));
    // A placeholder per project keeps the list in order and shows progress while
    // the per-project stats load in parallel, rather than leaving the panel blank
    // until the slowest one returns.
    const slots = projects.map((project) => {
      const slot = h('div', { className: 'card mb-4' }, [
        h('h4', { className: 'card__title' }, project.name),
        h('div', { className: 'skeleton', style: { height: '14px', width: '40%', marginTop: '8px' } }),
      ]);
      logsContainer.appendChild(slot);
      return slot;
    });

    // Fan out, capped, so one slow project does not hold up the rest and a busy
    // instance does not open a request per project all at once.
    await mapWithConcurrency(projects, 6, async (project, i) => {
      if (ctx.scope.disposed) return;
      try {
        const stats = await get(`/admin/api/projects/${project.id}/llm/stats`);
        slots[i].replaceWith(projectUsageCard(project, stats));
      } catch (err) {
        console.warn(`Failed to load usage stats for project ${project.id}:`, err);
        // Say so: a project that silently vanished from this list reads as
        // "no usage", which is a different statement.
        slots[i].replaceWith(h('div', { className: 'card mb-4' }, [
          h('h4', { className: 'card__title' }, project.name),
          h('p', { className: 'text-sm text-danger' }, t('usage.projectFailed')),
        ]));
      }
    });
  }

  await load();
});

function statCard(label, value, sub) {
  return h('div', { className: 'stat-card' }, [
    h('div', { className: 'stat-card__label' }, label),
    h('div', { className: 'stat-card__value' }, value),
    h('div', { className: 'stat-card__sub' }, sub),
  ]);
}

function projectUsageCard(project, stats) {
  // Billed tokens = real cost basis: uncached input + cache-read + cache-creation
  // + output. Summing only input+output would silently drop cached tokens and
  // under-report usage.
  const billedInput = stats.total_billed_input_tokens || 0;
  const totalTokens = billedInput + (stats.total_output_tokens || 0);

  const details = [];
  details.push(tn('usage.calls', stats.total_calls || 0));
  if (stats.total_errors > 0) details.push(tn('usage.errors', stats.total_errors));
  const cacheHit = stats.cache_hit_ratio || 0;
  if (billedInput > 0) details.push(t('usage.cached', { percent: Math.round(cacheHit * 100) }));

  const modelInfo = (stats.by_model || [])
    .slice(0, 3)
    .map((m) => `${m.model} (${m.calls})`)
    .join(', ');

  return h('div', { className: 'card mb-4' }, [
    h('div', { className: 'card__header' }, [
      h('h4', { className: 'card__title' }, project.name),
      h('span', { className: 'badge badge--accent' }, t('usage.tokens', { count: totalTokens.toLocaleString(intlTag()) })),
    ]),
    h('p', { className: 'text-sm text-secondary' }, details.join(' · ')),
    modelInfo ? h('p', { className: 'text-xs text-tertiary mt-1' }, modelInfo) : null,
  ].filter(Boolean));
}
