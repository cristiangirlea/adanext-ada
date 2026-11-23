// Package schedule runs locally configured prompt and script jobs.
package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type field uint64

func (f field) has(n int) bool { return f&(1<<n) != 0 }

func number(s string) (int, error) {
	n, err := strconv.ParseUint(s, 10, 31)
	if err != nil {
		return 0, fmt.Errorf("invalid cron number %q: %w", s, err)
	}
	return int(n), nil
}

func parseField(s string, low, high int) (field, error) {
	var mask field
	for _, part := range strings.Split(s, ",") {
		span, stride, stepped := strings.Cut(part, "/")
		first, last, step := low, high, 1
		if span != "*" {
			a, b, ranged := strings.Cut(span, "-")
			var err error
			first, err = number(a)
			if err != nil {
				return 0, err
			}
			last = first
			if ranged {
				last, err = number(b)
				if err != nil {
					return 0, err
				}
			} else if stepped {
				return 0, fmt.Errorf("step requires a range or *")
			}
		}
		if stepped {
			var err error
			step, err = number(stride)
			if err != nil {
				return 0, err
			}
		}
		if first < low || last > high || first > last || step < 1 {
			return 0, fmt.Errorf("invalid cron field %q (range %d-%d)", part, low, high)
		}
		for n := first; ; n += step {
			mask |= 1 << n
			if step > last-n {
				break
			}
		}
	}
	return mask, nil
}

// Next uses after's location so callers can evaluate the same spec across clock changes.
func Next(spec string, after time.Time) (time.Time, error) {
	parts := strings.Fields(spec)
	if len(parts) == 2 && parts[0] == "every" {
		d, err := time.ParseDuration(parts[1])
		if err != nil {
			return time.Time{}, fmt.Errorf("parse interval: %w", err)
		}
		if d < time.Minute {
			return time.Time{}, fmt.Errorf("interval must be at least 1m")
		}
		return after.Add(d), nil
	}
	if len(parts) != 5 {
		return time.Time{}, fmt.Errorf("expected five cron fields or every <duration>")
	}
	bounds := [...][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}
	var fields [5]field
	for i, b := range bounds {
		f, err := parseField(parts[i], b[0], b[1])
		if err != nil {
			return time.Time{}, fmt.Errorf("cron field %d: %w", i+1, err)
		}
		fields[i] = f
	}
	// Return zero for a match, otherwise advance to the next possible local minute.
	advance := func(t time.Time) time.Duration {
		weekday := int(t.Weekday())
		dow := fields[4].has(weekday) || weekday == 0 && fields[4].has(7)
		dom := fields[2].has(t.Day())
		day := dom && dow
		if parts[2] != "*" && parts[4] != "*" {
			day = dom || dow
		}
		if !day || !fields[3].has(int(t.Month())) {
			return time.Duration(1440-t.Hour()*60-t.Minute()) * time.Minute
		}
		if !fields[1].has(t.Hour()) {
			return time.Duration(60-t.Minute()) * time.Minute
		}
		if fields[0].has(t.Minute()) {
			return 0
		}
		return time.Minute
	}
	fixedHours := !strings.ContainsAny(parts[1], "*/")
	// Even a leap day across a non-leap century occurs within eight years.
	limit := after.AddDate(9, 0, 0)
	for t := after.Truncate(time.Minute).Add(time.Minute); t.Before(limit); {
		start, end := t.ZoneBounds()
		if fixedHours && !start.IsZero() {
			_, before := start.Add(-time.Nanosecond).Zone()
			_, current := t.Zone()
			shift := time.Duration(current-before) * time.Second
			if shift < 0 && t.Before(start.Add(-shift)) {
				t = start.Add(-shift)
				continue
			}
			if shift > 0 && t.Equal(start) {
				// Use the old offset to walk the local minutes the clock skipped.
				missing := start.In(time.FixedZone("", before))
				for stop := missing.Add(shift); missing.Before(stop); {
					step := advance(missing)
					if step == 0 {
						return t, nil
					}
					missing = missing.Add(step)
				}
			}
		}
		step := advance(t)
		if step == 0 {
			return t, nil
		}
		next := t.Add(step)
		if end.After(t) && end.Before(next) {
			next = end
		}
		t = next
	}
	return time.Time{}, fmt.Errorf("cron schedule %q has no matching date", spec)
}
