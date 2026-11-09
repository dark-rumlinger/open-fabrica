// Package cron provides a minimal 5-field cron expression parser.
// Fields: minute hour day_of_month month day_of_week
// Supports: *, specific values, ranges (1-5), steps (*/5), lists (1,3,5)
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Validate checks whether a cron expression is syntactically valid.
// It returns an error describing the problem, or nil if valid.
func Validate(expr string) error {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return fmt.Errorf("must have 5 fields (minute hour day_of_month month day_of_week), got %d", len(fields))
	}

	names := []string{"minute", "hour", "day_of_month", "month", "day_of_week"}
	limits := [][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}

	// Validation IS parsing: each field goes through the same parseField that
	// NextTime uses, so Validate can never accept an expression NextTime later
	// rejects (or vice versa).
	for i, field := range fields {
		if _, err := parseField(field, limits[i][0], limits[i][1]); err != nil {
			return fmt.Errorf("field %d (%s): %w", i+1, names[i], err)
		}
	}
	return nil
}

// NextTime computes the next time a cron expression fires after `after`.
// It returns an error when the expression is malformed or has no occurrence
// within the next 366 days. Failing closed is deliberate: a bad expression
// must never silently fall back to firing every tick (the previous behaviour),
// which would hammer the LLM/scheduler. Callers validate with Validate before
// storing, so an error here means an impossible schedule or a row that bypassed
// validation - both of which should stop the task, not run it.
func NextTime(expr string, after time.Time) (time.Time, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return time.Time{}, fmt.Errorf("must have 5 fields (minute hour day_of_month month day_of_week), got %d", len(fields))
	}

	minuteSet, err := parseField(fields[0], 0, 59)
	if err != nil {
		return time.Time{}, fmt.Errorf("minute field: %w", err)
	}
	hourSet, err := parseField(fields[1], 0, 23)
	if err != nil {
		return time.Time{}, fmt.Errorf("hour field: %w", err)
	}
	domSet, err := parseField(fields[2], 1, 31)
	if err != nil {
		return time.Time{}, fmt.Errorf("day_of_month field: %w", err)
	}
	monthSet, err := parseField(fields[3], 1, 12)
	if err != nil {
		return time.Time{}, fmt.Errorf("month field: %w", err)
	}
	dowSet, err := parseField(fields[4], 0, 6)
	if err != nil {
		return time.Time{}, fmt.Errorf("day_of_week field: %w", err)
	}

	// POSIX day matching: when BOTH day fields are restricted (neither is "*"),
	// a day matches if EITHER field matches; when only one is restricted, that
	// field alone must match (the "*" side matches every day).
	domRestricted := fields[2] != "*"
	dowRestricted := fields[4] != "*"
	dayMatch := func(t time.Time) bool {
		domOK := domSet[t.Day()]
		dowOK := dowSet[int(t.Weekday())]
		if domRestricted && dowRestricted {
			return domOK || dowOK
		}
		return domOK && dowOK
	}

	// Start scanning from the next minute after `after`.
	candidate := after.Truncate(time.Minute).Add(time.Minute)
	limit := candidate.Add(366 * 24 * time.Hour)

	for candidate.Before(limit) {
		if monthSet[int(candidate.Month())] &&
			dayMatch(candidate) &&
			hourSet[candidate.Hour()] &&
			minuteSet[candidate.Minute()] {
			return candidate, nil
		}

		// Advance efficiently: skip to next valid minute/hour/day.
		if !monthSet[int(candidate.Month())] {
			candidate = time.Date(candidate.Year(), candidate.Month()+1, 1, 0, 0, 0, 0, candidate.Location())
			continue
		}
		if !dayMatch(candidate) {
			candidate = time.Date(candidate.Year(), candidate.Month(), candidate.Day()+1, 0, 0, 0, 0, candidate.Location())
			continue
		}
		if !hourSet[candidate.Hour()] {
			// Step to the next local hour boundary. Truncate(time.Hour) aligns to
			// UTC, which skips minutes in zones with a 30 or 45 minute offset.
			candidate = candidate.Add(time.Duration(60-candidate.Minute()) * time.Minute)
			continue
		}
		candidate = candidate.Add(time.Minute)
	}

	return time.Time{}, fmt.Errorf("no occurrence within 366 days")
}

// NextRun is a scheduled task's next fire time from its cron expression (when set)
// or its interval in seconds. ok is false when the task has neither, so it has no
// next run. A non-nil error means the cron expression is invalid or never fires.
// Every writer of of_scheduled_tasks.next_run goes through it.
func NextRun(expr string, intervalSec int, from time.Time) (next time.Time, ok bool, err error) {
	switch {
	case expr != "":
		next, err = NextTime(expr, from)
		if err != nil {
			return time.Time{}, false, err
		}
		return next, true, nil
	case intervalSec > 0:
		return from.Add(time.Duration(intervalSec) * time.Second), true, nil
	}
	return time.Time{}, false, nil
}

// parseField parses a single cron field into the set of values it matches.
// A field that fails to parse returns an error rather than silently matching
// everything: a malformed field must not fall through to "every value".
func parseField(field string, min, max int) (map[int]bool, error) {
	set := make(map[int]bool)

	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("empty value")
		}

		// Handle */N step syntax.
		if strings.Contains(part, "/") {
			parts := strings.SplitN(part, "/", 2)
			step, err := strconv.Atoi(parts[1])
			if err != nil || step <= 0 {
				return nil, fmt.Errorf("invalid step %q", parts[1])
			}
			rangeStart := min
			rangeEnd := max
			if parts[0] != "*" {
				if rangeParts := strings.SplitN(parts[0], "-", 2); len(rangeParts) == 2 {
					var err1, err2 error
					rangeStart, err1 = strconv.Atoi(rangeParts[0])
					rangeEnd, err2 = strconv.Atoi(rangeParts[1])
					if err1 != nil || err2 != nil {
						return nil, fmt.Errorf("invalid step range %q", parts[0])
					}
				} else {
					var err error
					rangeStart, err = strconv.Atoi(parts[0])
					if err != nil {
						return nil, fmt.Errorf("invalid step start %q", parts[0])
					}
				}
			}
			for i := rangeStart; i <= rangeEnd; i += step {
				if i >= min && i <= max {
					set[i] = true
				}
			}
			continue
		}

		// Handle * (all values).
		if part == "*" {
			for i := min; i <= max; i++ {
				set[i] = true
			}
			continue
		}

		// Handle range (1-5).
		if strings.Contains(part, "-") {
			rangeParts := strings.SplitN(part, "-", 2)
			start, err1 := strconv.Atoi(rangeParts[0])
			end, err2 := strconv.Atoi(rangeParts[1])
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("invalid range %q", part)
			}
			for i := start; i <= end; i++ {
				if i >= min && i <= max {
					set[i] = true
				}
			}
			continue
		}

		// Handle single value.
		val, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("invalid value %q", part)
		}
		if val < min || val > max {
			return nil, fmt.Errorf("value %d out of range %d-%d", val, min, max)
		}
		set[val] = true
	}

	if len(set) == 0 {
		return nil, fmt.Errorf("field %q matched no values", field)
	}

	return set, nil
}
