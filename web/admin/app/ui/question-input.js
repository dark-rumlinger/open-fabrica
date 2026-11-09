/**
 * Shared question input builder.
 * Renders the appropriate input UI for a question (fields, multiple-choice,
 * single-choice, open text, secret) and returns getValue/hasValue closures.
 *
 * Used by: chat cards (cards.js), project questions, and global questions views.
 */

import { h } from '../core/dom.js';
import { t } from '../core/i18n.js';

/**
 * Parse a JSON-or-string field into an array. When commaFallback is true,
 * a non-JSON string is split on commas (used for option lists entered by
 * hand in the admin UI).
 */
function parseJsonArray(raw, commaFallback = false) {
  if (!raw) return [];
  try {
    const parsed = typeof raw === 'string' ? JSON.parse(raw) : raw;
    if (Array.isArray(parsed)) return parsed;
  } catch {
    if (commaFallback && typeof raw === 'string') {
      return raw.split(',').map(s => s.trim()).filter(Boolean);
    }
  }
  return [];
}

/**
 * True for a multi-select question. `multi_choice` is the value the PLAN schema
 * emits and the DB documents.
 */
export function isMultiChoiceType(type) {
  return type === 'multi_choice';
}

/**
 * Build the input UI for a question.
 *
 * @param {Object} questionData, { fields, options, type, secret_name }
 * @param {Object} [opts]
 * @param {Function} [opts.onInput], called whenever the user changes a value
 * @param {string} [opts.wrapClass], CSS class for the outer wrapper div
 * @returns {{ inputEl: HTMLElement, getValue: () => string, hasValue: () => boolean }}
 */
