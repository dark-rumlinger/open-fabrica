package public

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"openfabrica/contracts"
)

// TestE2EEWireDenylistMatchesRuntime keeps the e2ee-wire contract's enumerated
// plaintext-body fields in lockstep with the fields the relay actually drops.
// The contract is transcluded into the e2ee guide, so a field the runtime blocks
// but the contract omits reads to the model as a safe non-body field - the exact
// drift that once let caption/note ship undocumented. The contract's only probe is
// type:'chat', so no other guard covers this list.
func TestE2EEWireDenylistMatchesRuntime(t *testing.T) {
	body := contracts.Render(contracts.IDE2EEWire)
	if body == "" {
		t.Fatal("e2ee-wire contract rendered empty")
	}
	open := strings.Index(body, "plaintext body field (")
	if open < 0 {
		t.Fatal("e2ee-wire contract lost its 'plaintext body field (...)' enumeration")
	}
	rest := body[open:]
	end := strings.Index(rest, ")")
	if end < 0 {
		t.Fatal("e2ee-wire 'plaintext body field (' enumeration is not closed with ')'")
	}
	var documented []string
	for _, m := range regexp.MustCompile("`([^`]+)`").FindAllStringSubmatch(rest[:end], -1) {
		documented = append(documented, m[1])
	}

	got := append([]string(nil), e2eePlaintextBodyKeys...)
	sort.Strings(got)
	sort.Strings(documented)
	if strings.Join(got, ",") != strings.Join(documented, ",") {
		t.Fatalf("e2ee plaintext-body denylist drift:\n  runtime (e2eePlaintextBodyKeys): %v\n  contract (e2ee-wire):           %v\nadd or remove the field in BOTH", got, documented)
	}
}
