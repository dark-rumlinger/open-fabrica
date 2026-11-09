/**
 * Slim early release strip for the app shell, under the top bar.
 *
 * Dismissal is remembered per version, so the strip comes back after an
 * upgrade. The always-visible badge in the top bar (see topbar.js) keeps the
 * status in view once this is dismissed.
 */

import { h } from '../core/dom.js';
import { t } from '../core/i18n.js';
import * as state from '../core/state.js';
import { icon } from './icon.js';
import { previewSummary } from './preview-notice.js';

function storageKey(version) {
  return 'of:dismissed:preview-banner:' + (version || 'unknown');
}

function isDismissed(version) {
  try {
    return !!localStorage.getItem(storageKey(version));
  } catch {
    return false;
  }
}

/**
 * @returns {{ element: HTMLElement }}
 */
export function createPreviewBanner() {
  const text = h('span', { className: 'preview-banner__text' });

  const element = h('div', { className: 'preview-banner', role: 'note' }, [
    h('span', { innerHTML: icon('alert-circle') }),
    text,
    h('button', {
      className: 'btn btn--ghost btn--sm preview-banner__close',
      type: 'button',
      'aria-label': t('preview.dismiss'),
      onClick: () => {
        const version = (state.get('systemStatus') || {}).version;
        try { localStorage.setItem(storageKey(version), '1'); } catch { /* ignore */ }
        element.hidden = true;
      },
      innerHTML: icon('x'),
    }),
  ]);

  // The version can arrive after the shell is built, so recheck on change.
  function refresh() {
    const version = (state.get('systemStatus') || {}).version;
    text.textContent = previewSummary(version);
    element.hidden = isDismissed(version);
  }

  state.watch('systemStatus', refresh);
  refresh();

  return { element };
}
