/**
 * Renders the "where do I get an API key" hint shown beneath an API-key
 * input. URL is sourced from the provider record (signup_url column on
 * providers table) so adding a new provider is data-only, no UI patch
 * needed for the link to appear in the wizard or the Add modal.
 */

import { h } from '../core/dom.js';
import { t } from '../core/i18n.js';

function safeHostname(url) {
  try { return new URL(url).hostname; } catch { return url; }
}

/**
 * True for a provider on a subscription plan endpoint (providers.plan).
 * @param {{plan?: string}} provider
 */
export function isCodingPlan(provider) {
  return !!provider && provider.plan === 'coding';
}

/**
 * @param {string} signupUrl  e.g. "https://example.com/settings/keys"
 * @param {Object} [opts]
 * @param {string} [opts.fallback]  Hint shown when signupUrl is empty.
 * @returns {HTMLElement}
 */
export function signupHint(signupUrl, opts = {}) {
  const fallback = opts.fallback || t('signup.fallback');
  if (!signupUrl) return h('p', { className: 'form-hint' }, fallback);
  return h('p', { className: 'form-hint' }, [
    t('signup.getKey') + ' ',
    h('a', { href: signupUrl, target: '_blank', rel: 'noopener noreferrer' }, safeHostname(signupUrl)),
  ]);
}
