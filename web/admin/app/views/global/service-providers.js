/**
 * Global service providers list (read-only overview across all projects).
 */

import { h, clear } from '../../core/dom.js';
import { t } from '../../core/i18n.js';
import { get } from '../../core/http.js';
import { defineView } from '../../core/view.js';
import { createListView } from '../../ui/list-view.js';
import { pageHeader, pageBody } from '../../ui/card.js';
import { mapWithConcurrency } from '../../core/utils.js';

export const renderServiceProviders = defineView((ctx, container) => {
  const { scope } = ctx;
  clear(container);

  const header = pageHeader({
    title: t('nav.services'),
  });

  const body = pageBody();
  const listContainer = h('div');
  body.appendChild(listContainer);

  container.appendChild(header);
  container.appendChild(body);

  const list = createListView({
    scope,
    container: listContainer,
    // Fan out: load all projects, then their service providers in parallel
    // (capped), flattened in project order. A project that errors contributes
    // nothing rather than failing the whole list.
    fetch: async () => {
      const projects = await get('/admin/api/projects');
      const perProject = await mapWithConcurrency(projects, 6, async (project) => {
        try {
          const providers = await get(`/admin/api/projects/${project.id}/service-providers`);
          return providers.map((p) => ({
            ...p,
            _projectName: project.name || t('questions.projectFallback', { id: project.id }),
          }));
        } catch {
          return [];
        }
      });
      return perProject.flat();
    },
    renderItem: (prov) => {
      const enabledBadge = prov.is_enabled
        ? h('span', { className: 'badge badge--success' }, t('common.enabled'))
        : h('span', { className: 'badge badge--danger' }, t('common.disabled'));

      return h('tr', {}, [
        h('td', { dataset: { label: t('sp.provider') } }, [
          h('strong', {}, prov.name),
          h('p', { className: 'text-xs text-secondary mt-1' }, prov.description || ''),
        ]),
        h('td', { dataset: { label: t('sp.project') } }, prov._projectName),
        h('td', { className: 'text-xs', dataset: { label: 'URL' } }, prov.base_url),
        h('td', { dataset: { label: t('sp.auth') } }, prov.auth_type),
        h('td', { dataset: { label: t('sp.status') } }, [enabledBadge]),
      ]);
    },
    wrap: (rows) => h('div', { className: 'table-wrapper table-wrapper--responsive' }, [
      h('table', { className: 'table' }, [
        h('thead', {}, [
          h('tr', {}, [
            h('th', {}, t('sp.provider')),
            h('th', {}, t('sp.project')),
            h('th', {}, t('sp.baseUrl')),
            h('th', {}, t('sp.auth')),
            h('th', {}, t('sp.status')),
          ]),
        ]),
        h('tbody', {}, rows),
      ]),
    ]),
    empty: {
      iconName: 'shield',
      title: t('sp.empty.title'),
      description: t('sp.empty.description'),
    },
    errorMsg: t('sp.loadFailed'),
  });
  list.reload();
});
