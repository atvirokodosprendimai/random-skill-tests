package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/atvirokodosprendimai/random-skill-tests/internal/core"
	"github.com/atvirokodosprendimai/random-skill-tests/internal/rate"
	"github.com/atvirokodosprendimai/random-skill-tests/internal/render"
)

// whenLayouts are tried in order against a --from/--to argument. Most specific
// first, so "2026-08-17 09:00" is never mistaken for a bare date.
var whenLayouts = []string{
	"2006-01-02T15:04:05",
	"2006-01-02T15:04",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
	"15:04:05",
	"15:04",
}

// parseWhen resolves a human time argument against now, in the local zone.
//
// A bare time of day ("09:00") means that time *today*, which is what someone
// typing `tempo add web --from 09:00` means every time. Everything is parsed in
// local time and converted to UTC downstream, because the log is canonical
// storage and the person is not.
func parseWhen(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("a time is required: %w", core.ErrInvalid)
	}

	switch strings.ToLower(s) {
	case "now":
		return now, nil
	case "today":
		return core.Day(now), nil
	case "yesterday":
		return core.Day(now).AddDate(0, 0, -1), nil
	case "tomorrow":
		return core.Day(now).AddDate(0, 0, 1), nil
	}

	for _, layout := range whenLayouts {
		t, err := time.ParseInLocation(layout, s, now.Location())
		if err != nil {
			continue
		}
		// A time-of-day layout parses to year zero; graft it onto today so the
		// result is an actual moment rather than 1 January year 0.
		if t.Year() == 0 {
			y, m, d := now.Date()
			t = time.Date(y, m, d, t.Hour(), t.Minute(), t.Second(), 0, now.Location())
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("%q is not a time I understand (try 09:00, 2026-08-17, or 2026-08-17 09:00): %w", s, core.ErrInvalid)
}

// resolveEnd turns the mutually exclusive --to / --for pair into an end time.
// Requiring exactly one of them is deliberate: defaulting a missing end to
// "now" would silently log an interval nobody asked for.
func resolveEnd(to, dur string, start, now time.Time) (time.Time, error) {
	to, dur = strings.TrimSpace(to), strings.TrimSpace(dur)
	switch {
	case to != "" && dur != "":
		return time.Time{}, fmt.Errorf("--to and --for are mutually exclusive: %w", core.ErrInvalid)
	case to == "" && dur == "":
		return time.Time{}, fmt.Errorf("one of --to or --for is required: %w", core.ErrInvalid)
	case dur != "":
		d, err := time.ParseDuration(dur)
		if err != nil {
			return time.Time{}, fmt.Errorf("--for %q: %w", dur, core.ErrInvalid)
		}
		return start.Add(d), nil
	default:
		end, err := parseWhen(to, now)
		if err != nil {
			return time.Time{}, fmt.Errorf("--to: %w", err)
		}
		// A bare end time earlier than the start means the interval crossed
		// midnight ("--from 23:00 --to 01:00"); roll it to the next day rather
		// than rejecting what the user plainly meant.
		if end.Before(start) && !strings.ContainsAny(to, "-/") {
			end = end.AddDate(0, 0, 1)
		}
		return end, nil
	}
}

// parseRange resolves a --from/--to pair for the read commands, defaulting to
// the last defaultDaysBack days through the end of today.
func parseRange(from, to string, now time.Time, defaultDaysBack int) (time.Time, time.Time, error) {
	start := core.Day(now).AddDate(0, 0, defaultDaysBack)
	if strings.TrimSpace(from) != "" {
		v, err := parseWhen(from, now)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("--from: %w", err)
		}
		start = v
	}
	// The default upper bound is tomorrow midnight because the window is
	// half-open: anything else would drop today's work from today's report.
	end := core.Day(now).AddDate(0, 0, 1)
	if strings.TrimSpace(to) != "" {
		v, err := parseWhen(to, now)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("--to: %w", err)
		}
		end = v
	}
	return start, end, nil
}

// printRates writes the rate table.
//
// Rates get their formatting here rather than in internal/render because they
// are a two-column list with no layout decisions to make; giving the renderer a
// method for them would be an abstraction with one caller and nothing to say.
func printRates(a *app, rs []rate.Rate) error {
	if len(rs) == 0 {
		_, err := fmt.Println("no rates set — try 'tempo rate set <project> <amount>'")
		return err
	}
	width := 0
	for _, r := range rs {
		if len(r.Project) > width {
			width = len(r.Project)
		}
	}
	for _, r := range rs {
		if _, err := fmt.Printf("%-*s  %s / hour\n", width, r.Project, render.Money(r.CentsPerHour, r.Currency)); err != nil {
			return err
		}
	}
	return nil
}
