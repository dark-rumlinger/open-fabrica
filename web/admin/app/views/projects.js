/**
 * Projects list view.
 */

import { h, clear } from '../core/dom.js';
import { t, tn } from '../core/i18n.js';
import { get, post, del, rawFetch } from '../core/http.js';
import { navigate } from '../core/router.js';
import { defineView } from '../core/view.js';
import { icon } from '../ui/icon.js';
import * as toast from '../ui/toast.js';
import * as modal from '../ui/modal.js';
import * as state from '../core/state.js';
import { emptyState, formatPublicUrl, formGroup, showLoadError } from '../ui/helpers.js';
import { pageHeader, pageBody } from '../ui/card.js';
import { humanizeError } from '../ui/errors.js';
import { createPhaseBadge } from '../ui/phase-badge.js';

export const renderProjects = defineView((ctx, container) => {
  clear(container);

  const header = pageHeader({
    title: t('nav.projects'),
    actions: state.isAdmin()
      ? [
          { label: t('projects.import'), iconName: 'upload', kind: 'secondary', onClick: () => showImportModal() },
          { label: t('nav.newProject'), iconName: 'plus', onClick: () => navigate('/new') },
        ]
      : [],
  });
  const listContainer = pageBody();

  // System stats, moved here from the old dashboard: this is the page you open
  // to look at what exists. Fed from the shared systemStatus key that main.js
  // refreshes on every engine start/stop, so they stay current without polling.
  const statsBar = h('div', { className: 'projects-stats' });
  function renderStats() {
    clear(statsBar);
    statsBar.appendChild(systemStatsGrid(state.get('systemStatus')));
  }
  renderStats();
  state.select('systemStatus', renderStats, ctx.scope);

  // The list gets its own host: renderProjectsList() clears as it re-renders,
  // and the stats above it must survive that.
  const listHost = h('div');
  listContainer.appendChild(statsBar);
  listContainer.appendChild(listHost);

  container.appendChild(header);
  container.appendChild(listContainer);

  const PAGE_SIZE = 50;
  let allProjects = [];
  let totalCount = 0;

  async function loadProjects() {
    allProjects = [];
    totalCount = 0;
    try {
      const result = await get(`/admin/api/projects?limit=${PAGE_SIZE}&offset=0`);
      allProjects = result.items;
      totalCount = result.total;
      // The shared `projects` key (switcher, home, palette) is the full list and
      // is kept current by main.js; writing this first page into it would drop
      // every project past the page size from those.
      renderProjectsList();
    } catch (err) {
      showLoadError(listHost, t('projects.loadFailed'), err, loadProjects);
    }
  }

  async function loadMore() {
    try {
      const result = await get(`/admin/api/projects?limit=${PAGE_SIZE}&offset=${allProjects.length}`);
      allProjects = allProjects.concat(result.items);
      renderProjectsList();
    } catch (err) {
      toast.error(t('projects.loadMoreFailed', { error: humanizeError(err.message) }));
    }
  }

  function renderProjectsList() {
    clear(listHost);

    if (allProjects.length === 0) {
      listHost.appendChild(emptyState({
        iconName: 'globe',
        title: t('projects.empty.title'),
        description: t('projects.empty.description'),
        action: state.isAdmin() ? { label: t('nav.newProject'), onClick: () => navigate('/new') } : null,
      }));
      return;
    }

    const rows = allProjects.map(project => {
      const openProject = () => navigate(`/projects/${project.id}/chat`);
      // The name is a real link (keyboard focus, open in new tab, copy address);
      // the row click is a pointer convenience on top of it. The row itself is not
      // given role=button, which would break the table's semantics.
      const row = h('tr', {
        className: 'cursor-pointer',
        onClick: (e) => { if (!e.target.closest('a, button')) openProject(); },
      }, [
        h('td', { dataset: { label: t('projects.col.name') } }, [
          h('a', { href: `/projects/${project.id}/chat`, 'data-route': '', className: 'table__link' }, [
            h('strong', {}, project.name),
          ]),
        ]),
        h('td', { dataset: { label: t('projects.col.domain') } }, (() => {
          const { url, label } = formatPublicUrl(project, state.get('systemStatus'));
          if (url === '#') return h('span', { className: 'text-secondary' }, label);
          return h('a', {
            href: url,
            target: '_blank',
            rel: 'noopener',
            className: 'link',
            onClick: (e) => e.stopPropagation(),
          }, label);
        })()),
        // The dataset key is load-bearing: main.js patches this cell in place
        // from SSE rather than re-rendering, and finds it by this attribute.
        h('td', { dataset: { label: t('projects.col.phase'), projectPhase: project.id } },
          createPhaseBadge(project.phase, { withReason: true })),
        h('td', { dataset: { label: t('projects.col.site') } }, [
          h('span', {
            className: `badge ${!project.published ? 'badge--secondary' : 'badge--success'}`,
            title: !project.published
              ? t('projects.unpublishedTip')
              : t('projects.publishedTip'),
          }, !project.published ? t('projects.unpublished') : t('projects.published')),
        ]),
        h('td', {}, [
          h('div', { className: 'table__actions' }, [
            ...(state.isAdmin() ? [
              h('button', {
                className: 'btn btn--ghost btn--sm',
                title: t('projects.copy'),
                'aria-label': t('projects.copyNamed', { name: project.name }),
                onClick: (e) => { e.stopPropagation(); showCopyModal(project); },
                innerHTML: icon('copy'),
              }),
              h('button', {
                className: 'btn btn--ghost btn--sm',
                title: t('projects.delete'),
                'aria-label': t('projects.deleteNamed', { name: project.name }),
                onClick: (e) => { e.stopPropagation(); confirmDelete(project); },
                innerHTML: icon('trash'),
              }),
            ] : []),
          ]),
        ]),
      ]);
      return row;
    });

    const tableWrapper = h('div', { className: 'table-wrapper table-wrapper--responsive' }, [
      h('table', { className: 'table' }, [
        h('thead', {}, [
          h('tr', {}, [
            h('th', {}, t('projects.col.name')),
            h('th', {}, t('projects.col.domain')),
            h('th', {}, t('projects.col.phase')),
            h('th', {}, t('projects.col.site')),
            h('th', {}, state.isAdmin() ? t('projects.col.actions') : ''),
          ]),
        ]),
        h('tbody', {}, rows),
      ]),
    ]);

    listHost.appendChild(tableWrapper);

    if (allProjects.length < totalCount) {
      listHost.appendChild(
        h('div', { style: { textAlign: 'center', padding: '1rem' } }, [
          h('button', {
            className: 'btn btn--ghost',
            onClick: loadMore,
          }, t('projects.loadMore', { shown: allProjects.length, total: totalCount })),
        ])
      );
    }
  }

  function showImportModal() {
    const fileInput = h('input', {
      type: 'file',
      className: 'input',
      accept: '.of,application/zip',
    });

    const form = h('div', {}, [
      h('p', { className: 'form-hint mb-3' }, t('projects.importHint')),
      formGroup(t('projects.importFile'), fileInput),
      h('p', { className: 'form-hint mt-2' }, t('projects.importNote')),
    ]);

    modal.show(t('projects.import'), form, [
      modal.cancelBtn(),
      {
        label: t('projects.importBtn'),
        className: 'btn btn--primary',
        onClick: async () => {
          const file = fileInput.files && fileInput.files[0];
          if (!file) {
            toast.error(t('projects.importChoose'));
            return false;
          }

          const formData = new FormData();
          formData.append('file', file);

          try {
            // FormData: let rawFetch set only Authorization; the browser sets the
            // multipart Content-Type with its boundary.
            const res = await rawFetch('/admin/api/projects/import', {
              method: 'POST',
              body: formData,
            });
            const project = await res.json();
            toast.success(t('projects.imported'));
            await loadProjects();
            if (project && project.id) {
              navigate(`/projects/${project.id}/chat`);
            }
          } catch (err) {
            toast.error(t('projects.importFailed', { error: humanizeError(err.message) }));
            return false;
          }
        },
      },
    ]);
  }

  function showCopyModal(sourceProject) {
    const nameInput = h('input', {
      className: 'input',
      value: t('projects.copyOf', { name: sourceProject.name }),
      required: true,
    });
    const domainInput = h('input', {
      className: 'input',
      placeholder: t('projects.domainPlaceholder'),
      pattern: '[a-zA-Z0-9][a-zA-Z0-9.\\-]*',
    });

    let domainManuallyEdited = false;
    domainInput.addEventListener('input', () => { domainManuallyEdited = true; });
    nameInput.addEventListener('input', () => {
      if (domainManuallyEdited) return;
      const slug = nameInput.value.toLowerCase().replace(/[^a-z0-9]/g, '');
      domainInput.value = slug ? slug + '.localhost' : '';
    });
    // Seed initial domain from prefilled name.
    nameInput.dispatchEvent(new Event('input'));

    const content = h('div', {}, [
      formGroup(t('projects.col.name'), nameInput, { required: true, hint: t('projects.copyNameHint') }),
      formGroup(t('projects.col.domain'), domainInput, { hint: t('projects.copyDomainHint') }),
      h('div', { className: 'text-secondary text-sm' }, t('projects.copyNote')),
    ]);

    modal.show(t('projects.copy'), content, [
      modal.cancelBtn(),
      {
        label: t('projects.copyBtn'),
        className: 'btn btn--primary',
        onClick: async () => {
          const name = nameInput.value.trim();
          if (!name) {
            toast.error(t('projects.nameRequired'));
            return false;
          }
          try {
            await post(`/admin/api/projects/${sourceProject.id}/copy`, {
              name,
              domain: domainInput.value.trim() || null,
            });
            toast.success(t('projects.copied'));
            loadProjects();
          } catch (err) {
            toast.error(t('projects.copyFailed', { error: humanizeError(err.message) }));
            return false;
          }
        },
      },
    ]);

    nameInput.focus();
    nameInput.select();
  }

  function confirmDelete(project) {
    modal.confirmByTyping(
      t('projects.delete'),
      t('projects.deleteBody', { name: project.name }),
      project.name,
      async () => {
        try {
          await del(`/admin/api/projects/${project.id}`);
          toast.success(t('projects.deleted'));
          loadProjects();
        } catch (err) {
          toast.error(t('projects.deleteFailed', { error: humanizeError(err.message) }));
          return false;
        }
      },
      t('projects.delete'),
    );
  }

  loadProjects();
});

