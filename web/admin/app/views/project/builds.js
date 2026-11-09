/**
 * Builds panel. Two-pane layout:
 *  - Left: list of runs for this project, newest first.
 *  - Right: chronological timeline merging stages, LLM calls, tool calls,
 *    and engine events from the selected run.
 *
 * Click an LLM call to expand the request/response.
 */

import { h, clear } from '../../core/dom.js';
import { t } from '../../core/i18n.js';
import { formatCompact, formatDuration, formatNumber, formatRatio } from '../../core/format.js';
import { get, download } from '../../core/http.js';
import { createScope } from '../../core/lifecycle.js';
import * as toast from '../../ui/toast.js';
import { humanizeError } from '../../ui/errors.js';
import { emptyState, listSkeleton, showLoadError } from '../../ui/helpers.js';
import { pageHeader } from '../../ui/card.js';
import { appendDetailParts } from './logs/detail.js';
import { formatDateTime, formatRelative, formatTimeWithSeconds } from '../../core/datetime.js';

export async function renderProjectBuilds(container, projectId) {
  clear(container);
  const scope = createScope();

  // Use pageHeader + context-panel__body to match every other project page
  // (System Health, Settings, etc) - that's where the consistent left
  // padding comes from. Building the header by hand bypassed the body
  // class so the content was glued to the left edge.
  const header = pageHeader({
    title: t('tabs.builds'),
  });
  container.appendChild(header);

  const body = h('div', { className: 'context-panel__body' });
  container.appendChild(body);

  // Single-column layout: picker bar on top, timeline below.
  const pickerBar = h('div', { className: 'builds-picker' });
  const timelineHost = h('div', { className: 'builds-timeline-host' });
  body.appendChild(pickerBar);
  body.appendChild(timelineHost);

  await loadBuildList(pickerBar, timelineHost, projectId);
  return () => scope.dispose();
}

async function loadBuildList(pickerBar, timelineHost, projectId) {
  clear(pickerBar);
  for (let i = 0; i < 4; i++) {
    pickerBar.appendChild(h('div', { className: 'skeleton skeleton--pill' }));
  }
  try {
    const data = await get(`/admin/api/projects/${projectId}/builds`);
    renderBuildPicker(pickerBar, timelineHost, projectId, data.builds || [], data);
  } catch (err) {
    showLoadError(pickerBar, t('builds.loadFailed'), err, () => loadBuildList(pickerBar, timelineHost, projectId));
  }
}

function renderBuildPicker(pickerBar, timelineHost, projectId, builds, meta = {}) {
  clear(pickerBar);
  if (builds.length === 0) {
    // Hide the picker bar entirely so its divider line doesn't hang above the
    // empty state with nothing to pick.
    pickerBar.style.display = 'none';
    clear(timelineHost);
    timelineHost.appendChild(emptyState({
      iconName: 'activity',
      title: t('builds.empty.title'),
      description: t('builds.empty.description'),
    }));
    return;
  }
  pickerBar.style.display = '';
  pickerBar.appendChild(h('span', { className: 'builds-picker__label' }, t('builds.label')));

  let activePill = null;
  for (const b of builds) {
    const pill = renderBuildPill(b);
    pill.addEventListener('click', async () => {
      if (activePill) {
        activePill.classList.remove('is-active');
        activePill.setAttribute('aria-pressed', 'false');
      }
      pill.classList.add('is-active');
      pill.setAttribute('aria-pressed', 'true');
      activePill = pill;
      await loadTimeline(timelineHost, projectId, b.id);
    });
    pickerBar.appendChild(pill);
  }
  if (meta.truncated) {
    pickerBar.appendChild(h('span', {
      className: 'text-secondary text-sm',
      style: { alignSelf: 'center', marginInlineStart: '4px' },
      title: t('builds.olderHidden'),
    }, t('builds.showing', { shown: builds.length, total: meta.total })));
  }
  // Auto-select first (most recent) build.
  if (pickerBar.children.length > 1) pickerBar.children[1].click();
}

function renderBuildPill(b) {
  const dot = h('span', {
    className: 'build-pill__dot',
    title: b.status,
    style: { background: statusDotColor(b.status) },
  });
  const tokens = (b.total_input_tokens || 0) + (b.total_output_tokens || 0);
  return h('button', { className: 'build-pill', type: 'button', 'aria-pressed': false }, [
    dot,
    h('span', { title: formatDateTime(b.started_at) }, formatRelative(b.started_at)),
    tokens > 0
      ? h('span', { className: 'build-pill__tokens' }, fmtNum(tokens))
      : null,
  ].filter(Boolean));
}

