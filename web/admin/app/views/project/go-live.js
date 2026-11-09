/**
 * The human half of going live, as a to-do card driven by live status instead
 * of a modal sequence.
 *
 * Everything a machine can do on its own already happens without being asked:
 * the TLS proxy is driven by DB state, so saving a domain binds the ports,
 * orders the certificate, renews it, and retries once DNS propagates. What is
 * left is only what happens outside this computer - a DNS record at the
 * registrar, a port forward on the router, and a look from a device that is not
 * on this network - so those are the only things this card ever shows, and only
 * while they are still outstanding. When nothing is outstanding it renders
 * nothing at all.
 *
 * Checks run on demand. Nothing here polls a half-finished setup and reports the
 * intermediate state as an error, which is what made the old step-by-step
 * version throw failures at a setup that was merely still in progress.
 */

import { h, clear } from '../../core/dom.js';
import { t } from '../../core/i18n.js';
import { get, post } from '../../core/http.js';
import * as toast from '../../ui/toast.js';
import * as state from '../../core/state.js';
import { icon } from '../../ui/icon.js';
import { humanizeError } from '../../ui/errors.js';
import { formatPublicUrl, linkifyUrls, copyText } from '../../ui/helpers.js';
import { classifyHosting } from '../../ui/hosting-state.js';
import { fetchFirewallStatus, renderFirewallNotice } from '../../ui/firewall-notice.js';

/** Shared bits ------------------------------------------------------------ */

function taskTitle(text) {
  return h('h5', { style: 'margin:0 0 0.35rem;font-size:0.95rem' }, text);
}

function para(text) {
  return h('p', { className: 'text-secondary', style: 'margin:0 0 0.5rem' }, linkifyUrls(text));
}

function why(text) {
  return h('details', { style: 'margin-top:0.35rem' }, [
    h('summary', { className: 'form-hint', style: 'cursor:pointer' }, t('gl.why')),
    h('p', { className: 'form-hint', style: 'margin:0.35rem 0 0' }, text),
  ]);
}

function copyButton(value, label = t('common.copy')) {
  return h('button', {
    className: 'btn btn--secondary btn--sm',
    type: 'button',
    onClick: () => copyText(value, t('helpers.copied')),
  }, label);
}

/** One task block, separated from the previous one by a hairline rule. */
function task(children, first) {
  return h('div', {
    style: first
      ? 'margin-top:0.75rem'
      : 'margin-top:1rem;padding-top:1rem;border-top:1px solid var(--border)',
  }, children);
}

/** One DNS record line, with its value ready to copy when it is known. */
function recordRow(type, value, copyValue) {
  return h('div', {
    style: 'background:var(--bg-secondary);border-radius:6px;padding:0.75rem;margin:0.5rem 0;display:flex;align-items:center;gap:12px;flex-wrap:wrap',
  }, [
    h('code', {}, t('gl.dnsRecord', { type, value })),
    ...(copyValue ? [copyButton(copyValue, type === 'A' ? t('gl.copyIp') : t('gl.copyIpv6'))] : []),
  ]);
}

/**
 * The records to enter at the registrar. The A record carries the public IP
 * the router reported; behind a shared (carrier-grade NAT) address that IP is
 * not this computer's to use, so only the IPv6 record is offered then.
 */
function dnsRecords(net) {
  const rows = [];
  const v6 = (net.ipv6 || [])[0];
  if (!net.shared_address) {
    const value = net.public_ip || t('gl.yourPublicIp');
    rows.push(recordRow('A', value, net.public_ip));
    if (net.public_ip && net.public_ip_source === 'dns') {
      rows.push(h('p', { className: 'form-hint', style: 'margin:0 0 0.5rem' }, t('gl.dnsSource')));
    }
  }
  if (v6) {
    rows.push(recordRow('AAAA', v6, v6));
    if (!net.shared_address) {
      rows.push(h('p', { className: 'form-hint', style: 'margin:0 0 0.5rem' }, t('gl.aaaaNote')));
    }
  }
  if (!rows.length) {
    rows.push(para(t('gl.noAddress')));
  }
  return h('div', {}, rows);
}

/** Tasks ------------------------------------------------------------------ */

/**
 * No usable domain yet. The domain field itself lives in Settings > General
 * (its one home); this only explains what to get and shows the record to add,
 * so DNS can start propagating before the domain is even saved.
 */
