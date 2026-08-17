// Package report projects tempo's event log into a rollup over an arbitrary
// span: a week, a month, or any half-open range of days.
//
// It is a read model and it never writes. Reader accepts a core.Loader rather
// than a core.Log precisely so that it cannot append — taking the wider
// interface would quietly grant write access to a package whose entire job is
// to look.
//
// Replay is not reimplemented here. core.BuildTimeline resolves the log into
// entries and core.Bucketize turns key→duration maps into shares; this package
// only decides how a span is carved into days and how an entry's time is
// attributed to them. All three read models share those two helpers, which is
// what keeps them from drifting apart on what an entry is or how a share is
// computed.
package report

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/atvirokodosprendimai/random-skill-tests/internal/core"
)

// maxWindowDays caps how wide a span Range will project. Days is dense — one
// element per calendar day — so an accidental year-9999 bound would ask for a
// multi-million element slice before any event is even read. Refusing is
// cheaper and far more informative than the allocation.
const maxWindowDays = 3660

// Reader projects the event log into a rollup over an arbitrary span.
//
// A Reader holds no projected state. It is safe for concurrent use provided the
// underlying core.Loader and core.Clock are.
type Reader struct {
	log   core.Loader
	clock core.Clock
}

// NewReader returns a Reader over log, measuring running entries against clock.
func NewReader(log core.Loader, clock core.Clock) *Reader {
	return &Reader{log: log, clock: clock}
}

// Range returns the rollup for [from, to), in from's location.
//
// The window is snapped to whole days: it runs from midnight of from's day up
// to, but not including, midnight of to's day, both taken in from's location.
// A to at or before from is reported as an error wrapping core.ErrInvalid, as
// is a span wider than 3660 days. A to after from that lands on the same
// calendar day is not an error: it resolves to an empty window and yields a
// valid zero report, the same shape an empty log produces.
//
// Time is attributed to days by overlap, so an entry that crosses midnight is
// split across the days it touches and Total always equals the sum of Days.
// ByProject partitions that total: its shares sum to 1. ByTag does not — an
// entry carrying N tags contributes its full duration to each one, because tags
// are labels rather than a partition, so tag shares can legitimately sum to
// more than 1.
func (r *Reader) Range(ctx context.Context, from, to time.Time) (core.RangeReport, error) {
	start, end, err := window(from, to)
	if err != nil {
		return core.RangeReport{}, err
	}

	// The log is re-read and re-folded on every call, deliberately: there is no
	// cached projection to go stale. A read model that remembered its answer
	// would keep serving it after the next append, which is the exact failure
	// CQRS exists to avoid. Folding is cheap next to being wrong.
	events, err := r.log.Load(ctx, 0)
	if err != nil {
		return core.RangeReport{}, fmt.Errorf("report: load events: %w", err)
	}

	now := r.clock()
	tl, err := core.BuildTimeline(events, now)
	if err != nil {
		return core.RangeReport{}, fmt.Errorf("report: build timeline: %w", err)
	}

	return rollUp(tl, start, end, now), nil
}

// Week returns the rollup for the ISO week (Monday-based) containing day.
//
// The week is resolved in day's own location: a Sunday closes the week that
// began six days earlier rather than opening a new one.
func (r *Reader) Week(ctx context.Context, day time.Time) (core.RangeReport, error) {
	start := core.Day(day)
	// Go numbers Sunday as 0; ISO weeks start on Monday. Rotating by 6 before
	// the modulo maps Monday→0 … Sunday→6, so the subtraction always walks back
	// to the correct Monday.
	start = start.AddDate(0, 0, -((int(start.Weekday()) + 6) % 7))
	return r.Range(ctx, start, start.AddDate(0, 0, 7))
}

// Month returns the rollup for the calendar month containing day.
//
// The month is resolved in day's own location and runs from the 1st through the
// 1st of the next month, so its length follows the calendar (28, 29, 30 or 31
// days) rather than a fixed stride.
func (r *Reader) Month(ctx context.Context, day time.Time) (core.RangeReport, error) {
	d := core.Day(day)
	y, m, _ := d.Date()
	start := time.Date(y, m, 1, 0, 0, 0, 0, d.Location())
	// AddDate on the 1st is unambiguous: it always lands on the 1st of the next
	// month, where the same call on the 31st would normalise into the month
	// after that.
	return r.Range(ctx, start, start.AddDate(0, 1, 0))
}