function statusDotColor(s) {
  if (s === 'completed') return 'var(--success)';
  if (s === 'failed') return 'var(--danger)';
  if (s === 'running') return 'var(--accent)';
  if (s === 'paused') return 'var(--warning)';
  return 'var(--text-tertiary)';
}

function statusBadgeClass(s) {
  if (s === 'completed') return 'badge--success';
  if (s === 'failed') return 'badge--danger';
  if (s === 'running') return 'badge--info';
  if (s === 'paused') return 'badge--warning';
  return 'badge--neutral';
}

async function loadTimeline(right, projectId, buildID) {
  clear(right);
  right.appendChild(listSkeleton());
  // Pills can be clicked faster than a timeline loads. Only the latest click may
  // draw, or a slow earlier build would overwrite the one now selected.
  const seq = (right._timelineSeq = (right._timelineSeq || 0) + 1);
  try {
    const data = await get(`/admin/api/projects/${projectId}/builds/${buildID}/timeline`);
    if (seq !== right._timelineSeq) return;
    renderTimeline(right, projectId, buildID, data.build, data.rows || []);
  } catch (err) {
    if (seq !== right._timelineSeq) return;
    showLoadError(right, t('builds.timelineFailed'), err, () => loadTimeline(right, projectId, buildID));
  }
}

function renderTimeline(right, projectId, buildID, build, rows) {
  clear(right);

  right.appendChild(renderBuildHeader(build, projectId, buildID));

  // Cost panel: token economics for this build. Loads async so it never
  // blocks the timeline render; fills in once the /cost aggregate returns.
  const costHost = h('div', { className: 'mb-3' });
  right.appendChild(costHost);
  loadCost(costHost, projectId, buildID);

  if (rows.length === 0) {
    right.appendChild(h('p', { className: 'text-secondary text-sm' }, t('builds.noEvents')));
    return;
  }

  // Use the platform's standard `.table` class so we inherit striped rows,
  // hover highlighting, and border styling that already match every other
  // admin table. Per-column widths live as inline styles on the <th>s so
  // we don't have to ship CSS for one panel.
  const table = h('table', { className: 'table builds-timeline' });

  const thead = h('thead');
  const headerRow = h('tr');
  const headerCells = [
    [t('builds.col.time'),  '72px'],
    ['',      '24px'],   // type glyph
    [t('builds.col.stage'), '70px'],
    [t('builds.col.iter'),  '48px'],
    [t('builds.col.detail'), 'auto'],
  ];
  for (const [label, width] of headerCells) {
    const th = h('th', { style: { width } }, label);
    headerRow.appendChild(th);
  }
  thead.appendChild(headerRow);
  table.appendChild(thead);

  const tbody = h('tbody');
  for (const row of rows) {
    appendTimelineRow(tbody, projectId, buildID, row);
  }
  table.appendChild(tbody);
  // Wrap so the fixed-width timeline columns scroll horizontally on narrow
  // screens instead of forcing the whole panel wider than the viewport.
  right.appendChild(h('div', { className: 'table-wrapper' }, [table]));
}

async function loadCost(host, projectId, buildID) {
  try {
    const data = await get(`/admin/api/projects/${projectId}/builds/${buildID}/cost`);
    renderCostPanel(host, data);
  } catch (err) {
    // Non-fatal: the timeline is still useful without the cost panel.
    console.warn('Failed to load build cost:', err);
    host.appendChild(h('p', { className: 'text-secondary text-xs' }, t('builds.noCost')));
  }
}

