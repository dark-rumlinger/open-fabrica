package main

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestVersionInfoMatchesSource guards the one real version-drift risk: main.go's
// Version is the single source of truth, but versioninfo.json (the Windows PE
// metadata, patched from Version at build time by the Makefile / scripts/
// release.sh) is a second copy. In the committed tree the two must agree; if
// Version is bumped without re-patching versioninfo.json, this fails and names
// the fix instead of shipping a binary whose Windows metadata lies.
func TestVersionInfoMatchesSource(t *testing.T) {
	raw, err := os.ReadFile("versioninfo.json")
	if err != nil {
		t.Fatalf("read versioninfo.json: %v", err)
	}
	var vi struct {
		FixedFileInfo struct {
			FileVersion    struct{ Major, Minor, Patch int }
			ProductVersion struct{ Major, Minor, Patch int }
		}
		StringFileInfo struct {
			FileVersion    string
			ProductVersion string
		}
	}
	if err := json.Unmarshal(raw, &vi); err != nil {
		t.Fatalf("parse versioninfo.json: %v", err)
	}

	// StringFileInfo carries the "v"-prefixed semver; main.Version is bare.
	want := "v" + Version
	if vi.StringFileInfo.FileVersion != want {
		t.Errorf("StringFileInfo.FileVersion = %q, want %q (bump it alongside main.go Version)", vi.StringFileInfo.FileVersion, want)
	}
	if vi.StringFileInfo.ProductVersion != want {
		t.Errorf("StringFileInfo.ProductVersion = %q, want %q", vi.StringFileInfo.ProductVersion, want)
	}

	// FixedFileInfo is numeric; compare to the semver core, dropping any
	// pre-release suffix (e.g. "0.3.0-rc1" -> 0.3.0).
	core := strings.SplitN(Version, "-", 2)[0]
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		t.Fatalf("main.Version = %q is not MAJOR.MINOR.PATCH", Version)
	}
	major, minor, patch := atoiVersionPart(t, parts[0]), atoiVersionPart(t, parts[1]), atoiVersionPart(t, parts[2])
	if fv := vi.FixedFileInfo.FileVersion; fv.Major != major || fv.Minor != minor || fv.Patch != patch {
		t.Errorf("FixedFileInfo.FileVersion = %d.%d.%d, want %d.%d.%d", fv.Major, fv.Minor, fv.Patch, major, minor, patch)
	}
	if pv := vi.FixedFileInfo.ProductVersion; pv.Major != major || pv.Minor != minor || pv.Patch != patch {
		t.Errorf("FixedFileInfo.ProductVersion = %d.%d.%d, want %d.%d.%d", pv.Major, pv.Minor, pv.Patch, major, minor, patch)
	}
}

func atoiVersionPart(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("version part %q is not an integer: %v", s, err)
	}
	return n
}