export function buildQuestionInput(questionData, { onInput, wrapClass } = {}) {
  const { fields, options, type, secret_name } = questionData;
  const isSecret = type === 'secret';

  const parsedFields = parseJsonArray(fields);
  const parsedOpts = parseJsonArray(options, true);

  const inputEl = h('div', wrapClass ? { className: wrapClass } : {});
  let getValue, hasValue;

  if (isSecret) {
    // Secret questions ALWAYS render a password input regardless of fields/options.
    // The submitted value is the actual secret the owner types, never an option label.
    if (secret_name) {
      inputEl.appendChild(h('p', { className: 'text-sm text-secondary', style: { opacity: 0.7, marginBottom: '4px' } },
        t('qi.encrypted', { name: secret_name })));
    }
    const input = h('input', {
      type: 'password',
      className: 'input input--sm',
      placeholder: t('qi.secretPlaceholder'),
      onInput: () => onInput?.(),
    });
    inputEl.appendChild(input);
    getValue = () => input.value.trim();
    hasValue = () => input.value.trim() !== '';
    return { inputEl, getValue, hasValue };
  }

  if (parsedFields.length > 0) {
    // Multi-field structured input
    const fieldInputs = {};
    for (const field of parsedFields) {
      const input = h('input', {
        type: field.type === 'secret' ? 'password' : 'text',
        className: 'input input--sm',
        placeholder: field.label || field.name,
        onInput: () => onInput?.(),
      });
      fieldInputs[field.name] = input;
      inputEl.appendChild(h('div', { className: 'chat-card__field-row' }, [
        h('label', { className: 'chat-card__field-label' }, field.label || field.name),
        input,
      ]));
    }
    getValue = () => {
      const values = {};
      let has = false;
      for (const field of parsedFields) {
        const val = fieldInputs[field.name].value.trim();
        if (val) has = true;
        values[field.name] = val;
      }
      return has ? JSON.stringify(values) : '';
    };
    hasValue = () => parsedFields.some(f => fieldInputs[f.name].value.trim());

  } else if (type === 'form') {
    // Explicitly a form, but its field definitions are missing or invalid JSON
    // (parsedFields is empty). Show an error and block submission rather than
    // falling through to a text box that would send a bare string the backend
    // can't read as field values. Gated to type==='form' so ordinary text/open
    // questions that happen to carry a stray `fields` value still render their
    // input normally.
    inputEl.appendChild(h('p', { className: 'text-sm', style: { color: 'var(--danger)' } },
      t('qi.formBroken')));
    getValue = () => '';
    hasValue = () => false;

  } else if (parsedOpts.length > 0 && isMultiChoiceType(type)) {
    // Multiple choice toggle buttons with "Other..." option
    inputEl.appendChild(h('p', { className: 'text-sm text-secondary', style: { margin: '0 0 8px' } }, t('qi.multiHint')));
    const selected = new Set();
    const optContainer = h('div', { className: 'chat-card__options' });

    for (const opt of parsedOpts) {
      const label = typeof opt === 'string' ? opt : opt.label || opt;
      const btn = h('button', {
        className: 'btn btn--sm btn--ghost chat-card__option-btn',
        onClick: () => {
          if (selected.has(label)) { selected.delete(label); btn.classList.remove('btn--active'); }
          else { selected.add(label); btn.classList.add('btn--active'); }
          onInput?.();
        },
      }, label);
      optContainer.appendChild(btn);
    }

    // "Other..." toggle button + text input
    const otherInput = h('input', {
      type: 'text',
      className: 'input input--sm',
      placeholder: t('qi.typeAnswer'),
      style: { display: 'none', marginTop: '8px' },
      onInput: () => onInput?.(),
    });
    const otherBtn = h('button', {
      className: 'btn btn--sm btn--ghost chat-card__option-btn',
      onClick: () => {
        if (selected.has('__other__')) {
          selected.delete('__other__');
          otherBtn.classList.remove('btn--active');
          otherInput.style.display = 'none';
          otherInput.value = '';
        } else {
          selected.add('__other__');
          otherBtn.classList.add('btn--active');
          otherInput.style.display = 'block';
          otherInput.focus();
        }
        onInput?.();
      },
    }, t('qi.other'));
    optContainer.appendChild(otherBtn);
    inputEl.appendChild(optContainer);
    inputEl.appendChild(otherInput);

    getValue = () => {
      const values = [...selected].filter(v => v !== '__other__');
      const otherVal = otherInput.value.trim();
      if (selected.has('__other__') && otherVal) values.push(otherVal);
      return values.length > 0 ? values.join(', ') : '';
    };
    hasValue = () => {
      const hasSelected = [...selected].some(v => v !== '__other__');
      const hasOther = selected.has('__other__') && otherInput.value.trim() !== '';
      return hasSelected || hasOther;
    };

  } else if (parsedOpts.length > 0) {
    // Single choice buttons with "Other..." option
    let selectedValue = '';
    let otherActive = false;
    const allBtns = [];
    const optContainer = h('div', { className: 'chat-card__options' });

    const otherInput = h('input', {
      type: 'text',
      className: 'input input--sm',
      placeholder: t('qi.typeAnswer'),
      style: { display: 'none', marginTop: '8px' },
      onInput: () => {
        if (otherActive) {
          selectedValue = otherInput.value.trim();
          onInput?.();
        }
      },
    });

    for (const opt of parsedOpts) {
      const label = typeof opt === 'string' ? opt : opt.label || opt;
      const btn = h('button', {
        className: 'btn btn--sm btn--ghost chat-card__option-btn',
        onClick: () => {
          allBtns.forEach(b => b.classList.remove('btn--active'));
          btn.classList.add('btn--active');
          selectedValue = label;
          otherActive = false;
          otherInput.style.display = 'none';
          otherInput.value = '';
          onInput?.();
        },
      }, label);
      allBtns.push(btn);
      optContainer.appendChild(btn);
    }

    // "Other..." button
    const otherBtn = h('button', {
      className: 'btn btn--sm btn--ghost chat-card__option-btn',
      onClick: () => {
        allBtns.forEach(b => b.classList.remove('btn--active'));
        otherBtn.classList.add('btn--active');
        otherActive = true;
        selectedValue = otherInput.value.trim();
        otherInput.style.display = 'block';
        otherInput.focus();
        onInput?.();
      },
    }, t('qi.other'));
    allBtns.push(otherBtn);
    optContainer.appendChild(otherBtn);
    inputEl.appendChild(optContainer);
    inputEl.appendChild(otherInput);

    getValue = () => selectedValue;
    hasValue = () => selectedValue !== '';

  } else {
    // Open text input (secrets handled at the top of the function).
    const input = h('input', {
      type: 'text',
      className: 'input input--sm',
      placeholder: t('qi.typeAnswer'),
      onInput: () => onInput?.(),
    });
    inputEl.appendChild(input);
    getValue = () => input.value.trim();
    hasValue = () => input.value.trim() !== '';
  }

  return { inputEl, getValue, hasValue };
}
