/**
 * Question card renderers, single source of truth.
 *
 * Two visual containers:
 *   - chat group/single (used in the live chat feed)        -> createQuestionGroup, createQuestionCard
 *   - panel grouped/single (used in the questions panels)   -> createGroupedForm, createApprovalCard, createAnsweredCard
 *
 * Shared internals:
 *   - makeProgressTracker, disabled-submit + filled-badge logic
 *   - postAnswer         , single POST + global pending-questions badge sync
 *   - switchPanel        , context-panel navigation (set by project/index.js)
 */

import { h } from '../core/dom.js';
import { t, tn } from '../core/i18n.js';
import { post } from '../core/http.js';
import { bumpPending } from '../core/questions.js';
import { icon } from './icon.js';
import { isSubmitEnter } from './a11y.js';
import * as toast from './toast.js';
import { buildQuestionInput, isMultiChoiceType } from './question-input.js';
import { formatDateTime } from '../core/datetime.js';
import { humanizeError } from './errors.js';

// --- Panel switcher injection (set by project/index.js) ---
let _switchPanel = null;

export function setPanelSwitcher(fn) {
  _switchPanel = fn;
}

function switchPanel(panel) {
  if (_switchPanel) _switchPanel(panel);
}

// --- Shared internals ---

/**
 * Wires the "X/Y filled" + disabled-submit + filled-number-badge behaviour
 * shared by every grouped question form.
 */
function makeProgressTracker(items, { progressEl, submitBtn, submitProgressEl, listEl }) {
  function update() {
    const filled = items.filter(i => i.hasValue()).length;
    const total = items.length;
    if (progressEl) progressEl.textContent = t('qc.progress', { filled, total });
    if (submitProgressEl) submitProgressEl.textContent = t('qc.submitProgress', { filled, total });
    if (submitBtn) submitBtn.disabled = filled < total;
    for (const item of items) {
      item.numberEl.classList.toggle('question-group__number--filled', item.hasValue());
    }
  }
  if (listEl) {
    listEl.addEventListener('input', update);
    listEl.addEventListener('click', () => setTimeout(update, 0));
  }
  return update;
}

/**
 * POST a single answer and decrement the per-project pending-questions badge.
 * Throws on failure so callers can update UI.
 */
async function postAnswer(questionId, answer, projectId) {
  // Always send project_id: question ids are per-project (each project DB
  // autoincrements from 1), so without it the backend falls back to a
  // cross-project id search and can route the answer to a colliding id in a
  // different project, leaving this project's question pending forever.
  await post(`/admin/api/questions/${questionId}/answer`, projectId ? { answer, project_id: projectId } : { answer });
  if (projectId) bumpPending(projectId, -1);
}

/**
 * Context line worth showing the owner. Approval questions carry an internal
 * "approval:<key>" tag in the same column, which is bookkeeping, not prose.
 */
function visibleContext(context) {
  if (!context || /^approval:/.test(context)) return '';
  return context;
}

/**
 * POST a deferred (opt-out) answer for a question. The engine treats this
 * as "no longer pending" so the pipeline resumes, but the owner can fill
 * in the real value later from the questions panel (see createDeferredCard).
 */
async function postAnswerDeferred(questionId, projectId) {
  await post(`/admin/api/questions/${questionId}/answer`, projectId ? { deferred: true, project_id: projectId } : { deferred: true });
  if (projectId) bumpPending(projectId, -1);
}

/**
 * Render a small ghost "Skip" button that flips the given card into the
 * deferred-answered state on click.
 */
function makeSkipButton(questionId, cardEl, projectId) {
  return h('button', {
    className: 'btn btn--ghost btn--sm chat-card__skip-btn',
    title: t('qc.skipTip'),
    onClick: () => submitChatAnswerDeferred(questionId, cardEl, projectId),
  }, t('qc.skip'));
}

// =============================================================
//  CHAT VARIANTS, used inside the live chat feed
// =============================================================

/**
 * Create an inline question card with interactive option buttons.
 * Used for single questions appearing in the chat feed.
 */