function renderCostPanel(host, data) {
  clear(host);
  const totals = data.totals || {};
  if (!totals.calls) return; // nothing billed yet

  const billed = totals.billed_input_tokens || 0;
  const out = totals.output_tokens || 0;
  const cacheHit = Math.round((totals.cache_hit_ratio || 0) * 100);
  const overhead = totals.overhead_ratio || 0;

  // Headline chips: the four numbers worth seeing at a glance.
  const chip = (label, value, title) => h('span', {
    title: title || '',
    style: {
      display: 'inline-flex', flexDirection: 'column', gap: '1px',
      padding: '4px 10px', borderRadius: '6px',
      background: 'var(--bg-tertiary)',
      border: '1px solid var(--border)',
    },
  }, [
    h('span', { style: { fontSize: '10px', textTransform: 'uppercase', letterSpacing: '0.5px', color: 'var(--text-tertiary)' } }, label),
    h('span', { style: { fontFamily: 'var(--font-mono)', fontSize: '13px', color: 'var(--text-primary)' } }, value),
  ]);

  const chips = h('div', { style: { display: 'flex', gap: '8px', flexWrap: 'wrap', marginBottom: '10px' } }, [
    chip(t('builds.billedInput'), fmtNum(billed), t('builds.billedInputTip')),
    chip(t('builds.output'), fmtNum(out)),
    chip(t('builds.cacheHit'), cacheHit + '%', t('builds.cacheHitTip')),
    chip(t('builds.overhead'), formatRatio(overhead), t('builds.overheadTip')),
  ]);

  // Right-aligned header cell. Inline style (not the `.text-right` class): the
  // `.table th { text-align: left }` rule in css/tables.css outranks that
  // utility class, so headers would render left while their numeric columns are
  // right-aligned - the mismatch that made these tables look off.
  const rTh = (label) => h('th', { style: { textAlign: 'end' } }, label);

  // Per-stage table.
  const table = h('table', { className: 'table' });
  const thead = h('thead', {}, [h('tr', {}, [
    h('th', {}, t('builds.col.stage')),
    rTh(t('builds.col.calls')),
    rTh(t('builds.col.billedIn')),
    rTh(t('builds.col.cached')),
    rTh(t('builds.output')),
    rTh(t('builds.overhead')),
  ])]);
  const tbody = h('tbody');
  for (const s of (data.stages || [])) {
    const billedIn = s.billed_input_tokens || 0;
    const cached = billedIn > 0 ? Math.round(((s.cache_read_tokens || 0) / billedIn) * 100) : 0;
    const numCell = (v) => h('td', { style: { textAlign: 'end', fontFamily: 'var(--font-mono)', fontSize: '11px' } }, v);
    tbody.appendChild(h('tr', {}, [
      h('td', { style: { fontFamily: 'var(--font-mono)', fontSize: '11px' } }, s.stage || '-'),
      numCell(String(s.calls || 0)),
      numCell(fmtNum(billedIn)),
      numCell(cached + '%'),
      numCell(fmtNum(s.output_tokens || 0)),
      numCell(formatRatio(s.overhead_ratio || 0)),
    ]));
  }
  table.appendChild(thead);
  table.appendChild(tbody);

  host.appendChild(chips);
  host.appendChild(h('div', { className: 'table-wrapper' }, [table]));

  // Per-task cost. The build runs as one continuous session, so this is a
  // best-effort estimate: build-stage call cost windowed onto task-completion
  // events (server buildTaskAttribution). Absent for older builds (no events).
  const tasks = data.tasks || [];
  if (tasks.length) {
    const anyEstimated = tasks.some((task) => task.estimated);
    const numCell = (v) => h('td', { style: { textAlign: 'end', fontFamily: 'var(--font-mono)', fontSize: '11px' } }, v);
    const ttable = h('table', { className: 'table' });
    const tthead = h('thead', {}, [h('tr', {}, [
      h('th', {}, t('builds.col.task')),
      rTh(t('builds.col.calls')),
      rTh(t('builds.col.effectiveIn')),
      rTh(t('builds.output')),
      rTh(t('builds.col.wall')),
    ])]);
    const ttbody = h('tbody');
    for (const task of tasks) {
      // Truncate in JS (not CSS): text-overflow:ellipsis is unreliable on a bare
      // <td> in an auto-layout table, so a long title would blow out the first
      // column and shove the numeric columns off their headers. Full title stays
      // in the tooltip.
      const rawName = task.title || task.task_id || '-';
      const name = (rawName.length > 46 ? rawName.slice(0, 45) + '…' : rawName) + (task.estimated ? ' *' : '');
      ttbody.appendChild(h('tr', {}, [
        h('td', {
          title: rawName + (task.estimated ? '  ' + t('builds.estimatedTip') : ''),
          style: { fontSize: '11px', whiteSpace: 'nowrap' },
        }, name),
        numCell(String(task.calls || 0)),
        numCell(fmtNum(task.effective_input_tokens || 0)),
        numCell(fmtNum(task.output_tokens || 0)),
        numCell(fmtDuration(task.wall_ms || 0)),
      ]));
    }
    const resid = data.tasks_unattributed;
    if (resid && (resid.calls || resid.effective_input_tokens)) {
      ttbody.appendChild(h('tr', { style: { opacity: '0.65' } }, [
        h('td', {
          title: t('builds.unattributedTip'),
          style: { fontSize: '11px', fontStyle: 'italic' },
        }, t('builds.unattributed')),
        numCell(String(resid.calls || 0)),
        numCell(fmtNum(resid.effective_input_tokens || 0)),
        numCell(fmtNum(resid.output_tokens || 0)),
        numCell(fmtDuration(resid.wall_ms || 0)),
      ]));
    }
    ttable.appendChild(tthead);
    ttable.appendChild(ttbody);
    host.appendChild(h('div', {
      style: { fontSize: '10px', textTransform: 'uppercase', letterSpacing: '0.5px', color: 'var(--text-tertiary)', margin: '12px 0 4px' },
    }, t('builds.perTask') + (anyEstimated ? ' ' + t('builds.estimatedMark') : '')));
    host.appendChild(h('div', { className: 'table-wrapper' }, [ttable]));
  }
}

