/**
 * Owner-facing stage vocabulary: everything a user sees maps the internal
 * stage codenames (EXPAND/PLAN/BUILD/LIVE) through here so the chat speaks
 * plainly. Backend mirror is engine/publish.go (friendlyStageName +
 * stageStartLine); keep the two in sync. Unknown codes fall back to the raw
 * code so nothing renders blank.
 */

import { t } from '../../core/i18n.js';

// Past-tense labels for the "stage complete" divider, e.g. "Got to know your
// idea · 1m 20s". Reads as a finished step rather than "EXPAND complete".
const STAGE_DIVIDER_LABELS = {
  EXPAND: t('stage.divider.expand'),
  PLAN: t('stage.divider.plan'),
  BUILD: t('stage.divider.build'),
  LIVE: t('phase.live'),
};

// Short present-tense labels for the always-visible pipeline tracker pills.
export const STAGE_PILL_LABELS = {
  EXPAND: t('stage.pill.expand'),
  PLAN: t('stage.pill.plan'),
  BUILD: t('stage.pill.build'),
  LIVE: t('phase.live'),
};

// Per-stage icon (rendered SVG, not a text glyph). Mirrors the tracker's
// existing icon vocabulary so the divider and the pills look related.
export const STAGE_ICONS = {
  EXPAND: 'sparkle',
  PLAN: 'search',
  BUILD: 'zap',
  LIVE: 'activity',
};

/** Friendly past-tense label for a completed stage, for the divider. */
export function stageDividerLabel(code) {
  return STAGE_DIVIDER_LABELS[code] || code;
}

/** Icon name for a stage, falling back to a generic check. */
export function stageIcon(code) {
  return STAGE_ICONS[code] || 'check';
}
