/**
 * Questions management view.
 * Groups pending questions per project with one submit button per group.
 */

import { h, clear } from '../../core/dom.js';
import { t, tn } from '../../core/i18n.js';
import { get, post } from '../../core/http.js';
import { icon } from '../../ui/icon.js';
import { defineView } from '../../core/view.js';
import * as toast from '../../ui/toast.js';
import * as state from '../../core/state.js';
import { emptyState, showLoadError } from '../../ui/helpers.js';
import { createGroupedForm, createApprovalCard, createAnsweredCard, createDeferredCard } from '../../ui/question-cards.js';
import { pageHeader, pageBody } from '../../ui/card.js';
import { humanizeError } from '../../ui/errors.js';

export const renderQuestions = defineView((ctx, container) => {
  clear(container);

  const header = pageHeader({
    title: t('nav.questions'),
  });

  const listContainer = pageBody();

  container.appendChild(header);
  container.appendChild(listContainer);

  async function loadQuestions() {
    try {
      const questions = await get('/admin/api/questions');
      renderList(questions);
    } catch (err) {
      showLoadError(listContainer, t('questions.loadFailed'), err, loadQuestions);
    }
  }

  // The project's name when the shared list knows it; the id is only a fallback.
  function projectLabel(projectId) {
    const project = (state.get('projects') || []).find((p) => String(p.id) === String(projectId));
    return project ? project.name : t('questions.projectFallback', { id: projectId });
  }

  function renderList(questions) {
    clear(listContainer);

    if (questions.length === 0) {
      listContainer.appendChild(emptyState({
        iconName: 'help-circle',
        title: t('questions.empty.title'),
        description: t('questions.empty.description'),
      }));
      return;
    }

    // Group by project
    const byProject = {};
    for (const q of questions) {
      (byProject[q.project_id] ??= []).push(q);
    }

    for (const [projectId, projectQuestions] of Object.entries(byProject)) {
      const pending = projectQuestions.filter(q => q.status === 'pending');
      const answered = projectQuestions.filter(q => q.status !== 'pending');
      const pendingRegular = pending.filter(q => q.type !== 'approval');
      const pendingApproval = pending.filter(q => q.type === 'approval');

      // Project section header
      listContainer.appendChild(h('div', { className: 'flex items-center gap-2 mb-3 mt-4' }, [
        h('span', { innerHTML: icon('globe') }),
        h('h4', {}, projectLabel(projectId)),
        pending.length > 0
          ? h('span', { className: 'badge badge--question' }, tn('questions.pendingCount', pending.length))
          : null,
      ].filter(Boolean)));

      if (pendingRegular.length > 0) {
        listContainer.appendChild(createGroupedForm(pendingRegular, {
          onSubmit: async (answers) => {
            let sent = 0;
            let failed = 0;
            for (const a of answers) {
              const body = a.deferred ? { deferred: true } : { answer: a.answer };
              body.project_id = Number(projectId); // ids collide across projects, route explicitly
              try {
                await post(`/admin/api/questions/${a.questionId}/answer`, body);
                sent += 1;
              } catch (err) {
                failed += 1;
              }
            }
            if (sent > 0) toast.success(tn('questions.answersSubmitted', sent));
            if (failed > 0) toast.error(tn('questions.answersFailed', failed));
            loadQuestions();
          },
        }));
      }

      for (const q of pendingApproval) {
        listContainer.appendChild(createApprovalCard(q, {
          onRespond: (qid, ans) => submitSingle(qid, ans, q.project_id),
          onSkip: (qid) => skipSingle(qid, q.project_id),
        }));
      }

      for (const q of answered) {
        listContainer.appendChild(q.status === 'deferred'
          ? createDeferredCard(q, { onAnswer: (qid, ans) => submitSingle(qid, ans, q.project_id) })
          : createAnsweredCard(q));
      }
    }
  }

  async function submitSingle(questionId, answer, projectId) {
    try {
      await post(`/admin/api/questions/${questionId}/answer`, projectId ? { answer, project_id: projectId } : { answer });
      toast.success(t('questions.responseSubmitted'));
      loadQuestions();
    } catch (err) {
      toast.error(humanizeError(err.message));
    }
  }

  async function skipSingle(questionId, projectId) {
    try {
      await post(`/admin/api/questions/${questionId}/answer`, projectId ? { deferred: true, project_id: projectId } : { deferred: true });
      toast.success(t('questions.deferred'));
      loadQuestions();
    } catch (err) {
      toast.error(humanizeError(err.message));
    }
  }

  loadQuestions();
});
