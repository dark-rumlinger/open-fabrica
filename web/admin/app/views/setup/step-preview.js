/**
 * Setup Step 2: Early release notice. Shown before anything is asked of the
 * user, so they know what they are getting into.
 */

import { h } from '../../core/dom.js';
import { t } from '../../core/i18n.js';
import { icon } from '../../ui/icon.js';
import {
  PREVIEW_TITLE,
  PREVIEW_LEDE,
  PREVIEW_POINTS,
} from '../../ui/preview-notice.js';

export function renderPreview({ body, setHeader, setActions, next, back }) {
  setHeader(PREVIEW_TITLE, PREVIEW_LEDE);

  // One quiet row per point: a small icon, then the sentence, hairline-separated.
  const icons = ['alert-triangle', 'rotate-ccw', 'zap'];
  body.appendChild(h('ul', { className: 'preview-list' }, PREVIEW_POINTS.map((p, i) => h('li', { className: 'preview-list__item' }, [
    h('span', { className: 'preview-list__icon', innerHTML: icon(icons[i % icons.length]) }),
    h('span', { className: 'preview-list__text' }, p),
  ]))));

  setActions([
    h('button', { className: 'btn btn--ghost', onClick: back }, t('common.back')),
    h('button', { className: 'btn btn--primary', onClick: next }, t('setup.understand')),
  ]);
}
