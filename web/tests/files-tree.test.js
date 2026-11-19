import test from 'node:test';
import assert from 'node:assert/strict';

import {
  normalizeFilePath,
  joinFolderPath,
  parentFolder,
  listFolderEntries,
} from '../admin/app/views/project/files-tree.js';

test('normalizeFilePath strips leading/trailing slashes and normalizes separators', () => {
  assert.equal(normalizeFilePath('/assets/css/app.css'), 'assets/css/app.css');
  assert.equal(normalizeFilePath('assets\\js\\main.js'), 'assets/js/main.js');
  assert.equal(normalizeFilePath('///a//b/'), 'a/b');
  assert.equal(normalizeFilePath(''), '');
});

test('joinFolderPath joins current folder with filename unless root-prefixed', () => {
  assert.equal(joinFolderPath('', 'app.js'), 'app.js');
  assert.equal(joinFolderPath('assets/css', 'app.css'), 'assets/css/app.css');
  assert.equal(joinFolderPath('assets/css', 'sub/theme.css'), 'assets/css/sub/theme.css');
  assert.equal(joinFolderPath('assets/css', '/root.txt'), 'root.txt');
});

test('parentFolder steps up one directory level', () => {
  assert.equal(parentFolder('assets/css/themes'), 'assets/css');
  assert.equal(parentFolder('assets'), '');
  assert.equal(parentFolder(''), '');
});

test('listFolderEntries groups flat files into folders and direct files', () => {
  const files = [
    { id: 1, filename: 'index.js' },
    { id: 2, filename: '/assets/css/app.css' },
    { id: 3, filename: 'assets/css/theme.css' },
    { id: 4, filename: 'assets/logo.png' },
    { id: 5, filename: 'of_test.js', kind: 'test' },
  ];

  const root = listFolderEntries(files, '');
  assert.deepEqual(root.folders, [
    { name: 'assets', path: 'assets', count: 3 },
  ]);
  assert.deepEqual(
    root.files.map((f) => f.displayName),
    ['index.js', 'of_test.js'],
  );

  const assets = listFolderEntries(files, 'assets');
  assert.deepEqual(assets.folders, [
    { name: 'css', path: 'assets/css', count: 2 },
  ]);
  assert.deepEqual(
    assets.files.map((f) => f.displayName),
    ['logo.png'],
  );

  const css = listFolderEntries(files, 'assets/css');
  assert.deepEqual(css.folders, []);
  assert.deepEqual(
    css.files.map((f) => f.displayName),
    ['app.css', 'theme.css'],
  );
});

test('listFolderEntries includes empty virtual folders without duplicating real ones', () => {
  const files = [{ id: 1, filename: 'assets/logo.png' }];
  const extra = new Set(['assets/icons', 'docs', 'assets']);

  const root = listFolderEntries(files, '', extra);
  assert.deepEqual(root.folders, [
    { name: 'assets', path: 'assets', count: 1 },
    { name: 'docs', path: 'docs', count: 0 },
  ]);

  const assets = listFolderEntries(files, 'assets', extra);
  assert.deepEqual(assets.folders, [
    { name: 'icons', path: 'assets/icons', count: 0 },
  ]);
});
