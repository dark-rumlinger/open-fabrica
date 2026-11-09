/**
 * Early release messaging, kept in one place so the setup step, the shell
 * banner and the About dialog all say the same thing.
 */

import { t } from '../core/i18n.js';

export const PREVIEW_TITLE = t('preview.title');

export const PREVIEW_LEDE = t('preview.lede');

// Shown as a short list in the setup step.
export const PREVIEW_POINTS = [
  t('preview.point1'),
  t('preview.point2'),
  t('preview.point3'),
];

/** One-line summary for the banner and the About dialog. */
export function previewSummary(version) {
  return version
    ? t('preview.summaryVersion', { version })
    : t('preview.summary');
}
