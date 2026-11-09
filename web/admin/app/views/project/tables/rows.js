import { h, clear } from '../../../core/dom.js';
import { t } from '../../../core/i18n.js';
import { get, download, latest } from '../../../core/http.js';
import { icon } from '../../../ui/icon.js';
import * as toast from '../../../ui/toast.js';
import * as modal from '../../../ui/modal.js';
import * as state from '../../../core/state.js';
import { emptyState, showLoadError } from '../../../ui/helpers.js';
import { humanizeError } from '../../../ui/errors.js';
import { showAddRowModal, showEditRowModal, confirmDeleteRow, showAddColumnModal, columnType } from './row-modal.js';

/**
 * Show one table's rows. `hooks.reload`, if given, is filled with a function that
 * refreshes the rows in place, so a build writing to this table updates what the
 * person is looking at instead of throwing them back to the list.
 */
export async function showTableRows(container, table, projectId, onBack, hooks) {
  clear(container);
  const canWrite = state.isAdmin();

  let currentOffset = 0;
  const pageSize = 50;
  let sortCol = '';
  let sortDir = 'desc';
  let query = '';
  const guardedLoad = latest(async (url) => get(url));

  const back = h('button', {
    className: 'btn btn--ghost btn--sm mb-3',
    onClick: () => onBack(),
  }, [
    h('span', { innerHTML: icon('chevron-right'), style: { transform: 'rotate(180deg)', display: 'inline-flex' } }),
    ' ' + t('tbl.back'),
  ]);

  // One primary action (Add row); the others are secondary. The bar wraps on a
  // narrow screen instead of running off the edge.
  const headerRow = h('div', { className: 'flex items-center justify-between mb-3', style: { flexWrap: 'wrap', gap: 'var(--space-2)' } }, [
    h('h4', {}, table.table_name),
    h('div', { className: 'flex gap-2', style: { flexWrap: 'wrap' } }, [
      h('button', {
        className: 'btn btn--sm btn--secondary',
        title: t('tbl.exportCsv'),
        onClick: () => exportCsv(projectId, table),
      }, [h('span', { innerHTML: icon('download') }), ' CSV']),
      ...(canWrite ? [
        h('button', {
          className: 'btn btn--sm btn--secondary',
          title: t('tbl.addColumn'),
          onClick: () => showAddColumnModal(projectId, table, () => showTableRows(container, table, projectId, onBack, hooks)),
        }, [h('span', { innerHTML: icon('plus') }), ' ' + t('tbl.column')]),
        h('button', {
          className: 'btn btn--primary btn--sm',
          onClick: () => showAddRowModal(projectId, table, reload),
        }, [h('span', { innerHTML: icon('plus') }), ' ' + t('tbl.addRow')]),
      ] : []),
    ]),
  ]);

  // Filter and sort are server-side (the table may be large, so client-only
  // sort would only see the current page). Both reset to the first page.
  const filterInput = h('input', {
    type: 'search',
    className: 'list-filter',
    placeholder: t('list.filterPlaceholder'),
    'aria-label': t('list.filterPlaceholder'),
  });
  let filterDebounce;
  filterInput.addEventListener('input', () => {
    clearTimeout(filterDebounce);
    filterDebounce = setTimeout(() => {
      const next = filterInput.value.trim();
      if (next === query) return;
      query = next;
      currentOffset = 0;
      loadRows();
    }, 200);
  });

  container.appendChild(back);
  container.appendChild(headerRow);
  container.appendChild(h('div', { className: 'list-filter-bar' }, [filterInput]));
  const dataContainer = h('div');
  container.appendChild(dataContainer);

  function reload() { loadRows(); }
  function changePage(offset) { currentOffset = offset; loadRows(); }
  function onSort(col) {
    if (sortCol === col) sortDir = sortDir === 'asc' ? 'desc' : 'asc';
    else { sortCol = col; sortDir = 'asc'; }
    currentOffset = 0;
    loadRows();
  }
  if (hooks) hooks.reload = reload;

  async function loadRows() {
    const params = new URLSearchParams({ limit: String(pageSize), offset: String(currentOffset) });
    if (sortCol) { params.set('sort', sortCol); params.set('dir', sortDir); }
    if (query) params.set('q', query);
    const url = `/admin/api/projects/${projectId}/tables/${table.table_name}/rows?${params.toString()}`;
    let r;
    try {
      r = await guardedLoad(url);
    } catch (err) {
      showLoadError(dataContainer, t('tbl.rowsFailed'), err, loadRows);
      return;
    }
    if (r.stale) return;
    renderRows(dataContainer, r.data, projectId, table, currentOffset, pageSize, changePage, reload, canWrite, { sortCol, sortDir, onSort });
  }

  await loadRows();
}

async function exportCsv(projectId, table) {
  try {
    await download(
      `/admin/api/projects/${projectId}/tables/${table.table_name}/csv`,
      `${table.table_name}.csv`,
    );
    toast.success(t('tbl.csvExported'));
  } catch (err) {
    toast.error(t('set.exportFailed', { error: humanizeError(err.message) }));
  }
}

