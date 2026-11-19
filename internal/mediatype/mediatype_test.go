package mediatype

import "testing"

func TestClassifyDownloadExt(t *testing.T) {
	if kind, ok := ClassifyDownloadExt(".png", nil); !ok || kind != "image" {
		t.Errorf("got (%q, %v), want (image, true)", kind, ok)
	}
	if kind, ok := ClassifyDownloadExt(".woff", nil); !ok || kind != "font" {
		t.Errorf("got (%q, %v), want (font, true)", kind, ok)
	}
	if _, ok := ClassifyDownloadExt(".sh", nil); ok {
		t.Error("expected .sh to be rejected")
	}
	if _, ok := ClassifyDownloadExt(".sh", []string{".sh"}); ok {
		t.Error("expected .sh to remain rejected even when listed in extra")
	}
	if kind, ok := ClassifyDownloadExt(".wasm", []string{"wasm"}); !ok || kind != "custom" {
		t.Errorf("expected .wasm in extra to be allowed as custom, got (%q, %v)", kind, ok)
	}
}

func TestIsDangerousMIME(t *testing.T) {
	if !IsDangerousMIME("application/x-msdownload") {
		t.Error("expected application/x-msdownload to be dangerous")
	}
	if !IsDangerousMIME("application/x-executable") {
		t.Error("expected application/x-executable to be dangerous")
	}
	if IsDangerousMIME("image/png") {
		t.Error("image/png should not be dangerous")
	}
}