export function createQuestionCard(questionData) {
  const { id, question, urgency, context, type, options, project_id: projectId, fields } = questionData;

  const card = h('div', { className: 'chat-card chat-card--question', dataset: { questionType: type || '' } });

  const header = h('div', { className: 'chat-card__header chat-card__header--question' }, [
    h('span', { innerHTML: icon('help-circle'), className: 'chat-card__icon chat-card__icon--question' }),
    h('span', { className: 'chat-card__label' }, t('qc.question')),
    urgency === 'high'
      ? h('span', { className: 'badge badge--danger' }, t('qc.urgent'))
      : h('span', { className: 'badge badge--question' }, t('qc.needsInput')),
  ]);

  const body = h('div', { className: 'chat-card__body' }, [
    h('p', { className: 'chat-card__text' }, question),
  ]);

  if (visibleContext(context)) {
    body.appendChild(h('p', { className: 'chat-card__context text-sm text-secondary' }, context));
  }

  card.appendChild(header);
  card.appendChild(body);

  // For approval-style questions with options (Approve / Skip / Other...),
  // single-click submit. Otherwise fall back to the standard
  // "buttons + manual Submit" flow so multi-select / structured-form
  // questions still work as before.
  const isSingleChoice = (type === 'single_choice' || !type) && options;
  const isApprovalChoice = type === 'approval' && options;
  const { inputEl, getValue } = buildQuestionInput(questionData);

  if (isSingleChoice || isApprovalChoice) {
    const btns = inputEl.querySelectorAll('.chat-card__option-btn');
    btns.forEach(btn => {
      // Skip the "Other…" toggle, it expands an inline text input rather
      // than submitting; the user types their answer and presses Enter.
      if (btn.textContent === t('qi.other')) {
        return;
      }
      btn.addEventListener('click', () => submitChatAnswer(id, btn.textContent, card, projectId));
    });
    // Wire Enter on the "Other…" text input to submit whatever the owner typed.
    const otherInput = inputEl.querySelector('input[type="text"]');
    if (otherInput) {
      otherInput.addEventListener('keydown', (e) => {
        if (isSubmitEnter(e)) {
          const v = getValue();
          if (v) submitChatAnswer(id, v, card, projectId);
        }
      });
    }
    card.appendChild(inputEl);
    card.appendChild(h('div', { className: 'chat-card__custom-answer' }, [makeSkipButton(id, card, projectId)]));
  } else if (questionData.fields) {
    const submitBtn = h('button', {
      className: 'btn btn--sm btn--primary',
      onClick: () => { if (getValue()) submitChatAnswer(id, getValue(), card, projectId); },
    }, t('qc.submit'));
    card.appendChild(inputEl);
    card.appendChild(h('div', { className: 'chat-card__field-actions' }, [submitBtn, makeSkipButton(id, card, projectId)]));
  } else if (isMultiChoiceType(type)) {
    const submitBtn = h('button', {
      className: 'btn btn--sm btn--primary',
      onClick: () => { if (getValue()) submitChatAnswer(id, getValue(), card, projectId); },
    }, t('qc.submit'));
    card.appendChild(inputEl);
    card.appendChild(h('div', { className: 'chat-card__custom-answer' }, [submitBtn, makeSkipButton(id, card, projectId)]));
  } else {
    const input = inputEl.querySelector('input');
    if (input) {
      input.addEventListener('keydown', (e) => {
        if (isSubmitEnter(e) && getValue()) submitChatAnswer(id, getValue(), card, projectId);
      });
    }
    card.appendChild(h('div', { className: 'chat-card__custom-answer' }, [
      inputEl,
      h('button', {
        className: 'btn btn--sm btn--primary',
        onClick: () => { if (getValue()) submitChatAnswer(id, getValue(), card, projectId); },
      }, t('qc.send')),
      makeSkipButton(id, card, projectId),
    ]));
  }

  return { element: card, questionId: id };
}

