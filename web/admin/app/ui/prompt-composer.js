/**
 * The "what are you building" field stack behind the home screen's composer.
 *
 * It owns the fields only, never the chrome or the submit: home/composer.js
 * brings the button and decides what to do with values(). It was shared with a
 * new-project modal and a setup-wizard step before both were folded into this
 * one screen, which is why the seam is still here - the split keeps the field
 * behaviour (and slugifyDomain) testable without a submit path.
 */

import { h } from '../core/dom.js';
import { t } from '../core/i18n.js';
import { get } from '../core/http.js';
import * as state from '../core/state.js';
import { formGroup, segmented } from './helpers.js';
import { createModelPicker } from './model-picker.js';
import { promptChips } from './project-examples.js';

const AMBITION_LEVELS = ['off', 'low', 'medium', 'high'];
const AMBITION_KEY = 'of:ambitionLevel';
const USER_FLOWS_KEY = 'of:userFlows';
const MODEL_KEY = 'of:lastModelId';
const MAX_SLUG_LEN = 30;
const DETAIL_LABELS = {
  off: t('composer.detailOff'),
  low: t('composer.detailLow'),
  medium: t('composer.detailMedium'),
  high: t('composer.detailHigh'),
};

/**
 * Derive a *.localhost domain from a project name.
 * Takes up to 3 leading tokens and joins with hyphens, then caps length at the
 * last full token that still fits inside MAX_SLUG_LEN. Avoids run-on slugs like
 * "amodernlandingpageformysaas" from long prompts.
 * @param {string} str
 * @returns {string} the domain, or '' when there is nothing to slugify.
 */
export function slugifyDomain(str) {
  const tokens = String(str || '').toLowerCase().split(/[^a-z0-9]+/).filter(Boolean);
  if (tokens.length === 0) return '';
  let slug = tokens.slice(0, 3).join('-');
  if (slug.length > MAX_SLUG_LEN) {
    slug = slug.slice(0, MAX_SLUG_LEN).replace(/-?[^-]*$/, '') || slug.slice(0, MAX_SLUG_LEN);
  }
  return slug + '.localhost';
}

// Greeting variants. {,name} is replaced with ", <first name>" when available,
// otherwise stripped so the prompt still reads naturally for anonymous or
// display-name-less users.
const TITLE_VARIANTS = [
  t('composer.title1'),
  t('composer.title2'),
  t('composer.title3'),
  t('composer.title4'),
  t('composer.title5'),
  t('composer.title6'),
  t('composer.title7'),
  t('composer.title8'),
  t('composer.title9'),
  t('composer.title10'),
];

/** Pick a randomized greeting for whichever surface is asking. */
export function pickPromptTitle() {
  const template = TITLE_VARIANTS[Math.floor(Math.random() * TITLE_VARIANTS.length)];
  const name = state.displayName();
  return template.replace('{,name}', name ? t('composer.nameSep') + name : '');
}

function readStoredAmbition() {
  try {
    const raw = localStorage.getItem(AMBITION_KEY);
    if (AMBITION_LEVELS.includes(raw)) return raw;
  } catch { /* localStorage may be unavailable */ }
  return 'high';
}

/** A titled group of fields inside the open Advanced fold. */
function advancedSection(title, children) {
  return h('div', { className: 'adv-section' }, [
    h('p', { className: 'adv-section__title' }, title),
    ...children,
  ]);
}

/**
 * A setting row: title and one-line description on the left, the control on the
 * right. `tag` 'label' makes the whole row toggle its checkbox. `help` is the
 * longer explanation, shown as a tooltip.
 */
function advancedRow(title, description, control, help, tag = 'div') {
  return h(tag, { className: 'adv-row' + (tag === 'label' ? ' adv-row--click' : ''), title: help }, [
    h('div', { className: 'adv-row__text' }, [
      h('div', { className: 'adv-row__title' }, title),
      h('div', { className: 'adv-row__desc' }, description),
    ]),
    control,
  ]);
}

/** The last user-flow choice ('off' or 'on'); off is the default (experimental). */
function readStoredUserFlows() {
  try {
    return localStorage.getItem(USER_FLOWS_KEY) === 'on';
  } catch { /* localStorage may be unavailable */ }
  return false;
}

function readLastModelId() {
  try {
    const raw = localStorage.getItem(MODEL_KEY);
    if (raw) return parseInt(raw, 10) || null;
  } catch { /* localStorage may be unavailable */ }
  return null;
}

/** Remember the model the user just built with, so repeat runs don't re-pick. */
export function rememberModelId(id) {
  try { localStorage.setItem(MODEL_KEY, String(id)); } catch { /* ignore */ }
}

/**
 * Build the composer.
 * Async because the model picker needs the provider catalog.
 * @returns {Promise<object>} the composer controller.
 */
