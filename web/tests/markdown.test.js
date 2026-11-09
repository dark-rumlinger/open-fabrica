/**
 * Tests for the admin markdown renderer, which turns model-authored text into
 * HTML for the chat feed - the one place untrusted LLM output becomes markup.
 *
 * Run with `node --test "web/tests/*.test.js"` (or `make test-js`). No test framework and no
 * dependencies: node:test and node:assert are built in.
 *
 * These live here rather than next to the source because embed.go embeds
 * `all:web/admin`, so a *.test.js under that path would be compiled into the
 * shipped binary and served by the admin panel.
 *
 * What they pin is the safety invariant documented in markdown.js: input is
 * escaped FIRST, so attribute breakout is structurally impossible, and every URL
 * that reaches an href/src has passed a scheme allowlist. Both properties are
 * load-bearing and neither is visible from the rendered output, so they are
 * asserted rather than assumed.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { render } from '../admin/app/core/markdown.js';

/**
 * True when out contains a LIVE event-handler attribute on a real tag.
 *
 * Substring-matching "onerror=" is not good enough: escaped text legitimately
 * contains it (`&quot;onload=&quot;` inside a <code> block is inert content, not
 * an attribute). The breakout that actually matters is the handler becoming part
 * of a tag, which is what this matches.
 */
function hasLiveHandler(out) {
  return /<[a-z][^>]*\son[a-z]+\s*=/i.test(out);
}

// --- harness self-check ----------------------------------------------------

test('hasLiveHandler detects a real handler (guard is not vacuous)', () => {
  // Several tests below prove a payload is safe by asserting !hasLiveHandler(out).
  // If the detector matched nothing they would all pass while proving nothing,
  // so pin that it fires on genuine markup and stays quiet on inert text.
  assert.ok(hasLiveHandler('<img src=x onerror=alert(1)>'), 'should match a bare handler');
  assert.ok(hasLiveHandler('<a href="/" onmouseover="x">t</a>'), 'should match a quoted handler');
  assert.ok(!hasLiveHandler('<code>&quot;onload=&quot;alert(1)</code>'), 'escaped text is not a handler');
  assert.ok(!hasLiveHandler('<p>onload= is just words</p>'), 'prose is not a handler');
});

// --- the escape-first invariant -------------------------------------------

test('escapes raw HTML so a script tag cannot survive', () => {
  const out = render('<script>alert(1)</script>');
  assert.ok(!out.includes('<script>'), `raw <script> survived: ${out}`);
  assert.ok(out.includes('&lt;script&gt;'));
});

test('escapes an img onerror payload', () => {
  const out = render('<img src=x onerror=alert(1)>');
  assert.ok(!hasLiveHandler(out), `live onerror survived: ${out}`);
});

test('a quote in a link url cannot break out of the href attribute', () => {
  // The " is already &quot; by the time the link regex builds href="...", so the
  // attribute cannot be closed and a handler appended.
  const out = render('[hi](/a"onmouseover="alert(1))');
  assert.ok(!hasLiveHandler(out), `attribute breakout: ${out}`);
});

test('a quote in an image url cannot break out of the src attribute', () => {
  const out = render('![alt](/a"onerror="alert(1))');
  assert.ok(!hasLiveHandler(out), `attribute breakout: ${out}`);
});

// --- link scheme allowlist -------------------------------------------------

test('rejects javascript: links, keeping the text', () => {
  const out = render('[click](javascript:alert(1))');
  assert.ok(!out.includes('href'), `javascript: url reached an href: ${out}`);
  assert.ok(out.includes('click'), 'link text should survive as plain text');
});

test('rejects vbscript: links', () => {
  const out = render('[click](vbscript:msgbox(1))');
  assert.ok(!out.includes('href'), `vbscript: url reached an href: ${out}`);
});

test('rejects protocol-relative links', () => {
  // //evil.com would inherit the page scheme and leave the origin.
  const out = render('[click](//evil.com)');
  assert.ok(!out.includes('href'), `protocol-relative url reached an href: ${out}`);
});

test('rejects data: links', () => {
  const out = render('[click](data:text/html,<script>alert(1)</script>)');
  assert.ok(!out.includes('href'), `data: url reached an href: ${out}`);
});

test('allows http, https, absolute-path and anchor links', () => {
  for (const url of ['https://example.com', 'http://example.com', '/projects/1', '#section']) {
    const out = render(`[go](${url})`);
    assert.ok(out.includes(`href="${url}"`), `legitimate url was rejected: ${url} -> ${out}`);
  }
});

test('external links carry noopener noreferrer', () => {
  const out = render('[go](https://example.com)');
  assert.ok(out.includes('rel="noopener noreferrer"'), out);
});

// --- image scheme allowlist ------------------------------------------------

