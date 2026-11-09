package store

import "fmt"

// codedError carries a machine-readable failcode alongside its message so a
// store-layer save gate can have its rejection classified the same way a
// tool-layer tools.FailCode would (triage healthy/friction split, UI branching).
// tools.FailErr reads the code through the structural interface { Code() string },
// so store never has to import tools.
type codedError struct {
	code string
	msg  string
}

func (e *codedError) Error() string { return e.msg }
func (e *codedError) Code() string  { return e.code }

// CodedErrorf builds a store error tagged with a stable failcode (e.g.
// "page_not_fragment"). Store gates that want their rejection classified return
// one instead of a bare fmt.Errorf; the code rides through to the tool result.
func CodedErrorf(code, format string, args ...any) error {
	return &codedError{code: code, msg: fmt.Sprintf(format, args...)}
}
