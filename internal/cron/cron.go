// Package cron provides a minimal cron expression parser and matcher.
//
// Supports standard 5-field expressions: minute hour day-of-month month day-of-week.
// Syntax per field: *, N, */N, N-M, N,M, N-M/S.
// Named days/months, L, W, #, and @shortcuts are not supported.
package cron

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Schedule holds expanded value sets for each cron field.
type Schedule struct {
	Minute []int
	Hour   []int
	Dom    []int // day of month
	Month  []int
	Dow    []int // day of week (0=Sunday)
}

// fieldSpec defines the valid range for a cron field.
type fieldSpec struct {
	name string
	min  int
	max  int
}

var fields = []fieldSpec{
	{"minute", 0, 59},
	{"hour", 0, 23},
	{"day-of-month", 1, 31},
	{"month", 1, 12},
	{"day-of-week", 0, 6},
}

// Parse parses a 5-field cron expression into a Schedule.
func Parse(expr string) (*Schedule, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return nil, fmt.Errorf("expected 5 fields, got %d", len(parts))
	}

	var parsed [5][]int
	for i, part := range parts {
		vals, err := parseField(part, fields[i].min, fields[i].max)
		if err != nil {
			return nil, fmt.Errorf("field %s (%q): %w", fields[i].name, part, err)
		}
		parsed[i] = vals
	}

	return &Schedule{
		Minute: parsed[0],
		Hour:   parsed[1],
		Dom:    parsed[2],
		Month:  parsed[3],
		Dow:    parsed[4],
	}, nil
}

// Matches returns true if the schedule matches the given time (truncated to minute).
func (s *Schedule) Matches(t time.Time) bool {
	t = t.Truncate(time.Minute)
	return contains(s.Minute, t.Minute()) &&
		contains(s.Hour, t.Hour()) &&
		contains(s.Dom, t.Day()) &&
		contains(s.Month, int(t.Month())) &&
		contains(s.Dow, int(t.Weekday()))
}

// parseField parses a single cron field into a sorted list of values.
func parseField(field string, min, max int) ([]int, error) {
	var result []int
	seen := make(map[int]bool)

	for _, item := range strings.Split(field, ",") {
		vals, err := parseItem(item, min, max)
		if err != nil {
			return nil, err
		}
		for _, v := range vals {
			if !seen[v] {
				seen[v] = true
				result = append(result, v)
			}
		}
	}

	sort.Ints(result)
	return result, nil
}

// parseItem parses a single comma-separated item: *, */N, N-M, N-M/S, or N.
func parseItem(item string, min, max int) ([]int, error) {
	// Check for step: X/N
	step := 1
	if idx := strings.Index(item, "/"); idx >= 0 {
		s, err := strconv.Atoi(item[idx+1:])
		if err != nil || s <= 0 {
			return nil, fmt.Errorf("invalid step %q", item[idx+1:])
		}
		step = s
		item = item[:idx]
	}

	var start, end int

	switch {
	case item == "*":
		start, end = min, max
	case strings.Contains(item, "-"):
		parts := strings.SplitN(item, "-", 2)
		var err error
		start, err = strconv.Atoi(parts[0])
		if err != nil {
			return nil, fmt.Errorf("invalid range start %q", parts[0])
		}
		end, err = strconv.Atoi(parts[1])
		if err != nil {
			return nil, fmt.Errorf("invalid range end %q", parts[1])
		}
		if start > end {
			return nil, fmt.Errorf("range start %d > end %d", start, end)
		}
	default:
		n, err := strconv.Atoi(item)
		if err != nil {
			return nil, fmt.Errorf("invalid value %q", item)
		}
		if step > 1 {
			// N/S means "starting at N, every S" through max
			start, end = n, max
		} else {
			start, end = n, n
		}
	}

	// Validate range
	if start < min || start > max {
		return nil, fmt.Errorf("value %d out of range %d-%d", start, min, max)
	}
	if end < min || end > max {
		return nil, fmt.Errorf("value %d out of range %d-%d", end, min, max)
	}

	var vals []int
	for i := start; i <= end; i += step {
		vals = append(vals, i)
	}
	return vals, nil
}

func contains(set []int, val int) bool {
	for _, v := range set {
		if v == val {
			return true
		}
	}
	return false
}
