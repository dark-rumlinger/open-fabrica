/**
 * The single home for "what phase is this project in, and what do we call it".
 * The project header, the projects list, the project switcher, the chat stat
 * card and the pipeline feed all read this, so one project cannot be "Building"
 * in one place and waiting in another.
 *
 * The server derives the phase (internal/projectphase) and sends it as
 * `project.phase`; this module only names it and picks the one action that
 * moves it forward. "Live" is a phase and nothing else: whether visitors can
 * reach the site is the separate `project.published` flag.
 *
 * Phase keys: building, needs_you, live, paused.
 */

import { t, tn } from '../core/i18n.js';

const VIEWS = {
  building: {
    label: t('phase.building'),
    kind: 'info',
    dot: 'status-dot--active',
    sentence: t('phase.buildingSentence'),
  },
  needs_you: {
    label: t('phase.needsYou'),
    kind: 'warning',
    dot: 'status-dot--warning',
    sentence: t('phase.needsYouSentence'),
  },
  live: {
    label: t('phase.live'),
    kind: 'success',
    dot: 'status-dot--live',
    sentence: t('phase.liveSentence'),
  },
  paused: {
    label: t('phase.paused'),
    kind: 'secondary',
    dot: '',
    sentence: t('phase.pausedSentence'),
  },
};

// The server sends each reason as an English line (the Telegram bot shows it as
// written) and as a key; the key is what this UI translates. `pending` carries the
// count for the plural one.
const REASONS = {
  answers: (phase) => tn('phase.reason.answers', phase.pending || 1),
  answers_none: () => t('phase.reason.answersNone'),
  approval: () => t('phase.reason.approval'),
  engine_down: () => t('phase.reason.engineDown'),
  stopped_owner: () => t('phase.reason.stoppedOwner'),
  stopped_error: () => t('phase.reason.stoppedError'),
  paused_owner: () => t('phase.reason.pausedOwner'),
};

/**
 * The reason for a phase in the interface language. A key this build does not
 * know (a newer server) falls back to the server's English line, and a phase with
 * no reason gives an empty string.
 */
export function phaseReason(phase) {
  const make = REASONS[phase.reason_key];
  return make ? make(phase) : (phase.reason || '');
}

/**
 * Maps a phase to a view.
 *   key       stable id, for callers that branch on phase
 *   label     the badge text
 *   kind      badge flavor
 *   dot       the status-dot modifier class, empty for a plain dot
 *   error     true when the project paused after a build error, so the badge
 *             reads as a warning rather than a plain grey pause
 *   reason    one short line saying why, may be empty
 *   sentence  the reason when there is one, else the general explanation
 *   action    the one thing that moves the project forward (see phaseAction)
 */
export function describePhase(phase) {
  const base = VIEWS[phase.key];
  const stalled = !!phase.stalled;
  // A pause after an error is not a deliberate one, so it wears warning
  // colours (like a stalled build) instead of the quiet grey of "Paused by you".
  const error = phase.key === 'paused' && phase.reason_key === 'stopped_error';
  const reason = phaseReason(phase);
  return {
    key: phase.key,
    label: base.label,
    kind: stalled || error ? 'warning' : base.kind,
    dot: stalled || error ? 'status-dot--warning' : base.dot,
    error,
    reason,
    sentence: reason || base.sentence,
    action: phaseAction(phase),
  };
}

/**
 * The single primary action for a phase.
 *   start   the engine is shut down, so nothing progresses until it starts
 *   answer  open the questions tab
 *   pause   stop the pipeline; `interrupt` says whether a build is in flight
 *           (park it at the next safe boundary) or the project is idle (flip
 *           the mode)
 *   resume  return to `to`, the mode the project was in before it paused
 */
export function phaseAction(phase) {
  switch (phase.key) {
    case 'needs_you':
      return { id: 'answer', label: t('phase.answer') };
    case 'paused':
      return { id: 'resume', label: t('phase.resume'), to: phase.resume_to };
    case 'live':
      return { id: 'pause', label: t('phase.pause'), interrupt: false };
    default:
      if (phase.stalled) return { id: 'start', label: t('phase.start') };
      return { id: 'pause', label: t('phase.pause'), interrupt: true };
  }
}