function domainTask(st, net, first) {
  const current = st.domain_kind === 'none'
    ? t('gl.noDomain')
    : t('gl.localOnly', { domain: st.domain });
  return task([
    taskTitle(t('gl.getDomain')),
    para(t('gl.domainBody', { current })),
    para(t('gl.domainRecord')),
    dnsRecords(net),
    // Worth knowing before paying for a domain: whether this connection can
    // accept visitors at all. Only asked when the router could not tell us.
    ...(!net.router && net.nat_likely ? [addressCheck()] : []),
    why(t('gl.domainWhy')),
  ], first);
}

/**
 * A public domain is saved but HTTPS has not come up. The most likely reason by
 * far is that the DNS record is missing or still spreading, so show the record
 * and let the owner check when they are ready. The check is a button, never an
 * automatic poll: DNS routinely takes hours, and an automatic "not found" reads
 * as a failure when it only means "not yet".
 */
function dnsTask(st, net, first) {
  const result = h('p', { className: 'form-hint', style: 'margin:0.5rem 0 0' });
  const checkBtn = h('button', {
    className: 'btn btn--primary btn--sm',
    type: 'button',
    onClick: async () => {
      checkBtn.disabled = true;
      result.textContent = t('hs.checking');
      try {
        const res = await get('/admin/api/domain/validate?domain=' + encodeURIComponent(st.domain));
        result.textContent = res.valid
          ? t('gl.resolves')
          : (res.error || (res.warnings || []).join(' ') || t('gl.notResolving')) + ' ' + t('gl.dnsSpread');
      } catch (err) {
        result.textContent = t('hs.checkFailed', { error: humanizeError(err.message) });
      }
      checkBtn.disabled = false;
    },
  }, t('gl.checkNow'));

  return task([
    taskTitle(t('gl.pointDomain')),
    para(t('gl.pointBody', { domain: st.domain })),
    dnsRecords(net),
    h('div', {}, [checkBtn]),
    result,
    why(t('gl.pointWhy')),
  ], first);
}

/**
 * Local OS firewall. The one step here that is genuinely one click.
 *
 * The notice carries the server's own description of what it found, rather than
 * restating it as "the firewall is blocking incoming connections", which the check
 * cannot actually establish - it only knows whether OUR rule exists, and any
 * other rule may already be letting visitors through.
 */
function firewallTask(status, first, onAllowed) {
  // The notice is a nested card sitting straight under a heading, with no
  // paragraph in between like the other tasks have, so it needs its own top
  // margin. 1rem is the same 16px gap cards get from each other elsewhere; the
  // notice's own 0.5rem collapses through this wrapper and loses to it.
  const noticeBox = h('div', { style: 'margin-top:1rem' });
  renderFirewallNotice(noticeBox, status, { onAllowed });
  if (!noticeBox.firstChild) return null;
  return task([
    taskTitle(t('gl.acceptVisitors')),
    noticeBox,
    why(t('gl.firewallWhy')),
  ], first);
}

/** The by-hand port forward steps, for routers that cannot be asked. */
function manualForwardSteps(net) {
  const localIP = (net.local_ips || [])[0];
  return [
    h('ol', { className: 'text-secondary', style: 'margin:0 0 0.5rem 1.25rem' }, [
      h('li', {}, t('gl.step1')),
      h('li', {}, t('gl.step2')),
      h('li', {}, t('gl.step3', { ip: localIP || t('gl.thisComputerIp') })),
    ]),
    ...(localIP ? [h('div', {}, [copyButton(localIP, t('gl.copyValue', { value: localIP }))])] : []),
    para(t('gl.routerGuide')),
  ];
}

/**
 * Router port forwarding. When the router answered UPnP, NAT-PMP or PCP it
 * can be asked to do it, so that is one click; the by-hand steps stay
 * available underneath for routers that refuse.
 */
