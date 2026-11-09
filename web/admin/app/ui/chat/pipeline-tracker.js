/**
 * Pipeline stage tracker widget.
 * Displays a visual progress bar for the engine pipeline stages.
 * Stages: PLAN -> BUILD -> LIVE.
 */

import { h } from '../../core/dom.js';
import { t } from '../../core/i18n.js';
import { icon } from '../icon.js';
import { STAGE_PILL_LABELS, STAGE_ICONS } from './stage-copy.js';

const DEFAULT_STAGES = [
  { key: 'PLAN', label: STAGE_PILL_LABELS.PLAN, icon: STAGE_ICONS.PLAN },
  { key: 'BUILD', label: STAGE_PILL_LABELS.BUILD, icon: STAGE_ICONS.BUILD },
  { key: 'LIVE', label: STAGE_PILL_LABELS.LIVE, icon: STAGE_ICONS.LIVE },
];

// EXPAND is optional: it only runs when the project opts into brief expansion,
// so it isn't part of DEFAULT_STAGES. It is spliced in as the leading node the
// first time the backend references it (see ensureExpandStage), and a run that
// skips EXPAND never shows it because reset() restores DEFAULT_STAGES.
const EXPAND_STAGE = { key: 'EXPAND', label: STAGE_PILL_LABELS.EXPAND, icon: STAGE_ICONS.EXPAND };

