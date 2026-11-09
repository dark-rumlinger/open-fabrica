/**
 * Small accessibility helpers shared by hand-built widgets.
 */

/**
 * Make a non-button element behave as a disclosure toggle: focusable, announced
 * as a button with an expanded state, activated by click, Enter or Space.
 * `toggle` flips the visual state and returns whether the section is now open.
 * Returns `sync(open)` for the rare caller that changes the state elsewhere.
 *
 * Several chat cards used a bare click listener on a div, which a keyboard or
 * screen-reader user could neither reach nor operate.
 * @param {HTMLElement} header
 * @param {() => boolean} toggle
 * @param {boolean} [initialOpen]
 */
export function disclosure(header, toggle, initialOpen = false) {
  const sync = (open) => header.setAttribute('aria-expanded', String(!!open));
  header.setAttribute('role', 'button');
  header.setAttribute('tabindex', '0');
  sync(initialOpen);

  header.addEventListener('click', () => sync(toggle()));
  header.addEventListener('keydown', (e) => {
    // Only the header itself: Enter or Space on a button inside it must keep its own meaning.
    if (e.target !== header) return;
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      header.click();
    }
  });
  return sync;
}

/**
 * True while an input method editor is composing text (Japanese, Chinese, Hindi
 * and other keyboards that build a character from several keystrokes). Enter
 * then confirms a candidate, so it must not also submit a form or send a message.
 * Safari reports the committing Enter with `isComposing` already false and
 * keyCode 229, hence the second test.
 * @param {KeyboardEvent} e
 */
export function isComposing(e) {
  return !!e.isComposing || e.keyCode === 229;
}

/**
 * An Enter keypress that means "submit this": not Shift+Enter, and not the
 * Enter that confirms an IME candidate.
 * @param {KeyboardEvent} e
 */
export function isSubmitEnter(e) {
  return e.key === 'Enter' && !e.shiftKey && !isComposing(e);
}
