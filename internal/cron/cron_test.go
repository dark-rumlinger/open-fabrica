package cron

import (
	"testing"
	"time"
)

func TestNextTimeValid(t *testing.T) {
	// Reference: 2026-06-14 12:30 (a Sunday).
	after := time.Date(2026, 6, 14, 12, 30, 0, 0, time.UTC)

	cases := []struct {
		expr string
		want time.Time
	}{
		{"* * * * *", time.Date(2026, 6, 14, 12, 31, 0, 0, time.UTC)},
		{"0 * * * *", time.Date(2026, 6, 14, 13, 0, 0, 0, time.UTC)},
		{"*/15 * * * *", time.Date(2026, 6, 14, 12, 45, 0, 0, time.UTC)},
		{"30 13 * * *", time.Date(2026, 6, 14, 13, 30, 0, 0, time.UTC)},
		{"0 0 1 * *", time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		got, err := NextTime(c.expr, after)
		if err != nil {
			t.Errorf("NextTime(%q) unexpected error: %v", c.expr, err)
			continue
		}
		if !got.Equal(c.want) {
			t.Errorf("NextTime(%q) = %v, want %v", c.expr, got, c.want)
		}
	}
}

// TestNextTimeDomDowPosixOr pins the POSIX day rule: with BOTH day_of_month and
// day_of_week restricted (neither is "*"), the schedule fires when EITHER
// matches; with only one restricted, that field alone decides the day.
func TestNextTimeDomDowPosixOr(t *testing.T) {
	cases := []struct {
		expr  string
		after time.Time
		want  time.Time
	}{
		// Both restricted: from Sunday 2026-06-14, Monday the 15th beats the 1st.
		{"0 9 1 * 1", time.Date(2026, 6, 14, 12, 30, 0, 0, time.UTC), time.Date(2026, 6, 15, 9, 0, 0, 0, time.UTC)},
		// Both restricted: past June's last Monday firing, the 1st (a Wednesday) fires.
		{"0 9 1 * 1", time.Date(2026, 6, 29, 9, 0, 0, 0, time.UTC), time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)},
		// Only dow restricted: the "*" dom must not widen the match to every day.
		{"0 9 * * 1", time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC), time.Date(2026, 6, 22, 9, 0, 0, 0, time.UTC)},
		// Only dom restricted: the "*" dow must not widen the match to every day.
		{"0 9 15 * *", time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC), time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		got, err := NextTime(c.expr, c.after)
		if err != nil {
			t.Errorf("NextTime(%q, %v) unexpected error: %v", c.expr, c.after, err)
			continue
		}
		if !got.Equal(c.want) {
			t.Errorf("NextTime(%q, %v) = %v, want %v", c.expr, c.after, got, c.want)
		}
	}
}

// TestNextTimeFailsClosed is the regression for the silent default-to-all
// behaviour: a malformed expression must return an error, NOT fall back to
// "fire every minute".
func TestNextTimeFailsClosed(t *testing.T) {
	after := time.Date(2026, 6, 14, 12, 30, 0, 0, time.UTC)
	bad := []string{
		"",            // no fields
		"* * * *",     // 4 fields
		"* * * * * *", // 6 fields
		"bad * * * *", // non-numeric minute
		"60 * * * *",  // minute out of range
		"* 24 * * *",  // hour out of range
		"* * 0 * *",   // day_of_month below range
		"* * * 13 *",  // month out of range
		"* * * * 7",   // day_of_week out of range
		"*/0 * * * *", // zero step
		"*/x * * * *", // non-numeric step
		"5- * * * *",  // half-open range
		"0 0 30 2 *",  // impossible: Feb 30 never occurs
	}
	for _, expr := range bad {
		got, err := NextTime(expr, after)
		if err == nil {
			t.Errorf("NextTime(%q) returned %v with no error; want fail-closed error", expr, got)
		}
		if !got.IsZero() {
			t.Errorf("NextTime(%q) returned non-zero time %v on error", expr, got)
		}
	}
}

// TestValidateAcceptsParse: anything Validate accepts, NextTime must parse
// without a parse error (it may still report no-occurrence for impossible dates,
// which Validate does not check).
func TestValidateAcceptsParse(t *testing.T) {
	after := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	good := []string{"* * * * *", "0 9 * * 1-5", "*/5 0-6 1,15 * *", "0 0 1 1 *"}
	for _, expr := range good {
		if err := Validate(expr); err != nil {
			t.Fatalf("Validate(%q) rejected a valid expression: %v", expr, err)
		}
		if _, err := NextTime(expr, after); err != nil {
			t.Errorf("NextTime(%q) errored on a Validate-accepted expression: %v", expr, err)
		}
	}
}

// TestNextTimeHalfHourZone pins hour stepping in zones whose offset is not a whole
// hour: the next match must land on a local hour boundary, not a UTC one.
func TestNextTimeHalfHourZone(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	after := time.Date(2026, 6, 14, 12, 40, 0, 0, ist)

	got, err := NextTime("15 14 * * *", after)
	if err != nil {
		t.Fatalf("NextTime: %v", err)
	}
	want := time.Date(2026, 6, 14, 14, 15, 0, 0, ist)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}

	got, err = NextTime("0 9 * * *", after)
	if err != nil {
		t.Fatalf("NextTime: %v", err)
	}
	want = time.Date(2026, 6, 15, 9, 0, 0, 0, ist)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestNextTimeSpringForwardTerminates guards the hour step across a DST gap.
func TestNextTimeSpringForwardTerminates(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("tzdata unavailable")
	}
	after := time.Date(2026, 3, 8, 0, 30, 0, 0, loc)
	got, err := NextTime("30 2 * * *", after)
	if err != nil {
		t.Fatalf("NextTime: %v", err)
	}
	if !got.After(after) {
		t.Errorf("got %v, not after %v", got, after)
	}
}

func TestNextRun(t *testing.T) {
	from := time.Date(2026, 6, 14, 12, 30, 0, 0, time.UTC)
	if next, ok, err := NextRun("0 13 * * *", 0, from); err != nil || !ok || !next.Equal(time.Date(2026, 6, 14, 13, 0, 0, 0, time.UTC)) {
		t.Fatalf("cron: next=%v ok=%v err=%v", next, ok, err)
	}
	if next, ok, err := NextRun("", 90, from); err != nil || !ok || !next.Equal(from.Add(90*time.Second)) {
		t.Fatalf("interval: next=%v ok=%v err=%v", next, ok, err)
	}
	// Neither a cron nor an interval: no next run, never an invented default.
	if _, ok, err := NextRun("", 0, from); ok || err != nil {
		t.Fatalf("no schedule: ok=%v err=%v, want false/nil", ok, err)
	}
	if _, ok, err := NextRun("not a cron", 0, from); ok || err == nil {
		t.Fatal("an invalid cron must fail")
	}
}