function renderBuildHeader(build, projectId, buildID) {
  const exportBtn = h('button', {
    className: 'btn btn--primary btn--sm',
    title: t('builds.exportTip'),
    onClick: () => exportBuild(projectId, buildID),
  }, t('builds.exportLog'));

  const meta = [
    h('span', { className: 'badge ' + statusBadgeClass(build.status) }, build.status),
    build.final_stage ? h('span', { className: 'badge badge--neutral' }, build.final_stage) : null,
    h('span', { className: 'text-xs text-secondary' }, build.id),
    h('span', { className: 'text-xs text-secondary' }, formatDateTime(build.started_at)),
    h('div', { style: { flex: '1' } }),
    exportBtn,
  ].filter(Boolean);
  return h('div', { className: 'builds-pane__build-header flex items-center gap-2 mb-2' }, meta);
}

async function exportBuild(projectId, buildID) {
  try {
    // The server already serves this as an attachment (Content-Disposition), so
    // the shared download() helper handles the blob + save and the auth path,
    // rather than hand-rolling the fetch and temp-anchor dance here.
    await download(`/admin/api/projects/${projectId}/builds/${buildID}/export`, `build-${buildID}.json`);
  } catch (err) {
    toast.error(t('builds.exportFailed', { error: humanizeError(err.message) }));
  }
}

const KIND_GLYPH = {
  stage_start:    { glyph: '▶', color: 'var(--info)' },          // play triangle
  stage_complete: { glyph: '■', color: 'var(--success)' },       // filled square
  llm_call:       { glyph: '♦', color: 'var(--accent)' },        // diamond
  tool_call:      { glyph: '·', color: 'var(--text-tertiary)' }, // middle dot
  brain_event:    { glyph: '•', color: 'var(--text-tertiary)' }, // bullet
};

function appendTimelineRow(tbody, projectId, buildID, row) {
  const tr = h('tr', { className: 'builds-timeline__row builds-timeline__row--' + row.kind });

  // Tighter row height than the default .table padding - 12px is too tall
  // for a dense log. Override per-cell.
  const td = (content, style) => {
    const cell = h('td', { style: Object.assign({ padding: '6px 12px', verticalAlign: 'middle' }, style || {}) });
    if (content !== null && content !== undefined) {
      if (Array.isArray(content)) {
        for (const c of content) if (c) cell.appendChild(c);
      } else if (typeof content === 'string') {
        cell.appendChild(document.createTextNode(content));
      } else {
        cell.appendChild(content);
      }
    }
    return cell;
  };

  const glyph = KIND_GLYPH[row.kind] || { glyph: '·', color: 'inherit' };
  const iterText = row.kind === 'llm_call'
    ? ((row.iteration === undefined || row.iteration === null) ? '?' : String(row.iteration))
    : '';

  // Column 1: time (locale-aware clock, full timestamp in tooltip)
  tr.appendChild(td(
    h('span', { title: row.ts, style: { color: 'var(--text-tertiary)', fontFamily: 'var(--font-mono)', fontSize: '11px' } },
      formatTimeWithSeconds(row.ts)),
  ));

  // Column 2: type glyph
  tr.appendChild(td(
    h('span', { style: { color: glyph.color, fontSize: '14px', fontWeight: '600' } }, glyph.glyph),
    { textAlign: 'center' },
  ));

  // Column 3: stage
  const stageText = row.stage || '';
  tr.appendChild(td(
    stageText
      ? h('span', {
          style: { fontFamily: 'var(--font-mono)', fontSize: '11px', color: 'var(--text-secondary)' },
        }, stageText)
      : null,
  ));

  // Column 4: iteration (LLM only)
  tr.appendChild(td(
    iterText
      ? h('span', { style: { fontFamily: 'var(--font-mono)', fontSize: '11px', color: 'var(--text-secondary)' } }, '#' + iterText)
      : null,
  ));

  // Column 5: detail (kind-specific content; click an LLM row to expand it)
  tr.appendChild(td(renderDetailCell(projectId, buildID, row)));

  // Click-to-expand on LLM rows.
  if (row.kind === 'llm_call') {
    tr.style.cursor = 'pointer';
    // Keyboard users expand a call with Enter or Space, like a click.
    tr.tabIndex = 0;
    tr.setAttribute('aria-expanded', 'false');
    const detailTR = h('tr', { className: 'builds-timeline__detail' });
    detailTR.style.display = 'none';
    const detailCell = h('td', {
      colspan: '5',
      style: { padding: '8px 16px 16px 16px', background: 'var(--bg-tertiary, rgba(255,255,255,0.02))' },
    });
    detailTR.appendChild(detailCell);

    let loaded = false;
    tr.addEventListener('keydown', (e) => {
      if (e.target !== tr) return;
      if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); tr.click(); }
    });
    tr.addEventListener('click', async (e) => {
      if (e.target && e.target.tagName === 'BUTTON') return;
      if (detailTR.style.display === 'none') {
        detailTR.style.display = '';
        tr.setAttribute('aria-expanded', 'true');
        if (!loaded) {
          loaded = true;
          detailCell.appendChild(h('p', { className: 'text-secondary text-xs' }, t('builds.loadingDetail')));
          try {
            const detail = await get(`/admin/api/projects/${projectId}/builds/llm/${row.data.log_id}`);
            clear(detailCell);
            appendDetailParts(detailCell, detail);
          } catch (err) {
            clear(detailCell);
            detailCell.appendChild(h('p', { className: 'text-danger text-xs' }, t('builds.callFailed', { error: humanizeError(err.message) })));
            loaded = false; // let a second expand retry
          }
        }
      } else {
        detailTR.style.display = 'none';
        tr.setAttribute('aria-expanded', 'false');
      }
    });
    tbody.appendChild(tr);
    tbody.appendChild(detailTR);
    return;
  }

  tbody.appendChild(tr);
}

