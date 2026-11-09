import { h, clear } from '../../core/dom.js';
import { t } from '../../core/i18n.js';
import { get, post, put, del } from '../../core/http.js';
import { createScope } from '../../core/lifecycle.js';
import { icon } from '../../ui/icon.js';
import * as toast from '../../ui/toast.js';
import * as modal from '../../ui/modal.js';
import * as state from '../../core/state.js';
import { card } from '../../ui/card.js';
import { createListView } from '../../ui/list-view.js';
import { createTabbedPanel } from '../../ui/tabbed-panel.js';
import { formGroup, formatInterval, describeCron } from '../../ui/helpers.js';
import { humanizeError } from '../../ui/errors.js';
import { runAction } from '../../ui/actions.js';
import { createFormModal } from '../../ui/form-modal.js';
import { formatDateTime } from '../../core/datetime.js';

export function renderProjectTasks(container, projectId, section) {
  const scope = createScope();

  // --- Scheduled Jobs tab ---
  const scheduledBody = h('div', { className: 'list-block' });
  if (state.isAdmin()) {
    scheduledBody.appendChild(h('div', { className: 'list-toolbar flex items-center justify-end mb-3' }, [
      h('button', {
        className: 'btn btn--primary btn--sm',
        onClick: () => showCreateModal(projectId, () => list.reload()),
      }, [h('span', { innerHTML: icon('plus') }), ' ' + t('tasks.create')]),
    ]));
  }

  const listEl = h('div');
  scheduledBody.appendChild(listEl);

  const list = createListView({
    scope,
    container: listEl,
    fetch: () => get(`/admin/api/projects/${projectId}/tasks`),
    renderItem: (task) => renderTask(task, projectId, () => list.reload()),
    filter: true,
    empty: {
      iconName: 'clock',
      title: t('tasks.empty.title'),
      description: t('tasks.empty.description'),
      action: state.isAdmin()
        ? { label: t('tasks.create'), onClick: () => showCreateModal(projectId, () => list.reload()) }
        : null,
    },
    errorMsg: t('tasks.loadFailed'),
  });
  list.reload();

  // --- Background jobs tab ---
  // Background job queue (of_jobs): one-shot deferred work the engine or event
  // triggers enqueue. Read-only here apart from cancelling work still pending.
  const backgroundBody = h('div');
  const jobsEl = h('div');
  backgroundBody.appendChild(jobsEl);
  const jobsList = createListView({
    scope,
    container: jobsEl,
    fetch: () => get(`/admin/api/projects/${projectId}/jobs`),
    renderItem: (job) => renderJob(job, projectId, () => jobsList.reload()),
    empty: {
      iconName: 'box',
      title: t('tasks.jobs.empty.title'),
      description: t('tasks.jobs.empty.description'),
    },
    errorMsg: t('tasks.jobs.loadFailed'),
  });
  jobsList.reload();
  // The queue changes on its own (jobs start, finish, fail), so keep it fresh
  // without a skeleton flash while the person watches it.
  scope.interval(() => { if (!document.hidden) jobsList.reload({ quiet: true }); }, 10000);

  createTabbedPanel({
    container,
    title: t('tasks.title'),
    activeSection: section,
    tabs: [
      {
        id: 'scheduled',
        label: t('tabs.scheduled'),
        content: scheduledBody,
      },
      {
        id: 'background',
        label: t('tabs.background'),
        content: backgroundBody,
      },
    ],
  });

  return () => scope.dispose();
}

// The first entry of each pair is the label key; the second is seconds.
const INTERVAL_UNITS = [
  ['tasks.unit.minutes', 60],
  ['tasks.unit.hours', 3600],
  ['tasks.unit.days', 86400],
  ['tasks.unit.seconds', 1],
];

/**
 * A "repeat every" control: a number and a unit, read back as seconds. Typing
 * 3600 into a seconds box and hoping it means an hour is error-prone; this lets
 * the person say "1 hour".
 */
