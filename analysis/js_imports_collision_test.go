package analysis

import "testing"

// A bare specifier can be unresolvable for two opposite reasons, and the fixes are
// opposite too: an UNKNOWN name wants a library declaration, a COLLIDING one wants a
// rename (declaring it as a library cannot help, and neither can "import it by its bare
// logical name" - that name is precisely what is ambiguous). UnresolvedModuleSpecifiers
// is where ProjectModuleMap's collision list is already in hand, so it is where the two
// are told apart; this pins that it actually is.
func TestUnresolvedModuleSpecifiers_MarksCollisions(t *testing.T) {
	bodies := map[string]string{
		// Two modules share the basename "utils", so `utils` maps to neither.
		"js/a/utils.js": "export const a = 1;",
		"js/b/utils.js": "export const b = 2;",
		"js/solo.js":    "export const s = 3;",
		"js/main.js": "import { a } from 'utils';\n" +
			"import { s } from 'solo';\n" +
			"import confetti from 'canvas-confetti';\n",
	}

	got := map[string]bool{} // spec -> Collides
	for _, u := range UnresolvedModuleSpecifiers(bodies, nil) {
		got[u.Spec] = u.Collides
	}

	if collides, found := got["utils"]; !found {
		t.Error("'utils' should be unresolvable: two modules share the basename")
	} else if !collides {
		t.Error("'utils' is unresolvable because of a COLLISION - Collides must be true, or the validator prescribes a library declaration that cannot work")
	}

	if collides, found := got["canvas-confetti"]; !found {
		t.Error("'canvas-confetti' should be unresolvable: no project module or library provides it")
	} else if collides {
		t.Error("'canvas-confetti' is a genuinely unknown name, not a collision - Collides must be false so the library-declaration fix still gets offered")
	}

	if _, found := got["solo"]; found {
		t.Error("'solo' resolves to js/solo.js unambiguously and must not be reported at all")
	}
}