function renderDetailCell(projectId, buildID, row) {
  switch (row.kind) {
    case 'stage_start':
      return h('span', { className: 'text-sm', style: { color: 'var(--text-secondary)' } },
        t('builds.stageStarted', { stage: row.stage }));
    case 'stage_complete': {
      const ok = row.data.status === 'completed';
      const parts = [
        h('span', {
          className: 'badge ' + (ok ? 'badge--success' : 'badge--danger'),
          style: { fontSize: '10px', padding: '1px 6px', marginInlineEnd: '8px' },
        }, row.data.status || 'done'),
        h('span', { style: { fontFamily: 'var(--font-mono)', fontSize: '11px', color: 'var(--text-tertiary)' } },
          t('builds.stageSummary', {
            input: fmtNum(row.data.input_tokens || 0),
            output: fmtNum(row.data.output_tokens || 0),
            tools: row.data.tool_calls || 0,
            duration: fmtDuration(row.data.duration_ms || 0),
          })),
      ];
      if (row.data.error_message) {
        parts.push(h('span', { style: { color: 'var(--danger)', fontSize: '11px', marginInlineStart: '8px' } }, row.data.error_message));
      }
      return h('span', {}, parts);
    }
    case 'llm_call': {
      const model = row.data.model || '-';
      const tokens = t('builds.tokensInOut', { input: row.data.input_tokens || 0, output: row.data.output_tokens || 0 });
      const dur = fmtDuration(row.data.duration_ms || 0);
      return h('div', { style: { display: 'flex', alignItems: 'center', gap: '8px', width: '100%' } }, [
        h('span', { style: { fontSize: '12px' } }, model),
        h('span', { style: { fontFamily: 'var(--font-mono)', fontSize: '11px', color: 'var(--text-tertiary)' } },
          tokens + ' · ' + dur),
        row.data.replay_cache_hit
          ? h('span', { style: { fontSize: '11px', color: 'var(--success)' } }, t('builds.replayed'))
          : null,
        row.data.error_message
          ? h('span', { style: { color: 'var(--danger)', fontSize: '11px' } }, row.data.error_message)
          : null,
      ].filter(Boolean));
    }
    case 'tool_call':
      return h('span', { style: { fontFamily: 'var(--font-mono)', fontSize: '12px' } }, row.data.name || '?');
    case 'brain_event':
      return h('span', { style: { fontSize: '12px', color: 'var(--text-secondary)' } },
        (row.data.event_type ? '[' + row.data.event_type + '] ' : '') + (row.data.summary || ''));
    default:
      return null;
  }
}

function fmtNum(n) {
  return n >= 1000 ? formatCompact(n) : formatNumber(n);
}

function fmtDuration(ms) {
  return formatDuration(ms);
}
