package schedule

import (
	"testing"
	"time"
	_ "time/tzdata"
)

func TestNext(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ spec, from, want string }{
		{"* * * * *", "2024-01-01T00:00:59-05:00", "2024-01-01T00:01:00-05:00"},
		{"1,7,9 2-4 * * *", "2024-01-01T02:07:00-05:00", "2024-01-01T02:09:00-05:00"},
		{"*/15 6-10/2 * 2,4 *", "2024-02-01T06:45:00-05:00", "2024-02-01T08:00:00-05:00"},
		{"0 1 15 * 1", "2024-01-15T01:00:00-05:00", "2024-01-22T01:00:00-05:00"},
		{"0 1 16 * 1", "2024-01-15T01:00:00-05:00", "2024-01-16T01:00:00-05:00"},
		{"0 0 29 2 *", "2096-02-29T00:00:00-05:00", "2104-02-29T00:00:00-05:00"},
		{"0 0 31 * *", "2024-04-01T00:00:00-04:00", "2024-05-31T00:00:00-04:00"},
		{"every 1m", "2024-01-01T02:07:23-05:00", "2024-01-01T02:08:23-05:00"},
	} {
		t.Run(tc.spec, func(t *testing.T) {
			start, err := time.Parse(time.RFC3339, tc.from)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Next(tc.spec, start.In(loc))
			if err != nil || got.Format(time.RFC3339) != tc.want {
				t.Fatalf("got %v, %v; want %s", got, err, tc.want)
			}
		})
	}
}

func TestNextAcrossClockChanges(t *testing.T) {
	for _, tc := range []struct{ zone, spec, from, want string }{
		{"America/New_York", "30 2 * * 0", "2024-03-09T00:00:00-05:00", "2024-03-10T03:00:00-04:00"},
		{"America/New_York", "30 2 * * 0", "2024-03-10T03:00:00-04:00", "2024-03-17T02:30:00-04:00"},
		{"America/New_York", "30 2 * * 1", "2024-03-10T01:59:00-05:00", "2024-03-11T02:30:00-04:00"},
		{"America/New_York", "30 1 * * 7", "2024-11-03T00:59:00-04:00", "2024-11-03T01:30:00-04:00"},
		{"America/New_York", "30 1 * * 7", "2024-11-03T01:30:00-04:00", "2024-11-10T01:30:00-05:00"},
		{"Europe/Berlin", "30 2 * * *", "2024-03-31T01:59:59+01:00", "2024-03-31T03:00:00+02:00"},
		{"Europe/Berlin", "30 2 * * *", "2024-10-27T01:59:00+02:00", "2024-10-27T02:30:00+02:00"},
		{"Europe/Berlin", "30 2 * * *", "2024-10-27T02:10:00+01:00", "2024-10-28T02:30:00+01:00"},
		{"Europe/Berlin", "*/15 * * * *", "2024-10-27T02:45:00+02:00", "2024-10-27T02:00:00+01:00"},
		// A stepped minute in a fixed hour does not run the repeated hour a second time.
		{"Europe/Berlin", "*/15 2 * * *", "2024-10-27T02:45:00+02:00", "2024-10-28T02:00:00+01:00"},
		// An hour range counts as fixed: its skipped 02:00 runs at the change.
		{"Europe/Berlin", "0 1-3 * * *", "2024-03-31T01:59:00+01:00", "2024-03-31T03:00:00+02:00"},
		{"Europe/Berlin", "15 0-4/2 * * *", "2024-10-27T02:45:00+02:00", "2024-10-27T02:15:00+01:00"},
		{"Europe/Berlin", "15 */2 * * *", "2024-03-31T01:59:00+01:00", "2024-03-31T04:15:00+02:00"},
	} {
		t.Run(tc.zone+"/"+tc.spec+"/"+tc.from, func(t *testing.T) {
			loc, err := time.LoadLocation(tc.zone)
			if err != nil {
				t.Fatal(err)
			}
			from, err := time.Parse(time.RFC3339, tc.from)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Next(tc.spec, from.In(loc))
			if err != nil || got.Format(time.RFC3339) != tc.want {
				t.Fatalf("got %v, %v; want %s", got, err, tc.want)
			}
		})
	}
}

func TestInvalidSpecs(t *testing.T) {
	for _, spec := range []string{"every 59s", "every nonsense", "* * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "* * * * 8", "*/0 * * * *", "1-0 * * * *", "1,,3 * * * *", "2/3 * * * *", "0 0 31 2 *"} {
		if _, err := Next(spec, time.Now()); err == nil {
			t.Errorf("accepted %q", spec)
		}
	}
}
