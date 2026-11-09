/**
 * Password rules, with the same vectors as server/admin/password_rules_test.go.
 * The two implementations must agree, and neither may assume the Latin alphabet.
 *
 * Run with `node --test "web/tests/*.test.js"` (or `make test-js`).
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { passwordProblem, hasMinLength, hasUpper, hasNumber, hasSpecial } from '../admin/app/core/password.js';

const vectors = [
  ['Abcdefg1!', true],
  ['Éclair12', true],
  ['Пароль12!', true],
  ['パスワードです1!', true],
  ['كلمةالسر١٢٣!', true],
  ['पासवर्ड१२३४!', true],
  ['あいうえおか1!', true],
  ['abcdefg1!', false],
  ['Abcdefg!!', false],
  ['Abcdefg12', false],
  ['Ab1!', false],
  ['あいうえおか1', false],
  ['пароль12!', false],
  ['ПАРОЛЬ', false],
  ['1234567890!', false],
];

test('the rules judge every vector the way the server does', () => {
  for (const [password, ok] of vectors) {
    assert.equal(passwordProblem(password) === null, ok, password);
  }
});

test('length counts characters, including ones outside the basic plane', () => {
  assert.equal(hasMinLength('😀😀😀😀😀😀😀😀'), true);
  assert.equal(hasMinLength('😀😀😀😀😀😀😀'), false);
});

test('a caseless script satisfies the capital rule; lowercase does not', () => {
  assert.equal(hasUpper('日本語'), true);
  assert.equal(hasUpper('Привет'), true);
  assert.equal(hasUpper('привет'), false);
  assert.equal(hasUpper('straße'), false);
  assert.equal(hasUpper('123!'), false);
});

test('any decimal digit counts as a number', () => {
  assert.equal(hasNumber('٣'), true);
  assert.equal(hasNumber('४'), true);
  assert.equal(hasNumber('x'), false);
});

test('special means anything but an ASCII letter or digit', () => {
  assert.equal(hasSpecial('abc123'), false);
  assert.equal(hasSpecial('abc 123'), true);
  assert.equal(hasSpecial('ab_1'), true);
});
