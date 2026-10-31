package backup

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed five-field cron expression: minute, hour, day of month,
// month and day of week (C-41). Each field is a value set, so `*/15`, `1-5` and
// `1,15` describe one set; a field never carries a name the kernel would have
// to translate.
type Schedule struct {
	minute, hour, day, month, weekday []bool
	isDayRestricted                   bool
	isWeekdayRestricted               bool
}

// ParseSchedule parses the five-field form; seconds and names are refused
// because the policy's schedule element is a cron expression and not a
// scheduler dialect (C-41).
func ParseSchedule(expression string) (Schedule, error) {
	fields := strings.Fields(expression)
	if len(fields) != 5 {
		return Schedule{}, fmt.Errorf("schedule %q needs five fields", expression)
	}
	minute, _, err := parseField(fields[0], 0, 59)
	if err != nil {
		return Schedule{}, fmt.Errorf("schedule minute: %w", err)
	}
	hour, _, err := parseField(fields[1], 0, 23)
	if err != nil {
		return Schedule{}, fmt.Errorf("schedule hour: %w", err)
	}
	day, dayRestricted, err := parseField(fields[2], 1, 31)
	if err != nil {
		return Schedule{}, fmt.Errorf("schedule day of month: %w", err)
	}
	month, _, err := parseField(fields[3], 1, 12)
	if err != nil {
		return Schedule{}, fmt.Errorf("schedule month: %w", err)
	}
	weekday, weekdayRestricted, err := parseField(fields[4], 0, 7)
	if err != nil {
		return Schedule{}, fmt.Errorf("schedule day of week: %w", err)
	}
	if weekday[7] {
		weekday[0] = true
	}
	return Schedule{
		minute: minute, hour: hour, day: day, month: month, weekday: weekday,
		isDayRestricted: dayRestricted, isWeekdayRestricted: weekdayRestricted,
	}, nil
}

// parseField reads one field into a value set. It reports whether the field was
// restricted, which decides how day of month and day of week combine.
func parseField(text string, minimum, maximum int) ([]bool, bool, error) {
	set := make([]bool, maximum+1)
	restricted := text != "*" && text != "?"
	for _, part := range strings.Split(text, ",") {
		step := 1
		rangeText := part
		if index := strings.Index(part, "/"); index >= 0 {
			rangeText = part[:index]
			parsed, err := strconv.Atoi(part[index+1:])
			if err != nil || parsed <= 0 {
				return nil, false, fmt.Errorf("step in %q is not a positive number", part)
			}
			step = parsed
		}
		low, high := minimum, maximum
		if rangeText != "*" && rangeText != "?" {
			if index := strings.Index(rangeText, "-"); index >= 0 {
				parsed, err := strconv.Atoi(rangeText[:index])
				if err != nil {
					return nil, false, fmt.Errorf("range start in %q is not a number", part)
				}
				low = parsed
				parsed, err = strconv.Atoi(rangeText[index+1:])
				if err != nil {
					return nil, false, fmt.Errorf("range end in %q is not a number", part)
				}
				high = parsed
			} else {
				parsed, err := strconv.Atoi(rangeText)
				if err != nil {
					return nil, false, fmt.Errorf("%q is not a number", part)
				}
				low = parsed
				if step == 1 {
					high = low
				}
			}
		}
		if low < minimum || high > maximum || low > high {
			return nil, false, fmt.Errorf("value in %q is outside %d-%d", part, minimum, maximum)
		}
		for value := low; value <= high; value += step {
			set[value] = true
		}
	}
	return set, restricted, nil
}

// Next returns the first instant after the given one that the schedule matches,
// on the process wall clock. A schedule that never matches yields the zero time.
func (s Schedule) Next(after time.Time) time.Time {
	instant := after.Truncate(time.Minute).Add(time.Minute)
	limit := instant.AddDate(5, 0, 0)
	for instant.Before(limit) {
		if !s.month[int(instant.Month())] {
			instant = time.Date(instant.Year(), instant.Month(), 1, 0, 0, 0, 0, instant.Location()).AddDate(0, 1, 0)
			continue
		}
		if !s.matchesDay(instant) {
			instant = time.Date(instant.Year(), instant.Month(), instant.Day(), 0, 0, 0, 0, instant.Location()).AddDate(0, 0, 1)
			continue
		}
		if !s.hour[instant.Hour()] {
			instant = time.Date(instant.Year(), instant.Month(), instant.Day(), instant.Hour(), 0, 0, 0, instant.Location()).Add(time.Hour)
			continue
		}
		if !s.minute[instant.Minute()] {
			instant = instant.Add(time.Minute)
			continue
		}
		return instant
	}
	return time.Time{}
}

// matchesDay applies the rule day of month and day of week share: when both are
// restricted, either one matching the day is enough.
func (s Schedule) matchesDay(instant time.Time) bool {
	dayMatches := s.day[instant.Day()]
	weekdayMatches := s.weekday[int(instant.Weekday())]
	if s.isDayRestricted && s.isWeekdayRestricted {
		return dayMatches || weekdayMatches
	}
	return dayMatches && weekdayMatches
}
