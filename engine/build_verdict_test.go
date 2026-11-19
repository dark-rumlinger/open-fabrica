package engine

import "testing"

// makeBuildVerdict freezes "what ships" in one place; its order (validators, then flows,
// then gaps) is what the go-live report records, and the empty-flows case is the flow
// gate that did not run. The query methods must read that one frozen value.
func TestBuildVerdictAssemblesInOrder(t *testing.T) {
	validators := []Issue{{Severity: "error", Category: "runtime", Message: "v"}}
	flows := []Issue{{Severity: "error", Category: "flow_tests", Message: "f"}}
	gaps := []Issue{{Severity: "error", Category: specGapCategory, Message: "g"}}

	v := makeBuildVerdict(validators, flows, gaps)
	if got := len(v.issues); got != 3 {
		t.Fatalf("verdict holds %d issues, want 3: %+v", got, v.issues)
	}
	for i, want := range []string{"v", "f", "g"} {
		if v.issues[i].Message != want {
			t.Errorf("issue %d = %q, want %q (order validators, flows, gaps)", i, v.issues[i].Message, want)
		}
	}

	// A flow gate that did not run contributes no findings.
	if got := len(makeBuildVerdict(validators, nil, gaps).issues); got != 2 {
		t.Errorf("nil flows added %d extra issues, want the other two only", got-2)
	}

	// The queries read the one frozen value and agree with the shared filters.
	if len(v.fixable()) != len(FixableErrors(v.issues)) || len(v.blocking()) != len(blockingErrors(v.issues)) {
		t.Error("verdict queries disagree with the shared filters they wrap")
	}
}
