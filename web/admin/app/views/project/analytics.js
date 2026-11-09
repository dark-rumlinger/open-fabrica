/**
 * Project-scoped analytics panel.
 */

import { h, clear } from '../../core/dom.js';
import { t, tn } from '../../core/i18n.js';
import { get } from '../../core/http.js';
import { createScope } from '../../core/lifecycle.js';
import { icon } from '../../ui/icon.js';
import * as toast from '../../ui/toast.js';
import { emptyState, listSkeleton, showLoadError } from '../../ui/helpers.js';
import { pageHeader } from '../../ui/card.js';
import { formatDate } from '../../core/datetime.js';

export async function renderProjectAnalytics(container, projectId, project) {
  clear(container);
  const scope = createScope();

  // Date range state
  const now = new Date();
  const weekAgo = new Date(now);
  weekAgo.setDate(weekAgo.getDate() - 7);

  const startInput = h('input', {
    type: 'date',
    'aria-label': t('ana.start'),
    className: 'input',
    style: { width: 'auto', fontSize: 'var(--text-sm)' },
    value: fmt(weekAgo),
  });
  const endInput = h('input', {
    type: 'date',
    'aria-label': t('ana.end'),
    className: 'input',
    style: { width: 'auto', fontSize: 'var(--text-sm)' },
    value: fmt(now),
  });

  const body = h('div', { className: 'context-panel__body' });
  const dataContainer = h('div');

  // Which preset the current range came from, so it reads as selected. A
  // hand-edited date clears it.
  let activeDays = 7;
  const presetButtons = [];
  function markPreset() {
    presetButtons.forEach(([btn, days]) => {
      const on = days === activeDays;
      btn.classList.toggle('btn--secondary', on);
      btn.classList.toggle('btn--ghost', !on);
      btn.setAttribute('aria-pressed', String(on));
    });
  }

  // A slow earlier request must not overwrite a newer range.
  let loadSeq = 0;
  function load() {
    if (startInput.value && endInput.value && startInput.value > endInput.value) {
      toast.warning(t('ana.startAfterEnd'));
      return;
    }
    return loadAnalytics(dataContainer, projectId, startInput.value, endInput.value, ++loadSeq, () => loadSeq);
  }

  function setRange(days) {
    activeDays = days;
    markPreset();
    const endDate = new Date();
    const startDate = new Date(endDate);
    if (days === 0) { startDate.setHours(0, 0, 0, 0); }
    else if (days > 0) { startDate.setDate(startDate.getDate() - days); }
    else {
      // "all time": the honest start is when the project was created. Fall back to
      // a fixed far-back date only if the creation time is somehow unavailable.
      const created = project && project.created_at ? new Date(project.created_at) : null;
      if (created && !Number.isNaN(created.getTime())) startDate.setTime(created.getTime());
      else startDate.setFullYear(2020);
    }
    startInput.value = fmt(startDate);
    endInput.value = fmt(endDate);
    load();
  }
  for (const input of [startInput, endInput]) {
    input.addEventListener('change', () => { activeDays = null; markPreset(); load(); });
  }

  const presets = h('div', { className: 'flex gap-2' });
  for (const [label, days] of [[t('ana.today'), 0], [t('ana.days7'), 7], [t('ana.days30'), 30], [t('ana.allTime'), -1]]) {
    const btn = h('button', { className: 'btn btn--sm btn--ghost', type: 'button', onClick: () => setRange(days) }, label);
    presetButtons.push([btn, days]);
    presets.appendChild(btn);
  }
  markPreset();

  const refreshBtn = h('button', {
    className: 'btn btn--sm btn--secondary',
    type: 'button',
    onClick: () => load(),
  }, t('common.refresh'));

  const header = pageHeader({
    title: t('tabs.analytics'),
  });

  const controls = h('div', { className: 'flex items-center gap-2 mb-3', style: { flexWrap: 'wrap' } }, [
    startInput, h('span', { className: 'text-secondary' }, t('ana.to')), endInput,
    presets, refreshBtn,
  ]);

  container.appendChild(header);
  body.appendChild(controls);
  body.appendChild(dataContainer);
  container.appendChild(body);

  await load();
  return () => scope.dispose();
}

