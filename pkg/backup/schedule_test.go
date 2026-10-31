package backup

import (
	"testing"
	"time"
)

// TestScheduleNext covers the five-field parse and the next-instant rule a
// policy's schedule element relies on (C-41).
func TestScheduleNext(t *testing.T) {
	cases := []struct {
		name       string
		expression string
		after      time.Time
		want       time.Time
	}{
		{
			name: "daily at 03:00", expression: "0 3 * * *",
			after: time.Date(2026, 10, 31, 4, 0, 0, 0, time.UTC),
			want:  time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC),
		},
		{
			name: "every fifteen minutes", expression: "*/15 * * * *",
			after: time.Date(2026, 10, 31, 4, 2, 0, 0, time.UTC),
			want:  time.Date(2026, 10, 31, 4, 15, 0, 0, time.UTC),
		},
		{
			name: "first of the month or Monday", expression: "0 0 1 * 1",
			after: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC),
			want:  time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "hour and minute lists", expression: "30 1,13 * * *",
			after: time.Date(2026, 10, 31, 2, 0, 0, 0, time.UTC),
			want:  time.Date(2026, 10, 31, 13, 30, 0, 0, time.UTC),
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			schedule, err := ParseSchedule(test.expression)
			if err != nil {
				t.Fatalf("parse %q: %v", test.expression, err)
			}
			if got := schedule.Next(test.after); !got.Equal(test.want) {
				t.Fatalf("next after %s = %s, want %s", test.after, got, test.want)
			}
		})
	}
}

// TestScheduleRejectsMalformed covers the values a policy's spec must refuse
// instead of running at an unintended instant (C-41).
func TestScheduleRejectsMalformed(t *testing.T) {
	for _, expression := range []string{
		"",
		"0 3 * *",
		"60 * * * *",
		"* 24 * * *",
		"*/0 * * * *",
		"0 0 * * x",
		"0 0 0 * *",
	} {
		t.Run(expression, func(t *testing.T) {
			if _, err := ParseSchedule(expression); err == nil {
				t.Fatalf("schedule %q was accepted", expression)
			}
		})
	}
}