function intervalField(initialSeconds, hint) {
  let unit = INTERVAL_UNITS[0];
  if (initialSeconds > 0) {
    unit = INTERVAL_UNITS.find(([, size]) => size > 1 && initialSeconds % size === 0) || INTERVAL_UNITS[3];
  }
  const input = h('input', {
    className: 'input',
    type: 'number',
    min: '1',
    step: '1',
    value: initialSeconds > 0 ? String(initialSeconds / unit[1]) : '',
    placeholder: t('tasks.intervalPlaceholder'),
    'aria-label': t('tasks.repeatEvery'),
  });
  const select = h('select', { className: 'input', 'aria-label': t('tasks.intervalUnit') },
    INTERVAL_UNITS.map(([key, size]) => h('option', { value: String(size), selected: size === unit[1] }, t(key))));
  const row = h('div', { className: 'flex gap-2' }, [h('div', { style: { flex: '1' } }, [input]), select]);
  return {
    input,
    group: formGroup(t('tasks.repeatEvery'), row, { hint }),
    getSeconds: () => Math.floor(Number(input.value) * Number(select.value)) || 0,
  };
}

function jobStatusBadge(status) {
  const kind = {
    completed: 'badge--success',
    running: 'badge--info',
    pending: 'badge--warning',
    failed: 'badge--danger',
    cancelled: 'badge--warning',
  }[status] || 'badge--info';
  const known = ['completed', 'running', 'pending', 'failed', 'cancelled'];
  const label = known.includes(status) ? t('tasks.status.' + status) : status.charAt(0).toUpperCase() + status.slice(1);
  return h('span', { className: `badge ${kind}` }, label);
}

function renderJob(job, projectId, reload) {
  const canCancel = state.isAdmin() && (job.status === 'pending' || job.status === 'running');
  const actions = canCancel ? [{
    label: t('common.cancel'), iconName: 'x', kind: 'danger',
    onClick: () => modal.confirmDanger(t('tasks.cancelJob'), t('tasks.cancelBody', { id: job.id, type: job.type }),
      () => runAction(() => post(`/admin/api/projects/${projectId}/jobs/${job.id}/cancel`, {}), {
        success: t('tasks.cancelled'),
        onDone: reload,
        errorPrefix: t('tasks.cancelFailed'),
      }), t('tasks.cancelJob')),
  }] : [];
  return card({
    title: `#${job.id} ${job.type}`,
    iconName: 'box',
    badge: jobStatusBadge(job.status),
    actions,
    children: [
      h('div', { className: 'mt-2 flex gap-4 text-xs text-secondary' }, [
        h('span', {}, t('tasks.attempts', { n: job.attempts, max: job.max_attempts })),
        job.created_at ? h('span', {}, t('tasks.created', { when: formatDateTime(job.created_at) })) : null,
        job.completed_at ? h('span', {}, t('tasks.done', { when: formatDateTime(job.completed_at) })) : null,
      ].filter(Boolean)),
      job.error ? h('p', { className: 'text-xs mt-1 text-danger' }, job.error) : null,
    ].filter(Boolean),
  });
}

function scheduleLabel(task) {
  if (task.cron_expression) {
    const desc = describeCron(task.cron_expression);
    return desc !== task.cron_expression
      ? `${desc} (${task.cron_expression})`
      : t('tasks.cronLabel', { expr: task.cron_expression });
  }
  if (task.interval_seconds) return t('tasks.everyInterval', { interval: formatInterval(task.interval_seconds) });
  return t('tasks.noSchedule');
}

function renderTask(task, projectId, reload) {
  const enabledBadge = task.is_enabled
    ? h('span', { className: 'badge badge--success' }, t('common.enabled'))
    : h('span', { className: 'badge badge--danger' }, t('common.disabled'));

  const adminActions = state.isAdmin() ? [
    { label: t('tasks.history'), onClick: () => showRunsModal(projectId, task) },
    {
      label: t('tasks.runNow'), iconName: 'zap',
      onClick: () => modal.confirm(t('tasks.runNowTitle'), t('tasks.runNowBody', { name: task.name }),
        () => runAction(() => post(`/admin/api/projects/${projectId}/tasks/${task.id}/run`, {}), {
          success: t('tasks.started', { name: task.name }),
          onDone: reload,
          errorPrefix: t('tasks.runFailed'),
        }), t('tasks.runNow')),
    },
    { label: t('common.edit'), iconName: 'edit', onClick: () => showEditModal(projectId, task, reload) },
    {
      label: task.is_enabled ? t('tasks.disable') : t('tasks.enable'),
      onClick: () => runAction(() => post(`/admin/api/projects/${projectId}/tasks/${task.id}/toggle`, {}), {
        success: task.is_enabled ? t('tasks.disabled') : t('tasks.enabled'),
        onDone: reload,
        errorPrefix: t('tasks.toggleFailed'),
      }),
    },
    {
      label: t('common.delete'), iconName: 'trash', kind: 'danger',
      onClick: () => modal.confirmDanger(
        t('tasks.deleteTitle'),
        t('tasks.deleteBody', { name: task.name }),
        () => runAction(() => del(`/admin/api/projects/${projectId}/tasks/${task.id}`), {
          success: t('tasks.deleted'),
          onDone: reload,
          errorPrefix: t('tasks.deleteFailed'),
        }),
      ),
    },
  ] : [];

  return card({
    title: task.name,
    iconName: 'clock',
    badge: enabledBadge,
    actions: adminActions,
    children: [
      h('div', { className: 'mt-2' }, [
        task.description ? h('p', { className: 'text-sm text-secondary mb-2' }, task.description) : null,
        h('div', { className: 'flex gap-4 text-xs text-secondary' }, [
          h('span', {}, scheduleLabel(task)),
          task.last_run ? h('span', {}, t('tasks.last', { when: formatDateTime(task.last_run) })) : null,
          task.next_run ? h('span', {}, t('tasks.next', { when: formatDateTime(task.next_run) })) : null,
        ].filter(Boolean)),
      ]),
    ],
  });
}