async function submitChatAnswer(questionId, answer, cardEl, projectId) {
  try {
    await postAnswer(questionId, answer, projectId);

    cardEl.innerHTML = '';
    cardEl.className = 'chat-card chat-card--question chat-card--answered';

    cardEl.appendChild(h('div', { className: 'chat-card__header' }, [
      h('span', { innerHTML: icon('check'), className: 'chat-card__icon chat-card__icon--success' }),
      h('span', { className: 'chat-card__label' }, t('qc.answered')),
    ]));

    cardEl.appendChild(h('div', { className: 'chat-card__body' }, [
      // A secret (API key, password) must never be echoed back on screen.
      h('p', { className: 'chat-card__answer-text' }, cardEl.dataset?.questionType === 'secret' ? t('qc.hidden') : answer),
      h('button', {
        className: 'btn btn--ghost btn--sm mt-2',
        onClick: () => switchPanel('questions'),
      }, t('qc.viewAll')),
    ]));

    document.dispatchEvent(new CustomEvent('of:questionAnswered', {
      detail: { questionId, answer },
    }));

    toast.success(t('qc.answerSubmitted'));
  } catch (err) {
    toast.error(t('qc.submitFailed', { error: humanizeError(err.message) }));
  }
}

async function submitChatAnswerDeferred(questionId, cardEl, projectId) {
  try {
    await postAnswerDeferred(questionId, projectId);

    cardEl.innerHTML = '';
    cardEl.className = 'chat-card chat-card--question chat-card--answered';

    cardEl.appendChild(h('div', { className: 'chat-card__header' }, [
      h('span', { innerHTML: icon('check'), className: 'chat-card__icon chat-card__icon--success' }),
      h('span', { className: 'chat-card__label' }, t('qc.deferred')),
    ]));

    cardEl.appendChild(h('div', { className: 'chat-card__body' }, [
      h('p', { className: 'chat-card__answer-text' }, t('qc.fillLater')),
      h('button', {
        className: 'btn btn--ghost btn--sm mt-2',
        onClick: () => switchPanel('questions'),
      }, t('qc.viewAll')),
    ]));

    document.dispatchEvent(new CustomEvent('of:questionAnswered', {
      detail: { questionId, answer: null, deferred: true },
    }));

    toast.success(t('questions.deferred'));
  } catch (err) {
    toast.error(t('qc.deferFailed', { error: humanizeError(err.message) }));
  }
}

/**
 * Create a grouped question card that collects answers for multiple questions
 * and submits them all at once.
 * Returns { element, addQuestion(data), questionIds }.
 */
