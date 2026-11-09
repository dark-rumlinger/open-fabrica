/**
 * Project-scoped diagnostics panel.
 */

import { h, clear } from '../../core/dom.js';
import { t } from '../../core/i18n.js';
import { get } from '../../core/http.js';
import { createScope } from '../../core/lifecycle.js';
import { icon } from '../../ui/icon.js';
import { pageHeader } from '../../ui/card.js';
import { showLoadError } from '../../ui/helpers.js';
import { humanizeError } from '../../ui/errors.js';
import { formatDateTime } from '../../core/datetime.js';

export async function renderProjectDiagnostics(container, projectId) {
  clear(container);
  const scope = createScope();

  const header = pageHeader({
    title: t('tabs.health'),
  });

  const body = h('div', { className: 'context-panel__body' });
  const dataContainer = h('div');

  const lastRun = h('span', { className: 'text-xs text-secondary', role: 'status' });
  const refreshBtn = h('button', {
    className: 'btn btn--sm btn--secondary',
    type: 'button',
    onClick: run,
  }, t('diag.run'));
  const toolbar = h('div', { className: 'flex items-center gap-3 mb-3' }, [refreshBtn, lastRun]);

  // The checks hit the database several times; ignore clicks while one is
  // running rather than stacking them.
  let running = false;
  async function run() {
    if (running || scope.disposed) return;
    running = true;
    refreshBtn.disabled = true;
    try {
      const ok = await loadAll(dataContainer, projectId, run);
      if (ok) lastRun.textContent = t('diag.lastRun', { when: formatDateTime(new Date().toISOString()) });
    } finally {
      running = false;
      refreshBtn.disabled = false;
    }
  }

  container.appendChild(header);
  body.appendChild(toolbar);
  body.appendChild(dataContainer);
  container.appendChild(body);

  await run();
  return () => scope.dispose();
}

async function loadAll(container, projectId, retry) {
  clear(container);
  container.appendChild(h('div', { className: 'skeleton-list', 'aria-busy': 'true' }, [
    h('div', { className: 'skeleton skeleton--card' }),
    h('div', { className: 'skeleton skeleton--card' }),
  ]));

  try {
    const [health, integrity, errors] = await Promise.all([
      get(`/admin/api/projects/${projectId}/diagnostics/health`),
      get(`/admin/api/projects/${projectId}/diagnostics/integrity`),
      get(`/admin/api/projects/${projectId}/diagnostics/errors`),
    ]);
    renderResults(container, health, integrity, errors);
    return true;
  } catch (err) {
    showLoadError(container, t('diag.failed'), err, retry);
    return false;
  }
}

function renderResults(container, health, integrity, errors) {
  clear(container);

  // Health section: project-facing stats up front, runtime internals collapsed.
  container.appendChild(sectionHeader(t('diag.overview'), 'activity'));
  const rt = health.runtime || {};
  const mem = health.memory || {};
  const project = health.project || {};
  container.appendChild(h('div', { className: 'stat-cards stat-cards--compact' }, [
    statCard(t('tabs.pages'), project.pages, 'file-text'),
    statCard(t('tabs.tables'), project.tables, 'database'),
    statCard(t('diag.memories'), project.memories, 'brain'),
    statCard(t('diag.memUsed'), t('diag.mb', { n: mem.alloc_mb ?? '-' }), 'cpu'),
  ]));
  container.appendChild(h('details', { className: 'mb-3' }, [
    h('summary', { className: 'text-xs text-secondary', style: { cursor: 'pointer' } }, t('diag.technical')),
    h('p', { className: 'text-xs text-secondary', style: { margin: '0.35rem 0 0' } },
      t('diag.techLine', { version: rt.go_version ?? '-', tasks: rt.goroutines ?? '-', gc: mem.num_gc ?? '-' })),
  ]));

  // Integrity section
  const hasRealIssues = (integrity.issues || []).some(i => i.level === 'error' || i.level === 'warning');
  container.appendChild(sectionHeader(t('diag.integrity'), integrity.ok ? 'check' : hasRealIssues ? 'alert-circle' : 'info'));
  const issues = integrity.issues || [];
  if (issues.length === 0) {
    container.appendChild(h('div', { className: 'card mb-3' }, [
      h('div', { className: 'flex items-center gap-2' }, [
        h('span', { innerHTML: icon('check'), style: { color: 'var(--success)' } }),
        h('span', { className: 'text-sm' }, t('diag.allGood')),
      ]),
    ]));
  } else {
    for (const issue of issues) {
      const levelIcon = issue.level === 'error' ? 'alert-circle' : issue.level === 'info' ? 'info' : 'alert-triangle';
      const levelColor = issue.level === 'error' ? 'var(--danger)' : issue.level === 'info' ? 'var(--info)' : 'var(--warning)';
      const badgeClass = issue.level === 'error' ? 'badge--danger' : issue.level === 'info' ? 'badge--info' : 'badge--warning';
      container.appendChild(h('div', { className: 'card mb-2' }, [
        h('div', { className: 'flex items-center gap-2' }, [
          h('span', {
            innerHTML: icon(levelIcon),
            style: { color: levelColor },
          }),
          h('span', { className: 'text-sm' }, issue.message),
          h('span', { className: `badge ${badgeClass}` },
            ['error', 'warning', 'info'].includes(issue.level) ? t('diag.level.' + issue.level) : issue.level),
        ]),
      ]));
    }
  }

  // Errors section
  container.appendChild(sectionHeader(t('diag.recentErrors'), 'zap'));
  if (errors.length === 0) {
    container.appendChild(h('div', { className: 'card mb-3' }, [
      h('div', { className: 'flex items-center gap-2' }, [
        h('span', { innerHTML: icon('check'), className: 'text-tertiary' }),
        h('span', { className: 'text-sm text-secondary' }, t('diag.noErrors')),
      ]),
    ]));
  } else {
    for (const err of errors) {
      container.appendChild(h('div', { className: 'card mb-2' }, [
        h('div', { className: 'card__header' }, [
          h('div', { className: 'flex items-center gap-2' }, [
            h('span', { className: 'badge badge--danger' }, err.event_type),
            h('span', { className: 'text-sm' }, humanizeError(err.summary)),
          ]),
          h('span', { className: 'text-xs text-secondary' },
            formatDateTime(err.created_at)),
        ]),
        ...(err.details ? [h('details', { className: 'mt-2' }, [
          h('summary', { className: 'text-xs text-secondary', style: { cursor: 'pointer' } }, t('diag.technical')),
          h('pre', { className: 'text-xs text-secondary pre-scroll' }, err.details),
        ])] : []),
      ]));
    }
  }
}

function sectionHeader(title, iconName) {
  return h('h4', { className: 'flex items-center gap-2 text-sm text-secondary mb-2 mt-4' }, [
    h('span', { innerHTML: icon(iconName) }),
    h('span', {}, title),
  ]);
}

function statCard(label, value, iconName) {
  return h('div', { className: 'stat-card stat-card--compact' }, [
    h('div', { className: 'flex items-center justify-between mb-2' }, [
      h('span', { className: 'stat-card__label' }, label),
      h('span', { innerHTML: icon(iconName), className: 'text-tertiary' }),
    ]),
    h('div', { className: 'stat-card__value' }, String(value ?? '-')),
  ]);
}
