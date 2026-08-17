package invoice

import (
	"context"
	"errors"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/random-skill-tests/internal/core"
)

// fakeLoader is an in-memory core.Loader. It counts Load calls so a test can
// assert that the Reader re-reads the log instead of caching a fold.
type fakeLoader struct {
	events []core.Event
	err    error
	loads  int
}

func (f *fakeLoader) Load(_ context.Context, sinceSeq int64) ([]core.Event, error) {
	f.loads++
	if f.err != nil {
		return nil, f.err
	}
	out := make([]core.Event, 0, len(f.events))
	for _, e := range f.events {
		if e.Seq > sinceSeq {
			out = append(out, e)
		}
	}
	return out, nil
}

// newLog assigns the store-side Seq the events would have received on append and
// returns a Loader over them. The events are cloned: parallel subtests share the
// slices a table is built from, and stamping Seq in place would write to them.
func newLog(events ...core.Event) *fakeLoader {
	cloned := slices.Clone(events)
	for i := range cloned {
		cloned[i].Seq = int64(i + 1)
	}
	return &fakeLoader{events: cloned}
}

// rateSet builds the event that records a project's billing rate.
func rateSet(t *testing.T, project string, cents int64, currency string) core.Event {
	t.Helper()
	ev, err := core.NewEvent(core.KindRateSet, core.RatesAggregate, time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC), core.RateSet{
		Project:      project,
		CentsPerHour: cents,
		Currency:     currency,
	})
	if err != nil {
		t.Fatalf("build rate.set event: %v", err)
	}
	return ev
}

// entry builds a completed work interval. Start and End are stored in UTC, as
// the real write models store them, so the tests exercise the read model's
// conversion into the window's location rather than sidestepping it.
func entry(t *testing.T, id, project, note string, start, end time.Time) core.Event {
	t.Helper()
	ev, err := core.NewEvent(core.KindEntryAdded, core.EntryAggregate(id), start, core.EntryAdded{
		EntryID: id,
		Project: project,
		Note:    note,
		Start:   start.UTC(),
		End:     end.UTC(),
	})
	if err != nil {
		t.Fatalf("build entry.added event: %v", err)
	}
	return ev
}

// started builds an open interval: a timer with no matching stop.
func started(t *testing.T, id, project, note string, start time.Time) core.Event {
	t.Helper()
	ev, err := core.NewEvent(core.KindTimerStarted, core.TimerAggregate, start, core.TimerStarted{
		EntryID:   id,
		Project:   project,
		Note:      note,
		StartedAt: start.UTC(),
	})
	if err != nil {
		t.Fatalf("build timer.started event: %v", err)
	}
	return ev
}

// utc builds a UTC instant on the March 2026 test dates.
func utc(day, hour, min int) time.Time {
	return time.Date(2026, time.March, day, hour, min, 0, 0, time.UTC)
}

// utcDay builds UTC midnight, the shape core.Day produces for a UTC window.
func utcDay(day int) time.Time {
	return time.Date(2026, time.March, day, 0, 0, 0, 0, time.UTC)
}

// wantLine is the expected shape of one core.InvoiceLine.
type wantLine struct {
	date     time.Time
	notes    []string
	duration time.Duration
	cents    int64
}