export function createQuestionGroup(questions) {
  const items = []; // { id, data, getValue, hasValue, numberEl }
  const questionIds = new Set();

  const card = h('div', { className: 'chat-card chat-card--question-group' });

  const countBadge = h('span', { className: 'badge badge--question' }, tn('qc.count', questions.length));
  const progressEl = h('span', { className: 'question-group__progress' }, t('qc.progress', { filled: 0, total: questions.length }));

  const header = h('div', { className: 'chat-card__header chat-card__header--question' }, [
    h('span', { innerHTML: icon('help-circle'), className: 'chat-card__icon chat-card__icon--question' }),
    h('span', { className: 'chat-card__label' }, t('nav.questions')),
    countBadge,
    progressEl,
  ]);
  card.appendChild(header);

  const list = h('div', { className: 'question-group__list' });
  card.appendChild(list);

  const submitProgressEl = h('span', { className: 'question-group__submit-progress' });
  const submitBtn = h('button', {
    className: 'btn btn--primary',
    disabled: true,
    onClick: () => submitAll(),
  }, t('qc.submitAll'));

  card.appendChild(h('div', { className: 'question-group__submit' }, [submitProgressEl, submitBtn]));

  const updateProgress = makeProgressTracker(items, { progressEl, submitBtn, submitProgressEl, listEl: list });

  function addItem(qData) {
    const { id, question, context: ctx } = qData;
    if (questionIds.has(id)) return;
    questionIds.add(id);

    const idx = items.length + 1;
    const numberEl = h('span', { className: 'question-group__number' }, String(idx));
    const questionText = h('span', { className: 'question-group__question-text' }, question);

    const itemEl = h('div', { className: 'question-group__item' });
    itemEl.appendChild(h('div', { className: 'question-group__item-header' }, [numberEl, questionText]));

    if (visibleContext(ctx)) {
      itemEl.appendChild(h('p', { className: 'text-sm text-secondary', style: { paddingInlineStart: '32px', marginBottom: '8px' } }, ctx));
    }

    const { inputEl, getValue, hasValue } = buildQuestionInput(qData, {
      onInput: updateProgress,
      wrapClass: 'question-group__input',
    });

    itemEl.appendChild(inputEl);

    const item = { id, data: qData, getValue, hasValue, numberEl, deferred: false, itemEl, secret: qData.type === 'secret' };
    // Deferred items count as filled for the Submit-All gate and as their
    // own "answer" at submit time; the per-row Skip flips the input to a
    // muted placeholder so the owner sees the choice they made.
    const skipBtn = h('button', {
      className: 'btn btn--ghost btn--sm question-group__skip-btn',
      title: t('qc.skipTip'),
      onClick: () => {
        item.deferred = !item.deferred;
        if (item.deferred) {
          inputEl.style.opacity = '0.4';
          inputEl.style.pointerEvents = 'none';
          skipBtn.textContent = t('qc.undoSkip');
        } else {
          inputEl.style.opacity = '';
          inputEl.style.pointerEvents = '';
          skipBtn.textContent = t('qc.skip');
        }
        updateProgress();
      },
    }, t('qc.skip'));
    itemEl.appendChild(h('div', { className: 'question-group__item-actions' }, [skipBtn]));

    list.appendChild(itemEl);

    // Override hasValue so deferred items count as filled.
    item.hasValue = () => item.deferred || hasValue();
    items.push(item);

    countBadge.textContent = tn('qc.count', items.length);
    updateProgress();
  }

  async function submitAll() {
    const answers = [];
    for (const item of items) {
      if (item.deferred) {
        answers.push({ questionId: item.id, deferred: true, displayAnswer: t('qc.deferredValue') });
        continue;
      }
      const val = item.getValue();
      if (!val) return;
      answers.push({ questionId: item.id, answer: val, displayAnswer: item.secret ? t('qc.hidden') : val });
    }

    submitBtn.disabled = true;
    submitBtn.textContent = t('qc.submitting');

    try {
      let succeeded = 0;
      let failed = 0;
      const groupProjectId = items[0]?.data?.project_id;
      for (const a of answers) {
        try {
          // Don't decrement badge per-call here, we'll do it in bulk below
          // so we can correctly handle partial failure.
          // Include project_id so the backend doesn't misroute the answer to a
          // colliding question id in another project (ids are per-project).
          const body = a.deferred ? { deferred: true } : { answer: a.answer };
          if (groupProjectId) body.project_id = groupProjectId;
          await post(`/admin/api/questions/${a.questionId}/answer`, body);
          succeeded++;
        } catch {
          failed++;
        }
      }

      if (failed > 0) {
        toast.error(tn('qc.someFailed', failed));
        if (succeeded === 0) {
          submitBtn.disabled = false;
          submitBtn.textContent = t('qc.submitAll');
          return;
        }
      }

      if (groupProjectId && succeeded > 0) bumpPending(groupProjectId, -succeeded);

      card.className = 'chat-card chat-card--question-group chat-card--answered';
      card.innerHTML = '';

      card.appendChild(h('div', { className: 'chat-card__header' }, [
        h('span', { innerHTML: icon('check'), className: 'chat-card__icon chat-card__icon--success' }),
        h('span', { className: 'chat-card__label' }, tn('qc.answeredCount', answers.length)),
      ]));

      const summary = h('div', { className: 'question-group__answered' });
      for (let i = 0; i < items.length; i++) {
        summary.appendChild(h('div', { className: 'question-group__answered-item' }, [
          h('div', { className: 'question-group__answered-q' }, items[i].data.question),
          h('div', { className: 'question-group__answered-a' }, answers[i].displayAnswer),
        ]));
      }
      card.appendChild(summary);

      card.appendChild(h('div', { className: 'chat-card__body' }, [
        h('button', {
          className: 'btn btn--ghost btn--sm',
          onClick: () => switchPanel('questions'),
        }, t('qc.viewAll')),
      ]));

      for (const a of answers) {
        document.dispatchEvent(new CustomEvent('of:questionAnswered', {
          detail: { questionId: a.questionId, answer: a.answer || null, deferred: !!a.deferred },
        }));
      }

      toast.success(tn('questions.answersSubmitted', answers.length));
    } catch (err) {
      submitBtn.disabled = false;
      submitBtn.textContent = t('qc.submitAll');
      toast.error(t('qc.submitAnswersFailed', { error: humanizeError(err.message) }));
    }
  }

  for (const q of questions) {
    addItem(q);
  }

  return {
    element: card,
    addQuestion: (data) => {
      addItem(data);
      card.scrollIntoView({ behavior: 'smooth', block: 'end' });
    },
    get questionIds() { return questionIds; },
  };
}