function showCreateModal(projectId, reload) {
  const nameInput = h('input', { className: 'input', placeholder: t('tasks.namePlaceholder') });
  const descInput = h('textarea', { className: 'input', rows: 2, placeholder: t('tasks.descPlaceholder') });
  const scheduleSelect = h('select', { className: 'input' }, [
    h('option', { value: 'cron' }, t('tasks.cronExpression')),
    h('option', { value: 'interval' }, t('tasks.repeatingInterval')),
  ]);
  const cronInput = h('input', { className: 'input', placeholder: '0 */6 * * *' });
  const interval = intervalField(0, t('tasks.intervalHint'));
  const intervalInput = interval.input;
  const promptInput = h('textarea', { className: 'input', rows: 4, placeholder: t('tasks.promptPlaceholder') });

  const CRON_HINT = t('tasks.cronHint');
  const cronPreview = h('p', { className: 'form-hint' }, CRON_HINT);
  const cronGroup = h('div', { className: 'form-group' }, [
    h('label', {}, t('tasks.scheduleCron')),
    cronInput,
    cronPreview,
  ]);
  cronInput.addEventListener('input', () => {
    const desc = describeCron(cronInput.value.trim());
    cronPreview.textContent = desc && desc !== cronInput.value.trim()
      ? t('tasks.runs', { desc })
      : CRON_HINT;
  });
  const intervalGroup = interval.group;
  intervalGroup.style.display = 'none';
  scheduleSelect.addEventListener('change', () => {
    cronGroup.style.display = scheduleSelect.value === 'cron' ? '' : 'none';
    intervalGroup.style.display = scheduleSelect.value === 'interval' ? '' : 'none';
  });

  const fields = [
    formGroup(t('projects.col.name'), nameInput, { required: true, hint: t('tasks.nameHint') }),
    formGroup(t('rc.description'), descInput, { hint: t('tasks.descHint') }),
    formGroup(t('tasks.scheduleType'), scheduleSelect, {
      hint: t('tasks.scheduleTypeHint'),
    }),
    cronGroup,
    intervalGroup,
    formGroup(t('tasks.whatToDo'), promptInput, {
      required: true,
      hint: t('tasks.whatToDoHint'),
    }),
  ];

  const clearErrors = () => {
    [nameInput, promptInput, cronInput, intervalInput].forEach(i => i.classList.remove('input--error'));
  };

  const buildBody = () => {
    const body = {
      name: nameInput.value.trim(),
      description: descInput.value.trim(),
      prompt: promptInput.value.trim(),
    };
    if (scheduleSelect.value === 'cron') body.cron_expression = cronInput.value.trim();
    else body.interval_seconds = interval.getSeconds();
    return body;
  };

  createFormModal({
    title: t('tasks.createTitle'),
    fields,
    submitLabel: t('rc.create'),
    validate: () => {
      clearErrors();
      const body = buildBody();
      const errors = [];
      if (!body.name) { errors.push([t('tasks.nameRequired'), nameInput]); }
      if (!body.prompt) { errors.push([t('tasks.promptRequired'), promptInput]); }
      if (scheduleSelect.value === 'cron' && !body.cron_expression) errors.push([t('tasks.cronRequired'), cronInput]);
      if (scheduleSelect.value === 'interval' && (!body.interval_seconds || body.interval_seconds <= 0)) {
        errors.push([t('tasks.intervalPositive'), intervalInput]);
      }
      for (const [, el] of errors) el.classList.add('input--error');
      return errors.length ? errors[0][0] : null;
    },
    submit: () => post(`/admin/api/projects/${projectId}/tasks`, buildBody()),
    success: t('tasks.jobCreated'),
    onDone: reload,
  });
}