func TestReaderFor(t *testing.T) {
	t.Parallel()

	// berlin is a fixed +02:00 zone rather than a named one: the point of the
	// zone cases is that days are cut in the window's location, not that tzdata
	// is installed.
	berlin := time.FixedZone("CEST", 2*60*60)

	tests := []struct {
		name         string
		events       []core.Event
		now          time.Time
		project      string
		from, to     time.Time
		wantCurrency string
		wantRate     int64
		wantFrom     time.Time
		wantTo       time.Time
		wantTotal    time.Duration
		wantCents    int64
		wantLines    []wantLine
	}{
		{
			name: "single day single entry bills exact money",
			events: []core.Event{
				rateSet(t, "acme", 9000, "EUR"),
				entry(t, "e1", "acme", "spec review", utc(2, 9, 0), utc(2, 10, 30)),
			},
			now:          utc(5, 12, 0),
			project:      "acme",
			from:         utcDay(2),
			to:           utcDay(3),
			wantCurrency: "EUR",
			wantRate:     9000,
			wantFrom:     utcDay(2),
			wantTo:       utcDay(3),
			wantTotal:    90 * time.Minute,
			// 1.5h at 90.00/h is exactly 135.00; no rounding involved.
			wantCents: 13500,
			wantLines: []wantLine{
				{date: utcDay(2), notes: []string{"spec review"}, duration: 90 * time.Minute, cents: 13500},
			},
		},
		{
			name: "half a cent rounds up",
			events: []core.Event{
				rateSet(t, "acme", 1, "EUR"),
				entry(t, "e1", "acme", "", utc(2, 9, 0), utc(2, 9, 30)),
			},
			now:          utc(5, 12, 0),
			project:      "acme",
			from:         utcDay(2),
			to:           utcDay(3),
			wantCurrency: "EUR",
			wantRate:     1,
			wantFrom:     utcDay(2),
			wantTo:       utcDay(3),
			wantTotal:    30 * time.Minute,
			// 0.5h at 1 cent/h is exactly half a cent: half-up bills it.
			wantCents: 1,
			wantLines: []wantLine{
				{date: utcDay(2), duration: 30 * time.Minute, cents: 1},
			},
		},
		{
			name: "total is the sum of the lines not of the total duration",
			events: []core.Event{
				rateSet(t, "acme", 1, "EUR"),
				entry(t, "e1", "acme", "", utc(2, 9, 0), utc(2, 9, 30)),
				entry(t, "e2", "acme", "", utc(3, 9, 0), utc(3, 9, 30)),
			},
			now:          utc(5, 12, 0),
			project:      "acme",
			from:         utcDay(2),
			to:           utcDay(4),
			wantCurrency: "EUR",
			wantRate:     1,
			wantFrom:     utcDay(2),
			wantTo:       utcDay(4),
			wantTotal:    time.Hour,
			// Two half-cent lines round up to 1 each. A footer computed from the
			// 1h total would say 1 cent and disagree with the column.
			wantCents: 2,
			wantLines: []wantLine{
				{date: utcDay(2), duration: 30 * time.Minute, cents: 1},
				{date: utcDay(3), duration: 30 * time.Minute, cents: 1},
			},
		},
		{
			name: "days without billable time are omitted",
			events: []core.Event{
				rateSet(t, "acme", 6000, "EUR"),
				entry(t, "e1", "acme", "monday", utc(2, 9, 0), utc(2, 11, 0)),
				entry(t, "e2", "acme", "wednesday", utc(4, 9, 0), utc(4, 10, 0)),
			},
			now:          utc(6, 12, 0),
			project:      "acme",
			from:         utcDay(2),
			to:           utcDay(6),
			wantCurrency: "EUR",
			wantRate:     6000,
			wantFrom:     utcDay(2),
			wantTo:       utcDay(6),
			wantTotal:    3 * time.Hour,
			wantCents:    18000,
			wantLines: []wantLine{
				{date: utcDay(2), notes: []string{"monday"}, duration: 2 * time.Hour, cents: 12000},
				{date: utcDay(4), notes: []string{"wednesday"}, duration: time.Hour, cents: 6000},
			},
		},
		{
			name: "entry spanning midnight splits across two lines",
			events: []core.Event{
				rateSet(t, "acme", 6000, "EUR"),
				entry(t, "e1", "acme", "night shift", utc(2, 22, 0), utc(3, 2, 0)),
			},
			now:          utc(5, 12, 0),
			project:      "acme",
			from:         utcDay(2),
			to:           utcDay(4),
			wantCurrency: "EUR",
			wantRate:     6000,
			wantFrom:     utcDay(2),
			wantTo:       utcDay(4),
			wantTotal:    4 * time.Hour,
			wantCents:    24000,
			wantLines: []wantLine{
				{date: utcDay(2), notes: []string{"night shift"}, duration: 2 * time.Hour, cents: 12000},
				{date: utcDay(3), notes: []string{"night shift"}, duration: 2 * time.Hour, cents: 12000},
			},
		},
		{
			name: "notes are de-duplicated in first-seen order",
			events: []core.Event{
				rateSet(t, "acme", 3600, "EUR"),
				entry(t, "e1", "acme", "review", utc(2, 9, 0), utc(2, 10, 0)),
				entry(t, "e2", "acme", "deploy", utc(2, 10, 0), utc(2, 11, 0)),
				entry(t, "e3", "acme", "review", utc(2, 11, 0), utc(2, 12, 0)),
				entry(t, "e4", "acme", "   ", utc(2, 12, 0), utc(2, 13, 0)),
			},
			now:          utc(5, 12, 0),
			project:      "acme",
			from:         utcDay(2),
			to:           utcDay(3),
			wantCurrency: "EUR",
			wantRate:     3600,
			wantFrom:     utcDay(2),
			wantTo:       utcDay(3),
			wantTotal:    4 * time.Hour,
			wantCents:    14400,
			wantLines: []wantLine{
				{date: utcDay(2), notes: []string{"review", "deploy"}, duration: 4 * time.Hour, cents: 14400},
			},
		},
		{
			name: "other projects are excluded",
			events: []core.Event{
				rateSet(t, "acme", 6000, "EUR"),
				rateSet(t, "globex", 9900, "USD"),
				entry(t, "e1", "acme", "ours", utc(2, 9, 0), utc(2, 10, 0)),
				entry(t, "e2", "globex", "theirs", utc(2, 10, 0), utc(2, 18, 0)),
			},
			now:          utc(5, 12, 0),
			project:      "acme",
			from:         utcDay(2),
			to:           utcDay(3),
			wantCurrency: "EUR",
			wantRate:     6000,
			wantFrom:     utcDay(2),
			wantTo:       utcDay(3),
			wantTotal:    time.Hour,
			wantCents:    6000,
			wantLines: []wantLine{
				{date: utcDay(2), notes: []string{"ours"}, duration: time.Hour, cents: 6000},
			},
		},
		{
			name: "running entry bills up to the clock",
			events: []core.Event{
				rateSet(t, "acme", 6000, "EUR"),
				started(t, "e1", "acme", "still going", utc(2, 9, 0)),
			},
			now:          utc(2, 11, 0),
			project:      "acme",
			from:         utcDay(2),
			to:           utcDay(3),
			wantCurrency: "EUR",
			wantRate:     6000,
			wantFrom:     utcDay(2),
			wantTo:       utcDay(3),
			wantTotal:    2 * time.Hour,
			wantCents:    12000,
			wantLines: []wantLine{
				{date: utcDay(2), notes: []string{"still going"}, duration: 2 * time.Hour, cents: 12000},
			},
		},
		{
			name: "entries are clipped to the half-open window",
			events: []core.Event{
				rateSet(t, "acme", 6000, "EUR"),
				// Starts the day before the window: only the inside part bills.
				entry(t, "e1", "acme", "overnight in", utc(1, 22, 0), utc(2, 1, 0)),
				// Runs past the end of the window: the tail is not billed.
				entry(t, "e2", "acme", "overnight out", utc(3, 23, 0), utc(4, 3, 0)),
				// Starts exactly at the exclusive end: entirely outside.
				entry(t, "e3", "acme", "next month", utc(4, 0, 0), utc(4, 5, 0)),
			},
			now:          utc(6, 12, 0),
			project:      "acme",
			from:         utcDay(2),
			to:           utcDay(4),
			wantCurrency: "EUR",
			wantRate:     6000,
			wantFrom:     utcDay(2),
			wantTo:       utcDay(4),
			wantTotal:    2 * time.Hour,
			wantCents:    12000,
			wantLines: []wantLine{
				{date: utcDay(2), notes: []string{"overnight in"}, duration: time.Hour, cents: 6000},
				{date: utcDay(3), notes: []string{"overnight out"}, duration: time.Hour, cents: 6000},
			},
		},
		{
			name: "a rate with no time in the window is a valid zero invoice",
			events: []core.Event{
				rateSet(t, "acme", 6000, "EUR"),
				entry(t, "e1", "acme", "last month", utc(1, 9, 0), utc(1, 17, 0)),
			},
			now:          utc(6, 12, 0),
			project:      "acme",
			from:         utcDay(2),
			to:           utcDay(4),
			wantCurrency: "EUR",
			wantRate:     6000,
			wantFrom:     utcDay(2),
			wantTo:       utcDay(4),
			wantTotal:    0,
			wantCents:    0,
			wantLines:    nil,
		},
		{
			name: "days are cut in the window's location",
			events: []core.Event{
				rateSet(t, "acme", 6000, "EUR"),
				// 22:30 UTC is 00:30 the next day in +02:00, so this belongs to
				// the 3rd locally even though it is the 2nd in storage.
				entry(t, "e1", "acme", "late", utc(2, 22, 30), utc(2, 23, 30)),
			},
			now:          utc(6, 12, 0),
			project:      "acme",
			from:         time.Date(2026, time.March, 2, 0, 0, 0, 0, berlin),
			to:           time.Date(2026, time.March, 5, 0, 0, 0, 0, berlin),
			wantCurrency: "EUR",
			wantRate:     6000,
			wantFrom:     time.Date(2026, time.March, 2, 0, 0, 0, 0, berlin),
			wantTo:       time.Date(2026, time.March, 5, 0, 0, 0, 0, berlin),
			wantTotal:    time.Hour,
			wantCents:    6000,
			wantLines: []wantLine{
				{date: time.Date(2026, time.March, 3, 0, 0, 0, 0, berlin), notes: []string{"late"}, duration: time.Hour, cents: 6000},
			},
		},
		{
			name: "the project argument is trimmed",
			events: []core.Event{
				rateSet(t, "acme", 6000, "EUR"),
				entry(t, "e1", "acme", "work", utc(2, 9, 0), utc(2, 10, 0)),
			},
			now:          utc(5, 12, 0),
			project:      "  acme  ",
			from:         utcDay(2),
			to:           utcDay(3),
			wantCurrency: "EUR",
			wantRate:     6000,
			wantFrom:     utcDay(2),
			wantTo:       utcDay(3),
			wantTotal:    time.Hour,
			wantCents:    6000,
			wantLines: []wantLine{
				{date: utcDay(2), notes: []string{"work"}, duration: time.Hour, cents: 6000},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := NewReader(newLog(tt.events...), core.FixedClock(tt.now))
			got, err := r.For(context.Background(), tt.project, tt.from, tt.to)
			if err != nil {
				t.Fatalf("For() error = %v, want nil", err)
			}

			if got.Project != "acme" {
				t.Errorf("Project = %q, want %q", got.Project, "acme")
			}
			if !got.From.Equal(tt.wantFrom) {
				t.Errorf("From = %s, want %s", got.From, tt.wantFrom)
			}
			if !got.To.Equal(tt.wantTo) {
				t.Errorf("To = %s, want %s", got.To, tt.wantTo)
			}
			if got.Currency != tt.wantCurrency {
				t.Errorf("Currency = %q, want %q", got.Currency, tt.wantCurrency)
			}
			if got.CentsPerHour != tt.wantRate {
				t.Errorf("CentsPerHour = %d, want %d", got.CentsPerHour, tt.wantRate)
			}
			if got.Total != tt.wantTotal {
				t.Errorf("Total = %s, want %s", got.Total, tt.wantTotal)
			}
			if got.TotalCents != tt.wantCents {
				t.Errorf("TotalCents = %d, want %d", got.TotalCents, tt.wantCents)
			}

			// The renderer draws an empty state from a valid invoice, so Lines is
			// never nil — not even when nothing was billable.
			if got.Lines == nil {
				t.Fatal("Lines = nil, want non-nil slice")
			}
			if len(got.Lines) != len(tt.wantLines) {
				t.Fatalf("len(Lines) = %d, want %d (lines: %+v)", len(got.Lines), len(tt.wantLines), got.Lines)
			}
			for i, want := range tt.wantLines {
				line := got.Lines[i]
				if !line.Date.Equal(want.date) {
					t.Errorf("Lines[%d].Date = %s, want %s", i, line.Date, want.date)
				}
				if line.Duration != want.duration {
					t.Errorf("Lines[%d].Duration = %s, want %s", i, line.Duration, want.duration)
				}
				if line.AmountCents != want.cents {
					t.Errorf("Lines[%d].AmountCents = %d, want %d", i, line.AmountCents, want.cents)
				}
				if !slices.Equal(line.Notes, want.notes) {
					t.Errorf("Lines[%d].Notes = %q, want %q", i, line.Notes, want.notes)
				}
			}

			// Whatever the case, the footer must be exactly what a client gets by
			// adding up the column.
			var sum int64
			var dur time.Duration
			for _, line := range got.Lines {
				sum += line.AmountCents
				dur += line.Duration
			}
			if sum != got.TotalCents {
				t.Errorf("TotalCents = %d, want sum of lines %d", got.TotalCents, sum)
			}
			if dur != got.Total {
				t.Errorf("Total = %s, want sum of line durations %s", got.Total, dur)
			}
			// Lines must be ascending by date, one per calendar day.
			for i := 1; i < len(got.Lines); i++ {
				if !got.Lines[i-1].Date.Before(got.Lines[i].Date) {
					t.Errorf("Lines[%d].Date = %s not before Lines[%d].Date = %s", i-1, got.Lines[i-1].Date, i, got.Lines[i].Date)
				}
			}
		})
	}
}

