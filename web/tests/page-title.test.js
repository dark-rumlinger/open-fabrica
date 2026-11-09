/**
 * Tests for ui/page-title.js: the tab title each route gets.
 *
 * Run with `node --test "web/tests/*.test.js"` (or `make test-js`). See markdown.test.js for
 * why these live outside web/admin.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { titleForPath } from '../admin/app/ui/page-title.js';

const links = [
  { path: '/users', label: 'Users' },
  { path: '/providers/llm', label: 'LLM providers' },
  { section: 'System' },
];
const projects = [{ id: 7, name: 'Bakery' }];
const panelLabel = (id) => ({ chat: 'Chat', pages: 'Pages', 'settings-advanced': 'Advanced' }[id] || '');

test('the composer and the root are "New project"', () => {
  assert.equal(titleForPath('/new'), 'New project');
  assert.equal(titleForPath('/'), 'New project');
  assert.equal(titleForPath(''), 'New project');
});

test('global pages use their menu label', () => {
  assert.equal(titleForPath('/users', { links }), 'Users');
  assert.equal(titleForPath('/providers/llm', { links }), 'LLM providers');
});

test('a project route names the panel and the project', () => {
  assert.equal(titleForPath('/projects/7/pages', { projects, panelLabel }), 'Pages | Bakery');
  assert.equal(titleForPath('/projects/7/settings-advanced', { projects, panelLabel }), 'Advanced | Bakery');
});

test('a bare project route is the chat', () => {
  assert.equal(titleForPath('/projects/7', { projects, panelLabel }), 'Chat | Bakery');
});

test('an unknown project or panel degrades instead of throwing', () => {
  assert.equal(titleForPath('/projects/99/pages', { projects, panelLabel }), 'Pages | Project');
  assert.equal(titleForPath('/projects/7/mystery', { projects, panelLabel }), 'Bakery');
});

test('an unknown path adds nothing', () => {
  assert.equal(titleForPath('/nope', { links }), '');
});
