package store

import "testing"

func TestSandboxValueIsBlocking(t *testing.T) {
	// allow-scripts without allow-same-origin => opaque origin => blocking.
	if !sandboxValueIsBlocking("allow-scripts allow-popups") {
		t.Error("allow-scripts w/o allow-same-origin should be blocking")
	}
	// With allow-same-origin it is not blocking.
	if sandboxValueIsBlocking("allow-scripts allow-same-origin") {
		t.Error("allow-scripts + allow-same-origin should not be blocking")
	}
	// Neither token present.
	if sandboxValueIsBlocking("allow-forms") {
		t.Error("no allow-scripts should not be blocking")
	}
}

func TestValidateFilePath(t *testing.T) {
	if err := ValidateFilePath("page", ""); err == nil {
		t.Error("empty path should be rejected")
	}
	if err := ValidateFilePath("page", "a/../b"); err == nil {
		t.Error("`..` traversal should be rejected")
	}
	if err := ValidateFilePath("page", "a\x01b"); err == nil {
		t.Error("control character should be rejected")
	}
	if err := ValidateFilePath("layout", "dir/main"); err == nil {
		t.Error("layout path with `/` should be rejected")
	}
	if err := ValidateFilePath("page", "css/app.css"); err != nil {
		t.Errorf("valid page path rejected: %v", err)
	}
	long := make([]byte, 300)
	for i := range long {
		long[i] = 'a'
	}
	if err := ValidateFilePath("page", string(long)); err == nil {
		t.Error("over-long path should be rejected")
	}
}
