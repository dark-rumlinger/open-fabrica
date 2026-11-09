/**
 * Static check: a file that calls a helper exported by another admin module, or
 * uses a shared namespace (toast, modal, state, router), must import it.
 *
 * A removed or forgotten import is invisible to `node --check` and to the
 * module-load smoke test: the reference only fails when the code path runs, so a
 * rarely used error branch ships broken. There is no linter in this repo (no
 * dependencies, nothing to install), so this scans the sources directly. It is
 * deliberately conservative: it only flags calls to names that some app module
 * exports, written as a plain call (not a method call or object key).
 *
 * Run with `node --test "web/tests/*.test.js"` (or `make test-js`). See markdown.test.js for
 * why these live outside web/admin.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';
import { fileURLToPath } from 'node:url';

const appDir = fileURLToPath(new URL('../admin/app/', import.meta.url));

function walk(dir) {
  return readdirSync(dir).flatMap((name) => {
    const full = join(dir, name);
    return statSync(full).isDirectory() ? walk(full) : full.endsWith('.js') ? [full] : [];
  });
}

// Source with comments and string/template contents blanked, so a word inside a
// sentence or a regex is not mistaken for code. Template `${...}` bodies are
// code but rare enough to ignore here.
function code(src) {
  return src
    .replace(/\/\*[\s\S]*?\*\//g, ' ')
    .replace(/(^|[^:\\])\/\/.*$/gm, '$1')
    .replace(/'(?:\\.|[^'\\\n])*'/g, "''")
    .replace(/"(?:\\.|[^"\\\n])*"/g, '""')
    .replace(/`(?:\\.|[^`\\])*`/g, '``');
}

const files = walk(appDir).map((file) => ({
  rel: relative(appDir, file).replace(/\\/g, '/'),
  src: readFileSync(file, 'utf8'),
}));

// Every name some module exports.
const exported = new Set();
for (const { src } of files) {
  for (const m of src.matchAll(/export\s+(?:async\s+)?(?:function\*?|const|let|class)\s+([A-Za-z_$][\w$]*)/g)) exported.add(m[1]);
  for (const m of src.matchAll(/export\s*\{([^}]*)\}/g)) {
    for (const part of m[1].split(',')) {
      const name = part.trim().split(/\s+as\s+/).pop();
      if (name) exported.add(name);
    }
  }
}

const NAMESPACES = ['toast', 'modal', 'state', 'router'];

for (const { rel, src } of files) {
  test(`imports what it calls: ${rel}`, () => {
    const body = code(src);
    const missing = [];

    const isDeclared = (name) => {
      const n = name.replace(/\$/g, '\\$');
      // `name(...) {` at the start of a line is an object/class method definition,
      // not a call.
      if (new RegExp(`^\\s*(?:async\\s+)?${n}\\([^)]*\\)\\s*\\{`, 'm').test(body)) return true;
      return new RegExp(
        `import\\s[^;]*\\b${n}\\b|(?:function\\*?|const|let|var|class)\\s+${n}\\b|[{,]\\s*${n}\\s*[},=:]|\\(\\s*${n}\\s*[,)=]|,\\s*${n}\\s*[,)=]|\\b${n}\\s*=>`,
      ).test(body);
    };

    for (const name of exported) {
      if (name.length < 4) continue;
      const call = new RegExp(`(?<![.\\w$])${name.replace(/\$/g, '\\$')}\\(`);
      if (call.test(body) && !isDeclared(name)) missing.push(name);
    }

    for (const ns of NAMESPACES) {
      const use = new RegExp(`(?<![.\\w$])${ns}\\.[A-Za-z_]`);
      if (use.test(body) && !new RegExp(`import\\s+\\*\\s+as\\s+${ns}\\b|(?:const|let|var)\\s+${ns}\\b`).test(body)) {
        missing.push(ns);
      }
    }

    assert.deepEqual(missing, [], `${rel} uses ${missing.join(', ')} without importing or declaring it`);
  });
}