// errLoad is the failure a store surfaces; the Reader must pass it through
// unchanged so a caller can tell "the log is unreadable" from "your arguments
// were wrong".
var errLoad = errors.New("store unavailable")

func TestReaderForErrors(t *testing.T) {
	t.Parallel()

	billable := []core.Event{
		rateSet(t, "acme", 6000, "EUR"),
		entry(t, "e1", "acme", "work", utc(2, 9, 0), utc(2, 10, 0)),
	}

	tests := []struct {
		name     string
		events   []core.Event
		loadErr  error
		project  string
		from, to time.Time
		want     error
	}{
		{
			name:    "empty project",
			events:  billable,
			project: "",
			from:    utcDay(2),
			to:      utcDay(3),
			want:    core.ErrInvalid,
		},
		{
			name:    "whitespace-only project",
			events:  billable,
			project: "   ",
			from:    utcDay(2),
			to:      utcDay(3),
			want:    core.ErrInvalid,
		},
		{
			name:    "to equals from",
			events:  billable,
			project: "acme",
			from:    utcDay(2),
			to:      utcDay(2),
			want:    core.ErrInvalid,
		},
		{
			name:    "to before from",
			events:  billable,
			project: "acme",
			from:    utcDay(3),
			to:      utcDay(2),
			want:    core.ErrInvalid,
		},
		{
			name: "project has time but no rate",
			events: []core.Event{
				entry(t, "e1", "acme", "work", utc(2, 9, 0), utc(2, 10, 0)),
			},
			project: "acme",
			from:    utcDay(2),
			to:      utcDay(3),
			want:    core.ErrNotFound,
		},
		{
			name:    "unknown project",
			events:  billable,
			project: "globex",
			from:    utcDay(2),
			to:      utcDay(3),
			want:    core.ErrNotFound,
		},
		{
			name: "negative rate",
			events: []core.Event{
				rateSet(t, "acme", -6000, "EUR"),
				entry(t, "e1", "acme", "work", utc(2, 9, 0), utc(2, 10, 0)),
			},
			project: "acme",
			from:    utcDay(2),
			to:      utcDay(3),
			want:    core.ErrInvalid,
		},
		{
			name: "rate large enough to overflow int64 cents",
			events: []core.Event{
				rateSet(t, "acme", math.MaxInt64/2, "EUR"),
				entry(t, "e1", "acme", "work", utc(2, 9, 0), utc(2, 10, 0)),
			},
			project: "acme",
			from:    utcDay(2),
			to:      utcDay(3),
			want:    core.ErrInvalid,
		},
		{
			name:    "log unreadable",
			events:  billable,
			loadErr: errLoad,
			project: "acme",
			from:    utcDay(2),
			to:      utcDay(3),
			want:    errLoad,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			log := newLog(tt.events...)
			log.err = tt.loadErr

			r := NewReader(log, core.FixedClock(utc(5, 12, 0)))
			got, err := r.For(context.Background(), tt.project, tt.from, tt.to)
			if !errors.Is(err, tt.want) {
				t.Fatalf("For() error = %v, want errors.Is(err, %v)", err, tt.want)
			}
			if got.Lines != nil || got.TotalCents != 0 {
				t.Errorf("For() = %+v, want zero Invoice on error", got)
			}
		})
	}
}

