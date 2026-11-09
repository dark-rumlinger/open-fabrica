package replaybudget

import (
	"testing"
	"time"
)

// The replay budget allows max replays per project inside the window, frees them
// as they age out, and keeps projects independent.
func TestSliding(t *testing.T) {
	b := NewSliding(3, time.Hour)
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if !b.Take(1, start.Add(time.Duration(i)*time.Minute)) {
			t.Fatalf("replay %d must be within budget", i+1)
		}
	}
	if b.Take(1, start.Add(10*time.Minute)) {
		t.Fatal("the 4th replay inside the hour must be refused")
	}
	if !b.Take(2, start.Add(10*time.Minute)) {
		t.Fatal("another project has its own budget")
	}
	if !b.Take(1, start.Add(61*time.Minute)) {
		t.Fatal("a replay must be allowed again once the oldest ages out")
	}
}

// A replay that fails no fewer flows than the best so far is a stall. Progress resets
// the count, a green full run clears everything, and the cap is reached on the fourth
// red of the same size because the first only sets the baseline.
func TestStallsCountReplaysWithoutProgress(t *testing.T) {
	s := NewStalls()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	step := func(failed int, full bool) int {
		now = now.Add(time.Minute)
		return s.Observe(1, failed, 0, full, now)
	}
	if got := step(3, true); got != 0 {
		t.Fatalf("the first red sets the baseline, got %d", got)
	}
	if got := step(3, true); got != 1 {
		t.Fatalf("same size again is a stall, got %d", got)
	}
	if got := step(2, true); got != 0 {
		t.Fatalf("fewer failed flows is progress, got %d", got)
	}
	for want := 1; want <= MaxStalls; want++ {
		if got := step(2, true); got != want {
			t.Fatalf("stall %d: got %d", want, got)
		}
	}
	if s.Count(1, now) < MaxStalls {
		t.Fatal("the cap must be reached after MaxStalls stalls")
	}
	if !s.LastRed(1, now) {
		t.Fatal("the latest replay was red")
	}
	if got := step(0, true); got != 0 || s.LastRed(1, now) {
		t.Fatalf("a green full run clears the state, got %d", got)
	}
}

// The same number of failing flows is still progress when they fail further into
// their steps: a script fix that moves a flow from step 3 to step 12 got closer to green.
func TestStallsDepthCountsAsProgress(t *testing.T) {
	s := NewStalls()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	step := func(failed, depth int) int {
		now = now.Add(time.Minute)
		return s.Observe(1, failed, depth, true, now)
	}
	if got := step(2, 8); got != 0 {
		t.Fatalf("the first red sets the baseline, got %d", got)
	}
	if got := step(2, 15); got != 0 {
		t.Fatalf("failing further in is progress, got %d", got)
	}
	if got := step(2, 15); got != 1 {
		t.Fatalf("the same depth again is a stall, got %d", got)
	}
	if got := step(2, 10); got != 2 {
		t.Fatalf("failing earlier than the best is a stall, got %d", got)
	}
	if got := step(1, 3); got != 0 {
		t.Fatalf("fewer failed flows is progress whatever the depth, got %d", got)
	}
}

// A partial run: a red adds a stall (it says nothing about the suite), a green means the
// fix it checked landed. Another project's state is independent, and state expires.
func TestStallsPartialRunsIndependenceAndExpiry(t *testing.T) {
	s := NewStalls()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	s.Observe(1, 2, 0, true, now)
	if got := s.Observe(1, 1, 0, false, now.Add(time.Minute)); got != 1 {
		t.Fatalf("a red partial run is a stall, got %d", got)
	}
	if got := s.Observe(1, 0, 0, false, now.Add(2*time.Minute)); got != 0 {
		t.Fatalf("a green partial run resets the count, got %d", got)
	}
	if s.Count(2, now) != 0 || s.LastRed(2, now) {
		t.Fatal("another project has its own state")
	}
	s.Observe(1, 2, 0, true, now.Add(3*time.Minute))
	s.Observe(1, 2, 0, true, now.Add(4*time.Minute))
	if s.Count(1, now.Add(4*time.Minute+stallExpiry)) != 0 {
		t.Fatal("stale state must expire")
	}
	s.Observe(1, 2, 0, true, now)
	s.Reset(1)
	if s.LastRed(1, now) {
		t.Fatal("Reset drops the state")
	}
}

// Left reports what the window still allows.
func TestSlidingLeft(t *testing.T) {
	b := NewSliding(3, time.Hour)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	b.Take(1, now)
	if got := b.Left(1, now); got != 2 {
		t.Fatalf("left = %d, want 2", got)
	}
	if got := b.Left(1, now.Add(2*time.Hour)); got != 3 {
		t.Fatalf("left after the window = %d, want 3", got)
	}
}