function showEditModal(projectId, task, reload) {
  const nameInput = h('input', { className: 'input', value: task.name });
  const descInput = h('textarea', { className: 'input', rows: 2, value: task.description || '' });
  const cronInput = h('input', { className: 'input', value: task.cron_expression || '', placeholder: '0 */6 * * *' });
  const interval = intervalField(task.interval_seconds || 0, t('tasks.altToCron'));
  const intervalInput = interval.input;
  const promptInput = h('textarea', { className: 'input', rows: 4, value: task.prompt || '' });

  // Only send the fields that changed; the backend treats absent keys as
  // "keep current".
  const buildBody = () => {
    const body = {};
    const name = nameInput.value.trim();
    if (name && name !== task.name) body.name = name;
    const desc = descInput.value.trim();
    if (desc !== (task.description || '')) body.description = desc;
    const cron = cronInput.value.trim();
    if (cron !== (task.cron_expression || '')) body.cron_expression = cron;
    const seconds = interval.getSeconds();
    if (seconds !== (task.interval_seconds || 0)) body.interval_seconds = seconds;
    const prompt = promptInput.value.trim();
    if (prompt !== (task.prompt || '')) body.prompt = prompt;
    return body;
  };

  createFormModal({
    title: t('tasks.editTitle', { name: task.name }),
    fields: [
      formGroup(t('projects.col.name'), nameInput),
      formGroup(t('rc.description'), descInput),
      formGroup(t('tasks.scheduleCron'), cronInput, { hint: t('tasks.cronExample') }),
      interval.group,
      formGroup(t('tasks.whatToDo'), promptInput),
    ],
    validate: () => {
      if (!cronInput.value.trim() && interval.getSeconds() <= 0) {
        cronInput.classList.add('input--error');
        intervalInput.classList.add('input--error');
        return t('tasks.needSchedule');
      }
      if (Object.keys(buildBody()).length === 0) return t('tasks.noChanges');
      return null;
    },
    submit: () => put(`/admin/api/projects/${projectId}/tasks/${task.id}`, buildBody()),
    success: t('tasks.updated'),
    onDone: reload,
  });
}

// Terminal run statuses and their badges. Anything not listed is still in
// flight, so it falls back to "Running" - which is why every status the engine
// can write has to appear here, or a finished run reads as running forever.
const RUN_BADGES = {
  completed: ['tasks.run.completed', 'badge--success'],
  failed: ['tasks.run.failed', 'badge--danger'],
  stopped: ['tasks.run.stopped', 'badge--secondary'],
  skipped: ['tasks.run.skipped', 'badge--secondary'],
};

function showRunsModal(projectId, task) {
  const content = h('div', {}, [h('p', { className: 'text-secondary' }, t('common.loading'))]);
  modal.show(t('tasks.runHistory', { name: task.name }), content, [modal.cancelBtn(t('common.close'))]);

  get(`/admin/api/projects/${projectId}/tasks/${task.id}/runs`).then(runs => {
    clear(content);
    if (runs.length === 0) {
      content.appendChild(h('p', { className: 'text-secondary' }, t('tasks.noRuns')));
      return;
    }
    for (const run of runs) {
      const badge = h('span', { className: `badge ${RUN_BADGES[run.status]?.[1] || 'badge--warning'}` },
        RUN_BADGES[run.status] ? t(RUN_BADGES[run.status][0]) : t('tasks.status.running'));
      const children = [
        h('div', { className: 'flex items-center justify-between' }, [
          badge,
          h('span', { className: 'text-xs text-secondary' }, formatDateTime(run.started_at)),
        ]),
      ];
      if (run.error_message) children.push(h('p', { className: 'text-sm text-danger mt-2' }, run.error_message));
      content.appendChild(h('div', { className: 'card mb-2', style: { padding: '8px 12px' } }, children));
    }
  }).catch(err => {
    clear(content);
    content.appendChild(h('p', { className: 'text-danger' }, t('tasks.runsFailed', { error: humanizeError(err.message) })));
  });
}
