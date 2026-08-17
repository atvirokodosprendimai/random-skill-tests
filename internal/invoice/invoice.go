// Package invoice is the read model for billable work: the event log replayed,
// enriched with the rate that applied, and projected into money.
//
// The enrichment lives here, on the read side, and never in the write path. The
// log records that work happened; what that work is worth is a question asked at
// render time, against whatever rate the log says applies. Storing money in the
// events themselves would freeze an answer that the next RateSet is allowed to
// change.
//
// A Reader accepts core.Loader rather than core.Log on purpose. A read model
// that takes the wider interface has quietly granted itself write access, and
// this package is precisely where that would be tempting: an invoice is the one
// view whose output someone might want to "save".
package invoice

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/atvirokodosprendimai/random-skill-tests/internal/core"
)

// The units of the duration→money conversion, in nanoseconds. They are int64
// rather than time.Duration because they are multiplied by a cents-per-hour
// rate, and a Duration typed value there would invite treating cents as a
// duration.
const (
	hourNanos     = int64(time.Hour)
	halfHourNanos = hourNanos / 2
)

// Reader projects the event log into a billable invoice.
//
// It is safe for concurrent use: it holds no derived state, only its two
// dependencies. Everything it reports is folded fresh from the log on each call.
type Reader struct {
	log   core.Loader
	clock core.Clock
}

// NewReader returns a Reader over log, measuring running entries against clock.
//
// log is a core.Loader, not a core.Log: an invoice is a projection of the log
// and has no business appending to it.
func NewReader(log core.Loader, clock core.Clock) *Reader {
	return &Reader{log: log, clock: clock}
}

// For returns the invoice for project over [from, to).
//
// The window is [core.Day(from), core.Day(to)) resolved in from's location:
// billing is a calendar question, and a client reading "March" means their
// midnights, not UTC's. Entries are stored in UTC and converted before they are
// compared or split. A to that is not after from wraps core.ErrInvalid, as does
// a project that is empty once trimmed.
//
// The rate is whatever the log last recorded for project. A project with no rate
// wraps core.ErrNotFound: an invoice with no rate is not an empty invoice, it is
// an unanswerable question, and answering it with zero would be a bill someone
// might send. A project that has a rate but no time in the window is a different
// thing entirely — that is a valid invoice with a zero total and a non-nil,
// empty Lines slice, which is what a renderer needs to draw an empty state.
//
// Each line covers exactly one calendar day: an entry that spans midnight is
// clipped to the window and split across the days it touches. Days with no
// billable time are omitted, which is a deliberate departure from the range
// report's dense daily sparkline — a sparkline needs its zeros to keep its
// shape, whereas a client-facing invoice must not list days it is not charging
// for.
func (r *Reader) For(ctx context.Context, project string, from, to time.Time) (core.Invoice, error) {
	project = strings.TrimSpace(project)
	if project == "" {
		return core.Invoice{}, fmt.Errorf("invoice: project must not be empty: %w", core.ErrInvalid)
	}
	// Validated against the caller's own arguments rather than the truncated
	// window, so that an inverted span is reported as the mistake it is instead
	// of silently collapsing to an empty invoice.
	if !to.After(from) {
		return core.Invoice{}, fmt.Errorf("invoice: %q: to %s must be after from %s: %w",
			project, to.Format(time.RFC3339), from.Format(time.RFC3339), core.ErrInvalid)
	}

	// The window is half-open and lives in from's location; to is converted into
	// that location first so that a caller mixing zones still gets one coherent
	// run of calendar days rather than a window whose ends disagree.
	loc := from.Location()
	winStart := core.Day(from)
	winEnd := core.Day(to.In(loc))

	// The whole log, on every call, with nothing cached between them. The log is
	// the truth; a cached fold is the stale-read failure mode that CQRS exists to
	// avoid, and on an invoice it would mean billing against a rate or an entry
	// that has since been corrected.
	events, err := r.log.Load(ctx, 0)
	if err != nil {
		return core.Invoice{}, fmt.Errorf("invoice: %q: load log: %w", project, err)
	}
	now := r.clock()
	tl, err := core.BuildTimeline(events, now)
	if err != nil {
		return core.Invoice{}, fmt.Errorf("invoice: %q: %w", project, err)
	}

	centsPerHour, ok := tl.Rates[project]
	if !ok {
		return core.Invoice{}, fmt.Errorf("invoice: %q: no rate recorded: %w", project, core.ErrNotFound)
	}
	// A negative rate cannot come from a valid command, but it can come from a
	// log written by a buggy or older binary, and half-up rounding is only
	// defined here for non-negative money. Refuse rather than emit a credit note
	// nobody asked for.
	if centsPerHour < 0 {
		return core.Invoice{}, fmt.Errorf("invoice: %q: negative rate %d cents/hour: %w", project, centsPerHour, core.ErrInvalid)
	}
	currency := tl.Currencies[project]

	days := collect(tl, project, winStart, winEnd, now)

	// Non-nil even when empty: the renderer distinguishes "no billable days" from
	// "no invoice", and a nil slice reads as the latter.
	lines := make([]core.InvoiceLine, 0, len(days))
	var total time.Duration
	var totalCents int64
	for _, d := range days {
		amount, err := lineAmount(d.duration, centsPerHour)
		if err != nil {
			return core.Invoice{}, fmt.Errorf("invoice: %q: %s: %w", project, d.date.Format("2006-01-02"), err)
		}
		lines = append(lines, core.InvoiceLine{
			Date:        d.date,
			Notes:       d.notes,
			Duration:    d.duration,
			AmountCents: amount,
		})
		total += d.duration
		// TotalCents is the sum of the line amounts, never a fresh computation
		// from total. A client adds up the column; if the footer were computed
		// independently from the total duration, per-line rounding would make it
		// disagree with the lines by a cent or two, and that is exactly the kind
		// of error a client notices and a bookkeeper cannot reconcile.
		totalCents += amount
	}

	return core.Invoice{
		Project:      project,
		From:         winStart,
		To:           winEnd,
		Currency:     currency,
		CentsPerHour: centsPerHour,
		Total:        total,
		Lines:        lines,
		TotalCents:   totalCents,
	}, nil
}

