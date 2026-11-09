import { h, clear } from '../../core/dom.js';
import { t } from '../../core/i18n.js';
import { get, post, put, del } from '../../core/http.js';
import { createScope } from '../../core/lifecycle.js';
import * as toast from '../../ui/toast.js';
import { icon } from '../../ui/icon.js';
import * as modal from '../../ui/modal.js';
import { card } from '../../ui/card.js';
import { createListView } from '../../ui/list-view.js';
import { createTabbedPanel } from '../../ui/tabbed-panel.js';
import { formGroup, copyText, publicOrigin } from '../../ui/helpers.js';
import * as state from '../../core/state.js';
import { humanizeError } from '../../ui/errors.js';
import { runAction } from '../../ui/actions.js';
import { createFormModal } from '../../ui/form-modal.js';
import { formatDateTime } from '../../core/datetime.js';

// Where visitors reach this project; incoming hooks are called there, not on
// the admin origin. Set per render, one panel is mounted at a time.
let publicBase = '';

export function renderProjectWebhooks(container, projectId, section, project) {
  const scope = createScope();
  publicBase = publicOrigin(project, state.get('systemStatus'));
  const canWrite = state.isAdmin();

  // --- Event Hooks tab: outbound/inbound webhooks ---
  const hooksBody = h('div', { className: 'list-block' });
  if (canWrite) {
    hooksBody.appendChild(h('div', { className: 'list-toolbar flex items-center justify-end mb-3' }, [
      h('button', {
        className: 'btn btn--primary btn--sm',
        onClick: () => showCreateModal(projectId, () => list.reload()),
      }, [h('span', { innerHTML: icon('plus') }), ' ' + t('wh.create')]),
    ]));
  }

  const listEl = h('div');
  hooksBody.appendChild(listEl);

  const list = createListView({
    scope,
    container: listEl,
    fetch: () => get(`/admin/api/projects/${projectId}/webhooks`),
    renderItem: (wh) => renderWebhook(wh, projectId, () => list.reload()),
    empty: {
      iconName: 'link',
      title: t('wh.empty.title'),
      description: t('wh.empty.description'),
      action: canWrite ? { label: t('wh.create'), onClick: () => showCreateModal(projectId, () => list.reload()) } : null,
    },
    errorMsg: t('wh.loadFailed'),
  });
  list.reload();

  // --- Automated actions tab ---
  // Server-side triggers (of_triggers): actions that run inside the project
  // when an event fires (send an email, write a row), distinct from the
  // outbound webhooks above. The assistant authors them via manage_triggers;
  // operators can disable or remove them here.
  const actionsBody = h('div');
  const trigEl = h('div');
  actionsBody.appendChild(trigEl);
  const trigList = createListView({
    scope,
    container: trigEl,
    fetch: () => get(`/admin/api/projects/${projectId}/triggers`),
    renderItem: (trig) => renderTrigger(trig, projectId, () => trigList.reload()),
    empty: {
      iconName: 'zap',
      title: t('wh.trig.empty.title'),
      description: t('wh.trig.empty.description'),
    },
    errorMsg: t('wh.trig.loadFailed'),
  });
  trigList.reload();

  createTabbedPanel({
    container,
    title: t('tabs.eventHooks'),
    activeSection: section,
    tabs: [
      {
        id: 'hooks',
        label: t('tabs.eventHooks'),
        content: hooksBody,
      },
      {
        id: 'actions',
        label: t('tabs.automated'),
        content: actionsBody,
      },
    ],
  });

  return () => scope.dispose();
}

function renderTrigger(trig, projectId, reload) {
  const enabledBadge = trig.is_enabled
    ? h('span', { className: 'badge badge--success' }, t('common.enabled'))
    : h('span', { className: 'badge badge--danger' }, t('common.disabled'));
  const details = [
    h('div', { className: 'text-sm text-secondary mt-2' }, [
      h('strong', {}, t('wh.when') + ' '), h('code', { style: { fontSize: 'var(--text-xs)' } }, trig.event_type),
      h('span', {}, '  →  '),
      h('strong', {}, t('wh.do') + ' '), h('code', { style: { fontSize: 'var(--text-xs)' } }, trig.trigger_type),
    ]),
  ];
  if (trig.event_filter) {
    details.push(h('div', { className: 'text-xs text-secondary mt-1' },
      [t('wh.filter') + ' ', h('code', { style: { fontSize: 'var(--text-xs)', wordBreak: 'break-all' } }, trig.event_filter)]));
  }
  return card({
    title: trig.name,
    iconName: 'zap',
    badge: enabledBadge,
    actions: [
      {
        label: trig.is_enabled ? t('tasks.disable') : t('tasks.enable'),
        adminOnly: true,
        onClick: () => runAction(() => post(`/admin/api/projects/${projectId}/triggers/${trig.id}/toggle`), {
          success: trig.is_enabled ? t('common.disabled') : t('common.enabled'),
          onDone: reload,
          errorPrefix: t('wh.trig.toggleFailed'),
        }),
      },
      {
        label: t('common.delete'), kind: 'danger', adminOnly: true,
        onClick: () => modal.confirmDanger(t('wh.trig.deleteTitle'), t('wh.trig.deleteBody', { name: trig.name }),
          () => runAction(() => del(`/admin/api/projects/${projectId}/triggers/${trig.id}`), {
            success: t('wh.deleted'),
            onDone: reload,
            errorPrefix: t('wh.trig.deleteFailed'),
          })),
      },
    ],
    children: details,
  });
}

