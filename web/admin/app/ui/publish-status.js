/**
 * Publish-status strip: one sentence that reconciles the four inputs deciding
 * whether visitors can actually see a project (published or unpublished, pages
 * built, domain kind, HTTPS state), plus the actions that fix what's missing.
 * Used on the project home and in Settings > Sharing & status.
 */

import { h, clear } from '../core/dom.js';
import { t } from '../core/i18n.js';
import { get, put } from '../core/http.js';
import * as state from '../core/state.js';
import * as toast from './toast.js';
import * as modal from './modal.js';
import { humanizeError } from './errors.js';
import { formatPublicUrl, linkifyUrls } from './helpers.js';
import { classifyHosting } from './hosting-state.js';

/**
 * Pure state derivation, exported for reuse and easy reasoning. The two
 * project-level answers (disabled, nothing published) come first because they
 * outrank any hosting detail; everything below them is the shared hosting
 * classification, so this strip and the status chip always agree.
 */
function derivePublishState({ published, pagesCount, hosting, url }) {
  if (!published) {
    return {
      key: 'offline', label: t('projects.unpublished'), kind: 'secondary',
      detail: t('ps.unpublishedDetail'),
    };
  }
  if (pagesCount === 0) {
    return {
      key: 'building', label: t('ps.nothingYet'), kind: 'warning',
      detail: t('ps.nothingDetail'),
    };
  }
  const view = classifyHosting(hosting, { url });
  return { key: view.key, label: view.label, kind: view.kind, detail: view.sentence };
}

/**
 * Builds the strip and loads its data. It redraws itself after the
 * publish/unpublish toggle, so hosts never need to rebuild around it. Returns
 * the element with a .refresh() method.
 */
export function createPublishStrip(projectId, opts = {}) {
  // Spacing is the mount point's job: sibling cards get it from `.card + .card`,
  // the project home passes a wrapper class with its own margins.
  const root = h('div', { className: 'card' + (opts.className ? ' ' + opts.className : '') });

  async function load() {
    let project, summary, hosting;
    try {
      [project, summary, hosting] = await Promise.all([
        get(`/admin/api/projects/${projectId}`),
        get(`/admin/api/projects/${projectId}/summary`),
        get(`/admin/api/projects/${projectId}/hosting/status`),
      ]);
    } catch {
      // The strip is a summary surface; keep quiet on load failure.
      return;
    }
    render(project, summary, hosting);
  }

  function render(project, summary, hosting) {
    clear(root);
    const { url } = formatPublicUrl(project, state.get('systemStatus'));
    const ps = derivePublishState({
      published: project.published,
      pagesCount: summary.pages_count ?? 0,
      hosting,
      url: url === '#' ? '' : url,
    });

    const actions = [];
    if (ps.key === 'live' || ps.key === 'unverified' || ps.key === 'tls-off' || ps.key === 'local' || ps.key === 'lan') {
      actions.push(h('a', { className: 'btn btn--primary btn--sm', href: url, target: '_blank', rel: 'noopener' }, t('ps.openSite')));
    }
    if (state.isAdmin()) {
      const published = project.published;
      const toggle = async () => {
          toggleBtn.disabled = true;
          try {
            const updated = await put(`/admin/api/projects/${projectId}/published`, { published: !published });
            toast.success(updated.published ? t('ps.nowPublished') : t('ps.nowUnpublished'));
            const projects = await get('/admin/api/projects').catch(() => null);
            if (projects) state.set('projects', projects);
            await load();
          } catch (err) {
            toast.error(t(published ? 'ps.unpublishFailed' : 'ps.publishFailed', { error: humanizeError(err.message) }));
            toggleBtn.disabled = false;
          }
      };
      const toggleBtn = h('button', {
        className: `btn ${published ? 'btn--danger' : 'btn--success'} btn--sm`,
        // Unpublishing takes the site offline for every visitor, so it asks first.
        onClick: () => (published
          ? modal.confirmDanger(t('ps.unpublishTitle'),
            t('ps.unpublishBody'),
            toggle, t('ps.unpublishBtn'))
          : toggle()),
      }, published ? t('ps.unpublishBtn') : t('ps.publishBtn'));
      actions.push(toggleBtn);
    }

    root.append(
      h('div', { className: 'card__header' }, [
        h('div', { className: 'flex items-center gap-2' }, [
          h('span', { className: `badge badge--${ps.kind}` }, ps.label),
          h('h4', { className: 'card__title' }, t('ps.visitors')),
        ]),
        h('div', { className: 'flex items-center gap-2' }, actions),
      ]),
      h('p', { className: 'form-hint', style: 'margin:0.5rem 0 0' }, linkifyUrls(ps.detail)),
    );
  }

  load();
  root.refresh = load;
  return root;
}
