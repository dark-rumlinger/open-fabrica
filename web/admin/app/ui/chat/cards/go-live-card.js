/**
 * The go-live summary card and its sub-parts (flows, pages, sign-in, cred rows),
 * shown once when a build finishes. Split out of message.js; createBrainMessage
 * dispatches to createGoLiveCard when the brain message carries a go-live marker.
 */

import { h } from '../../../core/dom.js';
import { t } from '../../../core/i18n.js';
import { render as renderMarkdown } from '../../../core/markdown.js';
import { icon } from '../../icon.js';
import { appPath } from '../../../core/router.js';
import { goLiveFlowView } from './flows-view-model.js';
/** Subtitle of a go-live card that shipped with known issues: names why. */
function issuesSubtitle(reasons) {
  if (!Array.isArray(reasons) || reasons.length === 0) return t('chat.issuesGeneric');
  return t('chat.issuesReasons', { reasons: reasons.join('; ') });
}

/**
 * Create the go-live summary card, shown once when a build finishes. Built
 * from the payload the engine embeds in the message (see live_summary.go),
 * so it renders identically on a live event and on history replay. Returns
 * null when the payload lacks the essentials; the caller then falls back to
 * the plain markdown.
 */
export function createGoLiveCard(badge, p) {
  if (!p || !p.preview_url) return null;
  const known = !!p.known_issues;

  const header = h('div', { className: 'golive__header' }, [
    h('span', {
      className: 'golive__chip' + (known ? ' golive__chip--warn' : ''),
      innerHTML: icon(known ? 'alert-triangle' : 'sparkle'),
    }),
    h('div', {}, [
      h('div', { className: 'golive__title' }, known ? t('chat.deployedIssues') : t('chat.live1')),
      h('div', { className: 'golive__sub' }, known ? issuesSubtitle(p.issue_reasons) : t('chat.readyShare')),
    ]),
  ]);

  const actions = [goLiveAction('external-link', t('chat.previewLocally'), p.preview_url, true)];
  if (p.settings_url) actions.push(goLiveAction('globe', t('chat.share'), p.settings_url, false));
  const children = [header, h('div', { className: 'golive__actions' }, actions)];

  // "What's inside" stat tiles - one per non-empty category. The full name list
  // rides along as a title attribute so hovering a tile shows what it counts.
  const tiles = [
    goLiveTile('file-text', t('tabs.pages'), p.pages),
    goLiveTile('database', t('chat.tablesTile'), p.tables),
    goLiveTile('zap', t('tabs.apis'), p.apis),
  ].filter(Boolean);
  if (tiles.length) {
    const inside = h('div', { className: 'golive__inside' }, [
      h('div', { className: 'golive__section-label' }, t('chat.whatsInside')),
      h('div', { className: 'golive__tiles' }, tiles),
    ]);
    if (p.has_auth_api) {
      inside.appendChild(h('div', { className: 'golive__authnote' }, [
        h('span', { className: 'golive__authnote-icon', innerHTML: icon('lock') }),
        h('span', {}, t('chat.plusSignIn')),
      ]));
    }
    const pages = createGoLivePages(p.page_links);
    if (pages) inside.appendChild(pages);
    children.push(inside);
  }

  // Things only the build knows that the owner needs, such as a passcode it saved.
  const notes = Array.isArray(p.notes) ? p.notes.filter(Boolean) : [];
  if (notes.length) {
    const list = h('ul', { className: 'golive__notes' }, notes.map((n) => {
      const li = h('li', {});
      li.innerHTML = renderMarkdown(n);
      return li;
    }));
    children.push(h('div', { className: 'golive__inside' }, [
      h('div', { className: 'golive__section-label' }, t('chat.goodToKnow')),
      list,
    ]));
  }

  // The user flow test result. Shown as a note, not as a known issue: the app or
  // the flow itself may be out of date.
  const flows = createGoLiveFlows(p);
  if (flows) children.push(flows);

  if (p.login && p.login.login_url) children.push(createGoLiveSignin(p.login));

  const body = h('div', { className: 'message__body' }, [
    h('div', { className: 'golive' + (known ? ' golive--warn' : '') }, children),
  ]);
  return h('div', { className: 'message message--brain message--golive' }, [badge, body]);
}

// The "Flow tests" block of the go-live card: what the final replay found, with
// the failed flows named. Falls back to the older flow_unverified list for
// messages saved before flow_check existed.
function createGoLiveFlows(p) {
  const legacy = Array.isArray(p.flow_unverified) && p.flow_unverified.length
    ? { failed: p.flow_unverified }
    : null;
  const v = goLiveFlowView(p.flow_check || legacy);
  if (!v) return null;
  const box = h('div', { className: `golive__flowbox golive__flowbox--${v.tone}` }, [
    h('span', { className: 'golive__flowchip', innerHTML: icon(v.icon) }),
    h('div', { className: 'golive__flowtext' }, [
      h('div', { className: 'golive__flowtitle' }, v.title),
      h('div', { className: 'golive__flowsub' }, v.sub),
      v.failed.length
        ? h('div', { className: 'golive__pills' }, v.failed.map((name) => h('span', { className: 'golive__pill' }, name)))
        : null,
    ]),
  ]);
  return h('div', { className: 'golive__inside' }, [
    h('div', { className: 'golive__section-label' }, t('files.flowTests')),
    box,
  ]);
}

