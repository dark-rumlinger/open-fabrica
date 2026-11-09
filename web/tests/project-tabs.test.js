/**
 * Tests for the project tab map.
 *
 * ui/project-tabs.js lists the panels a user can reach; views/project/index.js
 * maps panel ids to renderers. Nothing in the code ties them together, and both
 * failure modes are silent: a tab entry with no renderer falls through
 * `panels[panel] || panels.pages` and quietly shows Pages instead, and a
 * renderer with no tab entry is a working screen nobody can navigate to.
 *
 * Run with `node --test "web/tests/*.test.js"` (or `make test-js`). See markdown.test.js for
 * why these live outside web/admin.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const { GROUPS, groupForPanel, splitPanelId } = await import('../admin/app/ui/project-tabs.js');

/**
 * The panels the tab bar needs a renderer for, minus the chat (index.js
 * handles that apart). Section ids like settings-advanced collapse to the
 * panel that renders them, which is what the route map is keyed by.
 */
function tabPanelIds() {
  const bases = GROUPS.flatMap(g => g.panels.map(p => splitPanelId(p.id).panel));
  return [...new Set(bases)].filter(id => id !== 'chat').sort();
}

/** Panel ids index.js can actually render, read out of its `panels` map. */
function routedPanelIds() {
  const src = readFileSync(new URL('../admin/app/views/project/index.js', import.meta.url), 'utf8');
  const start = src.indexOf('const panels = {');
  assert.notEqual(start, -1, 'index.js no longer has a `panels` map; this test needs updating');
  const block = src.slice(start, src.indexOf('};', start));
  // Key followed by an arrow function, whatever arguments it happens to take.
  return [...block.matchAll(/^\s+([a-zA-Z]+): \(/gm)].map(m => m[1]).sort();
}

test('every tab entry has a renderer behind it', () => {
  const routed = routedPanelIds();
  const orphans = tabPanelIds().filter(id => !routed.includes(id));
  assert.deepEqual(orphans, [], 'these tabs would silently render Pages instead');
});

test('every renderer is reachable from a tab', () => {
  const tabs = tabPanelIds();
  const unreachable = routedPanelIds().filter(id => !tabs.includes(id));
  assert.deepEqual(unreachable, [], 'these screens work but nothing navigates to them');
});

test('each tab lands on a panel of its own group', () => {
  for (const group of GROUPS) {
    assert.ok(group.panels.length > 0, `${group.id} has no panels to land on`);
    const landing = group.panels[0].id;
    assert.equal(groupForPanel(landing).id, group.id,
      `${group.id} lands on ${landing}, which belongs to another tab`);
  }
});

test('panel ids are unique across tabs', () => {
  const all = GROUPS.flatMap(g => g.panels.map(p => p.id));
  assert.equal(new Set(all).size, all.length, 'a panel in two tabs makes the active tab ambiguous');
});

test('an unknown panel still resolves to a real group', () => {
  // renderContextPanel falls back rather than blowing up, so the bar has to as
  // well - otherwise a bad URL renders a panel with no navigation above it.
  const group = groupForPanel('not-a-panel');
  assert.ok(group && Array.isArray(group.panels) && group.panels.length > 0);
});

test('splitPanelId separates a section from its panel', () => {
  assert.deepEqual(splitPanelId('pages'), { panel: 'pages', section: null });
  assert.deepEqual(splitPanelId('settings-advanced'), { panel: 'settings', section: 'advanced' });
  // Only the first dash separates, so a multi-word section stays whole.
  assert.deepEqual(splitPanelId('tasks-background-jobs'), { panel: 'tasks', section: 'background-jobs' });
  assert.deepEqual(splitPanelId(''), { panel: '', section: null });
  assert.deepEqual(splitPanelId(undefined), { panel: undefined, section: null });
});

test('no base panel id contains a dash', () => {
  // splitPanelId takes the first dash as the separator, so a hyphenated panel
  // name would silently route to a panel that does not exist.
  const bases = GROUPS.flatMap(g => g.panels.map(p => splitPanelId(p.id).panel));
  assert.deepEqual(bases.filter(b => b.includes('-')), []);
});
