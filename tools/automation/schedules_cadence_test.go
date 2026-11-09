package automation

import (
	"testing"
	"time"
)

func TestScheduleCadence(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		cron     string
		interval int
		wantOK   bool
		wantGap  time.Duration
	}{
		{"every minute cron", "*/1 * * * *", 0, true, time.Minute},
		{"every 5 min cron", "*/5 * * * *", 0, true, 5 * time.Minute},
		{"hourly cron", "0 * * * *", 0, true, time.Hour},
		{"daily cron", "0 3 * * *", 0, true, 24 * time.Hour},
		{"interval 60s", "", 60, true, time.Minute},
		{"interval hourly", "", 3600, true, time.Hour},
		{"no schedule", "", 0, false, 0},
	}
	for _, c := range cases {
		gap, ok := scheduleCadence(c.cron, c.interval, now)
		if ok != c.wantOK {
			t.Errorf("%s: ok=%v want %v", c.name, ok, c.wantOK)
			continue
		}
		if ok && gap != c.wantGap {
			t.Errorf("%s: gap=%v want %v", c.name, gap, c.wantGap)
		}
	}
}

func TestGuardLLMCadence(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	native := `{"type":"count_rows","table":"world_blocks"}`

	// An every-minute PROMPT heartbeat must be refused.
	if r := guardLLMCadence("confirm world is persisted", "", "*/1 * * * *", 0, now); r == "" {
		t.Error("an every-minute prompt schedule must be refused")
	}
	// Same cadence, but a native_action (no LLM): allowed.
	if r := guardLLMCadence("", native, "*/1 * * * *", 0, now); r != "" {
		t.Errorf("a native_action schedule must be exempt from the floor, got: %s", r)
	}
	// Both set: native wins, allowed (the worker runs the native path).
	if r := guardLLMCadence("do thing", native, "*/1 * * * *", 0, now); r != "" {
		t.Errorf("a schedule with a native_action must be exempt even with a prompt, got: %s", r)
	}
	// Hourly prompt schedule (a digest): allowed - hourly is exactly the floor.
	if r := guardLLMCadence("hourly digest", "", "0 * * * *", 0, now); r != "" {
		t.Errorf("an hourly prompt schedule must be allowed, got: %s", r)
	}
	// Daily prompt schedule: allowed.
	if r := guardLLMCadence("daily summary", "", "0 3 * * *", 0, now); r != "" {
		t.Errorf("a daily prompt schedule must be allowed, got: %s", r)
	}
	// Sub-hour interval prompt schedule: refused.
	if r := guardLLMCadence("ping", "", "", 60, now); r == "" {
		t.Error("a 60-second-interval prompt schedule must be refused")
	}
	// Hourly interval prompt schedule: allowed.
	if r := guardLLMCadence("digest", "", "", 3600, now); r != "" {
		t.Errorf("a 3600-second-interval prompt schedule must be allowed, got: %s", r)
	}
}