// A single action pill (Preview / Share). External targets open in a new tab;
// an in-app route stays in the current tab and is navigated by the router.
function goLiveAction(iconName, label, href, external) {
  const route = external ? null : appPath(href);
  const attrs = { className: 'golive__action', href: route || href };
  if (route) attrs['data-route'] = true;
  if (external) { attrs.target = '_blank'; attrs.rel = 'noopener noreferrer'; }
  return h('a', attrs, [
    h('span', { className: 'golive__action-icon', innerHTML: icon(iconName) }),
    h('span', {}, label),
  ]);
}

// Every page as a link pill showing its route, so the owner finds pages like
// /board or /staff that no nav links to. A parameterised route has no URL and
// renders as plain text.
function createGoLivePages(links) {
  const arr = Array.isArray(links) ? links.filter((l) => l && l.path) : [];
  if (!arr.length) return null;
  return h('div', { className: 'golive__pages' }, arr.map((l) => {
    const label = [
      h('span', { className: 'golive__page-title' }, l.title || l.path),
      h('span', { className: 'golive__page-path' }, l.path),
    ];
    return l.url
      ? h('a', { className: 'golive__page', href: l.url, target: '_blank', rel: 'noopener noreferrer' }, label)
      : h('span', { className: 'golive__page' }, label);
  }));
}

// A "what's inside" stat tile. Returns null for an empty category so the grid
// only shows what the project actually has.
function goLiveTile(iconName, label, names) {
  const arr = Array.isArray(names) ? names : [];
  if (!arr.length) return null;
  return h('div', { className: 'golive__tile', title: arr.join(', ') }, [
    h('div', { className: 'golive__tile-label' }, [
      h('span', { className: 'golive__tile-icon', innerHTML: icon(iconName) }),
      h('span', {}, label),
    ]),
    h('div', { className: 'golive__tile-count' }, String(arr.length)),
  ]);
}

// The sign-in sub-card. Shows working credentials when a demo login was minted;
// otherwise points the owner at the seeded users. Credential values are
// click-to-copy as a convenience.
function createGoLiveSignin(login) {
  const head = h('div', { className: 'golive__signin-head' }, [
    h('span', { className: 'golive__signin-icon', innerHTML: icon('lock') }),
    h('span', { className: 'golive__signin-title' }, t('chat.signInToTry')),
    login.role ? h('span', { className: 'golive__role' }, t('chat.role', { role: login.role })) : null,
  ].filter(Boolean));

  const children = [head];
  if (login.minted) {
    children.push(h('div', { className: 'golive__signin-rows' }, [
      goLiveCredRow(login.label || t('chat.email'), login.identifier),
      goLiveCredRow(t('login.password'), login.password),
    ]));
  }

  let note;
  if (!login.minted) {
    note = t('chat.noteSeeded');
  } else if (login.adopted) {
    note = t('chat.noteAdopted');
  } else {
    note = t('chat.noteDemo');
  }
  children.push(h('div', { className: 'golive__signin-note' }, note));
  children.push(h('a', {
    className: 'golive__signin-link', href: login.login_url, target: '_blank', rel: 'noopener noreferrer',
  }, [
    h('span', {}, t('chat.openLogin')),
    h('span', { className: 'golive__signin-icon', innerHTML: icon('external-link') }),
  ]));

  return h('div', { className: 'golive__signin' }, children);
}

// A label/value credential row. The value is click-to-copy; a brief "Copied"
// swap confirms it. Copy is progressive enhancement - selecting the text still
// works where the clipboard API is unavailable.
function goLiveCredRow(label, value) {
  const val = value || '';
  const valEl = h('span', { className: 'golive__cred-val', title: t('chat.clickCopy') }, val);
  if (val && navigator.clipboard) {
    valEl.classList.add('golive__cred-val--copyable');
    valEl.addEventListener('click', () => {
      navigator.clipboard.writeText(val).then(() => {
        const prev = valEl.textContent;
        valEl.textContent = t('helpers.copied');
        valEl.classList.add('golive__cred-val--copied');
        setTimeout(() => {
          valEl.textContent = prev;
          valEl.classList.remove('golive__cred-val--copied');
        }, 1100);
      }).catch(() => { /* clipboard blocked; the value stays selectable */ });
    });
  }
  return h('div', { className: 'golive__cred' }, [
    h('span', { className: 'golive__cred-key' }, label),
    valEl,
  ]);
}
