/**
 * Router port forwarding status for the General tab: while the router
 * forwards ports 80 and 443 here, say so and offer to stop. Turning it on
 * lives in the going-live card, where it is one of the outstanding steps; this
 * is the lasting home for turning it off once that card has gone away.
 */

import { h, clear } from '../core/dom.js';
import { t } from '../core/i18n.js';
import { get, post } from '../core/http.js';
import * as toast from './toast.js';
import * as modal from './modal.js';
import * as state from '../core/state.js';
import { humanizeError } from './errors.js';
import { isPublicDomain } from './helpers.js';

const PROTOCOL_NAMES = { upnp: 'UPnP', natpmp: 'NAT-PMP', pcp: 'PCP' };

/**
 * Renders the notice into container when forwarding is enabled. Admin-only
 * and best-effort: any error renders nothing.
 */
export async function loadRouterForwardingNotice(container, project) {
  if (!state.isAdmin()) return;
  if (!isPublicDomain(project.domain)) return;
  let status;
  try {
    status = await get('/admin/api/system/portmap/status');
  } catch {
    return;
  }
  render(container, status);
}

function render(container, status) {
  clear(container);
  if (!status || !status.enabled) return;

  const via = PROTOCOL_NAMES[status.protocol] || t('rf.automatic');
  const ip = status.internal_ip ? ` (${status.internal_ip})` : '';
  const text = status.active
    ? t('rf.active', { ip, via })
    : t(status.error ? 'rf.failedWithError' : 'rf.failed', { error: status.error });

  const stop = async () => {
    stopBtn.disabled = true;
    try {
      render(container, await post('/admin/api/system/portmap/disable'));
      toast.success(t('rf.stopped'));
    } catch (err) {
      toast.error(t('rf.stopFailed', { error: humanizeError(err.message) }));
      stopBtn.disabled = false;
    }
  };
  const stopBtn = h('button', {
    className: 'btn btn--secondary btn--sm',
    type: 'button',
    // Stopping makes the site unreachable from the internet, so it asks first.
    onClick: () => modal.confirmDanger(t('rf.stopTitle'),
      t('rf.stopBody'),
      stop, t('rf.stopTitle')),
  }, t('rf.stopTitle'));

  container.appendChild(h('div', { className: 'notice' }, [
    h('p', { className: 'form-hint' }, text),
    stopBtn,
  ]));
}
