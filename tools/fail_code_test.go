package tools

import (
	"fmt"
	"testing"
)

type codeErr struct{ code, msg string }

func (e codeErr) Error() string { return e.msg }
func (e codeErr) Code() string  { return e.code }

// FailErr surfaces a code-carrying error's Code() onto the Result (directly or
// through a wrap), so a store-layer gate (store.CodedErrorf) classifies the same
// as a tool-layer FailCode. A plain error leaves Code empty.
func TestFailErr_SurfacesCode(t *testing.T) {
	r := FailErr(codeErr{code: "page_not_fragment", msg: "nope"})
	if r.Success || r.Code != "page_not_fragment" || r.Error != "nope" {
		t.Errorf("direct coded error: got success=%v code=%q error=%q", r.Success, r.Code, r.Error)
	}
	if r := FailErr(fmt.Errorf("wrap: %w", codeErr{code: "x", msg: "m"})); r.Code != "x" {
		t.Errorf("wrapped coded error should surface code, got %q", r.Code)
	}
	if r := FailErr(fmt.Errorf("plain")); r.Code != "" {
		t.Errorf("plain error must have empty Code, got %q", r.Code)
	}
}