function renderRows(container, data, projectId, table, offset, limit, onPageChange, reload, canWrite, sortState) {
  clear(container);
  if (!data.rows || data.rows.length === 0) {
    // Deleting the last row on a later page leaves that page empty; step back
    // rather than showing "no rows" with no way to reach the earlier ones.
    if (offset > 0) { onPageChange(Math.max(0, offset - limit)); return; }
    container.appendChild(emptyState({
      iconName: 'database',
      title: t('tbl.noRows.title'),
      description: t('tbl.noRows.description'),
    }));
    return;
  }

  const cols = data.columns || [];
  const schema = data.schema || {};
  const tableEl = h('table', { className: 'table table--compact' });
  tableEl.appendChild(h('thead', {}, [
    h('tr', {}, [
      ...cols.map(col => headerCell(col, schema[col], sortState)),
      ...(canWrite ? [h('th', { style: { width: '100px' } }, t('projects.col.actions'))] : []),
    ]),
  ]));

  const tbody = h('tbody');
  for (const row of data.rows) {
    tbody.appendChild(buildRowTr(cols, row, projectId, table, data, reload, canWrite));
  }
  tableEl.appendChild(tbody);

  container.appendChild(h('div', { className: 'table-wrapper--responsive', style: { overflowX: 'auto' } }, [tableEl]));

  const page = Math.floor(offset / limit) + 1;
  const totalPages = Math.ceil(data.total / limit);
  container.appendChild(h('div', { className: 'flex items-center justify-between mt-2' }, [
    h('span', { className: 'text-sm text-secondary' }, t('tbl.showing', { shown: data.rows.length, total: data.total, page, pages: totalPages })),
    h('div', { className: 'flex gap-2' }, [
      offset > 0 ? h('button', {
        className: 'btn btn--sm btn--ghost',
        onClick: () => onPageChange(Math.max(0, offset - limit)),
      }, t('tbl.previous')) : null,
      (offset + limit) < data.total ? h('button', {
        className: 'btn btn--sm btn--ghost',
        onClick: () => onPageChange(offset + limit),
      }, t('tbl.next')) : null,
    ].filter(Boolean)),
  ]));
}

// A column header: the name, its type badge, and (when sortable) a click/Enter
// toggle that re-sorts server-side, with aria-sort and an arrow for the active
// column. The columnheader role is kept rather than made a button so the header
// still reads as a table header to assistive tech.
function headerCell(col, schemaType, sortState) {
  const typeLabel = columnType(schemaType);
  const active = sortState && sortState.sortCol === col;
  const children = [
    col,
    typeLabel ? h('span', {
      className: 'badge ml-1',
      style: { fontSize: 'var(--text-xs)', fontWeight: 'normal', opacity: '0.7' },
    }, typeLabel) : null,
    active ? h('span', { className: 'sort-arrow', 'aria-hidden': 'true' }, sortState.sortDir === 'asc' ? ' ↑' : ' ↓') : null,
  ].filter(Boolean);

  if (!sortState || !sortState.onSort) return h('th', {}, children);

  const th = h('th', {
    className: 'th-sortable',
    tabindex: '0',
    role: 'columnheader',
    'aria-sort': active ? (sortState.sortDir === 'asc' ? 'ascending' : 'descending') : 'none',
  }, children);
  th.addEventListener('click', () => sortState.onSort(col));
  th.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); sortState.onSort(col); }
  });
  return th;
}

function buildRowTr(cols, row, projectId, table, data, reload, canWrite) {
  return h('tr', {}, [
    ...cols.map(col => buildCellTd(col, row[col])),
    ...(canWrite ? [h('td', {}, [
      h('div', { className: 'flex items-center gap-1' }, [
        h('button', {
          className: 'btn btn--ghost btn--sm',
          title: t('tbl.editRow'),
          'aria-label': t('tbl.editRowNamed', { id: row.id }),
          onClick: () => showEditRowModal(projectId, table, row, data, reload),
        }, [h('span', { innerHTML: icon('edit') })]),
        h('button', {
          className: 'btn btn--ghost btn--sm',
          title: t('tbl.deleteRow'),
          'aria-label': t('tbl.deleteRowNamed', { id: row.id }),
          onClick: () => confirmDeleteRow(projectId, table, row, reload),
        }, [h('span', { innerHTML: icon('trash') })]),
      ]),
    ])] : []),
  ]);
}

function buildCellTd(col, val) {
  const display = val === null ? '--' : String(val);
  const isTruncated = display.length > 80;
  const td = h('td', {
    className: 'text-sm' + (isTruncated ? ' cursor-pointer' : ''),
    title: isTruncated ? display : '',
    dataset: { label: col },
  }, isTruncated ? display.substring(0, 80) + '...' : display);
  if (isTruncated) {
    td.addEventListener('click', (e) => {
      e.stopPropagation();
      modal.show(col, h('pre', {
        style: { whiteSpace: 'pre-wrap', wordBreak: 'break-word', maxHeight: '400px', overflow: 'auto' },
      }, display), [modal.cancelBtn(t('common.close'))]);
    });
  }
  return td;
}
