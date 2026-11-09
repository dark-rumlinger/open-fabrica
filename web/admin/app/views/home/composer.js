/**
 * The home screen's centerpiece: a greeting and one large prompt box.
 */

import { h } from '../../core/dom.js';
import { t } from '../../core/i18n.js';
import { post } from '../../core/http.js';
import { navigate } from '../../core/router.js';
import { icon } from '../../ui/icon.js';
import * as toast from '../../ui/toast.js';
import { humanizeError } from '../../ui/errors.js';
import { createPromptComposer, pickPromptTitle, rememberModelId } from '../../ui/prompt-composer.js';

// A half-written idea survives a reload, a stray navigation or a failed create.
// Storage can be blocked, so every access is guarded and the box works without it.
const DRAFT_KEY = 'of_draft:home';

function readDraft() {
  try { return localStorage.getItem(DRAFT_KEY) || ''; } catch { return ''; }
}

function writeDraft(text) {
  try {
    if (text) localStorage.setItem(DRAFT_KEY, text);
    else localStorage.removeItem(DRAFT_KEY);
  } catch { /* storage blocked */ }
}

function clearDraft() { writeDraft(''); }

/**
 * Create the project and go watch it get built.
 * @param {object} values - from composer.values()
 * @returns {Promise<boolean>} true on success; false leaves the caller's form open.
 */
async function createProject(values) {
  rememberModelId(values.model.id);
  try {
    const project = await post('/admin/api/projects', {
      // Name is optional: blank means the server mints an adjective-noun
      // identity (e.g. "Crimson Otter" / crimson-otter.localhost).
      name: values.name || null,
      domain: values.domain || null,
      description: values.description || null,
      llm_model_id: values.model.id,
      ambition_level: values.ambition,
      user_flows: values.userFlows,
    });
    toast.success(t('composer.created'));
    clearDraft();
    navigate(`/projects/${project.id}/chat`);
    return true;
  } catch (err) {
    toast.error(t('composer.createFailed', { error: humanizeError(err.message) }));
    return false;
  }
}

/**
 * Report whatever the composer is missing, and put the user where they can fix
 * it.
 * @param {string} problem - from composer.validate()
 * @param {object} composer
 */
function reportValidation(problem, composer) {
  if (problem === 'need-prompt') {
    toast.warning(t('composer.needPrompt'));
    composer.descInput.focus();
    return;
  }
  if (problem === 'need-model') {
    toast.warning(t('composer.needModel'));
    composer.openAdvanced();
  }
}

/**
 * Build the hero.
 * @returns {Promise<{element: HTMLElement, focus: Function}>}
 */
export async function createHomeComposer() {
  const composer = await createPromptComposer();

  const submitBtn = h('button', {
    className: 'btn btn--primary btn--lg home-hero__submit',
    onClick: async () => {
      const problem = composer.validate();
      if (problem) {
        reportValidation(problem, composer);
        return;
      }
      submitBtn.disabled = true;
      const ok = await createProject(composer.values());
      if (!ok) submitBtn.disabled = false;
    },
  }, [
    h('span', { innerHTML: icon('sparkle') }),
    t('composer.start'),
  ]);

  const savedDraft = readDraft();
  if (savedDraft && !composer.descInput.value) composer.descInput.value = savedDraft;
  composer.descInput.addEventListener('input', () => writeDraft(composer.descInput.value));

  // Ctrl/Cmd+Enter submits, matching the setup wizard. Plain Enter stays a
  // newline: prompts are usually more than one line.
  composer.descInput.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
      e.preventDefault();
      submitBtn.click();
    }
  });

  const element = h('div', { className: 'home-hero' }, [
    h('h1', { className: 'home-hero__title' }, pickPromptTitle()),
    composer.element,
    h('div', { className: 'home-hero__actions' }, [submitBtn]),
  ]);

  return {
    element,
    focus: () => composer.descInput.focus(),
  };
}