function renderWebhook(wh, projectId, reload) {
  const dirBadge = wh.direction === 'outgoing'
    ? h('span', { className: 'badge badge--info', title: t('wh.outgoingTip') }, t('wh.outgoing'))
    : h('span', { className: 'badge badge--success', title: t('wh.incomingTip') }, t('wh.incoming'));
  const enabledBadge = wh.is_enabled
    ? h('span', { className: 'badge badge--success' }, t('common.enabled'))
    : h('span', { className: 'badge badge--danger' }, t('common.disabled'));

  const details = [];
  if (wh.url) {
    details.push(h('div', { className: 'text-sm text-secondary mt-2' }, [
      h('strong', {}, t('wh.urlLabel') + ' '),
      h('code', { style: { fontSize: 'var(--text-xs)', wordBreak: 'break-all' } }, wh.url),
    ]));
  }
  if (wh.has_secret) {
    details.push(h('div', { className: 'text-sm text-secondary mt-1' }, [
      h('strong', {}, t('wh.secretLabelColon') + ' '),
      h('span', { style: { fontSize: 'var(--text-xs)' } }, t('wh.secretConfigured')),
    ]));
  }
  if (wh.last_triggered) {
    details.push(h('div', { className: 'text-xs text-secondary mt-1' },
      t('wh.lastTriggered', { when: formatDateTime(wh.last_triggered) })));
  }

  return card({
    title: wh.name,
    iconName: 'link',
    badge: h('div', { className: 'flex gap-2' }, [dirBadge, enabledBadge]),
    actions: [
      { label: t('wh.details'), onClick: () => showDetail(projectId, wh) },
      { label: t('common.edit'), adminOnly: true, onClick: () => showEditModal(projectId, wh, reload) },
      {
        label: wh.is_enabled ? t('tasks.disable') : t('tasks.enable'),
        adminOnly: true,
        onClick: () => runAction(() => post(`/admin/api/projects/${projectId}/webhooks/${wh.id}/toggle`), {
          success: wh.is_enabled ? t('common.disabled') : t('common.enabled'),
          onDone: reload,
          errorPrefix: t('wh.toggleFailed'),
        }),
      },
      {
        label: t('common.delete'), kind: 'danger', adminOnly: true,
        onClick: () => modal.confirmDanger(t('wh.deleteTitle'), t('wh.deleteBody', { name: wh.name }),
          () => runAction(() => del(`/admin/api/projects/${projectId}/webhooks/${wh.id}`), {
            success: t('wh.deleted'),
            onDone: reload,
            errorPrefix: t('wh.deleteFailed'),
          })),
      },
    ],
    children: details,
  });
}

