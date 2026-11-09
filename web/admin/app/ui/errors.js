/**
 * Translate technical error strings into plain language for end users.
 * Pass any error message (or `Error.message`) through this before displaying
 * it in a toast or empty state. Unmapped messages are returned unchanged.
 */

import { t } from '../core/i18n.js';

export function humanizeError(msg) {
  if (!msg) return msg;
  if (msg instanceof Error) msg = msg.message;
  if (typeof msg !== 'string') return msg;

  for (const [pattern, key] of MAPPINGS) {
    if (pattern.test(msg)) return t(key);
  }
  // The http.js fallback for any other status keeps its number, so a report can
  // still name it.
  const failed = /^\s*request failed \((\d{3})\)\s*$/i.exec(msg);
  if (failed) return t('errors.requestFailed', { status: failed[1] });
  return msg;
}

// Each entry pairs a pattern with a catalog key (errors.*).
const MAPPINGS = [
  // Provider-specific phrasings first: an "invalid api key" or unreachable-host
  // error from a provider test means something more specific than the generic
  // auth/network messages below, so match it before they get a chance to.
  [/invalid[_ ]?api[_ ]?key/i, 'errors.invalidApiKey'],
  // Hosting-specific phrasings, matched before the generic network ones: bind
  // and certificate failures already arrive humanized from the server, but the
  // raw variants can still surface via saves and rechecks.
  [/address already in use|only one usage/i, 'errors.portInUse'],
  [/bind.*permission denied|cap_net_bind_service/i, 'errors.bindDenied'],
  [/acme|let'?s encrypt/i, 'errors.acme'],
  [/tls certificate|obtaining certificate|issuing certificate/i, 'errors.tlsCert'],
  [/no such host|dial tcp|unreachable/i, 'errors.unreachableHost'],
  [/dns lookup|dns record|does not resolve/i, 'errors.dns'],
  [/UNIQUE constraint failed/i, 'errors.unique'],
  [/no such table/i, 'errors.noTable'],
  [/no such column/i, 'errors.noColumn'],
  [/FOREIGN KEY constraint failed/i, 'errors.foreignKey'],
  [/NOT NULL constraint failed/i, 'errors.notNull'],
  [/CHECK constraint failed/i, 'errors.check'],
  [/database is locked/i, 'errors.locked'],
  [/context deadline exceeded/i, 'errors.deadline'],
  [/connection refused/i, 'errors.refused'],
  [/rate limit/i, 'errors.rateLimit'],
  // Status words only match when they ARE the message (or the http.js fallback
  // "Request failed (NNN)"). A bare /401|404/ also rewrote specific server and
  // provider messages that merely contained those characters.
  [/^\s*unauthorized\s*$|^request failed \(401\)/i, 'errors.unauthorized'],
  [/^\s*forbidden\s*$|^request failed \(403\)/i, 'errors.forbidden'],
  [/^\s*not found\s*$|^request failed \(404\)/i, 'errors.notFound'],
  [/^network error|fetch failed|failed to fetch|load failed|networkerror/i, 'errors.network'],
  [/timeout|timed out/i, 'errors.timeout'],
  [/invalid json/i, 'errors.invalidJson'],
  [/cannot delete the last admin/i, 'errors.lastAdminDelete'],
  [/cannot demote the last admin/i, 'errors.lastAdminDemote'],
];
