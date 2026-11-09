import { h, clear } from '../../core/dom.js';
import { t, tn } from '../../core/i18n.js';
import { get } from '../../core/http.js';
import { icon } from '../../ui/icon.js';
import { createScope } from '../../core/lifecycle.js';
import { emptyState, clickable, watchToolReload, loadInto } from '../../ui/helpers.js';
import { showTableRows } from './tables/rows.js';
import { pageHeader } from '../../ui/card.js';

export async function renderProjectTables(container, projectId) {
  clear(container);
  const scope = createScope();
  // Set while a table's rows are on screen, so a build writing to the project
  // refreshes those rows instead of rebuilding the list and dumping the person
  // back out of the table they are reading.
  const browsing = { reload: null };

  const header = pageHeader({
    title: t('tabs.tables'),
  });
  const body = h('div', { className: 'context-panel__body' });
  container.appendChild(header);
  container.appendChild(body);

  watchToolReload(scope, projectId, { tools: ['create_table', 'insert_row', 'update_row', 'delete_row'] },
    () => (browsing.reload ? browsing.reload() : loadAndRenderTables(body, projectId, browsing)));

  await loadAndRenderTables(body, projectId, browsing);
  return () => scope.dispose();
}

async function loadAndRenderTables(container, projectId, browsing) {
  browsing.reload = null;
  await loadInto(
    () => get(`/admin/api/projects/${projectId}/tables`),
    (tables) => renderTablesList(container, tables, projectId, browsing),
    { errorMsg: t('tbl.loadFailed'), container },
  );
}

function renderTablesList(container, tables, projectId, browsing) {
  clear(container);
  if (!tables || tables.length === 0) {
    container.appendChild(emptyState({
      iconName: 'database',
      title: t('tbl.empty.title'),
      description: t('tbl.empty.description'),
    }));
    return;
  }

  for (const table of tables) {
    const card = h('div', { className: 'card mb-3' }, [
      h('div', { className: 'card__header' }, [
        h('div', { className: 'flex items-center gap-2' }, [
          h('span', { innerHTML: icon('database') }),
          h('strong', {}, table.table_name),
        ]),
        h('span', { className: 'badge badge--info', title: t('tbl.rowCountTip') }, tn('tbl.rowCount', table.row_count)),
      ]),
    ]);
    clickable(card, () => showTableRows(container, table, projectId, () => loadAndRenderTables(container, projectId, browsing), browsing));
    container.appendChild(card);
  }
}