// TestReaderForRereadsLog pins the no-caching rule: a second call must see an
// entry appended between the two, because the log is the truth and a fold held
// across calls is a stale read.
func TestReaderForRereadsLog(t *testing.T) {
	t.Parallel()

	log := newLog(
		rateSet(t, "acme", 6000, "EUR"),
		entry(t, "e1", "acme", "first", utc(2, 9, 0), utc(2, 10, 0)),
	)
	r := NewReader(log, core.FixedClock(utc(5, 12, 0)))

	first, err := r.For(context.Background(), "acme", utcDay(2), utcDay(3))
	if err != nil {
		t.Fatalf("For() error = %v", err)
	}
	if first.Total != time.Hour {
		t.Fatalf("Total = %s, want %s", first.Total, time.Hour)
	}

	appended := entry(t, "e2", "acme", "second", utc(2, 10, 0), utc(2, 11, 0))
	appended.Seq = int64(len(log.events) + 1)
	log.events = append(log.events, appended)

	second, err := r.For(context.Background(), "acme", utcDay(2), utcDay(3))
	if err != nil {
		t.Fatalf("For() error = %v", err)
	}
	if second.Total != 2*time.Hour {
		t.Errorf("Total = %s, want %s", second.Total, 2*time.Hour)
	}
	if second.TotalCents != 12000 {
		t.Errorf("TotalCents = %d, want %d", second.TotalCents, 12000)
	}
	if log.loads != 2 {
		t.Errorf("Load called %d times, want 2 (one full replay per call)", log.loads)
	}
}