// Local calendar date. toISOString() is UTC, so "today" and the default range
// would shift a day near midnight for anyone not on UTC.
function fmt(d) {
  const pad = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

async function loadAnalytics(container, projectId, start, end, seq, currentSeq) {
  clear(container);
  container.appendChild(listSkeleton(2));

  try {
    const data = await get(`/admin/api/projects/${projectId}/analytics/summary?start=${start}&end=${end}`);
    if (seq !== currentSeq()) return;
    renderAnalytics(container, data);
  } catch (err) {
    if (seq !== currentSeq()) return;
    showLoadError(container, t('ana.loadFailed'), err,
      () => loadAnalytics(container, projectId, start, end, seq, currentSeq));
  }
}

function renderAnalytics(container, data) {
  clear(container);

  if (data.total_views === 0) {
    container.appendChild(emptyState({
      iconName: 'bar-chart',
      title: t('ana.empty.title'),
      description: t('ana.empty.description'),
    }));
    return;
  }

  // Stat cards, same pattern as dashboard
  const statsGrid = h('div', { className: 'stat-cards' }, [
    h('div', { className: 'stat-card' }, [
      h('div', { className: 'flex items-center justify-between mb-2' }, [
        h('span', { className: 'stat-card__label' }, t('ana.totalViews')),
        h('span', { innerHTML: icon('activity'), className: 'text-tertiary' }),
      ]),
      h('div', { className: 'stat-card__value' }, String(data.total_views)),
    ]),
    h('div', { className: 'stat-card' }, [
      h('div', { className: 'flex items-center justify-between mb-2' }, [
        h('span', { className: 'stat-card__label' }, t('ana.unique')),
        h('span', { innerHTML: icon('users'), className: 'text-tertiary' }),
      ]),
      h('div', { className: 'stat-card__value' }, String(data.unique_visitors)),
    ]),
  ]);
  container.appendChild(statsGrid);

  // Daily chart
  if (data.daily.length > 0) {
    container.appendChild(h('h4', { className: 'text-sm text-secondary mb-2' }, t('ana.daily')));
    container.appendChild(renderBarChart(data.daily));
  }

  // Top pages
  if (data.top_pages.length > 0) {
    container.appendChild(h('h4', { className: 'text-sm text-secondary mb-2 mt-4' }, t('ana.topPages')));
    const maxViews = data.top_pages[0].views;
    for (const p of data.top_pages) {
      const pct = Math.round((p.views / maxViews) * 100);
      container.appendChild(h('div', { className: 'mb-2' }, [
        h('div', { className: 'flex justify-between text-sm mb-1' }, [
          h('code', { style: { fontSize: 'var(--text-xs)' } }, p.path),
          h('span', { className: 'text-secondary' }, String(p.views)),
        ]),
        h('div', { style: { height: '4px', background: 'var(--bg-secondary)', borderRadius: '2px' } }, [
          h('div', { style: { height: '100%', width: pct + '%', background: 'var(--accent)', borderRadius: '2px' } }),
        ]),
      ]));
    }
  }

  // Top referrers
  if (data.top_referrers.length > 0) {
    container.appendChild(h('h4', { className: 'text-sm text-secondary mb-2 mt-4' }, t('ana.topReferrers')));
    for (const r of data.top_referrers) {
      container.appendChild(h('div', { className: 'flex justify-between text-sm mb-1' }, [
        h('span', { style: { wordBreak: 'break-all' } }, r.referrer),
        h('span', { className: 'text-secondary' }, String(r.count)),
      ]));
    }
  }
}

function renderBarChart(daily) {
  const maxViews = Math.max(...daily.map(d => d.views), 1);
  const total = daily.reduce((n, d) => n + d.views, 0);

  // Faint gridlines at 0/50/100% behind the bars give the heights a scale to
  // read against; a dependency-free touch, all positioned with tokens.
  const gridline = (topPct) => h('div', {
    'aria-hidden': 'true',
    style: { position: 'absolute', left: '0', right: '0', top: topPct + '%', borderTop: '1px dashed var(--border)' },
  });

  const bars = daily.map(d => {
    const pct = Math.max((d.views / maxViews) * 100, 2);
    return h('div', {
      style: { flex: '1', height: pct + '%', background: 'var(--accent)', borderRadius: '2px 2px 0 0', minWidth: '4px' },
      title: t('ana.barTip', { date: d.date, views: d.views, unique: d.unique_visitors }),
    });
  });

  const plot = h('div', { style: { position: 'relative', flex: '1', height: '120px' } }, [
    gridline(0), gridline(50), gridline(100),
    h('div', { style: { position: 'absolute', inset: '0', display: 'flex', alignItems: 'flex-end', gap: '2px' } }, bars),
  ]);

  // Y-axis: the max at the top, 0 at the baseline, so a bar's value is legible.
  const axisStyle = { fontSize: 'var(--text-xs)', color: 'var(--text-tertiary)' };
  const yAxis = h('div', {
    style: { ...axisStyle, display: 'flex', flexDirection: 'column', justifyContent: 'space-between', height: '120px', textAlign: 'end', minWidth: '28px' },
  }, [h('span', {}, String(maxViews)), h('span', {}, '0')]);

  // X-axis: first and last day of the range (the bars in between are the trend).
  const xAxis = daily.length > 1
    ? h('div', { style: { ...axisStyle, display: 'flex', justifyContent: 'space-between', marginTop: '4px', marginInlineStart: '34px' } }, [
      h('span', {}, formatDate(daily[0].date)),
      h('span', {}, formatDate(daily[daily.length - 1].date)),
    ])
    : null;

  return h('div', {
    className: 'card mb-3',
    role: 'img',
    'aria-label': tn('ana.chartLabel', daily.length, { total }),
    style: { padding: '12px' },
  }, [
    h('div', { style: { display: 'flex', gap: '6px' } }, [yAxis, plot]),
    xAxis,
  ].filter(Boolean));
}