// dayLine accumulates one calendar day of billable work while entries are being
// clipped and split. It is separate from core.InvoiceLine because money is not
// computed until the day's duration is final — rounding a partial day and adding
// the pieces would round more than once.
type dayLine struct {
	date     time.Time
	duration time.Duration
	notes    []string
	// seen de-duplicates notes; the notes slice preserves first-seen order,
	// which the map cannot.
	seen map[string]struct{}
}

// addNote records note for the day unless it is blank or already present.
// Notes are trimmed: an entry noted with stray whitespace is the same line item
// as one without, and a whitespace-only note would render as an empty bullet on
// a document a client reads.
func (d *dayLine) addNote(note string) {
	note = strings.TrimSpace(note)
	if note == "" {
		return
	}
	if _, dup := d.seen[note]; dup {
		return
	}
	d.seen[note] = struct{}{}
	d.notes = append(d.notes, note)
}

// collect folds the timeline's entries for project into per-day accumulators,
// ascending by date. Entries are clipped to [winStart, winEnd) and split across
// every calendar day they touch, so that each accumulator — and therefore each
// invoice line — covers exactly one day.
//
// now closes running entries. A running entry is billable up to the moment the
// invoice is drawn and is clipped like any other, so an open timer cannot bill
// past the end of the window.
func collect(tl core.Timeline, project string, winStart, winEnd, now time.Time) []*dayLine {
	loc := winStart.Location()

	// Keyed by time.Time, which is sound here because every key comes from
	// core.Day applied to a time already converted into loc: the values carry no
	// monotonic reading and share one *Location, so equal days are ==.
	byDay := make(map[time.Time]*dayLine)

	// tl.Entries is sorted by Start then ID, so iterating it gives notes a
	// deterministic first-seen order.
	for _, ev := range tl.Entries {
		if ev.Project != project {
			continue
		}

		start, end := ev.Start, ev.End
		if ev.Running {
			end = now
		}
		// Entries are stored UTC; days are a question about loc.
		start, end = start.In(loc), end.In(loc)

		// Clip to the window before splitting, so a long entry contributes only
		// the part that falls inside it.
		if start.Before(winStart) {
			start = winStart
		}
		if end.After(winEnd) {
			end = winEnd
		}
		// Covers the entry that falls entirely outside the window and the
		// backwards interval a clock jump or a bad edit can leave behind; core's
		// own fold clamps those to zero rather than subtracting from a total.
		if !end.After(start) {
			continue
		}

		for day := core.Day(start); day.Before(end); {
			// Calendar arithmetic, not a fixed 24h step: across a DST boundary
			// the day is 23 or 25 hours long and the billed segment must be too.
			next := day.AddDate(0, 0, 1)

			segStart, segEnd := day, next
			if start.After(segStart) {
				segStart = start
			}
			if end.Before(segEnd) {
				segEnd = end
			}

			d, ok := byDay[day]
			if !ok {
				d = &dayLine{date: day, seen: make(map[string]struct{})}
				byDay[day] = d
			}
			d.duration += segEnd.Sub(segStart)
			d.addNote(ev.Note)

			day = next
		}
	}

	days := make([]*dayLine, 0, len(byDay))
	for _, d := range byDay {
		days = append(days, d)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].date.Before(days[j].date) })

	return days
}

// lineAmount converts one day's duration into cents at centsPerHour.
//
// The arithmetic is integer throughout: cents are the unit of account, and a
// float would introduce a representation error that compounds over a long
// invoice. Adding half an hour's worth of nanoseconds before dividing by an hour
// is round-half-up — the convention a client expects on an invoice, where a
// half-cent is billed rather than absorbed — and it is exact only because both
// operands are non-negative here.
//
// The multiplication is guarded rather than trusted: an absurd rate times a long
// duration overflows int64, and an overflowed product wraps to a negative
// amount, which would print as a credit on a bill. A refusal wrapping
// core.ErrInvalid is the honest answer.
func lineAmount(d time.Duration, centsPerHour int64) (int64, error) {
	nanos := int64(d)
	if centsPerHour > 0 && nanos > (math.MaxInt64-halfHourNanos)/centsPerHour {
		return 0, fmt.Errorf("duration %s at %d cents/hour overflows int64 cents: %w", d, centsPerHour, core.ErrInvalid)
	}
	return (nanos*centsPerHour + halfHourNanos) / hourNanos, nil
}
