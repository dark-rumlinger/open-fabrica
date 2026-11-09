package qrcode

import (
	"strings"
	"testing"
)

// isFinder reports whether the 7x7 block whose top-left module is (r0,c0) is a
// well-formed finder pattern: solid outer ring, light gap, solid 3x3 centre.
func isFinder(grid [][]bool, r0, c0 int) bool {
	for dr := 0; dr < 7; dr++ {
		for dc := 0; dc < 7; dc++ {
			want := dr == 0 || dr == 6 || dc == 0 || dc == 6 ||
				(dr >= 2 && dr <= 4 && dc >= 2 && dc <= 4)
			if grid[r0+dr][c0+dc] != want {
				return false
			}
		}
	}
	return true
}

func TestEncodeVersionSizing(t *testing.T) {
	// "HELLO WORLD" fits in the smallest QR (version 1 -> 21x21) at level M.
	grid, err := Encode("HELLO WORLD")
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(grid) != 21 {
		t.Fatalf("expected 21x21, got %dx%d", len(grid), len(grid))
	}
}

func TestEncodeFinderPatterns(t *testing.T) {
	grid, err := Encode("otpauth://totp/OpenFabrica:alice?secret=JBSWY3DPEHPK3PXP&issuer=OpenFabrica&algorithm=SHA1&digits=6&period=30")
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	n := len(grid)
	for _, corner := range [][2]int{{0, 0}, {0, n - 7}, {n - 7, 0}} {
		if !isFinder(grid, corner[0], corner[1]) {
			t.Errorf("missing/garbled finder pattern at (%d,%d)", corner[0], corner[1])
		}
	}
	// The dark module is always black.
	if !grid[n-8][8] {
		t.Errorf("dark module at (%d,8) is not black", n-8)
	}
}

func TestEncodeGrowsWithPayload(t *testing.T) {
	small, _ := Encode("a")
	large, _ := Encode(strings.Repeat("x", 300))
	if len(large) <= len(small) {
		t.Fatalf("expected larger payload to need a larger grid: %d vs %d", len(large), len(small))
	}
}

func TestEncodeTooLarge(t *testing.T) {
	// Far beyond version 40's level-M byte capacity (~2330 bytes).
	if _, err := Encode(strings.Repeat("x", 5000)); err == nil {
		t.Fatal("expected an error for an over-capacity payload")
	}
}

func TestDataURI(t *testing.T) {
	uri := DataURI("otpauth://totp/Acme:user?secret=JBSWY3DPEHPK3PXP&issuer=Acme")
	if !strings.HasPrefix(uri, "data:image/svg+xml;base64,") {
		t.Fatalf("unexpected data URI prefix: %.40q", uri)
	}
	if len(uri) < 200 {
		t.Fatalf("data URI suspiciously short: %d bytes", len(uri))
	}
}
