package store

import (
	"testing"

	"openfabrica/analysis"
)

// UndefinedRefs only earns the right to block a build if it stays silent on
// working code: a false positive is worse than the ReferenceError it hunts.
// TestUndefinedRefs_FixtureCorpus is the gate: clean modules must report nothing,
// broken modules exactly the expected refs.

// cleanModules must produce zero findings. Every name each one uses is bound by
// an import, a declaration, a parameter, or the known-globals allowlist.
var cleanModules = map[string]string{
	// Imports and module-level state satisfying later references, plus DOM
	// globals and event-handler closures - the shape of most shipped UI modules.
	"hud.js": `
import { roster, MAX_PLAYERS } from './state.js';
import { setMuted, isMuted } from './audio.js';

let dotsRow, countEl;
let presentList = [];

export function initHUD() {
  dotsRow = document.getElementById('playerDots');
  countEl = document.getElementById('playerCount');
  const cog = document.getElementById('settingsCog');
  if (cog) {
    cog.addEventListener('click', (e) => {
      e.stopPropagation();
      setMuted(!isMuted());
    });
  }
  renderDots();
}

export function setPresent(list) {
  presentList = list.slice(0, MAX_PLAYERS);
  renderDots();
}

function renderDots() {
  if (!dotsRow) return;
  dotsRow.textContent = String(presentList.length + roster.size);
  if (countEl) countEl.textContent = String(presentList.length);
}
`,
	// Function declarations are hoisted: helper() is called above its
	// declaration and is still bound. A scope walk that reads top-to-bottom
	// without hoisting would flag this.
	"hoisting.js": `
export function entry() {
  return helper() + later.value;
}
function helper() { return 1; }
const later = { value: 2 };
`,
	// Binding forms that are easy to miss: destructuring (object, array, nested,
	// renamed, defaulted, rest), for-of, catch params, class methods.
	"bindings.js": `
import config from './config.js';

export function run(input) {
  const { alpha, beta: renamed, gamma = 3, ...rest } = input;
  const [first, , third] = input.list;
  const { deep: { nested } } = config;
  let total = alpha + renamed + gamma + first + third + nested;
  for (const item of rest.items) { total += item; }
  try { total += config.extra; } catch (err) { total += err.code; }
  return total;
}

export class Widget {
  constructor(host) { this.host = host; }
  render() { return this.host.querySelector('.x'); }
}
`,
	// Globals the allowlist must cover, and identifiers appearing in positions
	// that are not references at all (property keys, member names, labels).
	"globals.js": `
export async function load(url) {
  const res = await fetch(url, { signal: AbortSignal.timeout(1000) });
  const data = await res.json();
  console.log(JSON.stringify(data), Math.max(1, 2), Date.now());
  const shape = { fetch: 1, console: 2, notAGlobal: 3 };
  return window.structuredClone(shape).notAGlobal;
}
`,
}

// brokenModules must produce exactly these findings - the real breaks the check
// exists to catch, one per RefKind.
var brokenModules = []struct {
	name string
	body string
	want []analysis.UndefinedRef
}{
	{
		// Calling a helper nobody declared or imported.
		name: "missing_call.js",
		body: "export function go() {\n  return missingHelper();\n}\n",
		want: []analysis.UndefinedRef{{Name: "missingHelper", Line: 2, Kind: analysis.RefCall}},
	},
	{
		// Reading a property off a name no import binds - the classic
		// dropped-import break.
		name: "missing_member.js",
		body: "import { present } from './x.js';\nexport function go() {\n  return present.a + absent.b;\n}\n",
		want: []analysis.UndefinedRef{{Name: "absent", Line: 3, Kind: analysis.RefMember}},
	},
	{
		// Assigning to an unbound name. An import binding is read-only, so this
		// cannot be fixed by importing - it is a distinct break from a read.
		name: "missing_assign.js",
		body: "export function go() {\n  stray = 5;\n  return stray;\n}\n",
		want: []analysis.UndefinedRef{{Name: "stray", Line: 2, Kind: analysis.RefAssign}},
	},
}

func TestUndefinedRefs_FixtureCorpus(t *testing.T) {
	t.Run("clean modules report nothing", func(t *testing.T) {
		for name, body := range cleanModules {
			got := analysis.UndefinedRefs(name, body)
			for _, r := range got {
				t.Errorf("%s: false positive - `%s` (%s) on line %d is bound, but was reported; a build-blocking check must not halt working code",
					name, r.Name, r.Kind, r.Line)
			}
		}
	})

	t.Run("broken modules report exactly their break", func(t *testing.T) {
		for _, c := range brokenModules {
			got := analysis.UndefinedRefs(c.name, c.body)
			if len(got) != len(c.want) {
				t.Errorf("%s: got %d finding(s) %+v, want %d %+v", c.name, len(got), got, len(c.want), c.want)
				continue
			}
			for i, w := range c.want {
				if got[i] != w {
					t.Errorf("%s: finding %d = %+v, want %+v", c.name, i, got[i], w)
				}
			}
		}
	})
}
