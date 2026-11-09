/**
 * Password rules, shared by every form that sets one (add/edit user, change
 * password, first-run setup). They mirror the server's validatePasswordComplexity
 * so the problem shows before the request instead of as a toast after it.
 *
 * The rules are about the kinds of characters, not about the Latin alphabet:
 *   - length counts characters, not UTF-16 units or bytes
 *   - "uppercase" is any capital letter, or a letter from a script with no case
 *     at all (Japanese, Chinese, Arabic, Hindi), so a person whose keyboard has
 *     no Latin capitals is not forced to switch input method to sign up
 *   - "number" is any decimal digit, including Arabic-Indic and Devanagari ones
 *   - "special" is anything that is not an ASCII letter or digit
 */

import { t } from './i18n.js';

export const PASSWORD_HINT = t('password.hint');

export const MIN_PASSWORD_LENGTH = 8;

/** A letter that is a capital, or belongs to a script that has no case. */
function isUpperish(ch) {
  return /\p{L}/u.test(ch) && (ch !== ch.toLowerCase() || ch === ch.toUpperCase());
}

export const hasMinLength = (p) => [...p].length >= MIN_PASSWORD_LENGTH;
export const hasUpper = (p) => [...p].some(isUpperish);
export const hasNumber = (p) => /\p{Nd}/u.test(p);
export const hasSpecial = (p) => /[^A-Za-z0-9]/.test(p);

/** First problem as a sentence, or null when the password is acceptable. */
export function passwordProblem(password) {
  if (!hasMinLength(password)) return t('password.tooShort');
  if (!hasUpper(password)) return t('password.needUpper');
  if (!hasNumber(password)) return t('password.needNumber');
  if (!hasSpecial(password)) return t('password.needSpecial');
  return null;
}
