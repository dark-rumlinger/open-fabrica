package engine

// Guard: a pack must not RESTATE, in its own words, a fact whose single home is
// a contract - not even a pack that ALSO transcludes that contract.
// TestNoContractRestatement catches a verbatim contract LINE; this catches a
// REWORDED copy of the load-bearing SHAPE via a curated fingerprint. Curated
// like ruleHomes in rule_singlehome_test.go: extend the map when a new contract
// fact proves restatement-prone.

import (
	"strings"
	"testing"

	"openfabrica/contracts"
	"openfabrica/pack"
)

// contractFingerprints maps a contract ID to distinctive signatures of its
// load-bearing SHAPE that a pack may reach ONLY by transcluding {{contract:id}}
// - never by restating in prose. Each fingerprint is asserted to occur in its
// own contract (a typo fails loudly) and in NO pack. A fingerprint must be
// distinctive of the SHAPE, not merely present in it: a signature generic
// enough to occur in innocent prose fails honest text. Anchor on what makes the
// shape THAT shape - here, the envelope's v/iv/ct fields.
var contractFingerprints = map[string][]string{
	"e2ee-wire":       {"type: 'chat', v: 1"},             // the ciphertext wire envelope
	"auth-window-api": {"resolves to the raw `Response`"}, // apiFetch returns Response, not parsed JSON
}

// squashWS lowercases and removes ALL whitespace so a fingerprint matches across a
// reflow or a cosmetic spacing change (`type:'chat'` == `type: 'chat'`).
func squashWS(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), "") }

func TestNoRewordedContractRestatement(t *testing.T) {
	cs, err := contracts.List()
	if err != nil {
		t.Fatalf("contracts.List: %v", err)
	}
	byID := make(map[string]string, len(cs))
	for _, c := range cs {
		byID[c.ID] = squashWS(c.Body)
	}
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}

	for id, fps := range contractFingerprints {
		body, ok := byID[id]
		if !ok {
			t.Errorf("contractFingerprints names unknown contract %q", id)
			continue
		}
		for _, fp := range fps {
			needle := squashWS(fp)
			if !strings.Contains(body, needle) {
				t.Errorf("fingerprint %q is not present in contract %q - it must be a real signature of the contract's text; fix the fingerprint.", fp, id)
				continue
			}
			for _, p := range ps {
				if strings.Contains(squashWS(p.Body), needle) {
					t.Errorf("pack %q restates contract %q's shape in prose (fingerprint %q) - reference {{contract:%s}} instead of re-spelling it (knowledge/stages/AUTHORING.md).",
						p.Slug, id, fp, id)
				}
			}
		}
	}
}