// window resolves the caller's instants into the half-open day range the report
// actually covers.
//
// Validation is on the RAW arguments, deliberately: only to <= from is refused.
// A sub-day span (09:00 to 17:00 on one date) truncates to an empty window and
// reports zero rather than failing. Every read model in tempo is handed the
// same "to before or equal to from is invalid" clause, so they must all read it
// the same way — internal/invoice answers that span with an empty result, and
// two commands disagreeing about identical flags is worse than either reading
// on its own.
//
// Days are taken in from's location. Entries are stored in UTC, so a window
// derived from raw UTC instants would silently open and close at the wrong wall
// clock for every caller who is not on Greenwich — an evening's work would land
// on tomorrow, or yesterday's on today.
func window(from, to time.Time) (start, end time.Time, err error) {
	if !to.After(from) {
		return time.Time{}, time.Time{}, fmt.Errorf(
			"report: range end %s is not after start %s: %w",
			to.Format(time.RFC3339), from.Format(time.RFC3339), core.ErrInvalid)
	}

	loc := from.Location()
	start = core.Day(from)
	end = core.Day(to.In(loc))

	// Sub saturates rather than wrapping, so even an absurd year-9999 bound
	// compares greater than the cap instead of overflowing into a small value.
	if end.Sub(start) > maxWindowDays*24*time.Hour {
		return time.Time{}, time.Time{}, fmt.Errorf(
			"report: range %s..%s spans more than %d days: %w",
			start.Format(time.RFC3339), end.Format(time.RFC3339), maxWindowDays, core.ErrInvalid)
	}
	return start, end, nil
}

// rollUp projects a folded timeline onto the day grid of [start, end).
func rollUp(tl core.Timeline, start, end, now time.Time) core.RangeReport {
	bounds := dayBounds(start, end)
	n := len(bounds) - 1

	// Days is dense: every calendar day in the window gets an element, idle
	// days included, in ascending order. A sparse series would let a sparkline
	// close the gap and draw a fortnight of three working days as three solid
	// days in a row — the zeros are the signal.
	days := make([]core.DayTotal, n)
	for i := range days {
		days[i] = core.DayTotal{Date: bounds[i]}
	}

	byProject := make(map[string]time.Duration)
	byTag := make(map[string]time.Duration)
	var total time.Duration

	for _, ev := range tl.Entries {
		s, e := interval(ev, now)
		// Clip to the window first: an entry that merely reaches into the span
		// contributes only the part inside it.
		if s.Before(start) {
			s = start
		}
		if e.After(end) {
			e = end
		}
		if !e.After(s) {
			continue
		}

		// Time is attributed per day by overlap, never wholesale to the start
		// day: a session from 23:00 to 01:00 puts one hour on each side of
		// midnight, not two hours on the day it began. Because the day bounds
		// tile the window exactly, every nanosecond of the clipped interval
		// lands in exactly one day — which is what makes Days sum precisely to
		// Total, with no rounding slack.
		i := sort.Search(n, func(i int) bool { return bounds[i+1].After(s) })
		for ; i < n && bounds[i].Before(e); i++ {
			d := overlap(s, e, bounds[i], bounds[i+1])
			if d <= 0 {
				continue
			}
			days[i].Duration += d
			total += d
			byProject[ev.Project] += d
			// A multi-tag entry contributes its FULL overlap to each of its
			// tags. Tags are labels, not a partition, so ByTag deliberately
			// double-counts and its shares may sum to more than 1; treating a
			// tag total as a slice of the day would be the actual bug.
			for _, tag := range ev.Tags {
				byTag[tag] += d
			}
		}
	}

	return core.RangeReport{
		From:      start,
		To:        end,
		Total:     total,
		Days:      days,
		ByProject: core.Bucketize(byProject, total),
		ByTag:     core.Bucketize(byTag, total),
	}
}

// dayBounds returns the n+1 midnights fencing the n days of [start, end). An
// empty window yields a single bound and therefore zero days, which is what
// makes a sub-day span fall out as a valid zero report instead of a special
// case threaded through the caller.
//
// It steps with AddDate rather than adding 24h so the fence stays on local
// midnight across a daylight-saving change, where a calendar day is 23 or 25
// hours long and a fixed stride would drift off the boundary for the rest of
// the window.
func dayBounds(start, end time.Time) []time.Time {
	bounds := make([]time.Time, 0, 8)
	for d := start; d.Before(end); d = d.AddDate(0, 0, 1) {
		bounds = append(bounds, d)
	}
	return append(bounds, end)
}

// interval returns the absolute span an entry occupies.
//
// A running entry has a zero End and is measured up to now, the same instant
// core.BuildTimeline used for its Duration, so a live timer is clipped to the
// window exactly like a finished one.
func interval(ev core.EntryView, now time.Time) (start, end time.Time) {
	if ev.Running {
		return ev.Start, now
	}
	return ev.Start, ev.End
}

// overlap returns the length of the intersection of [aStart, aEnd) and
// [bStart, bEnd), or zero when they do not meet.
func overlap(aStart, aEnd, bStart, bEnd time.Time) time.Duration {
	s := aStart
	if bStart.After(s) {
		s = bStart
	}
	e := aEnd
	if bEnd.Before(e) {
		e = bEnd
	}
	if !e.After(s) {
		return 0
	}
	return e.Sub(s)
}