/** The system stat tiles. Compact: they are context, not the point of the page. */
function systemStatsGrid(status) {
  const grid = h('div', { className: 'stat-cards stat-cards--compact' });

  const items = status ? [
    {
      label: t('nav.projects'),
      value: status.project_count,
      sub: tn('projects.stats.engines', status.brain_running),
      icon: 'globe',
    },
    {
      label: t('nav.providers'),
      value: status.provider_count > 0 ? t('projects.stats.connected') : t('projects.stats.noProvider'),
      sub: tn('projects.stats.providers', status.provider_count),
      icon: 'brain',
    },
    {
      label: t('nav.system'),
      value: status.version,
      sub: status.background_commands_dropped > 0
        ? tn('projects.stats.uptimeDrops', status.background_commands_dropped, { uptime: status.uptime })
        : t('projects.stats.uptime', { uptime: status.uptime }),
      icon: 'activity',
    },
  ] : [
    { label: t('projects.stats.systemStatus'), value: t('projects.stats.unavailable'), sub: t('projects.stats.loadFailed'), icon: 'activity' },
  ];

  for (const stat of items) {
    grid.appendChild(
      h('div', { className: 'stat-card stat-card--compact' }, [
        h('div', { className: 'flex items-center justify-between mb-2' }, [
          h('span', { className: 'stat-card__label' }, stat.label),
          h('span', { className: 'text-tertiary', innerHTML: icon(stat.icon) }),
        ]),
        h('div', { className: 'stat-card__value' }, String(stat.value)),
        h('div', { className: 'stat-card__sub' }, stat.sub),
      ])
    );
  }

  return grid;
}
