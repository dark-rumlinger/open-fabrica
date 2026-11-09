/**
 * The Engine card in Settings > Advanced. The engine is the background worker
 * that builds and maintains a project. Owners normally deal in phases (the
 * badge and its one action in the project header); this card is the manual
 * override for a worker that stopped or a phase that got stuck.
 */

import { h, clear } from '../../core/dom.js';
import { t } from '../../core/i18n.js';
import { get, post } from '../../core/http.js';
import * as toast from '../../ui/toast.js';
import * as modal from '../../ui/modal.js';
import { humanizeError } from '../../ui/errors.js';

const PHASE_CHOICES = [
  { mode: 'building', label: t('phase.building') },
  { mode: 'live', label: t('phase.live') },
  { mode: 'paused', label: t('phase.paused') },
];

export function createEngineCard(projectId) {
  const body = h('div');
  const card = h('div', { className: 'card' }, [
    h('h4', { className: 'card__title mb-2' }, t('eng.title')),
    body,
  ]);

  async function act(work, done) {
    try {
      await work();
      toast.success(done);
    } catch (err) {
      toast.error(humanizeError(err.message));
    }
    await load();
  }

  function paint(status) {
    clear(body);
    const running = !!status.running;
    // Needs you is a building project waiting on an answer, so it counts as
    // building for the purpose of setting the phase by hand.
    const current = status.phase.key === 'needs_you' ? 'building' : status.phase.key;

    body.appendChild(h('p', { className: 'form-hint' },
      t(running ? 'eng.explainRunning' : 'eng.explainStopped')));

    body.appendChild(h('div', { className: 'mt-3' }, [
      h('button', {
        className: `btn btn--sm ${running ? 'btn--danger' : 'btn--primary'}`,
        onClick: () => {
          const go = () => act(
            () => post(`/admin/api/engine/${projectId}/${running ? 'stop' : 'start'}`),
            running ? t('eng.shutDownDone') : t('eng.started'),
          );
          if (!running) { go(); return; }
          modal.confirmDanger(t('eng.shutDownTitle'),
            t('eng.shutDownBody'),
            go, t('eng.shutDownBtn'));
        },
      }, running ? t('eng.shutDownTitle') : t('phase.start')),
    ]));

    body.appendChild(h('h5', { className: 'mt-4 mb-1' }, t('eng.setPhase')));
    body.appendChild(h('p', { className: 'form-hint' }, t('eng.setPhaseHint')));
    body.appendChild(h('div', { className: 'flex gap-2 mt-2' }, PHASE_CHOICES.map(choice => h('button', {
      className: 'btn btn--sm btn--secondary',
      disabled: current === choice.mode,
      onClick: () => modal.confirm(t('eng.setPhaseTitle', { phase: choice.label }),
        t('eng.setPhaseBody', { phase: choice.label }),
        () => act(
          () => post(`/admin/api/engine/${projectId}/mode`, { mode: choice.mode }),
          t('eng.phaseSet', { phase: choice.label }),
        ), t('eng.setPhaseBtn')),
    }, choice.label))));
  }

  async function load() {
    try {
      paint(await get(`/admin/api/engine/${projectId}/status`));
    } catch {
      clear(body);
      body.appendChild(h('p', { className: 'form-hint', role: 'alert' }, t('eng.readFailed')));
      body.appendChild(h('button', { className: 'btn btn--secondary btn--sm mt-2', onClick: () => load() }, t('common.tryAgain')));
    }
  }

  load();
  return card;
}
