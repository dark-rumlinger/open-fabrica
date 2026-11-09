/**
 * One-click "allow through firewall" notice, shared by project settings and
 * the going-live task card. Read-only status fetch, then a button that triggers
 * the elevation prompt (UAC/polkit) on the host machine.
 */

import { h, clear } from '../core/dom.js';
import { t } from '../core/i18n.js';
import { get, post } from '../core/http.js';
import * as toast from './toast.js';
import * as state from '../core/state.js';
import { humanizeError } from './errors.js';
import { isPublicDomain } from './helpers.js';

/** Fetches firewall status; null when unavailable (treated as "nothing to do"). */
export async function fetchFirewallStatus() {
  try {
    return await get('/admin/api/firewall/status');
  } catch {
    return null;
  }
}

/**
 * Fetches the local firewall status and, if the firewall is blocking inbound
 * on a machine serving a public domain, renders the one-click control into
 * container. Admin-only and best-effort: any error or irrelevant state renders
 * nothing.
 */
export async function loadFirewallNotice(container, project) {
  if (!state.isAdmin()) return;
  if (!isPublicDomain(project.domain)) return;
  const status = await fetchFirewallStatus();
  if (status) renderFirewallNotice(container, status);
}

/**
 * Renders the notice for a known status. No-op when the platform has no
 * managed firewall, it is off, or our rule already exists.
 * opts.onAllowed fires after the rule is added (the wizard advances on it).
 */
export function renderFirewallNotice(container, status, opts = {}) {
  clear(container);
  if (!status || !status.active || status.rule_present) return;
  // Active but not manageable: a setting only the owner can change (macOS
  // "block all incoming connections"). Say what it is; no button can help.
  if (!status.supported) {
    if (status.detail) {
      container.appendChild(h('div', { className: 'notice notice--warning' }, [
        h('p', { className: 'form-hint' }, status.detail),
      ]));
    }
    return;
  }

  const allowBtn = h('button', {
    className: 'btn btn--primary btn--sm',
    type: 'button',
    onClick: async () => {
      allowBtn.disabled = true;
      const original = allowBtn.textContent;
      allowBtn.textContent = t('fwn.waiting');
      try {
        const res = await post('/admin/api/firewall/allow');
        if (res.ok) {
          toast.success(t('fwn.allowed'));
          renderFirewallNotice(container, { ...status, rule_present: true }, opts);
          if (opts.onAllowed) opts.onAllowed();
        } else if (res.cancelled) {
          toast.warning(t('fwn.cancelled'));
          allowBtn.disabled = false;
          allowBtn.textContent = original;
        } else {
          toast.error(res.error || t('fwn.failed'));
          allowBtn.disabled = false;
          allowBtn.textContent = original;
        }
      } catch (err) {
        toast.error(t('fwn.updateFailed', { error: humanizeError(err.message) }));
        allowBtn.disabled = false;
        allowBtn.textContent = original;
      }
    },
  }, t('fwn.allowBtn'));

  container.appendChild(h('div', { className: 'notice notice--warning' }, [
    h('p', { className: 'form-hint' },
      (status.detail ? status.detail + ' ' : '') + t('fwn.body')),
    allowBtn,
  ]));
}
