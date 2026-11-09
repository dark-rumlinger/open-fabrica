/**
 * Tests for the code editor's pure highlighting logic: language selection, the
 * tokenizer, and the HTML it produces. The critical property is that the
 * highlight layer only ever contains escaped text, so page source shown in the
 * editor can never execute. The DOM-driven editor instance is covered by the
 * module-load smoke test and in the browser.
 *
 * Run with `node --test "web/tests/*.test.js"` (or `make test-js`).
 */

import test from 'node:test';
import assert from 'node:assert/strict';

const { tokenize, highlightHTML, rulesFor } = await import('../admin/app/ui/code-editor.js');

test('rulesFor maps extensions to a rule set, or null for plain text', () => {
  assert.ok(rulesFor('app.js'));
  assert.ok(rulesFor('styles.css'));
  assert.ok(rulesFor('page.html'));
  assert.equal(rulesFor('notes.txt'), null);
  assert.equal(rulesFor('noext'), null);
});

test('tokenize classifies JS comments, strings, numbers and keywords', () => {
  const rules = rulesFor('x.js');
  const toks = tokenize('const x = 42; // hi', rules);
  const byClass = (cls) => toks.filter((t) => t.cls === cls).map((t) => t.text);
  assert.deepEqual(byClass('keyword'), ['const']);
  assert.deepEqual(byClass('number'), ['42']);
  assert.deepEqual(byClass('comment'), ['// hi']);
});

test('tokenize reassembles the exact source', () => {
  const src = 'function f(a) {\n  return a + 1;\n}\n';
  const joined = tokenize(src, rulesFor('f.js')).map((t) => t.text).join('');
  assert.equal(joined, src);
});

test('highlightHTML escapes everything, so markup cannot execute', () => {
  const html = highlightHTML('<script>alert(1)</script>', rulesFor('page.html'));
  assert.ok(!html.includes('<script>'), 'raw <script> must not appear');
  assert.ok(html.includes('&lt;script&gt;') || html.includes('&lt;script'), 'angle brackets are escaped');
});

test('highlightHTML with no rules returns escaped plain text', () => {
  assert.equal(highlightHTML('a < b && c > d', null), 'a &lt; b &amp;&amp; c &gt; d');
});

test('tokenize does not loop on a zero-width-capable rule set', () => {
  // Numbers can match empty-ish at boundaries; ensure termination and coverage.
  const src = 'a1 b2 c3';
  const joined = tokenize(src, rulesFor('x.js')).map((t) => t.text).join('');
  assert.equal(joined, src);
});