test('rejects javascript: image urls, keeping the alt text', () => {
  const out = render('![alt](javascript:alert(1))');
  assert.ok(!out.includes('<img'), `javascript: url reached an img src: ${out}`);
  assert.ok(out.includes('alt'));
});

test('rejects protocol-relative image urls', () => {
  const out = render('![alt](//evil.com/x.png)');
  assert.ok(!out.includes('<img'), `protocol-relative url reached an img src: ${out}`);
});

test('rejects a non-image data: url in an image', () => {
  const out = render('![alt](data:text/html,<script>alert(1)</script>)');
  assert.ok(!out.includes('<img'), `data:text/html reached an img src: ${out}`);
});

test('allows data:image/svg+xml only into img src', () => {
  // Permitted deliberately: script inside an SVG does not execute when the SVG
  // is loaded through <img src>. This pins that it stays confined to <img>.
  const out = render('![alt](data:image/svg+xml;base64,PHN2Zz48L3N2Zz4=)');
  assert.ok(out.includes('<img src="data:image/svg+xml;base64,'), out);
  assert.ok(!out.includes('<a href="data:'), `svg data url reached an href: ${out}`);
});

test('allows http(s) and absolute-path images', () => {
  for (const url of ['https://example.com/a.png', '/uploads/a.png']) {
    const out = render(`![alt](${url})`);
    assert.ok(out.includes(`<img src="${url}"`), `legitimate image was rejected: ${url} -> ${out}`);
  }
});

test('image rule runs before the link rule', () => {
  // The link pattern also matches ![...](...) because its [ is not anchored;
  // if the order ever flips, an image renders as a link.
  const out = render('![alt](https://example.com/a.png)');
  assert.ok(out.includes('<img'), `image was consumed by the link rule: ${out}`);
  assert.ok(!out.includes('<a href'), out);
});

// --- code fence language class --------------------------------------------

test('code fence language is a \\w* capture and cannot inject an attribute', () => {
  const out = render('```js\nconst a = 1;\n```');
  assert.ok(out.includes('class="lang-js"'), out);
});

test('a non-word fence language cannot break the class attribute', () => {
  // \w* does not match the quote, so the fence simply does not match and the
  // text stays inert. The payload appears in the output as escaped content -
  // that is fine; what must not happen is it becoming an attribute.
  const out = render('```js"onload="alert(1)\ncode\n```');
  assert.ok(!hasLiveHandler(out), `fence language broke out of the class attribute: ${out}`);
  assert.ok(!out.includes('class="lang-js"onload'), out);
});

// --- ordinary rendering still works ---------------------------------------

test('renders basic markdown', () => {
  assert.ok(render('# Title').includes('<h1>Title</h1>'));
  assert.ok(render('**bold**').includes('<strong>bold</strong>'));
  assert.ok(render('*it*').includes('<em>it</em>'));
  assert.ok(render('`code`').includes('<code>code</code>'));
});

test('empty input renders empty', () => {
  assert.equal(render(''), '');
  assert.equal(render(null), '');
  assert.equal(render(undefined), '');
});

test('fenced code is left exactly as written', () => {
  const out = render('Run this:\n\n```python\n# a comment\nprint(**kwargs)\n- not a list\n[x](javascript:alert(1))\n```\n\nDone.');
  assert.ok(out.includes('# a comment'), 'a # line inside code must not become a heading');
  assert.ok(out.includes('print(**kwargs)'), 'bold markers inside code must survive');
  assert.ok(out.includes('- not a list'), 'a dash line inside code must not become a list item');
  assert.ok(!out.includes('<h1>'), out);
  assert.ok(!out.includes('<strong>'), out);
  assert.ok(!/<li>/.test(out), out);
  assert.ok(!/<a href/.test(out), 'a link inside code must not render');
  assert.ok(out.includes('<pre><code class="lang-python">'), out);
});

test('inline code is left exactly as written', () => {
  const out = render('Use `**bold**` and `# not a header` here, **really bold**.');
  assert.ok(out.includes('<code>**bold**</code>'), out);
  assert.ok(out.includes('<code># not a header</code>'), out);
  assert.ok(out.includes('<strong>really bold</strong>'), out);
});

test('input cannot forge a code placeholder', () => {
  const out = render('hello \u0000B0\u0000 world');
  assert.ok(!out.includes('\u0000'), 'NUL characters are dropped');
  assert.ok(!out.includes('undefined'), out);
});

test('a backslash after the leading slash is not a same-origin path', () => {
  // Browsers normalise "/\evil.com" to "//evil.com", a protocol-relative host.
  const link = render('[go](/\\evil.com)');
  assert.ok(!link.includes('<a href'), `backslash path reached an href: ${link}`);
  const img = render('![alt](/\\evil.com/x.png)');
  assert.ok(!img.includes('<img'), `backslash path reached an img src: ${img}`);
});