async function showDetail(projectId, wh) {
  const content = h('div', {}, [h('p', { className: 'text-secondary' }, t('common.loading'))]);
  modal.show(t('wh.detailTitle', { name: wh.name }), content, [modal.cancelBtn(t('common.close'))]);

  let detail;
  try {
    detail = await get(`/admin/api/projects/${projectId}/webhooks/${wh.id}`);
  } catch (err) {
    clear(content);
    content.appendChild(h('p', { className: 'text-danger', role: 'alert' }, t('wh.detailFailed', { error: humanizeError(err.message) })));
    return;
  }

  clear(content);

  if (detail.subscriptions?.length) {
    content.appendChild(h('div', { className: 'mb-3' }, [
      h('p', { className: 'text-sm', style: { fontWeight: 600, marginBottom: '4px' } }, t('wh.subscribed')),
      h('div', { className: 'flex gap-1', style: { flexWrap: 'wrap' } },
        detail.subscriptions.map(s => h('span', { className: 'badge badge--info' }, s))),
    ]));
  }

  const logs = detail.logs || [];
  content.appendChild(h('p', { className: 'text-sm', style: { fontWeight: 600, marginBottom: '4px' } }, t('wh.recentLogs', { count: logs.length })));
  if (logs.length === 0) {
    content.appendChild(h('p', { className: 'text-secondary text-sm' }, t('wh.noLogs')));
  } else {
    for (const log of logs) {
      const statusBadge = log.success
        ? h('span', { className: 'badge badge--success' }, `${log.status_code || 'OK'}`)
        : h('span', { className: 'badge badge--danger' }, `${log.status_code || 'Error'}`);
      content.appendChild(h('div', {
        style: { padding: '4px 8px', background: 'var(--bg-secondary)', borderRadius: '4px', marginBottom: '2px' },
        className: 'flex items-center justify-between',
      }, [
        h('div', { className: 'flex items-center gap-2' }, [statusBadge, h('span', { className: 'text-xs' }, log.event_type)]),
        h('span', { className: 'text-xs text-secondary' }, formatDateTime(log.created_at)),
      ]));
    }
  }

  if (detail.direction === 'outgoing' && detail.url) {
    // The server sends it, signed the way a real delivery is. The browser
    // cannot: the admin page may only talk to its own origin, and it would
    // not prove the receiver checks the signature.
    const resultLine = h('p', { className: 'text-sm mt-2', role: 'status' });
    if (state.isAdmin()) {
      content.appendChild(h('button', {
        className: 'btn btn--sm btn--secondary mt-3',
        onClick: async (e) => {
          const btn = e.currentTarget;
          btn.disabled = true;
          btn.textContent = t('wh.sending');
          resultLine.textContent = '';
          try {
            const res = await post(`/admin/api/projects/${projectId}/webhooks/${wh.id}/test`);
            if (res.success) {
              resultLine.style.color = 'var(--success)';
              resultLine.textContent = t('wh.delivered', { status: res.status_code, ms: res.duration_ms });
            } else {
              resultLine.style.color = 'var(--danger)';
              resultLine.textContent = res.status_code
                ? (res.response
                  ? t('wh.receiverAnsweredWith', { status: res.status_code, response: res.response })
                  : t('wh.receiverAnswered', { status: res.status_code }))
                : t('wh.unreachable', { error: humanizeError(res.error || t('tg.unknownError')) });
            }
          } catch (err) {
            resultLine.style.color = 'var(--danger)';
            resultLine.textContent = t('wh.testFailed', { error: humanizeError(err.message) });
          } finally {
            btn.disabled = false;
            btn.textContent = t('wh.sendTest');
          }
        },
      }, t('wh.sendTest')));
      content.appendChild(resultLine);
    }
  }

  if (detail.direction === 'incoming') {
    content.appendChild(h('div', { className: 'mt-3' }, [
      h('p', { className: 'text-sm', style: { fontWeight: 600, marginBottom: '4px' } }, t('wh.example')),
      h('pre', {
        style: { fontSize: 'var(--text-xs)', padding: '8px', background: 'var(--bg-secondary)', borderRadius: '4px', overflow: 'auto' },
      }, `curl -X POST \\
  -H "Content-Type: application/json" \\
  -H "X-Webhook-Signature: sha256=<HMAC-SHA256 of the body, keyed with the secret>" \\
  -d '{"event":"test"}' \\
  ${publicBase}/webhooks/${detail.name}`),
    ]));
  }
}

/** Client-side check of an outgoing hook's URL. The server validates too. */
function urlProblemOf(raw) {
  const value = raw.trim();
  if (!value) return t('wh.urlRequired');
  let parsed;
  try { parsed = new URL(value); } catch { return t('wh.urlFull'); }
  if (parsed.protocol !== 'https:' && parsed.protocol !== 'http:') return t('wh.urlProtocol');
  return null;
}

/**
 * Show a signing secret exactly once. The server returns a generated secret
 * only in the create response and never again, so this is the owner's one
 * chance to copy it.
 */
function showSecretOnce(secret, hookName) {
  const input = h('input', { className: 'input', value: secret, readonly: true, 'aria-label': t('wh.signingSecret') });
  input.addEventListener('focus', () => input.select());
  modal.show(t('wh.secretTitle', { name: hookName }), h('div', {}, [
    h('p', { className: 'text-secondary mb-3' }, t('wh.secretOnce')),
    h('div', { className: 'flex items-center gap-2' }, [
      h('div', { style: { flex: '1' } }, [input]),
      h('button', { className: 'btn btn--secondary', type: 'button', onClick: () => copyText(secret, t('helpers.copied')) }, t('common.copy')),
    ]),
  ]), [{ label: t('wh.done'), className: 'btn btn--primary', onClick: () => {} }]);
}

