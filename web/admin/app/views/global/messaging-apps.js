/**
 * Messaging apps setup (Telegram). A three-step wizard while nothing is
 * connected, then a status page with the people who can use the bot.
 *
 *   1. Create the bot with @BotFather and paste its token.
 *   2. Connect yourself by opening a one-time invite link (or scanning its QR).
 *   3. Done: manage people, send a test message, disconnect.
 */

import { h, clear } from '../../core/dom.js';
import { t, tn } from '../../core/i18n.js';
import { get, post, put, del } from '../../core/http.js';
import { icon } from '../../ui/icon.js';
import { isSubmitEnter } from '../../ui/a11y.js';
import { defineView } from '../../core/view.js';
import * as toast from '../../ui/toast.js';
import * as state from '../../core/state.js';
import * as modal from '../../ui/modal.js';
import { pageHeader, pageBody } from '../../ui/card.js';
import { humanizeError } from '../../ui/errors.js';
import { copyText } from '../../ui/helpers.js';

const POLL_MS = 2500;

export const renderMessagingApps = defineView(async (ctx, container) => {
  clear(container);

  container.appendChild(pageHeader({ title: t('nav.messaging') }));
  const body = pageBody();
  container.appendChild(body);

  const isAdmin = state.isAdmin();
  let status = null;
  let statusKey = '';
  // Invite currently shown to the person setting up (step 2, or "Add someone").
  let invite = null;
  let inviteBusy = false;
  let inviteBaseline = 0;

  if (!isAdmin) {
    body.appendChild(h('div', { className: 'card' }, [
      h('p', { className: 'text-secondary' }, t('tg.adminOnly')),
    ]));
    return;
  }

  await refresh(true);
  ctx.scope.interval(() => { if (shouldPoll()) refresh(false); }, POLL_MS);

  function shouldPoll() {
    if (!status || document.hidden) return false;
    // Nothing changes on its own until a token is saved.
    return status.configured;
  }

  async function refresh(force) {
    let next;
    try {
      next = await get('/admin/api/telegram/status');
    } catch (err) {
      if (force) toast.error(t('tg.statusFailed', { error: humanizeError(err.message) }));
      if (!status) {
        // Nothing to render yet and polling is off until a status loads, so
        // offer a retry instead of leaving the page blank.
        clear(body);
        body.appendChild(h('div', { className: 'card' }, [
          h('p', { className: 'text-secondary' }, t('tg.statusUnavailable')),
          h('button', { className: 'btn btn--primary btn--sm mt-4', onClick: () => refresh(true) }, t('common.tryAgain')),
        ]));
      }
      return;
    }
    const key = JSON.stringify([next.configured, next.running, next.healthy, next.detail, next.username, next.users]);
    const changed = key !== statusKey;
    status = next;
    statusKey = key;

    // A new person appeared while an invite was showing: it was used.
    if (invite && status.users.length > inviteBaseline) {
      const newest = status.users[status.users.length - 1];
      toast.success(t('tg.connectedPerson', { name: newest.name || t('tg.someone') }));
      invite = null;
    }
    if (changed || force) render();
  }

  function step() {
    if (!status.configured) return 1;
    if (status.users.length === 0) return 2;
    return 3;
  }

  // Step 2 needs an invite the moment it is shown.
  async function ensureInvite() {
    if (inviteBusy) return;
    inviteBusy = true;
    try {
      inviteBaseline = status.users.length;
      invite = await post('/admin/api/telegram/pair', {});
    } catch (err) {
      toast.error(t('tg.inviteFailed', { error: humanizeError(err.message) }));
    }
    inviteBusy = false;
    render();
  }

  function render() {
    clear(body);
    const s = step();
    if (s === 2 && !invite && !inviteBusy) ensureInvite();

    body.appendChild(h('div', { className: 'card' }, [
      header(),
      ...(s < 3 ? [stepper(s)] : []),
      h('div', { className: 'mt-4' }, [
        s === 1 ? stepOne() : s === 2 ? stepTwo() : stepThree(),
      ]),
    ]));
  }

  function header() {
    let badge;
    if (!status.configured) {
      badge = h('span', { className: 'badge badge--secondary' }, t('bot.notSetUp'));
    } else if (status.running && status.healthy) {
      badge = h('span', { className: 'badge badge--success' }, t('projects.stats.connected'));
    } else {
      badge = h('span', { className: 'badge badge--warning' }, t('tg.connecting'));
    }
    return h('div', { className: 'card__header' }, [
      h('div', { className: 'flex items-center gap-3' }, [
        h('span', { innerHTML: icon('send') }),
        h('div', {}, [
          h('h4', { className: 'card__title' }, 'Telegram'),
          h('span', { className: 'text-xs text-secondary' }, t('tg.tagline')),
        ]),
      ]),
      badge,
    ]);
  }

  function stepper(current) {
    const labels = [t('tg.step1'), t('tg.step2'), t('tg.step3')];
    return h('div', { className: 'flex items-center gap-3 mt-4', style: { flexWrap: 'wrap' } },
      labels.map((label, i) => {
        const n = i + 1;
        const cls = n < current ? 'badge badge--success' : n === current ? 'badge badge--accent' : 'badge badge--secondary';
        return h('span', { className: cls }, (n < current ? '✓ ' : n + '. ') + label);
      }));
  }

  // --- Step 1: create the bot ---

  function stepOne() {
    const tokenInput = h('input', {
      className: 'input',
      type: 'password',
      autocomplete: 'off',
      placeholder: '123456789:AAF...',
    });
    const showBtn = h('button', {
      className: 'btn btn--ghost btn--sm',
      type: 'button',
      onClick: () => {
        const hidden = tokenInput.type === 'password';
        tokenInput.type = hidden ? 'text' : 'password';
        showBtn.textContent = hidden ? t('tg.hide') : t('tg.show');
      },
    }, t('tg.show'));
    const result = h('div', { className: 'mt-2 text-sm' });
    const connectBtn = h('button', { className: 'btn btn--primary' }, t('tg.connectBot'));

    async function connect() {
      const token = tokenInput.value.trim();
      clear(result);
      if (!token) {
        result.appendChild(h('span', { className: 'text-danger' }, t('tg.pasteToken')));
        return;
      }
      connectBtn.disabled = true;
      connectBtn.textContent = t('tg.checking');
      try {
        const resp = await post('/admin/api/telegram/token', { token });
        if (!resp.ok) {
          result.appendChild(h('span', { className: 'text-danger' },
            t('tg.tokenRejected', { error: resp.error || t('tg.unknownError') })));
        } else {
          toast.success(t('tg.connectedTo', { username: resp.username }));
          invite = null;
          await refresh(true);
          return;
        }
      } catch (err) {
        result.appendChild(h('span', { className: 'text-danger' }, t('tg.saveFailed', { error: humanizeError(err.message) })));
      }
      connectBtn.disabled = false;
      connectBtn.textContent = t('tg.connectBot');
    }
    connectBtn.addEventListener('click', connect);
    // Checking on paste means most people never have to press the button.
    tokenInput.addEventListener('paste', () => setTimeout(() => {
      if (/^\d+:[\w-]{20,}$/.test(tokenInput.value.trim())) connect();
    }, 0));
    tokenInput.addEventListener('keydown', (e) => { if (isSubmitEnter(e)) connect(); });

    return h('div', {}, [
      h('ol', { style: { paddingInlineStart: '20px', lineHeight: '1.7' } }, [
        h('li', {}, [
          t('tg.open') + ' ',
          h('a', { href: 'https://t.me/BotFather', target: '_blank', rel: 'noopener' }, '@BotFather'),
          ' ' + t('tg.inTelegramSend') + ' ',
          h('code', {}, '/newbot'),
          '.',
        ]),
        h('li', {}, t('tg.chooseName')),
        h('li', {}, t('tg.copyToken')),
      ]),
      h('div', { className: 'mt-3' }, [
        h('a', { className: 'btn btn--ghost', href: 'https://t.me/BotFather', target: '_blank', rel: 'noopener' }, t('tg.openBotFather')),
      ]),
      h('div', { className: 'mt-4 flex items-center gap-2' }, [tokenInput, showBtn]),
      h('div', { className: 'mt-3' }, [connectBtn]),
      result,
      h('p', { className: 'form-hint mt-3' }, t('tg.tokenNote')),
    ]);
  }

  // --- Step 2 / "Add someone": invite link ---

  function invitePanel(title) {
    if (!invite) {
      return h('p', { className: 'text-secondary' }, t('tg.creatingInvite'));
    }
    const copyBtn = h('button', {
      className: 'btn btn--secondary btn--sm',
      type: 'button',
      onClick: () => copyText(invite.deep_link, t('tg.linkCopied')),
    }, t('tg.copyLink'));
    // The link itself, selectable, for when the clipboard is unavailable.
    const linkField = h('input', { className: 'input mt-2', value: invite.deep_link, readonly: true, 'aria-label': t('tg.inviteLink') });
    linkField.addEventListener('focus', () => linkField.select());
    const minutes = Math.max(1, Math.round((new Date(invite.expires_at) - Date.now()) / 60000));

    return h('div', {}, [
      h('p', { style: { fontWeight: '600' } }, title),
      h('div', { className: 'mt-3 flex items-center gap-4', style: { flexWrap: 'wrap' } }, [
        h('img', {
          src: invite.qr,
          alt: t('tg.qrAlt'),
          style: { width: '160px', height: '160px', borderRadius: '8px', background: '#fff' },
        }),
        h('div', {}, [
          h('p', { className: 'text-sm' }, t('tg.scanHint')),
          h('div', { className: 'mt-3 flex items-center gap-2', style: { flexWrap: 'wrap' } }, [
            h('a', { className: 'btn btn--primary', href: invite.deep_link, target: '_blank', rel: 'noopener' },
              t('tg.openInTelegram')),
            copyBtn,
          ]),
          linkField,
          h('p', { className: 'form-hint mt-2' }, tn('tg.inviteNote', minutes)),
        ]),
      ]),
    ]);
  }

  function stepTwo() {
    return h('div', {}, [
      h('p', { className: 'text-sm mb-3' }, t('tg.botReady', { name: status.username ? '@' + status.username : '' })),
      invitePanel(t('tg.connectAccount')),
      h('div', { className: 'mt-4 flex items-center gap-2' }, [
        h('button', {
          className: 'btn btn--ghost btn--sm',
          onClick: () => { invite = null; ensureInvite(); },
        }, t('tg.newLink')),
        h('button', {
          className: 'btn btn--ghost btn--sm',
          onClick: () => disconnect(),
        }, t('tg.differentBot')),
      ]),
    ]);
  }

  // --- Step 3: connected ---

  function stepThree() {
    const healthLine = status.running && status.healthy
      ? h('span', { className: 'text-success' }, t('tg.healthy'))
      : h('span', { className: 'text-danger' },
        status.detail ? t('tg.troubleDetail', { detail: status.detail }) : t('tg.trouble'));

    const rows = status.users.map((u) => h('div', {
      className: 'flex items-center gap-3',
      style: { padding: '10px 0', borderTop: '1px solid var(--border, rgba(128,128,128,0.2))', flexWrap: 'wrap' },
    }, [
      h('span', { innerHTML: icon('user') }),
      h('div', { style: { flex: '1', minWidth: '140px' } }, [
        h('div', { style: { fontWeight: '600' } }, u.name || t('tg.telegramUser')),
        h('div', { className: 'text-xs text-secondary' }, 'ID ' + u.id),
      ]),
      h('button', { className: 'btn btn--ghost btn--sm', onClick: () => sendTest(u) }, t('tg.sendTest')),
      h('button', { className: 'btn btn--ghost btn--sm', onClick: () => removeUser(u) }, t('tg.remove')),
    ]));

    const link = status.deep_link
      ? h('a', { className: 'btn btn--ghost', href: status.deep_link, target: '_blank', rel: 'noopener' }, t('tg.openBot'))
      : null;

    return h('div', {}, [
      h('p', {}, [
        t('tg.connectedAs', { name: status.username ? '@' + status.username : t('tg.yourBot') }) + ' ',
        healthLine,
      ]),
      h('div', { className: 'mt-4' }, [
        h('h5', { style: { margin: '0 0 4px' } }, t('tg.people')),
        ...rows,
      ]),
      invite ? h('div', { className: 'mt-4' }, [invitePanel(t('tg.inviteAnother'))]) : null,
      h('div', { className: 'mt-4 flex items-center gap-2', style: { flexWrap: 'wrap' } }, [
        invite
          ? h('button', { className: 'btn btn--ghost', onClick: () => { invite = null; render(); } }, t('tg.cancelInvite'))
          : h('button', { className: 'btn btn--primary', onClick: () => ensureInvite() }, t('tg.addSomeone')),
        link,
        h('button', { className: 'btn btn--ghost', onClick: () => disconnect() }, t('tg.disconnect')),
      ]),
      h('p', { className: 'form-hint mt-3' }, t('tg.fullControl')),
    ].filter(Boolean));
  }

  // --- actions ---

  async function sendTest(u) {
    try {
      await post('/admin/api/telegram/test-message', { user_id: u.id });
      toast.success(t('tg.testSent', { name: u.name || t('tg.them') }));
    } catch (err) {
      toast.error(humanizeError(err.message));
    }
  }

  // Confirmation dialog with a verb that matches the action (confirmDanger's
  // button always says "Delete").
  function confirmAction(title, message, label, run) {
    modal.show(title, h('p', { className: 'text-secondary' }, message), [
      modal.cancelBtn(),
      { label, className: 'btn btn--danger', onClick: run },
    ]);
  }

  function removeUser(u) {
    confirmAction(t('tg.removeTitle', { name: u.name || t('tg.thisPerson') }),
      t('tg.removeBody'), t('tg.remove'), async () => {
        try {
          await del('/admin/api/telegram/users/' + encodeURIComponent(u.id));
          await refresh(true);
        } catch (err) {
          toast.error(humanizeError(err.message));
        }
      });
  }

  function disconnect() {
    confirmAction(t('tg.disconnectTitle'),
      t('tg.disconnectBody'), t('tg.disconnect'), async () => {
        try {
          await del('/admin/api/telegram/token');
          invite = null;
          await refresh(true);
        } catch (err) {
          toast.error(humanizeError(err.message));
        }
      });
  }
});