export function createPipelineTracker() {
  const reduce = window.matchMedia
    && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  let visible = false;
  let currentStageKey = null;
  let completedStages = new Set();
  let errorStage = null;
  let stages = DEFAULT_STAGES;
  const pendingTimers = [];

  // The stages are drawn with colour and animation; this gives the same state to
  // assistive technology: a named group, each stage described, the current one
  // marked, and changes announced.
  const trackerEl = h('div', { className: 'pipeline-tracker', role: 'group', 'aria-label': t('chat.buildProgress'), 'aria-live': 'polite' });
  trackerEl.style.display = 'none';

  let trackerNodes = {};
  let trackerLines = {};
  let stagesRow = null;

  function buildStagesDOM() {
    trackerNodes = {};
    trackerLines = {};
    if (stagesRow) stagesRow.remove();

    stagesRow = h('div', { className: 'pipeline-tracker__stages' });
    stages.forEach((s, i) => {
      if (i > 0) {
        const line = h('div', { className: 'pipeline-tracker__line' });
        stagesRow.appendChild(line);
        trackerLines[s.key] = line;
      }
      const circle = h('div', { className: 'pipeline-tracker__circle' });
      circle.innerHTML = icon(s.icon);
      const label = h('span', { className: 'pipeline-tracker__label' }, s.label);
      const node = h('div', { className: 'pipeline-tracker__node' }, [circle, label]);
      stagesRow.appendChild(node);
      trackerNodes[s.key] = { node, circle, label, originalIcon: s.icon };
    });
    trackerEl.appendChild(stagesRow);
  }

  buildStagesDOM();

  // Splice EXPAND in as the leading stage the first time it's referenced, then
  // rebuild the row so the node exists. Both entry points already fire for
  // EXPAND: a live brainStageChange carries it (-> setStage) and a completed
  // EXPAND transition from the run log replays on reload (-> markCompleted).
  // The caller re-applies stage state right after, so no rendering is done here.
  function ensureExpandStage() {
    if (stages.some(s => s.key === EXPAND_STAGE.key)) return;
    stages = [EXPAND_STAGE, ...stages];
    buildStagesDOM();
  }

  function show() {
    if (visible) return;
    visible = true;
    trackerEl.style.display = '';
    if (reduce) return;
    // Fade and slide the bar in rather than snapping it on: start from the
    // entering state, then drop it on the next frames so the transition runs.
    trackerEl.classList.add('pipeline-tracker--entering');
    requestAnimationFrame(() => requestAnimationFrame(() => {
      trackerEl.classList.remove('pipeline-tracker--entering');
    }));
  }

  function hide() {
    visible = false;
    trackerEl.style.display = 'none';
  }

  function reset() {
    hide();
    currentStageKey = null;
    completedStages = new Set();
    errorStage = null;
    pendingTimers.forEach(clearTimeout);
    pendingTimers.length = 0;
    stages = DEFAULT_STAGES;
    buildStagesDOM();
  }

  function animateCompletion(stageKey) {
    const node = trackerNodes[stageKey];
    if (!node) return;
    const { circle, label } = node;

    circle.className = 'pipeline-tracker__circle pipeline-tracker__circle--completing';
    label.className = 'pipeline-tracker__label pipeline-tracker__label--done';
    circle.innerHTML = icon('check');

    const timer = setTimeout(() => {
      circle.className = 'pipeline-tracker__circle pipeline-tracker__circle--done';
    }, 380);
    pendingTimers.push(timer);
  }

  function setStage(stageKey) {
    if (stageKey === EXPAND_STAGE.key) ensureExpandStage();
    currentStageKey = stageKey;
    show();

    const idx = stages.findIndex(s => s.key === stageKey);
    if (idx < 0) return;

    for (let i = 0; i < idx; i++) {
      completedStages.add(stages[i].key);
    }

    for (let i = 0; i < stages.length; i++) {
      const s = stages[i];
      const node = trackerNodes[s.key];
      if (!node) continue;
      const { circle, label, originalIcon } = node;

      if (completedStages.has(s.key)) {
        if (!circle.classList.contains('pipeline-tracker__circle--completing')) {
          circle.className = 'pipeline-tracker__circle pipeline-tracker__circle--done';
          label.className = 'pipeline-tracker__label pipeline-tracker__label--done';
          circle.innerHTML = icon('check');
        }
      } else if (s.key === stageKey && errorStage !== stageKey) {
        const modifier = stageKey === 'LIVE'
          ? 'pipeline-tracker__circle--live'
          : 'pipeline-tracker__circle--active';
        circle.className = 'pipeline-tracker__circle ' + modifier;
        label.className = stageKey === 'LIVE'
          ? 'pipeline-tracker__label pipeline-tracker__label--done'
          : 'pipeline-tracker__label pipeline-tracker__label--active';
        circle.innerHTML = icon(originalIcon);
      } else if (s.key === errorStage) {
        circle.className = 'pipeline-tracker__circle pipeline-tracker__circle--error';
        label.className = 'pipeline-tracker__label';
        circle.innerHTML = icon(originalIcon);
      } else {
        circle.className = 'pipeline-tracker__circle';
        label.className = 'pipeline-tracker__label';
        circle.innerHTML = icon(originalIcon);
      }

      if (trackerLines[s.key]) {
        trackerLines[s.key].className = 'pipeline-tracker__line';
        if (completedStages.has(s.key) || (i <= idx && completedStages.has(stages[i - 1]?.key))) {
          trackerLines[s.key].classList.add('pipeline-tracker__line--done');
        }
      }
    }

    const nextStage = stages[idx + 1];
    if (nextStage && trackerLines[nextStage.key] && stageKey !== 'LIVE') {
      trackerLines[nextStage.key].classList.add('pipeline-tracker__line--active');
    }

    for (const s of stages) {
      const node = trackerNodes[s.key];
      if (!node) continue;
      const done = completedStages.has(s.key);
      const current = s.key === stageKey;
      const failed = s.key === errorStage;
      const status = failed ? t('pipeline.needsAttention') : done ? t('pipeline.done') : current ? t('pipeline.inProgress') : t('pipeline.notStarted');
      node.node.setAttribute('role', 'img');
      node.node.setAttribute('aria-label', t('pipeline.stageStatus', { label: s.label, status }));
      if (current) node.node.setAttribute('aria-current', 'step');
      else node.node.removeAttribute('aria-current');
    }
  }

  function markCompleted(stageKey) {
    if (stageKey === EXPAND_STAGE.key) ensureExpandStage();
    const wasNew = !completedStages.has(stageKey);
    completedStages.add(stageKey);
    if (wasNew) {
      animateCompletion(stageKey);
    }
  }

  function setError(stageKey) {
    errorStage = stageKey;
    if (currentStageKey) setStage(currentStageKey);
  }

  /**
   * Set custom stages from backend config.
   * @param {Array<{key, label, icon}>} stageList
   */
  function setStages(stageList) {
    if (!stageList || !stageList.length) return;
    stages = stageList;
    buildStagesDOM();
    if (currentStageKey) setStage(currentStageKey);
  }

  return {
    element: trackerEl,
    setStage,
    setStages,
    show,
    hide,
    reset,
    markCompleted,
    setError,
    cleanup() { reset(); },
    get currentStageKey() { return currentStageKey; },
  };
}
