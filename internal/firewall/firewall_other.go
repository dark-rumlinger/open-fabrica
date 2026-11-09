//go:build !windows && !linux && !darwin

package firewall

import (
	"context"

	"openfabrica/internal/msgcat"
)

// Stub for platforms without a managed-firewall implementation. The feature is
// reported as unsupported so the UI hides the button.
type stubManager struct{}

func newManager() Manager { return &stubManager{} }

func (m *stubManager) Detect(_ context.Context, _ Rule) (Status, error) {
	return Status{Supported: false, Detail: msgcat.Mark("Firewall management is not available on this platform.")}, nil
}

func (m *stubManager) Ensure(_ context.Context, _ Rule) (Result, error) {
	return Result{Error: "not supported on this platform"}, nil
}

func (m *stubManager) Remove(_ context.Context, _ string) (Result, error) {
	return Result{OK: true}, nil
}

func applyRulesElevated(_ Rule, _ bool) Result {
	return Result{Error: "not supported on this platform"}
}
