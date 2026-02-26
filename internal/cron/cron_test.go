package cron

import (
	"fmt"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	tests := []struct {
		expr string
	}{
		{"* * * * *"},
		{"*/5 * * * *"},
		{"0 9 * * 1-5"},
		{"0,30 * * * *"},
		{"0 0 1 * *"},
		{"15 14 1 * *"},
		{"0 */2 * * *"},
		{"0 9-17 * * *"},
		{"*/15 * * * *"},
	}
	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			s, err := Parse(tt.expr)
			if err != nil {
				t.Fatalf("Parse(%q) error: %v", tt.expr, err)
			}
			if len(s.Minute) == 0 || len(s.Hour) == 0 || len(s.Dom) == 0 || len(s.Month) == 0 || len(s.Dow) == 0 {
				t.Errorf("Parse(%q) has empty field", tt.expr)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		expr string
		desc string
	}{
		{"* * * *", "too few fields"},
		{"* * * * * *", "too many fields"},
		{"60 * * * *", "minute out of range"},
		{"* 24 * * *", "hour out of range"},
		{"* * 0 * *", "dom out of range (0)"},
		{"* * 32 * *", "dom out of range (32)"},
		{"* * * 0 *", "month out of range (0)"},
		{"* * * 13 *", "month out of range (13)"},
		{"* * * * 7", "dow out of range (7)"},
		{"MON * * * *", "named day"},
		{"* * * JAN *", "named month"},
		{"@daily", "shortcut"},
		{"abc * * * *", "non-numeric"},
		{"5-2 * * * *", "reversed range"},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			_, err := Parse(tt.expr)
			if err == nil {
				t.Errorf("Parse(%q) should have failed: %s", tt.expr, tt.desc)
			}
		})
	}
}

func TestParseStep(t *testing.T) {
	s, err := Parse("*/15 * * * *")
	if err != nil {
		t.Fatal(err)
	}
	want := []int{0, 15, 30, 45}
	if fmt.Sprint(s.Minute) != fmt.Sprint(want) {
		t.Errorf("*/15 minute = %v, want %v", s.Minute, want)
	}

	s, err = Parse("1-10/3 * * * *")
	if err != nil {
		t.Fatal(err)
	}
	want = []int{1, 4, 7, 10}
	if fmt.Sprint(s.Minute) != fmt.Sprint(want) {
		t.Errorf("1-10/3 minute = %v, want %v", s.Minute, want)
	}
}

func TestParseRange(t *testing.T) {
	s, err := Parse("* 9-17 * * *")
	if err != nil {
		t.Fatal(err)
	}
	want := []int{9, 10, 11, 12, 13, 14, 15, 16, 17}
	if fmt.Sprint(s.Hour) != fmt.Sprint(want) {
		t.Errorf("9-17 hour = %v, want %v", s.Hour, want)
	}
}

func TestParseList(t *testing.T) {
	s, err := Parse("1,15,30 * * * *")
	if err != nil {
		t.Fatal(err)
	}
	want := []int{1, 15, 30}
	if fmt.Sprint(s.Minute) != fmt.Sprint(want) {
		t.Errorf("1,15,30 minute = %v, want %v", s.Minute, want)
	}
}

func TestParseListDedupe(t *testing.T) {
	s, err := Parse("1,1,2,2 * * * *")
	if err != nil {
		t.Fatal(err)
	}
	want := []int{1, 2}
	if fmt.Sprint(s.Minute) != fmt.Sprint(want) {
		t.Errorf("deduped = %v, want %v", s.Minute, want)
	}
}

func TestMatches(t *testing.T) {
	tests := []struct {
		expr string
		time string // RFC3339
		want bool
	}{
		// Every minute
		{"* * * * *", "2026-02-26T15:30:00Z", true},
		// Every 5 minutes — :30 matches */5
		{"*/5 * * * *", "2026-02-26T15:30:00Z", true},
		// Every 5 minutes — :31 does not
		{"*/5 * * * *", "2026-02-26T15:31:00Z", false},
		// 9am weekdays — Thursday at 9:00
		{"0 9 * * 1-5", "2026-02-26T09:00:00Z", true},
		// 9am weekdays — Thursday at 10:00 (wrong hour)
		{"0 9 * * 1-5", "2026-02-26T10:00:00Z", false},
		// 9am weekdays — Sunday at 9:00 (wrong day)
		{"0 9 * * 1-5", "2026-03-01T09:00:00Z", false},
		// First of month at midnight
		{"0 0 1 * *", "2026-03-01T00:00:00Z", true},
		{"0 0 1 * *", "2026-03-02T00:00:00Z", false},
		// Half-hour marks
		{"0,30 * * * *", "2026-02-26T15:00:00Z", true},
		{"0,30 * * * *", "2026-02-26T15:30:00Z", true},
		{"0,30 * * * *", "2026-02-26T15:15:00Z", false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s@%s", tt.expr, tt.time), func(t *testing.T) {
			s, err := Parse(tt.expr)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.expr, err)
			}
			ts, _ := time.Parse(time.RFC3339, tt.time)
			got := s.Matches(ts)
			if got != tt.want {
				t.Errorf("Matches(%s) = %v, want %v", tt.time, got, tt.want)
			}
		})
	}
}

func TestMatchesDow(t *testing.T) {
	// 2026-02-22 is a Sunday (weekday 0)
	s, _ := Parse("0 0 * * 0")
	sun, _ := time.Parse(time.RFC3339, "2026-02-22T00:00:00Z")
	if !s.Matches(sun) {
		t.Error("Sunday (0) should match dow=0")
	}

	// 2026-02-28 is a Saturday (weekday 6)
	s, _ = Parse("0 0 * * 6")
	sat, _ := time.Parse(time.RFC3339, "2026-02-28T00:00:00Z")
	if !s.Matches(sat) {
		t.Error("Saturday (6) should match dow=6")
	}

	// Monday through Friday
	s, _ = Parse("0 0 * * 1-5")
	if s.Matches(sun) {
		t.Error("Sunday should not match 1-5")
	}
	if s.Matches(sat) {
		t.Error("Saturday should not match 1-5")
	}
	// 2026-02-23 is Monday
	mon, _ := time.Parse(time.RFC3339, "2026-02-23T00:00:00Z")
	if !s.Matches(mon) {
		t.Error("Monday should match 1-5")
	}
}

func TestMatchesIgnoresSeconds(t *testing.T) {
	s, _ := Parse("30 15 * * *")
	// Time with non-zero seconds should still match (truncated to minute)
	ts, _ := time.Parse(time.RFC3339, "2026-02-26T15:30:45Z")
	if !s.Matches(ts) {
		t.Error("should match after truncating seconds")
	}
}
