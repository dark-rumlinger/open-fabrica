/**
 * The single home for "what is this project's hosting state, and what do we
 * call it". The compact chip (hosting-status.js) and the Visitors strip
 * (publish-status.js) both read this, so one state can never be a success in
 * one surface and a red failure in the other.
 *
 * These labels describe reachability only. "Live" is the project phase
 * (phase-state.js) and is never used here.
 *
 * Callers supply the visitor-facing url (helpers.formatPublicUrl) rather than
 * having this module build a second one.
 */

import { t } from '../core/i18n.js';

/**
 * Maps a /hosting/status payload to a view.
 *   key             stable id, for callers that branch on state
 *   kind            badge flavor
 *   chip            terse label, for the chip beside the domain field
 *   label           sentence-case label, for the Visitors strip badge
 *   sentence        the one-line explanation
 *   pending         something is still in flight, so keep polling
 *   needsPhoneCheck only a device off this network can settle it
 */
export function classifyHosting(st, { url = '' } = {}) {
  const https = (st && st.https) || {};

  switch (st.domain_kind) {
    case 'none':
      return {
        key: 'no-domain', kind: 'secondary', chip: t('hosting.noDomain.chip'), label: t('hosting.noDomain.label'),
        sentence: t('hosting.noDomain.sentence'),
      };
    case 'local':
      return {
        key: 'local', kind: 'info', chip: t('hosting.local.chip'), label: t('hosting.local.label'),
        sentence: t('hosting.local.sentence', { url }),
      };
    case 'ip':
      return {
        key: 'lan', kind: 'info', chip: t('hosting.lan.chip'), label: t('hosting.lan.label'),
        sentence: t('hosting.lan.sentence', { url }),
      };
  }

  if (st.tls_disabled || https.state === 'disabled') {
    return {
      key: 'tls-off', kind: 'warning', chip: t('hosting.tlsOff.chip'), label: t('hosting.tlsOff.label'),
      sentence: t('hosting.tlsOff.sentence', { url }),
    };
  }

  switch (https.state) {
    case 'active':
      return {
        key: 'live', kind: 'success', chip: t('hosting.live.chip'), label: t('hosting.live.label'),
        sentence: t('hosting.live.sentence', { url }),
      };
    // A loaded certificate means issuance worked and 443 is served. The server
    // reports unverified when its own self-dial could not confirm that through
    // the router; cert_loaded on a failed probe is the same situation seen from
    // a status that predates the unverified state. Neither is a problem.
    case 'unverified':
      return {
        key: 'unverified', kind: 'success', chip: t('hosting.unverified.chip'), label: t('hosting.live.label'),
        sentence: https.hint || t('hosting.unverified.sentence', { url }),
        needsPhoneCheck: true,
      };
    case 'failed':
      if (https.cert_loaded) {
        return {
          key: 'unverified', kind: 'success', chip: t('hosting.unverified.chip'), label: t('hosting.live.label'),
          sentence: t('hosting.unverified.sentenceFailed', { url }),
          needsPhoneCheck: true,
        };
      }
      return {
        key: 'problem', kind: 'danger', chip: t('hosting.problem.chip'), label: t('hosting.problem.label'),
        sentence: https.hint || t('hosting.problem.sentence'),
      };
    default:
      return {
        key: 'pending', kind: 'warning', chip: t('hosting.pending.chip'), label: t('hosting.pending.label'),
        sentence: t('hosting.pending.sentence'),
        pending: true,
      };
  }
}