function routerTask(net, st, first, onChange) {
  const reason = t('gl.routerReason');
  const routerAnswered = Boolean(net.router && net.router.can_map);
  // With built-in HTTPS off, something else serves 80 and 443, so Open Fabrica
  // does not claim the forwards for itself; the by-hand steps still apply.
  if (!routerAnswered || st.tls_disabled) {
    const intro = routerAnswered ? t('gl.routerIntroTls') : t('gl.routerIntroNoUpnp');
    return task([
      taskTitle(t('gl.routerTitle')),
      para(intro),
      ...manualForwardSteps(net),
      ...(routerAnswered ? [] : [addressCheck()]),
      why(reason),
    ], first);
  }

  const result = h('p', { className: 'form-hint', style: 'margin:0.5rem 0 0' });
  const openBtn = h('button', {
    className: 'btn btn--primary btn--sm',
    type: 'button',
    onClick: async () => {
      openBtn.disabled = true;
      result.textContent = t('gl.askingRouter');
      try {
        const res = await post('/admin/api/system/portmap/enable');
        if (res.active) {
          toast.success(t('gl.forwarded'));
          onChange();
          return;
        }
        result.textContent = (res.error || t('gl.routerRefused')) + ' ' + t('gl.byHandSee');
      } catch (err) {
        result.textContent = t('gl.openFailed', { error: humanizeError(err.message) });
      }
      openBtn.disabled = false;
    },
  }, t('gl.openPorts'));

  return task([
    taskTitle(t('gl.routerTitle')),
    para(t('gl.routerBody')),
    h('div', {}, [openBtn]),
    result,
    h('details', { style: 'margin-top:0.5rem' }, [
      h('summary', { className: 'form-hint', style: 'cursor:pointer' }, t('gl.byHand')),
      h('div', { style: 'margin-top:0.5rem' }, manualForwardSteps(net)),
    ]),
    why(reason + ' ' + t('gl.routerWhyExtra')),
  ], first);
}

/**
 * The router itself only has a shared address from the internet provider
 * (carrier-grade NAT). Port forwarding on it cannot help, so say so plainly
 * and point at the two ways out.
 */
function sharedAddressTask(net, first) {
  const hasV6 = (net.ipv6 || []).length > 0;
  const ext = (net.router && net.router.external_ip) || t('gl.itsAddress');
  return task([
    taskTitle(t('gl.sharedTitle')),
    para(t('gl.sharedBody', { ext })),
    h('ul', { className: 'text-secondary', style: 'margin:0 0 0.5rem 1.25rem' }, [
      ...(hasV6 ? [h('li', {}, t('gl.useIpv6'))] : []),
      h('li', {}, t('gl.askProvider')),
    ]),
    why(t('gl.sharedWhy')),
  ], first);
}

/**
 * The router's own internet address is a private one, so another router sits
 * in front of it, usually the provider's modem. Visitors have to get through
 * both, so the outer one needs the forward too, or has to step aside.
 */
function doubleNatTask(net, first) {
  const ext = net.router.external_ip;
  return task([
    taskTitle(t('gl.doubleTitle')),
    para(t('gl.doubleBody', { ext })),
    h('ul', { className: 'text-secondary', style: 'margin:0 0 0.5rem 1.25rem' }, [
      h('li', {}, t('gl.bridgeMode')),
      h('li', {}, t('gl.forwardOnModem', { ext })),
    ]),
    h('div', {}, [copyButton(ext, t('gl.copyValue', { value: ext }))]),
    why(t('gl.doubleWhy')),
  ], first);
}

/**
 * When the router does not answer, Open Fabrica cannot see whether the
 * connection can accept visitors at all. The router's admin page can: this is
 * the one number to look for there.
 */
function addressCheck() {
  return h('details', { style: 'margin-top:0.5rem' }, [
    h('summary', { className: 'form-hint', style: 'cursor:pointer' }, t('gl.notSure')),
    h('div', { style: 'margin-top:0.5rem' }, [
      para(t('gl.noAnswer')),
      h('ul', { className: 'text-secondary', style: 'margin:0 0 0.5rem 1.25rem' }, [
        h('li', {}, t('gl.range1')),
        h('li', {}, t('gl.range2')),
        h('li', {}, t('gl.range3')),
      ]),
      para(t('gl.hotspots')),
    ]),
  ]);
}

/**
 * The domain resolves, but to an address this computer no longer has. Home
 * connections get a new address now and then; the record has to follow.
 */
function ipChangedTask(st, first) {
  const m = st.dns_mismatch;
  return task([
    taskTitle(t('gl.changedTitle')),
    para(t('gl.changedBody', { domain: st.domain, from: m.resolves_to.join(', '), to: m.should_be })),
    recordRow('A', m.should_be, m.should_be),
    why(t('gl.changedWhy')),
  ], first);
}

/**
 * The certificate is in place but this machine cannot dial itself through its
 * own router, which most routers do not allow. Only a device off this network
 * can settle it, so this is the one check a phone has to make.
 */