export async function createPromptComposer() {
  const descInput = h('textarea', {
    className: 'input prompt-input',
    placeholder: t('composer.placeholder'),
    rows: '5',
    'aria-label': t('composer.label'),
  });

  // Example chips, clicking fills the prompt to nudge first-time users.
  const chips = promptChips(descInput);

  const nameInput = h('input', {
    className: 'input',
    placeholder: t('composer.namePlaceholder'),
  });
  const domainInput = h('input', {
    className: 'input',
    placeholder: t('projects.domainPlaceholder'),
    pattern: '[a-zA-Z0-9][a-zA-Z0-9.\\-]*',
  });

  // Auto-populate domain from project name, until the user touches the domain
  // themselves.
  let domainManuallyEdited = false;
  domainInput.addEventListener('input', () => { domainManuallyEdited = true; });
  nameInput.addEventListener('input', () => {
    if (domainManuallyEdited) return;
    domainInput.value = slugifyDomain(nameInput.value);
  });

  // Ambition level: default High, persisted so the user's last choice sticks
  // between visits.
  const ambitionControl = segmented([
    { value: 'off', label: t('composer.off') },
    { value: 'low', label: t('composer.low') },
    { value: 'medium', label: t('composer.medium') },
    { value: 'high', label: t('composer.high') },
  ], readStoredAmbition(), (val) => {
    try { localStorage.setItem(AMBITION_KEY, val); } catch { /* ignore */ }
    updateSummary();
  });
  ambitionControl.setAttribute('aria-label', t('composer.detailLevel'));

  // User-flow tests: experimental and off by default; the last choice sticks between
  // visits like the detail level. They replay what a real person does with the finished
  // project and cost extra build time and tokens, so an owner opts in.
  const userFlowsCheck = h('input', { type: 'checkbox', checked: readStoredUserFlows() });
  userFlowsCheck.setAttribute('aria-label', t('composer.flowsLabel'));
  userFlowsCheck.addEventListener('change', () => {
    try { localStorage.setItem(USER_FLOWS_KEY, userFlowsCheck.checked ? 'on' : 'off'); } catch { /* ignore */ }
    updateSummary();
  });

  // Provider/model picker. Catalog failures degrade to an empty picker rather
  // than taking the whole composer down.
  let providers = [];
  try {
    providers = (await get('/admin/api/providers/catalog').catch(() => [])) || [];
  } catch { /* graceful degradation */ }
  const lastModelId = readLastModelId();
  const picker = createModelPicker(providers, lastModelId ? { preferredModelId: lastModelId } : {});

  const advancedBody = h('div', { className: 'advanced-fields' }, [
    advancedSection(t('composer.sectionProject'), [
      h('div', { className: 'adv-grid' }, [
        formGroup(t('projects.col.name'), nameInput),
        formGroup(t('projects.col.domain'), domainInput),
      ]),
      h('p', { className: 'form-hint' }, t('composer.localhostNote')),
    ]),
    advancedSection(t('composer.sectionBuild'), [
      advancedRow(
        t('composer.detailLevelTitle'),
        t('composer.detailDesc'),
        ambitionControl,
        t('composer.detailHelp'),
      ),
    ]),
    advancedSection(t('composer.sectionTesting'), [
      advancedRow(
        [
          t('composer.flowsTitle'),
          h('span', { className: 'badge badge--warning', style: 'margin-inline-start: 8px;' }, t('composer.experimental')),
        ],
        t('composer.flowsDesc'),
        h('span', { className: 'toggle' }, [userFlowsCheck]),
        t('composer.flowsHelp'),
        'label',
      ),
    ]),
    advancedSection(t('composer.sectionModel'), [
      h('div', { className: 'adv-grid' }, [
        formGroup(t('composer.aiProvider'), picker.providerSelect, { required: true }),
        formGroup(t('composer.aiModel'), picker.modelSelect, { required: true }),
      ]),
    ]),
  ]);

  const advancedCaret = h('span', { className: 'advanced-toggle__caret' }, '▸');
  const advancedSummary = h('span', { className: 'advanced-toggle__summary' });
  const advancedToggle = h('button', {
    type: 'button',
    className: 'advanced-toggle',
    'aria-expanded': 'false',
  }, [t('composer.advanced') + ' ', advancedCaret, advancedSummary]);

  const advancedWrap = h('div', {
    className: 'advanced-wrap advanced-wrap--collapsed',
  }, [advancedBody]);

  // While the fold is closed, say what it currently holds.
  let advancedOpen = false;
  function updateSummary() {
    const tests = userFlowsCheck.checked ? t('composer.testsOn') : t('composer.testsOff');
    advancedSummary.textContent = advancedOpen ? '' : `${DETAIL_LABELS[ambitionControl._value] || ''} · ${tests}`;
  }

  function setAdvanced(open) {
    advancedOpen = open;
    advancedWrap.classList.toggle('advanced-wrap--collapsed', !open);
    advancedToggle.setAttribute('aria-expanded', String(open));
    advancedCaret.textContent = open ? '▾' : '▸';
    updateSummary();
  }
  updateSummary();
  advancedToggle.addEventListener('click', () => {
    setAdvanced(!advancedOpen);
  });

  const element = h('div', { className: 'prompt-composer' }, [
    h('div', { className: 'form-group prompt-group' }, [
      descInput,
      chips,
    ]),
    advancedToggle,
    advancedWrap,
  ]);

  return {
    element,
    descInput,
    nameInput,
    domainInput,
    userFlowsCheck,

    /** Current field values, trimmed. `model` is null until one is picked. */
    values() {
      return {
        description: descInput.value.trim(),
        name: nameInput.value.trim(),
        domain: domainInput.value.trim(),
        ambition: ambitionControl._value,
        userFlows: userFlowsCheck.checked,
        model: picker.getSelectedModel(),
      };
    },

    /**
     * @returns {null|'need-prompt'|'need-model'} the first unmet requirement.
     * Name is optional: blank means the server mints an adjective-noun identity
     * and a matching domain. We still need something to build, so require
     * either a prompt or an explicit name.
     */
    validate() {
      const { description, name, model } = this.values();
      if (!name && !description) return 'need-prompt';
      if (!model) return 'need-model';
      return null;
    },

    openAdvanced() { setAdvanced(true); },
  };
}