// =============================================================
//  PANEL VARIANTS, used inside the questions context panel
// =============================================================

/**
 * Create a grouped form card for multiple pending questions, used in the
 * questions panel. The caller handles POSTing via opts.onSubmit.
 */
export function createGroupedForm(questions, { onSubmit }) {
  const items = []; // { id, getValue, hasValue, numberEl }

  const card = h('div', { className: 'question-group-card' });

  const progressEl = h('span', { className: 'text-xs text-secondary ml-auto' },
    t('qc.progress', { filled: 0, total: questions.length }));

  card.appendChild(h('div', { className: 'question-group-card__header' }, [
    h('span', { className: 'badge badge--question' }, tn('questions.pendingCount', questions.length)),
    h('span', {}, t('qc.answerAll')),
    progressEl,
  ]));

  const listEl = h('div', { className: 'question-group-card__body' });

  for (let i = 0; i < questions.length; i++) {
    const q = questions[i];
    const numberEl = h('span', { className: 'question-group__number' }, String(i + 1));
    const itemEl = h('div', { className: 'question-group-card__item' });

    itemEl.appendChild(h('div', { className: 'question-group-card__item-header' }, [
      numberEl,
      h('div', { style: { flex: '1' } }, [
        h('p', { style: { fontWeight: '600', marginBottom: '4px' } }, q.question),
        visibleContext(q.context)
          ? h('p', { className: 'text-sm text-secondary', style: { margin: '0 0 6px' } }, q.context)
          : null,
        q.type === 'secret'
          ? h('span', { className: 'badge badge--info', style: { fontSize: 'var(--text-xs)' } }, t('qc.secret'))
          : null,
      ].filter(Boolean)),
    ]));

    const { inputEl, getValue, hasValue } = buildQuestionInput(q, {
      wrapClass: 'question-group__input',
    });
    itemEl.appendChild(inputEl);

    const item = { id: q.id, getValue, hasValue, numberEl, deferred: false, secret: q.type === 'secret' };
    const skipBtn = h('button', {
      className: 'btn btn--ghost btn--sm question-group__skip-btn',
      title: t('qc.skipTip'),
      onClick: () => {
        item.deferred = !item.deferred;
        if (item.deferred) {
          inputEl.style.opacity = '0.4';
          inputEl.style.pointerEvents = 'none';
          skipBtn.textContent = t('qc.undoSkip');
        } else {
          inputEl.style.opacity = '';
          inputEl.style.pointerEvents = '';
          skipBtn.textContent = t('qc.skip');
        }
        listEl.dispatchEvent(new Event('input', { bubbles: true }));
      },
    }, t('qc.skip'));
    itemEl.appendChild(h('div', { className: 'question-group__item-actions' }, [skipBtn]));
    listEl.appendChild(itemEl);

    item.hasValue = () => item.deferred || hasValue();
    items.push(item);
  }

  card.appendChild(listEl);

  const submitBtn = h('button', {
    className: 'btn btn--primary',
    disabled: true,
    onClick: async () => {
      const answers = [];
      for (const item of items) {
        if (item.deferred) {
          answers.push({ questionId: item.id, deferred: true });
          continue;
        }
        const val = item.getValue();
        if (!val) return;
        answers.push({ questionId: item.id, answer: val });
      }

      submitBtn.disabled = true;
      submitBtn.textContent = t('qc.submitting');

      try {
        await onSubmit(answers);
      } catch {
        submitBtn.disabled = false;
        submitBtn.textContent = t('qc.submitAll');
      }
    },
  }, t('qc.submitAll'));

  card.appendChild(h('div', { className: 'question-group-card__footer' }, [
    h('span', { className: 'text-xs text-secondary' }),
    submitBtn,
  ]));

  makeProgressTracker(items, { progressEl, submitBtn, listEl });

  return card;
}

