/**
 * Hosting status chip: a one-glance answer to "is my project reachable, and if
 * not, why". Reads /hosting/status, polls while certificate issuance is
 * pending, and (in detail mode) explains failures with the server's hint and a
 * recheck button, plus the port-conflict notice when the TLS proxy could not
 * bind ports 80/443.
 */

import { h, clear } from '../core/dom.js';
import { t } from '../core/i18n.js';
import { get, post } from '../core/http.js';
import * as state from '../core/state.js';
import * as toast from './toast.js';
import { humanizeError } from './errors.js';
import { formatPublicUrl, linkifyUrls } from './helpers.js';
import { classifyHosting } from './hosting-state.js';

const POLL_MS = 10000;

/**
 * Adapts the shared classification (hosting-state.js) to the chip's shape. The
 * state machine and the wording live there so this chip and the Visitors strip
 * can never disagree about whether a state is a success or a failure.
 */
function describeHosting(st) {
  const { url } = formatPublicUrl({ domain: st.domain }, state.get('systemStatus'));
  const view = classifyHosting(st, { url });
  return { label: view.chip, kind: view.kind, title: view.sentence, pending: view.pending };
}

/**
 * Creates the chip element and starts a fetch (+ poll while pending). Pass a
 * lifecycle scope so polling stops when the view unmounts.
 *   createHostingChip(projectId, { scope })            - compact chip (header)
 *   createHostingChip(projectId, { scope, detail: true }) - chip + hint text,
 *     recheck button, and the port-conflict notice (settings)
 * The element exposes .refresh() so callers can force a reload after saving a
 * domain.
 */
export function createHostingChip(projectId, { scope, detail = false } = {}) {
  const chip = h('span', { className: 'badge badge--secondary', style: { fontSize: '0.75rem' } }, '…');
  const extra = detail ? h('div', {}) : null;
  const root = h(detail ? 'div' : 'span', {}, [chip, extra].filter(Boolean));

  let stopped = false;
  let pollTimer = null;
  // A domain whose DNS is never set stays pending forever. Poll for a few
  // minutes after the last explicit check, then wait for the person to ask.
  let polls = 0;
  const MAX_POLLS = 30;
  if (scope) scope.add(() => { stopped = true; if (pollTimer) clearTimeout(pollTimer); });

  async function load(probe = false) {
    if (stopped) return;
    if (probe) polls = 0;
    let st;
    try {
      st = probe
        ? await post(`/admin/api/projects/${projectId}/hosting/recheck`)
        : await get(`/admin/api/projects/${projectId}/hosting/status`);
    } catch {
      // Status is decoration; leave the chip as-is rather than toasting on
      // every poll failure.
      return;
    }
    if (stopped) return;
    renderChip(st);
  }

  function renderChip(st) {
    const view = describeHosting(st);
    chip.className = `badge badge--${view.kind}`;
    chip.textContent = view.label;
    chip.title = view.title;

    if (extra) renderDetail(st, view);

    if (pollTimer) clearTimeout(pollTimer);
    if (!view.pending) polls = 0;
    if (view.pending && polls < MAX_POLLS) {
      // Stop polling once the element has left the document, so views rendered
      // without a lifecycle scope don't leak a poll loop.
      pollTimer = setTimeout(() => {
        if (!root.isConnected) return;
        polls += 1;
        load(false);
      }, POLL_MS);
    }
  }

  function renderDetail(st, view) {
    clear(extra);
    if (st.tls_bind_error) {
      extra.appendChild(bindErrorNotice(st.tls_bind_error));
    }
    if (view.kind !== 'danger' && !view.pending) return;

    const parts = [];
    if (view.kind === 'danger') {
      parts.push(h('p', { className: 'form-hint', style: 'margin:0.25rem 0 0' }, linkifyUrls(view.title)));
    }
    if (state.isAdmin()) {
      const btn = h('button', {
        className: 'btn btn--primary btn--sm',
        type: 'button',
        style: 'margin-top:0.35rem',
        onClick: async () => {
          btn.disabled = true;
          btn.textContent = t('hs.checking');
          try {
            await load(true);
          } catch (err) {
            toast.error(t('hs.checkFailed', { error: humanizeError(err.message) }));
          }
          btn.disabled = false;
          btn.textContent = t('hs.checkAgain');
        },
      }, t('hs.checkAgain'));
      parts.push(h('div', {}, [btn]));
    }
    for (const p of parts) extra.appendChild(p);
  }

  load(false);
  root.refresh = () => load(false);
  return root;
}

/**
 * Notice for a TLS port conflict: the humanized bind error from the server
 * plus the two ways out. Shown in settings detail mode only.
 */
function bindErrorNotice(message) {
  return h('div', { className: 'notice notice--danger' }, [
    h('p', { className: 'form-hint' }, t('hs.bindError', { message })),
    h('p', { className: 'form-hint' }, t('hs.bindHint')),
  ]);
}
