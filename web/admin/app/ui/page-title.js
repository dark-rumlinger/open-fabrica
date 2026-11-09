/**
 * What the browser tab should say for a route. Every page used to share one
 * constant title, so the tab list, history menu and screen-reader page
 * announcement could not tell "Users" from a project's chat.
 *
 * Pure on purpose (the lookups come in as arguments) so it is testable without
 * the DOM; ui/layout.js feeds it the live state.
 */

import { t } from '../core/i18n.js';

/**
 * @param {string} path - the current route path.
 * @param {object} [lookups]
 * @param {Array<{path?: string, label?: string}>} [lookups.links] - global menu entries.
 * @param {Array<{id: number, name: string}>} [lookups.projects]
 * @param {(panelId: string) => string} [lookups.panelLabel] - label for a project panel id.
 * @returns {string} the page part of the title, '' when there is nothing to add.
 */
export function titleForPath(path, { links = [], projects = [], panelLabel = () => '' } = {}) {
  if (!path || path === '/' || path === '/new') return t('nav.newProject');
  if (path === '/login') return t('login.signIn');

  const project = path.match(/^\/projects\/(\d+)(?:\/([^/?#]+))?/);
  if (project) {
    const found = projects.find((p) => p.id === Number(project[1]));
    const name = found ? found.name : t('nav.project');
    const label = panelLabel(project[2] || 'chat');
    return label ? `${label} | ${name}` : name;
  }

  const link = links.find((l) => l.path === path);
  return link && link.label ? link.label : '';
}