async function phoneTask(projectId, url, first) {
  let qr = null;
  try {
    qr = await get(`/admin/api/projects/${projectId}/hosting/qr`);
  } catch {
    // No QR code available; the copyable link below still does the job.
  }
  const target = (qr && qr.url) || url;
  return task([
    taskTitle(t('gl.phoneTitle')),
    para(t('gl.phoneBody')),
    h('div', { style: 'display:flex;align-items:center;gap:16px;flex-wrap:wrap;margin:0.5rem 0' }, [
      ...(qr && qr.qr ? [h('img', { src: qr.qr, alt: t('gl.qrAlt', { target }), style: 'width:140px;height:140px;border-radius:8px;background:#fff' })] : []),
      h('div', {}, [
        h('code', {}, target),
        h('div', { style: 'margin-top:0.5rem' }, [copyButton(target, t('tg.copyLink'))]),
      ]),
    ]),
    why(t('gl.phoneWhy')),
  ], first);
}

/** The card ---------------------------------------------------------------- */

/**
 * Creates the outstanding-work card for a project. Renders nothing while there
 * is nothing for a human to do. Returns the element with a .refresh() method so
 * callers can re-derive it after a domain is saved.
 */
export function createGoLiveTasks(projectId) {
  // The element is the card itself, not a wrapper around one. Sibling spacing
  // comes from `.card + .card`, which only matches between adjacent elements, so
  // a wrapper div would silently collapse the gaps for the cards on either side.
  // With nothing to do the card hides rather than emptying out: a hidden element
  // still satisfies that adjacency, so the neighbours keep their spacing while it
  // renders no box of its own.
  const root = h('div', { className: 'card', style: 'display:none' });

  function hide() {
    clear(root);
    root.style.display = 'none';
  }

  async function load() {
    try {
      await loadTasks();
    } catch (err) {
      // Best-effort card: a failed check hides it rather than leaving a stale
      // list, and the checks run again the next time the section is opened.
      console.warn('go-live tasks failed to load', err);
      hide();
    }
  }

  async function loadTasks() {
    const [st, firewall] = await Promise.all([
      get(`/admin/api/projects/${projectId}/hosting/status`).catch(() => null),
      fetchFirewallStatus(),
    ]);
    if (!st) {
      hide();
      return;
    }
    const netQuery = st.domain_kind === 'public' ? '?domain=' + encodeURIComponent(st.domain) : '';
    const net = await get('/admin/api/system/network' + netQuery).catch(() => ({
      public_ip: '', local_ips: [], nat_likely: true, shared_address: false, double_nat: false, ipv6: [], router: null, port_mapping: null,
    }));
    const mapping = net.port_mapping || {};
    const { url } = formatPublicUrl({ domain: st.domain }, state.get('systemStatus'));
    const view = classifyHosting(st, { url: url === '#' ? '' : url });

    const isPublic = st.domain_kind === 'public';
    // "Working" covers unverified too: the certificate exists and 443 is
    // served, so DNS and the port forward are demonstrably already right.
    const httpsWorking = view.key === 'live' || view.key === 'unverified';

    const tasks = [];
    const add = (taskEl) => { if (taskEl) tasks.push(taskEl); };
    if (!isPublic) add(domainTask(st, net, tasks.length === 0));
    // A changed address shows even while HTTPS still works: visitors whose
    // resolvers have the new record are already lost, and renewal will fail.
    if (isPublic && st.dns_mismatch) add(ipChangedTask(st, tasks.length === 0));
    if (isPublic && !httpsWorking && !st.dns_mismatch) add(dnsTask(st, net, tasks.length === 0));
    // Gated on httpsWorking like the others: if the certificate issued and the
    // site is serving, inbound is demonstrably not blocked, whatever the local
    // rule check thinks. Asking about the firewall then is pure noise.
    if (st.domain_kind !== 'none' && !httpsWorking) add(firewallTask(firewall, tasks.length === 0, load));
    // The shared-address and double-router notes show before a domain exists
    // too: they decide whether buying one can work, and what else it takes.
    if (net.shared_address && !httpsWorking) {
      add(sharedAddressTask(net, tasks.length === 0));
    } else {
      if (isPublic && !httpsWorking && net.nat_likely && !mapping.active) add(routerTask(net, st, tasks.length === 0, load));
      if (net.double_nat && !httpsWorking) add(doubleNatTask(net, tasks.length === 0));
    }
    if (view.needsPhoneCheck) add(await phoneTask(projectId, url, tasks.length === 0));

    clear(root);
    if (!tasks.length) {
      hide();
      return;
    }
    root.style.display = '';
    root.append(
      h('div', { className: 'card__header' }, [
        h('h4', { className: 'card__title' }, t('gl.title')),
        h('span', { innerHTML: icon('globe'), className: 'card__icon' }),
      ]),
      ...tasks,
    );
  }

  load();
  root.refresh = load;
  return root;
}