// TestReaderForDSTDay checks that days are stepped by the calendar rather than
// by a fixed 24 hours: on the European spring-forward day the local day is 23
// hours long, and a fixed step would bill an hour that never happened.
func TestReaderForDSTDay(t *testing.T) {
	t.Parallel()

	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}

	// 2026-03-29 is the EU spring-forward date: 02:00 CET becomes 03:00 CEST.
	start := time.Date(2026, time.March, 28, 22, 0, 0, 0, loc)
	end := time.Date(2026, time.March, 30, 1, 0, 0, 0, loc)

	log := newLog(
		rateSet(t, "acme", 100, "EUR"),
		entry(t, "e1", "acme", "marathon", start, end),
	)
	r := NewReader(log, core.FixedClock(time.Date(2026, time.April, 1, 0, 0, 0, 0, time.UTC)))

	got, err := r.For(context.Background(), "acme",
		time.Date(2026, time.March, 28, 0, 0, 0, 0, loc),
		time.Date(2026, time.March, 31, 0, 0, 0, 0, loc))
	if err != nil {
		t.Fatalf("For() error = %v", err)
	}

	want := []wantLine{
		{date: time.Date(2026, time.March, 28, 0, 0, 0, 0, loc), duration: 2 * time.Hour, cents: 200},
		{date: time.Date(2026, time.March, 29, 0, 0, 0, 0, loc), duration: 23 * time.Hour, cents: 2300},
		{date: time.Date(2026, time.March, 30, 0, 0, 0, 0, loc), duration: time.Hour, cents: 100},
	}
	if len(got.Lines) != len(want) {
		t.Fatalf("len(Lines) = %d, want %d (lines: %+v)", len(got.Lines), len(want), got.Lines)
	}
	for i, w := range want {
		if !got.Lines[i].Date.Equal(w.date) {
			t.Errorf("Lines[%d].Date = %s, want %s", i, got.Lines[i].Date, w.date)
		}
		if got.Lines[i].Duration != w.duration {
			t.Errorf("Lines[%d].Duration = %s, want %s", i, got.Lines[i].Duration, w.duration)
		}
		if got.Lines[i].AmountCents != w.cents {
			t.Errorf("Lines[%d].AmountCents = %d, want %d", i, got.Lines[i].AmountCents, w.cents)
		}
	}
	if got.Total != 26*time.Hour {
		t.Errorf("Total = %s, want %s", got.Total, 26*time.Hour)
	}
	if got.TotalCents != 2600 {
		t.Errorf("TotalCents = %d, want %d", got.TotalCents, 2600)
	}
}