/**
 * Create an approval card for a question that needs approve/deny.
 */
export function createApprovalCard(question, { onRespond, onSkip }) {
  const actions = [
    // The answer values stay English: they are what the engine receives.
    h('button', { className: 'btn btn--success btn--sm', onClick: () => onRespond(question.id, 'Approve') }, t('qc.approve')),
    h('button', { className: 'btn btn--danger btn--sm', onClick: () => onRespond(question.id, 'Deny') }, t('qc.deny')),
  ];
  if (onSkip) {
    actions.push(h('button', {
      className: 'btn btn--ghost btn--sm',
      title: t('qc.skipTip'),
      onClick: () => onSkip(question.id),
    }, t('qc.skip')));
  }
  return h('div', { className: 'card mb-3 card--approval' }, [
    h('div', { className: 'flex items-center justify-between mb-2' }, [
      h('span', { className: 'badge badge--danger' }, t('qc.approvalNeeded')),
      h('span', { className: 'text-xs text-secondary' }, formatDateTime(question.created_at)),
    ]),
    h('p', { style: { marginBottom: '8px' } }, question.question),
    h('div', { className: 'flex items-center gap-2 mt-3' }, actions),
  ]);
}

/**
 * Create a card for a question the owner skipped. It keeps the real input so the
 * value can still be given later; onAnswer(questionId, answer) does the POST.
 */
export function createDeferredCard(question, { onAnswer }) {
  const { inputEl, getValue, hasValue } = buildQuestionInput(question);
  const answerBtn = h('button', {
    className: 'btn btn--primary btn--sm',
    disabled: true,
    onClick: async () => {
      const value = getValue();
      if (!value) return;
      answerBtn.disabled = true;
      try {
        await onAnswer(question.id, value);
      } catch {
        answerBtn.disabled = !hasValue();
      }
    },
  }, t('qc.answerNow'));
  const refresh = () => { answerBtn.disabled = !hasValue(); };
  inputEl.addEventListener('input', refresh);
  inputEl.addEventListener('click', () => setTimeout(refresh, 0));

  return h('div', { className: 'card mb-3' }, [
    h('div', { className: 'flex items-center justify-between mb-2' }, [
      h('span', { className: 'badge badge--info' }, t('qc.skippedForNow')),
      h('span', { className: 'text-xs text-secondary' }, formatDateTime(question.created_at)),
    ]),
    h('p', { style: { marginBottom: '8px' } }, question.question),
    visibleContext(question.context)
      ? h('p', { className: 'text-sm text-secondary', style: { margin: '0 0 8px' } }, question.context)
      : null,
    inputEl,
    h('div', { className: 'flex items-center gap-2 mt-3' }, [answerBtn]),
  ].filter(Boolean));
}

/**
 * Create a read-only card showing an answered question.
 */
export function createAnsweredCard(question) {
  return h('div', { className: 'card mb-3' }, [
    h('div', { className: 'flex items-center justify-between mb-2' }, [
      h('span', { className: 'badge badge--success' }, t('qc.answered')),
      h('span', { className: 'text-xs text-secondary' }, formatDateTime(question.created_at)),
    ]),
    h('p', { style: { marginBottom: '8px' } }, question.question),
    question.answer ? h('div', { style: { opacity: '0.7', fontSize: 'var(--text-sm)' } }, [
      h('strong', {}, t('qc.answerLabel') + ' '),
      h('span', {}, question.type === 'secret' ? t('qc.hidden') : question.answer),
    ]) : null,
  ].filter(Boolean));
}
