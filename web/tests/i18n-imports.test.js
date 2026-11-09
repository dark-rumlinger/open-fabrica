/**
 * Static check for the translation helpers. import-hygiene.test.js skips names
 * shorter than four characters, so a missing `import { t }` would only surface
 * when that string is rendered. This scans for plain `t(` / `tn(` calls and
 * requires the file to import them from core/i18n.js (or define them itself),
 * and flags imported helpers that are never called.
 *
 * Run with `node --test "web/tests/*.test.js"`.
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

// Source with comments and string contents blanked (see import-hygiene.test.js).
function code(src) {
  return src
    .replace(/\/\*[\s\S]*?\*\//g, ' ')
    .replace(/(^|[^:\\])\/\/.*$/gm, '$1')
    .replace(/'(?:\\.|[^'\\\n])*'/g, "''")
    .replace(/"(?:\\.|[^"\\\n])*"/g, '""')
    .replace(/`(?:\\.|[^`\\])*`/g, '``');
}

const files = walk(appDir)
  .map((file) => ({ rel: relative(appDir, file).replace(/\\/g, '/'), src: readFileSync(file, 'utf8') }))
  .filter(({ rel }) => !rel.startsWith('locales/') && rel !== 'core/i18n.js');

function importedFromI18n(src) {
  const names = new Set();
  for (const m of src.matchAll(/import\s*\{([^}]*)\}\s*from\s*'[^']*i18n\.js'/g)) {
    for (const part of m[1].split(',')) {
      const name = part.trim().split(/\s+as\s+/).pop();
      if (name) names.add(name);
    }
  }
  return names;
}

for (const name of ['t', 'tn']) {
  const call = new RegExp(`(^|[^\\w.$])${name}\\(`);
  const def = new RegExp(`(function\\s+${name}\\b|(?:const|let|var)\\s+${name}\\b)`);

  test(`${name}() is imported wherever it is called`, () => {
    const missing = files
      .filter(({ src }) => call.test(code(src)) && !importedFromI18n(src).has(name) && !def.test(code(src)))
      .map(({ rel }) => rel);
    assert.deepEqual(missing, []);
  });

  test(`${name} is not imported without being called`, () => {
    const unused = files
      .filter(({ src }) => importedFromI18n(src).has(name) && !call.test(code(src)))
      .map(({ rel }) => rel);
    assert.deepEqual(unused, []);
  });
}