// TestLineAmount exercises the rounding rule directly, at the boundaries a table
// of whole invoices would only reach indirectly.
func TestLineAmount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		duration     time.Duration
		centsPerHour int64
		want         int64
		wantErr      error
	}{
		{name: "zero duration", duration: 0, centsPerHour: 9000, want: 0},
		{name: "zero rate", duration: time.Hour, centsPerHour: 0, want: 0},
		{name: "whole hour", duration: time.Hour, centsPerHour: 9000, want: 9000},
		{name: "ninety minutes", duration: 90 * time.Minute, centsPerHour: 9000, want: 13500},
		// Exactly half a cent: half-up bills the cent rather than dropping it.
		{name: "exactly half a cent rounds up", duration: 30 * time.Minute, centsPerHour: 1, want: 1},
		// Just under half a cent rounds down; just over rounds up.
		{name: "just under half a cent rounds down", duration: 30*time.Minute - time.Nanosecond, centsPerHour: 1, want: 0},
		{name: "just over half a cent rounds up", duration: 30*time.Minute + time.Nanosecond, centsPerHour: 1, want: 1},
		// 20 minutes at 100 cents/h is 33.33 cents: rounds to 33.
		{name: "third of an hour rounds down", duration: 20 * time.Minute, centsPerHour: 100, want: 33},
		// 40 minutes at 100 cents/h is 66.66 cents: rounds to 67.
		{name: "two thirds of an hour rounds up", duration: 40 * time.Minute, centsPerHour: 100, want: 67},
		{
			name:         "overflow is refused",
			duration:     time.Hour,
			centsPerHour: math.MaxInt64 / 2,
			wantErr:      core.ErrInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := lineAmount(tt.duration, tt.centsPerHour)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("lineAmount() error = %v, want errors.Is(err, %v)", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("lineAmount() error = %v, want nil", err)
			}
			if got != tt.want {
				t.Errorf("lineAmount(%s, %d) = %d, want %d", tt.duration, tt.centsPerHour, got, tt.want)
			}
		})
	}
}