function buildEditableForm(values, isOutgoing) {
  const nameInput = h('input', { className: 'input', value: values.name || '', placeholder: t('wh.namePlaceholder') });
  const urlInput = h('input', { className: 'input', value: values.url || '', placeholder: 'https://...' });
  const secretInput = h('input', {
    className: 'input',
    type: 'password',
    value: '',
    placeholder: values.secretPlaceholder || t('wh.secretPlaceholder'),
    autocomplete: 'new-password',
    autocapitalize: 'off',
    spellcheck: 'false',
  });
  const eventsInput = h('input', { className: 'input', value: (values.events || []).join(', '), placeholder: 'data.insert, data.update, payment.completed' });

  return { nameInput, urlInput, secretInput, eventsInput };
}

function showCreateModal(projectId, reload) {
  const dirSelect = h('select', { className: 'input', onChange: () => {
    urlGroup.style.display = dirSelect.value === 'outgoing' ? '' : 'none';
  }}, [
    h('option', { value: 'incoming' }, t('wh.incoming')),
    h('option', { value: 'outgoing' }, t('wh.outgoing')),
  ]);
  const fields = buildEditableForm({}, false);
  const urlGroup = formGroup('URL', fields.urlInput, { required: true });
  urlGroup.style.display = 'none';
  let created = null;
  let typedSecretAtSubmit = false;

  createFormModal({
    title: t('wh.create'),
    fields: [
      formGroup(t('projects.col.name'), fields.nameInput, { hint: t('wh.nameHint') }),
      formGroup(t('wh.direction'), dirSelect, {
        hint: t('wh.directionHint'),
      }),
      urlGroup,
      formGroup(t('wh.secretLabel'), fields.secretInput, {
        hint: t('wh.secretHint'),
      }),
      formGroup(t('wh.events'), fields.eventsInput, {
        hint: t('wh.eventsHint'),
      }),
    ],
    submitLabel: t('rc.create'),
    validate: () => {
      if (!fields.nameInput.value.trim()) return t('tasks.nameRequired');
      if (dirSelect.value === 'outgoing') {
        const urlProblem = urlProblemOf(fields.urlInput.value);
        if (urlProblem) return urlProblem;
      }
      return null;
    },
    submit: async () => {
      typedSecretAtSubmit = fields.secretInput.value.trim() !== '';
      created = await post(`/admin/api/projects/${projectId}/webhooks`, {
        name: fields.nameInput.value.trim(),
        direction: dirSelect.value,
        url: fields.urlInput.value.trim(),
        secret: fields.secretInput.value.trim(),
        events: fields.eventsInput.value.split(',').map(e => e.trim()).filter(Boolean),
      });
    },
    success: t('wh.created'),
    onDone: () => {
      reload();
      // The server generates a signing secret when none was typed and returns
      // it once. Without showing it here it would be unrecoverable.
      if (created && created.secret && !typedSecretAtSubmit) showSecretOnce(created.secret, fields.nameInput.value.trim());
    },
  });
}

async function showEditModal(projectId, wh, reload) {
  let currentEvents = [];
  try {
    const detail = await get(`/admin/api/projects/${projectId}/webhooks/${wh.id}`);
    currentEvents = detail.subscriptions || [];
  } catch {
    // Best-effort prefill, show form even if subscription fetch fails.
  }

  const isOutgoing = wh.direction === 'outgoing';
  const fields = buildEditableForm({
    name: wh.name,
    url: wh.url,
    events: currentEvents,
    secretPlaceholder: t('wh.keepSecretPlaceholder'),
  }, isOutgoing);
  const urlGroup = formGroup('URL', fields.urlInput);
  urlGroup.style.display = isOutgoing ? '' : 'none';

  createFormModal({
    title: t('wh.editTitle', { name: wh.name }),
    fields: [
      formGroup(t('projects.col.name'), fields.nameInput),
      h('div', { className: 'form-group' }, [
        h('label', {}, t('wh.direction')),
        h('div', { className: 'text-sm', style: { padding: '6px 0' } }, isOutgoing ? t('wh.outgoing') : t('wh.incoming')),
      ]),
      urlGroup,
      formGroup(t('wh.secretLabel'), fields.secretInput, { hint: t('wh.keepSecretHint') }),
      formGroup(t('wh.events'), fields.eventsInput, {
        hint: t('wh.eventsEditHint'),
      }),
    ],
    validate: () => (isOutgoing ? urlProblemOf(fields.urlInput.value) : null),
    submit: () => {
      const body = {};
      const name = fields.nameInput.value.trim();
      if (name && name !== wh.name) body.name = name;
      const url = fields.urlInput.value.trim();
      if (isOutgoing && url !== (wh.url || '')) body.url = url;
      const secret = fields.secretInput.value.trim();
      if (secret) body.secret = secret;
      body.events = fields.eventsInput.value.split(',').map(e => e.trim()).filter(Boolean);
      return put(`/admin/api/projects/${projectId}/webhooks/${wh.id}`, body);
    },
    success: t('wh.updated'),
    onDone: reload,
  });
}
