/**
 * Scheduled-backups card for the Settings page (admin only). Reads and saves the
 * backup schedule, lists the backups on disk, runs one on demand, and guides a
 * restore. The schedule itself is run by the server; this is its control surface.
 */

import { h, clear } from '../../core/dom.js';
import { t, intlTag } from '../../core/i18n.js';
import { get, put, post, del, download } from '../../core/http.js';
import { icon } from '../../ui/icon.js';
import * as toast from '../../ui/toast.js';
import * as modal from '../../ui/modal.js';
import { errorState, formGroup, formatBytes, copyText } from '../../ui/helpers.js';
import { wireSubmit } from '../../ui/form.js';
import { humanizeError } from '../../ui/errors.js';
import { formatDateTime, formatRelative } from '../../core/datetime.js';

export function scheduledBackupsCard() {
  const body = h('div');
  const card = h('div', { className: 'card mt-4' }, [
    h('h4', { className: 'card__title mb-2' }, t('backup.sched.title')),
    h('p', { className: 'form-hint' }, t('backup.sched.body')),
    body,
  ]);

  async function load() {
    clear(body);
    body.appendChild(h('p', { className: 'form-hint' }, t('common.loading')));
    let data;
    try {
      data = await get('/admin/api/system/backups');
    } catch (err) {
      clear(body);
      body.appendChild(errorState({ message: humanizeError(err.message), retry: load, reference: err.requestId }));
      return;
    }
    render(data);
  }

  function render(data) {
    clear(body);

    const scheduleSel = select([
      ['off', t('backup.sched.off')],
      ['daily', t('backup.sched.daily')],
      ['weekly', t('backup.sched.weekly')],
    ], data.schedule || 'off');
    const timeInput = h('input', { className: 'input', type: 'time' });
    timeInput.value = data.time || '03:00';
    const weekdaySel = select(weekdayNames().map((label, i) => [String(i), label]), String(data.weekday ?? '0'));
    const retentionInput = h('input', { className: 'input', type: 'number', min: '1', max: '365' });
    retentionInput.value = String(data.retention || 7);

    const timeGroup = formGroup(t('backup.sched.time'), timeInput);
    const weekdayGroup = formGroup(t('backup.sched.weekday'), weekdaySel);
    const retentionGroup = formGroup(t('backup.sched.retention'), retentionInput);

    function syncVisibility() {
      const v = scheduleSel.value;
      timeGroup.style.display = v === 'off' ? 'none' : '';
      retentionGroup.style.display = v === 'off' ? 'none' : '';
      weekdayGroup.style.display = v === 'weekly' ? '' : 'none';
    }
    scheduleSel.addEventListener('change', syncVisibility);
    syncVisibility();

    const saveBtn = h('button', { className: 'btn btn--primary btn--sm' }, t('common.save'));
    const saver = wireSubmit(null, saveBtn, {
      busyLabel: t('common.saving'),
      errorPrefix: t('backup.sched.saveFailed'),
      onSubmit: async () => {
        await put('/admin/api/system/backups', {
          schedule: scheduleSel.value,
          time: timeInput.value || '03:00',
          weekday: weekdaySel.value,
          retention: parseInt(retentionInput.value, 10) || 7,
        });
        toast.success(t('backup.sched.saved'));
        load();
      },
    });
    saveBtn.addEventListener('click', saver.submit);

    body.appendChild(h('div', { className: 'flex items-center gap-3 mb-3', style: { flexWrap: 'wrap' } }, [
      formGroup(t('backup.sched.schedule'), scheduleSel),
      timeGroup, weekdayGroup, retentionGroup,
    ]));
    // Save on its own line: a bare button in the controls row above would stretch
    // to the form-group height (the row's align-items is not flex-end).
    body.appendChild(h('div', { className: 'mb-3' }, [saveBtn]));

    const latest = data.items && data.items[0];
    body.appendChild(h('div', { className: 'flex items-center gap-4 mb-3 text-sm text-secondary', style: { flexWrap: 'wrap' } }, [
      latest
        ? h('span', {}, t('backup.sched.last', { when: formatRelative(latest.created_at), size: formatBytes(latest.size) }))
        : h('span', {}, t('backup.sched.none')),
      data.next_run ? h('span', {}, t('backup.sched.next', { when: formatDateTime(data.next_run) })) : null,
    ].filter(Boolean)));

    const runBtn = h('button', { className: 'btn btn--secondary btn--sm' }, [h('span', { innerHTML: icon('database') }), ' ' + t('backup.sched.runNow')]);
    const runner = wireSubmit(null, runBtn, {
      busyLabel: t('backup.sched.running'),
      errorPrefix: t('backup.sched.runFailed'),
      onSubmit: async () => { await post('/admin/api/system/backups/run'); toast.success(t('backup.sched.done')); load(); },
    });
    runBtn.addEventListener('click', runner.submit);
    body.appendChild(h('div', { className: 'flex gap-2 mb-3' }, [runBtn]));

    const list = h('div');
    if (data.items && data.items.length) {
      for (const it of data.items) list.appendChild(backupRow(it, data.dir, load));
    }
    body.appendChild(list);

    body.appendChild(h('div', { className: 'backup-note' }, [
      h('span', { className: 'backup-note__icon', innerHTML: icon('alert-triangle') }),
      h('span', { className: 'text-xs' }, t('backup.restore.note')),
    ]));
  }

  function backupRow(it, dir, reload) {
    const actions = h('div', { className: 'flex gap-1' }, [
      iconBtn('download', t('backup.sched.download'), () => downloadBackup(it.name)),
      iconBtn('rotate', t('backup.sched.restore'), () => showRestore(dir, it.name)),
      iconBtn('trash', t('common.delete'), () => confirmDeleteBackup(it, reload)),
    ]);
    return h('div', {
      className: 'flex items-center justify-between',
      style: { padding: '7px 0', borderBottom: '0.5px solid var(--border)' },
    }, [
      h('span', { className: 'text-sm', style: { fontFamily: 'var(--font-mono)', wordBreak: 'break-all' } }, it.name),
      h('div', { className: 'flex items-center gap-3' }, [
        h('span', { className: 'text-xs text-secondary' }, formatBytes(it.size)),
        actions,
      ]),
    ]);
  }

  async function downloadBackup(name) {
    try {
      await download('/admin/api/system/backups/' + encodeURIComponent(name), name);
    } catch (err) {
      toast.error(humanizeError(err.message));
    }
  }

  function confirmDeleteBackup(it, reload) {
    modal.confirmDanger(t('common.delete'), t('backup.sched.deleteBody', { name: it.name }), async () => {
      try {
        await del('/admin/api/system/backups/' + encodeURIComponent(it.name));
        toast.success(t('backup.sched.deleted'));
        reload();
      } catch (err) {
        toast.error(humanizeError(err.message));
        return false;
      }
    });
  }

  function showRestore(dir, name) {
    const cmd = `openfabrica restore ${dir}/${name}`;
    const cmdBox = h('div', {
      className: 'flex items-center gap-2 mt-1',
      style: { background: 'var(--bg-tertiary)', border: '0.5px solid var(--border)', borderRadius: 'var(--radius-sm)', padding: '8px 10px' },
    }, [
      h('code', { style: { flex: '1', fontFamily: 'var(--font-mono)', fontSize: 'var(--text-xs)', wordBreak: 'break-all' } }, cmd),
      iconBtn('copy', t('common.copy'), () => copyText(cmd)),
    ]);
    modal.show(t('backup.restore.title'), h('div', {}, [
      h('p', { className: 'text-sm text-secondary mb-2' }, t('backup.restore.intro')),
      h('p', { className: 'text-sm' }, t('backup.restore.steps')),
      cmdBox,
    ]), [modal.cancelBtn(t('common.close'))]);
  }

  load();
  return card;
}

function iconBtn(name, label, onClick) {
  return h('button', {
    className: 'btn btn--ghost btn--sm',
    title: label,
    'aria-label': label,
    onClick,
  }, [h('span', { innerHTML: icon(name) })]);
}

function select(options, value) {
  const el = h('select', { className: 'input' }, options.map(([v, label]) => {
    const o = h('option', {}, label);
    o.value = v;
    return o;
  }));
  el.value = value;
  return el;
}

// Locale-aware weekday names, Sunday first (index 0 matches the server's weekday
// setting). 2023-01-01 was a Sunday.
function weekdayNames() {
  const fmt = new Intl.DateTimeFormat(intlTag(), { weekday: 'long' });
  return Array.from({ length: 7 }, (_, i) => fmt.format(new Date(Date.UTC(2023, 0, 1 + i, 12))));
}
