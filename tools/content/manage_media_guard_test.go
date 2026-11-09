package content

import (
	"image"
	"runtime"
	"strings"
	"testing"
)

// osAbsPath is an absolute path on the current OS (IsAbs is platform-specific:
// "/etc/x" is not absolute on Windows). Used so the traversal tests exercise the
// absolute-path rejection on whichever platform they run on.
func osAbsPath() string {
	if runtime.GOOS == "windows" {
		return `C:\Windows\System32\evil`
	}
	return "/etc/passwd"
}

// TestMediaSaveImageRejectsTraversal verifies the write side is contained to the
// project's storage dir: a ../ out_path must be rejected before any os write, so
// a crafted out_path cannot overwrite host files. (Regression for the raw
// os.Create bypass of WriteFileToDisk.)
func TestMediaSaveImageRejectsTraversal(t *testing.T) {
	dataDir := t.TempDir() // isolates the one legitimate on-disk write below
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for _, bad := range []string{
		"../../../../etc/cron.d/x",
		"a/../../b",
		osAbsPath(),
	} {
		if _, _, err := mediaSaveImage(dataDir, 1, "files", bad, img, "png", 80); err == nil {
			t.Errorf("mediaSaveImage accepted traversal path %q, want rejection", bad)
		}
	}
	// A normal nested name is still allowed (it stays inside the project dir).
	if _, _, err := mediaSaveImage(dataDir, 1, "files", "thumbs/hero_200x200.png", img, "png", 80); err != nil {
		t.Errorf("mediaSaveImage rejected a legitimate nested name: %v", err)
	}
}

// TestMediaLoadImageRejectsTraversal verifies the read side rejects ../ before
// touching the filesystem, so a crafted source cannot read another project's or
// arbitrary host files.
func TestMediaLoadImageRejectsTraversal(t *testing.T) {
	for _, bad := range []string{"../../../../etc/passwd", "a/../../b", osAbsPath()} {
		if _, _, err := mediaLoadImage(t.TempDir(), 1, "files", bad); err == nil || !strings.Contains(err.Error(), "path traversal") {
			t.Errorf("mediaLoadImage(%q) = %v, want a path-traversal rejection", bad, err)
		}
	}
}

// TestMediaDimsExceedCap pins the OOM guard: dimensions within the cap pass,
// oversized ones (per-side or total pixels) are rejected, and a pathological
// value that would overflow int in a naive W*H check is still caught.
func TestMediaDimsExceedCap(t *testing.T) {
	cases := []struct {
		w, h    int
		exceeds bool
	}{
		{1920, 1080, false},
		{200, 200, false},
		{maxMediaSide, 1, false},
		{maxMediaSide + 1, 1, true}, // per-side ceiling
		{1, maxMediaSide + 1, true}, // per-side ceiling (height)
		{10000, 10000, true},        // 100 MP > 40 MP total
		{1 << 31, 1 << 31, true},    // would overflow a naive int W*H; must be caught
	}
	for _, c := range cases {
		if got := mediaDimsExceedCap(c.w, c.h); got != c.exceeds {
			t.Errorf("mediaDimsExceedCap(%d, %d) = %v, want %v", c.w, c.h, got, c.exceeds)
		}
	}
}
