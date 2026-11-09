/**
 * Persistent top-of-page banner that surfaces SSE connection state.
 * Hidden in normal/connected state. Shows "Reconnecting…" with the attempt
 * count during recovery, and "Disconnected" with a Retry button after the
 * client gives up.
 */

import { h } from '../core/dom.js';
import { t } from '../core/i18n.js';

export function attachConnectionBanner(parent, sseClient) {
  const text = h('span', { className: 'connection-banner__text' }, '');
  const retryBtn = h('button', {
    className: 'btn btn--sm btn--primary',
    onClick: () => sseClient.retry(),
  }, t('connection.retryNow'));
  retryBtn.style.display = 'none';

  const banner = h('div', { className: 'connection-banner', role: 'status', 'aria-live': 'polite' }, [
    h('span', { className: 'connection-banner__dot' }),
    text,
    retryBtn,
  ]);
  banner.style.display = 'none';

  parent.appendChild(banner);

  sseClient.onConnectionState((state, attempts) => {
    switch (state) {
      case 'connecting':
        if (attempts === 0) banner.style.display = 'none';
        else {
          banner.className = 'connection-banner connection-banner--warning';
          banner.style.display = '';
          text.textContent = t('connection.reconnecting');
          retryBtn.style.display = 'none';
        }
        break;
      case 'reconnecting':
        banner.className = 'connection-banner connection-banner--warning';
        banner.style.display = '';
        text.textContent = t('connection.lost', { attempt: attempts });
        retryBtn.style.display = 'none';
        break;
      case 'open':
        banner.style.display = 'none';
        break;
      case 'gave_up':
        banner.className = 'connection-banner connection-banner--danger';
        banner.style.display = '';
        text.textContent = t('connection.gaveUp');
        retryBtn.style.display = '';
        break;
      case 'closed':
        banner.style.display = 'none';
        break;
    }
  });

  return banner;
}
