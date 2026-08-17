// Package summary is tempo's day read model: it answers "what did I do today".
//
// It is a pure projection. The event log is the only truth, so this package
// never appends and never mutates — its Reader takes a core.Loader rather than
// a core.Log precisely so that write access is not available to it even by
// accident. Everything it reports is a fold over events plus arithmetic.
//
// The projection is built on the kernel's canonical helpers, core.BuildTimeline
// for replay and core.Bucketize for grouping, rather than on private
// reimplementations. Three read models (day, range, invoice) ask overlapping
// questions of the same log; sharing the replay and the grouping is what keeps
// them from quietly disagreeing about a total.
//
// # Midnight clipping
//
// A day view is a window, and an entry that crosses the window boundary belongs
// to both days. Such an entry contributes only its overlap with the window to
// Total and to both breakdowns, but the core.EntryView listed in Entries keeps
// its true Start and End so the reader still sees when the work actually
// happened. Only its Duration is the clipped amount — see Reader.Day.
//
// # Tag shares
//
// Tags are not a partition. An entry tagged both "dev" and "billable" counts in
// full toward each, so the shares in ByTag can sum to more than 1. That is
// intentional; see Reader.Day.
package summary

import (
	"context"
	"fmt"
	"time"

	"github.com/atvirokodosprendimai/random-skill-tests/internal/core"
)

// Reader projects the event log into a single day's view.
//
// A Reader holds no projected state. It is safe to keep one for the lifetime of
// the process and to call Day on it repeatedly; each call re-reads the log.
type Reader struct {
	log   core.Loader
	clock core.Clock
}

// NewReader returns a Reader over log, measuring running entries against clock.
//
// clock is what turns an open interval into a duration: a still-running entry
// has no End, so "how long so far" is only answerable relative to some now.
// Injecting it here is what makes the day view deterministic under test.
func NewReader(log core.Loader, clock core.Clock) *Reader {
	return &Reader{log: log, clock: clock}
}

// Day returns the view for the calendar day containing day, in day's own
// location.
//
// The window is [core.Day(day), core.Day(day)+24h), evaluated in day's location.
// An entry is included when its interval overlaps that window at all, not merely
// when it starts inside it, so work begun the previous evening still shows up on
// the day it ran into.
//
// Durations are clipped to the window. An entry running 23:00–01:00 contributes
// one hour to this day and one hour to the next, to Total and to both
// breakdowns alike. Its listed core.EntryView keeps the true Start and End —
// losing them would misreport when the work happened — while its Duration
// carries the clipped amount, which is what makes the listed durations sum
// exactly to Total.
//
// ByProject partitions the day: every entry belongs to exactly one project, so
// those shares sum to 1. ByTag does not. An entry with N tags contributes its
// full clipped duration to each of its N tags, because "how much of today was
// billable" and "how much was dev" are independent questions that may well
// overlap. Tag shares can therefore sum to more than 1, and an entry with no
// tags appears in ByProject while contributing to no tag bucket at all.
//
// Running is non-nil when an entry overlapping this window is still open; it is
// a copy of that entry exactly as it appears in Entries, clipped Duration
// included.
//
// An empty log is not an error: it yields a zero-Total view with non-nil empty
// slices and a nil Running, which is what the renderer needs in order to draw an
// empty state.
func (r *Reader) Day(ctx context.Context, day time.Time) (core.DayView, error) {
	// The whole log is re-read and re-folded on every call. Caching the
	// timeline would be the one failure mode this architecture exists to rule
	// out: a projection that answers from state older than the log has silently
	// stopped being a projection.
	events, err := r.log.Load(ctx, 0)
	if err != nil {
		return core.DayView{}, fmt.Errorf("summary: load events: %w", err)
	}

	// The same now measures running entries in the fold and in the clipping
	// below; reading the clock twice could place an entry's end on either side
	// of a boundary depending on which read won.
	now := r.clock()

	timeline, err := core.BuildTimeline(events, now)
	if err != nil {
		return core.DayView{}, fmt.Errorf("summary: replay log: %w", err)
	}

	// core.Day truncates in day's own location, so these two instants are the
	// caller's real midnights. Entries are stored in UTC and time.Time
	// comparison is instant-based, which makes the comparisons below correct
	// without converting anything; what would be wrong is truncating a
	// UTC-normalised day, since that shifts every non-UTC user's boundary by
	// their offset.
	windowStart := core.Day(day)
	windowEnd := windowStart.AddDate(0, 0, 1)

	view := core.DayView{
		Date: windowStart,
		// Non-nil even when nothing matches: the renderer distinguishes "no
		// work" from "no data", and a nil slice reads as neither.
		Entries: make([]core.EntryView, 0, len(timeline.Entries)),
	}
	byProject := make(map[string]time.Duration)
	byTag := make(map[string]time.Duration)

	// timeline.Entries is already sorted by Start then ID; filtering in place
	// preserves that, so Entries needs no re-sort.
	for _, entry := range timeline.Entries {
		start, end := interval(entry, now)
		if !start.Before(windowEnd) || !end.After(windowStart) {
			continue
		}

		clipped := overlap(start, end, windowStart, windowEnd)

		listed := entry
		listed.Duration = clipped
		view.Entries = append(view.Entries, listed)
		view.Total += clipped

		byProject[entry.Project] += clipped
		for _, tag := range entry.Tags {
			// Full clipped duration per tag, deliberately: see the doc comment.
			byTag[tag] += clipped
		}

		if listed.Running && view.Running == nil {
			// Copy so Running survives any later reslicing of Entries and
			// carries the same clipped Duration the list shows.
			running := listed
			view.Running = &running
		}
	}

	// Both breakdowns are shared against the day's Total, not against their own
	// column sums. For ByProject the two are the same number; for ByTag they are
	// not, and "fraction of my day" is the answer worth rendering.
	view.ByProject = core.Bucketize(byProject, view.Total)
	view.ByTag = core.Bucketize(byTag, view.Total)

	return view, nil
}

// interval resolves an entry to a well-formed [start, end) pair. A running entry
// has no End and is measured against now, matching core.BuildTimeline. An end
// before its start — a clock that jumped backwards, or an edit that moved Start
// past End — collapses to a zero-length interval rather than being allowed to
// subtract from the day's total.
func interval(entry core.EntryView, now time.Time) (start, end time.Time) {
	start, end = entry.Start, entry.End
	if entry.Running {
		end = now
	}
	if end.Before(start) {
		end = start
	}
	return start, end
}

// overlap returns the length of the intersection of [start, end) with
// [windowStart, windowEnd). Callers have already established that the two
// intervals intersect, so the result is never negative.
func overlap(start, end, windowStart, windowEnd time.Time) time.Duration {
	if start.Before(windowStart) {
		start = windowStart
	}
	if end.After(windowEnd) {
		end = windowEnd
	}
	return end.Sub(start)
}
