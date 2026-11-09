package sqliteretry

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestIsBusy(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated error", errors.New("no such table: foo"), false},
		{"SQLITE_BUSY code", errors.New("SQLITE_BUSY: database is busy"), true},
		{"database is locked", errors.New("database is locked"), true},
		{"database table is locked", errors.New("database table is locked"), true},
		// isBusy matches on the error string, so a wrapped busy error is still
		// classified as busy, the retry path must not be defeated by wrapping.
		{"wrapped busy", fmt.Errorf("exec failed: %w", errors.New("database is locked")), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isBusy(c.err); got != c.want {
				t.Errorf("isBusy(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestTruncateSQL(t *testing.T) {
	if got := truncateSQL("   SELECT 1   "); got != "SELECT 1" {
		t.Errorf("truncateSQL should trim surrounding whitespace, got %q", got)
	}

	long := strings.Repeat("x", 250)
	got := truncateSQL(long)
	if !strings.HasSuffix(got, "...") {
		t.Errorf("over-long SQL should be marked with an ellipsis, got suffix %q", got[len(got)-3:])
	}
	if len(got) != 200+len("...") {
		t.Errorf("truncated length = %d, want %d", len(got), 200+len("..."))
	}

	exactly200 := strings.Repeat("y", 200)
	if got := truncateSQL(exactly200); got != exactly200 {
		t.Errorf("SQL at the 200-char boundary must not be truncated (len=%d)", len(got))
	}
}

func TestBackoff(t *testing.T) {
	base := 5 * time.Millisecond
	// backoff = base*2^attempt + jitter, where jitter ∈ [0, delay/2]. Verify the
	// growth and that the jitter stays within its documented bound across many
	// draws.
	for attempt := 0; attempt < maxRetries; attempt++ {
		lo := base * (1 << attempt)
		hi := lo + lo/2
		for i := 0; i < 200; i++ {
			got := backoff(base, attempt)
			if got < lo || got > hi {
				t.Fatalf("backoff(base, %d) = %s, want within [%s, %s]", attempt, got, lo, hi)
			}
		}
	}
}
