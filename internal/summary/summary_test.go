package summary

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/random-skill-tests/internal/core"
)

// stubLoader is an in-package core.Loader over a fixed slice of events. It
// records how it was called so a test can prove Day replays the whole log
// (sinceSeq 0) on every single call rather than caching a timeline.
type stubLoader struct {
	events []core.Event
	err    error
	loads  int
	since  []int64
}

func (s *stubLoader) Load(_ context.Context, sinceSeq int64) ([]core.Event, error) {
	s.loads++
	s.since = append(s.since, sinceSeq)
	if s.err != nil {
		return nil, s.err
	}
	return s.events, nil
}

// builder accumulates events and assigns Seq the way the store would, since
// core.NewEvent deliberately leaves it zero.
type builder struct {
	t      *testing.T
	seq    int64
	events []core.Event
}

func newBuilder(t *testing.T) *builder {
	t.Helper()
	return &builder{t: t}
}

func (b *builder) append(kind core.Kind, aggregate string, at time.Time, payload any) {
	b.t.Helper()
	e, err := core.NewEvent(kind, aggregate, at, payload)
	if err != nil {
		b.t.Fatalf("core.NewEvent(%s): %v", kind, err)
	}
	b.seq++
	e.Seq = b.seq
	b.events = append(b.events, e)
}

// entry records a completed interval.
func (b *builder) entry(id, project string, tags []string, start, end time.Time) *builder {
	b.t.Helper()
	b.append(core.KindEntryAdded, core.EntryAggregate(id), end, core.EntryAdded{
		EntryID: id,
		Project: project,
		Tags:    tags,
		Start:   start,
		End:     end,
	})
	return b
}

// running records an interval that is still open.
func (b *builder) running(id, project string, tags []string, start time.Time) *builder {
	b.t.Helper()
	b.append(core.KindTimerStarted, core.TimerAggregate, start, core.TimerStarted{
		EntryID:   id,
		Project:   project,
		Tags:      tags,
		StartedAt: start,
	})
	return b
}

func (b *builder) done() []core.Event { return b.events }

// utc builds an instant in UTC, which is how the log stores every entry.
func utc(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, time.UTC)
}

// wantEntry is the part of a core.EntryView a table case asserts on: identity
// plus the clipped duration, which is the whole point of the projection.
type wantEntry struct {
	id       string
	duration time.Duration
	start    time.Time
	end      time.Time
	running  bool
}

func TestReaderDay(t *testing.T) {
	t.Parallel()

	// A fixed zone rather than a tzdata lookup: the day-window rule is about the
	// offset, and a hermetic test should not depend on the host's zone database.
	nyc := time.FixedZone("EDT", -4*60*60)

	tests := []struct {
		name        string
		events      []core.Event
		now         time.Time
		day         time.Time
		wantDate    time.Time
		wantTotal   time.Duration
		wantEntries []wantEntry
		wantProject []core.Bucket
		wantTag     []core.Bucket
		wantRunning *wantEntry
	}{
		{
			name: "single completed entry",
			events: newBuilder(t).
				entry("e1", "acme", []string{"dev"}, utc(2024, 3, 14, 9, 0), utc(2024, 3, 14, 10, 30)).
				done(),
			now:       utc(2024, 3, 14, 18, 0),
			day:       utc(2024, 3, 14, 12, 0),
			wantDate:  utc(2024, 3, 14, 0, 0),
			wantTotal: 90 * time.Minute,
			wantEntries: []wantEntry{
				{id: "e1", duration: 90 * time.Minute, start: utc(2024, 3, 14, 9, 0), end: utc(2024, 3, 14, 10, 30)},
			},
			wantProject: []core.Bucket{{Key: "acme", Duration: 90 * time.Minute, Share: 1}},
			wantTag:     []core.Bucket{{Key: "dev", Duration: 90 * time.Minute, Share: 1}},
		},
		{
			name: "two projects share the day",
			events: newBuilder(t).
				entry("e1", "acme", []string{"dev"}, utc(2024, 3, 14, 9, 0), utc(2024, 3, 14, 10, 0)).
				entry("e2", "beta", []string{"ops"}, utc(2024, 3, 14, 10, 0), utc(2024, 3, 14, 13, 0)).
				done(),
			now:       utc(2024, 3, 14, 18, 0),
			day:       utc(2024, 3, 14, 12, 0),
			wantDate:  utc(2024, 3, 14, 0, 0),
			wantTotal: 4 * time.Hour,
			wantEntries: []wantEntry{
				{id: "e1", duration: time.Hour, start: utc(2024, 3, 14, 9, 0), end: utc(2024, 3, 14, 10, 0)},
				{id: "e2", duration: 3 * time.Hour, start: utc(2024, 3, 14, 10, 0), end: utc(2024, 3, 14, 13, 0)},
			},
			// Bucketize orders longest first, so beta leads.
			wantProject: []core.Bucket{
				{Key: "beta", Duration: 3 * time.Hour, Share: 0.75},
				{Key: "acme", Duration: time.Hour, Share: 0.25},
			},
			wantTag: []core.Bucket{
				{Key: "ops", Duration: 3 * time.Hour, Share: 0.75},
				{Key: "dev", Duration: time.Hour, Share: 0.25},
			},
		},
		{
			name: "running entry measured against the clock",
			events: newBuilder(t).
				entry("e1", "acme", []string{"dev"}, utc(2024, 3, 14, 6, 0), utc(2024, 3, 14, 7, 0)).
				running("e2", "beta", []string{"dev"}, utc(2024, 3, 14, 8, 0)).
				done(),
			now:       utc(2024, 3, 14, 9, 30),
			day:       utc(2024, 3, 14, 12, 0),
			wantDate:  utc(2024, 3, 14, 0, 0),
			wantTotal: 150 * time.Minute,
			wantEntries: []wantEntry{
				{id: "e1", duration: time.Hour, start: utc(2024, 3, 14, 6, 0), end: utc(2024, 3, 14, 7, 0)},
				{id: "e2", duration: 90 * time.Minute, start: utc(2024, 3, 14, 8, 0), running: true},
			},
			wantProject: []core.Bucket{
				{Key: "beta", Duration: 90 * time.Minute, Share: 0.6},
				{Key: "acme", Duration: 60 * time.Minute, Share: 0.4},
			},
			wantTag:     []core.Bucket{{Key: "dev", Duration: 150 * time.Minute, Share: 1}},
			wantRunning: &wantEntry{id: "e2", duration: 90 * time.Minute, start: utc(2024, 3, 14, 8, 0), running: true},
		},
		{
			name: "midnight span clipped on the leading day",
			events: newBuilder(t).
				entry("e1", "acme", []string{"dev"}, utc(2024, 3, 14, 23, 0), utc(2024, 3, 15, 1, 0)).
				done(),
			now:       utc(2024, 3, 15, 8, 0),
			day:       utc(2024, 3, 14, 12, 0),
			wantDate:  utc(2024, 3, 14, 0, 0),
			wantTotal: time.Hour,
			wantEntries: []wantEntry{
				// True Start/End survive; only Duration is clipped.
				{id: "e1", duration: time.Hour, start: utc(2024, 3, 14, 23, 0), end: utc(2024, 3, 15, 1, 0)},
			},
			wantProject: []core.Bucket{{Key: "acme", Duration: time.Hour, Share: 1}},
			wantTag:     []core.Bucket{{Key: "dev", Duration: time.Hour, Share: 1}},
		},
		{
			name: "midnight span clipped on the trailing day",
			events: newBuilder(t).
				entry("e1", "acme", []string{"dev"}, utc(2024, 3, 14, 23, 0), utc(2024, 3, 15, 1, 0)).
				done(),
			now:       utc(2024, 3, 15, 8, 0),
			day:       utc(2024, 3, 15, 12, 0),
			wantDate:  utc(2024, 3, 15, 0, 0),
			wantTotal: time.Hour,
			wantEntries: []wantEntry{
				{id: "e1", duration: time.Hour, start: utc(2024, 3, 14, 23, 0), end: utc(2024, 3, 15, 1, 0)},
			},
			wantProject: []core.Bucket{{Key: "acme", Duration: time.Hour, Share: 1}},
			wantTag:     []core.Bucket{{Key: "dev", Duration: time.Hour, Share: 1}},
		},
		{
			name: "running entry spanning midnight clipped to the trailing day",
			events: newBuilder(t).
				running("e1", "acme", []string{"dev"}, utc(2024, 3, 14, 22, 0)).
				done(),
			now:       utc(2024, 3, 15, 2, 0),
			day:       utc(2024, 3, 15, 12, 0),
			wantDate:  utc(2024, 3, 15, 0, 0),
			wantTotal: 2 * time.Hour,
			wantEntries: []wantEntry{
				{id: "e1", duration: 2 * time.Hour, start: utc(2024, 3, 14, 22, 0), running: true},
			},
			wantProject: []core.Bucket{{Key: "acme", Duration: 2 * time.Hour, Share: 1}},
			wantTag:     []core.Bucket{{Key: "dev", Duration: 2 * time.Hour, Share: 1}},
			wantRunning: &wantEntry{id: "e1", duration: 2 * time.Hour, start: utc(2024, 3, 14, 22, 0), running: true},
		},
		{
			name: "entry outside the window is excluded",
			events: newBuilder(t).
				entry("yesterday", "acme", []string{"dev"}, utc(2024, 3, 13, 9, 0), utc(2024, 3, 13, 17, 0)).
				entry("tomorrow", "acme", []string{"dev"}, utc(2024, 3, 15, 9, 0), utc(2024, 3, 15, 17, 0)).
				entry("today", "acme", []string{"dev"}, utc(2024, 3, 14, 9, 0), utc(2024, 3, 14, 11, 0)).
				done(),
			now:       utc(2024, 3, 15, 20, 0),
			day:       utc(2024, 3, 14, 12, 0),
			wantDate:  utc(2024, 3, 14, 0, 0),
			wantTotal: 2 * time.Hour,
			wantEntries: []wantEntry{
				{id: "today", duration: 2 * time.Hour, start: utc(2024, 3, 14, 9, 0), end: utc(2024, 3, 14, 11, 0)},
			},
			wantProject: []core.Bucket{{Key: "acme", Duration: 2 * time.Hour, Share: 1}},
			wantTag:     []core.Bucket{{Key: "dev", Duration: 2 * time.Hour, Share: 1}},
		},
		{
			name: "entry abutting the window edges is excluded",
			events: newBuilder(t).
				// Ends exactly at this day's start, and starts exactly at its end.
				entry("before", "acme", nil, utc(2024, 3, 13, 22, 0), utc(2024, 3, 14, 0, 0)).
				entry("after", "acme", nil, utc(2024, 3, 15, 0, 0), utc(2024, 3, 15, 2, 0)).
				done(),
			now:         utc(2024, 3, 15, 8, 0),
			day:         utc(2024, 3, 14, 12, 0),
			wantDate:    utc(2024, 3, 14, 0, 0),
			wantTotal:   0,
			wantEntries: []wantEntry{},
			wantProject: []core.Bucket{},
			wantTag:     []core.Bucket{},
		},
		{
			name: "multi-tag entries double count so tag shares exceed one",
			events: newBuilder(t).
				entry("e1", "acme", []string{"dev", "billable"}, utc(2024, 3, 14, 9, 0), utc(2024, 3, 14, 11, 0)).
				// No tags at all: present in ByProject, absent from every bucket
				// in ByTag.
				entry("e2", "acme", nil, utc(2024, 3, 14, 11, 0), utc(2024, 3, 14, 13, 0)).
				done(),
			now:       utc(2024, 3, 14, 18, 0),
			day:       utc(2024, 3, 14, 12, 0),
			wantDate:  utc(2024, 3, 14, 0, 0),
			wantTotal: 4 * time.Hour,
			wantEntries: []wantEntry{
				{id: "e1", duration: 2 * time.Hour, start: utc(2024, 3, 14, 9, 0), end: utc(2024, 3, 14, 11, 0)},
				{id: "e2", duration: 2 * time.Hour, start: utc(2024, 3, 14, 11, 0), end: utc(2024, 3, 14, 13, 0)},
			},
			wantProject: []core.Bucket{{Key: "acme", Duration: 4 * time.Hour, Share: 1}},
			wantTag: []core.Bucket{
				{Key: "billable", Duration: 2 * time.Hour, Share: 0.5},
				{Key: "dev", Duration: 2 * time.Hour, Share: 0.5},
			},
		},
		{
			name: "day window follows the caller's location",
			events: newBuilder(t).
				// 2024-03-13 22:00–23:00 in EDT: the previous day locally.
				entry("prev", "acme", []string{"dev"}, utc(2024, 3, 14, 2, 0), utc(2024, 3, 14, 3, 0)).
				// 23:30 EDT the previous day through 00:30 EDT today: clipped to
				// the trailing half hour.
				entry("straddle", "acme", []string{"dev"}, utc(2024, 3, 14, 3, 30), utc(2024, 3, 14, 4, 30)).
				// 01:00–02:00 EDT today.
				entry("today", "beta", []string{"ops"}, utc(2024, 3, 14, 5, 0), utc(2024, 3, 14, 6, 0)).
				done(),
			now:       utc(2024, 3, 15, 0, 0),
			day:       time.Date(2024, 3, 14, 12, 0, 0, 0, nyc),
			wantDate:  time.Date(2024, 3, 14, 0, 0, 0, 0, nyc),
			wantTotal: 90 * time.Minute,
			wantEntries: []wantEntry{
				{id: "straddle", duration: 30 * time.Minute, start: utc(2024, 3, 14, 3, 30), end: utc(2024, 3, 14, 4, 30)},
				{id: "today", duration: time.Hour, start: utc(2024, 3, 14, 5, 0), end: utc(2024, 3, 14, 6, 0)},
			},
			wantProject: []core.Bucket{
				{Key: "beta", Duration: time.Hour, Share: 2.0 / 3.0},
				{Key: "acme", Duration: 30 * time.Minute, Share: 1.0 / 3.0},
			},
			wantTag: []core.Bucket{
				{Key: "ops", Duration: time.Hour, Share: 2.0 / 3.0},
				{Key: "dev", Duration: 30 * time.Minute, Share: 1.0 / 3.0},
			},
		},
		{
			name:        "empty log yields a valid empty view",
			events:      nil,
			now:         utc(2024, 3, 14, 12, 0),
			day:         utc(2024, 3, 14, 15, 0),
			wantDate:    utc(2024, 3, 14, 0, 0),
			wantTotal:   0,
			wantEntries: []wantEntry{},
			wantProject: []core.Bucket{},
			wantTag:     []core.Bucket{},
		},
		{
			name: "deleted and edited entries follow the canonical replay",
			events: func() []core.Event {
				b := newBuilder(t).
					entry("keep", "acme", []string{"dev"}, utc(2024, 3, 14, 9, 0), utc(2024, 3, 14, 10, 0)).
					entry("drop", "acme", []string{"dev"}, utc(2024, 3, 14, 10, 0), utc(2024, 3, 14, 12, 0))
				moved := utc(2024, 3, 14, 8, 0)
				b.append(core.KindEntryEdited, core.EntryAggregate("keep"), utc(2024, 3, 14, 13, 0), core.EntryEdited{
					EntryID: "keep",
					Start:   &moved,
				})
				b.append(core.KindEntryDeleted, core.EntryAggregate("drop"), utc(2024, 3, 14, 13, 0), core.EntryDeleted{
					EntryID: "drop",
				})
				return b.done()
			}(),
			now:       utc(2024, 3, 14, 18, 0),
			day:       utc(2024, 3, 14, 12, 0),
			wantDate:  utc(2024, 3, 14, 0, 0),
			wantTotal: 2 * time.Hour,
			wantEntries: []wantEntry{
				{id: "keep", duration: 2 * time.Hour, start: utc(2024, 3, 14, 8, 0), end: utc(2024, 3, 14, 10, 0)},
			},
			wantProject: []core.Bucket{{Key: "acme", Duration: 2 * time.Hour, Share: 1}},
			wantTag:     []core.Bucket{{Key: "dev", Duration: 2 * time.Hour, Share: 1}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := NewReader(&stubLoader{events: tt.events}, core.FixedClock(tt.now))
			got, err := r.Day(context.Background(), tt.day)
			if err != nil {
				t.Fatalf("Day() error = %v, want nil", err)
			}

			if !got.Date.Equal(tt.wantDate) {
				t.Errorf("Date = %s, want %s", got.Date, tt.wantDate)
			}
			// Date must be midnight in the caller's location, not merely the
			// right instant.
			if h, m, s := got.Date.Clock(); h|m|s != 0 {
				t.Errorf("Date = %s, want midnight", got.Date)
			}
			if got.Total != tt.wantTotal {
				t.Errorf("Total = %s, want %s", got.Total, tt.wantTotal)
			}

			if got.Entries == nil {
				t.Fatal("Entries = nil, want non-nil")
			}
			if got.ByProject == nil {
				t.Error("ByProject = nil, want non-nil")
			}
			if got.ByTag == nil {
				t.Error("ByTag = nil, want non-nil")
			}

			checkEntries(t, got.Entries, tt.wantEntries)
			checkBuckets(t, "ByProject", got.ByProject, tt.wantProject)
			checkBuckets(t, "ByTag", got.ByTag, tt.wantTag)

			// The invariant that justifies clipping: what the day lists adds up
			// to what the day totals.
			var sum time.Duration
			for _, e := range got.Entries {
				sum += e.Duration
			}
			if sum != got.Total {
				t.Errorf("sum of Entries durations = %s, want Total %s", sum, got.Total)
			}

			// Entries stay sorted by Start then ID, as core.BuildTimeline left them.
			for i := 1; i < len(got.Entries); i++ {
				prev, cur := got.Entries[i-1], got.Entries[i]
				if cur.Start.Before(prev.Start) || (cur.Start.Equal(prev.Start) && cur.ID < prev.ID) {
					t.Errorf("Entries not sorted at %d: %s/%s then %s/%s", i, prev.Start, prev.ID, cur.Start, cur.ID)
				}
			}

			checkRunning(t, got, tt.wantRunning)
		})
	}
}

func checkEntries(t *testing.T, got []core.EntryView, want []wantEntry) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len(Entries) = %d, want %d (%v)", len(got), len(want), ids(got))
	}
	for i, w := range want {
		g := got[i]
		if g.ID != w.id {
			t.Errorf("Entries[%d].ID = %q, want %q", i, g.ID, w.id)
		}
		if g.Duration != w.duration {
			t.Errorf("Entries[%d].Duration = %s, want %s", i, g.Duration, w.duration)
		}
		// True Start/End are preserved even when Duration is clipped.
		if !g.Start.Equal(w.start) {
			t.Errorf("Entries[%d].Start = %s, want %s", i, g.Start, w.start)
		}
		if w.running {
			if !g.Running {
				t.Errorf("Entries[%d].Running = false, want true", i)
			}
			if !g.End.IsZero() {
				t.Errorf("Entries[%d].End = %s, want zero while running", i, g.End)
			}
			continue
		}
		if g.Running {
			t.Errorf("Entries[%d].Running = true, want false", i)
		}
		if !g.End.Equal(w.end) {
			t.Errorf("Entries[%d].End = %s, want %s", i, g.End, w.end)
		}
	}
}

func checkBuckets(t *testing.T, name string, got, want []core.Bucket) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len(%s) = %d, want %d (%v)", name, len(got), len(want), got)
	}
	for i := range want {
		if got[i].Key != want[i].Key {
			t.Errorf("%s[%d].Key = %q, want %q", name, i, got[i].Key, want[i].Key)
		}
		if got[i].Duration != want[i].Duration {
			t.Errorf("%s[%d].Duration = %s, want %s", name, i, got[i].Duration, want[i].Duration)
		}
		if math.Abs(got[i].Share-want[i].Share) > 1e-9 {
			t.Errorf("%s[%d].Share = %g, want %g", name, i, got[i].Share, want[i].Share)
		}
	}
}

func checkRunning(t *testing.T, got core.DayView, want *wantEntry) {
	t.Helper()
	if want == nil {
		if got.Running != nil {
			t.Errorf("Running = %+v, want nil", *got.Running)
		}
		return
	}
	if got.Running == nil {
		t.Fatalf("Running = nil, want entry %q", want.id)
	}
	if got.Running.ID != want.id {
		t.Errorf("Running.ID = %q, want %q", got.Running.ID, want.id)
	}
	// Running must be the entry exactly as Entries shows it, clipping included.
	if got.Running.Duration != want.duration {
		t.Errorf("Running.Duration = %s, want %s", got.Running.Duration, want.duration)
	}
	for _, e := range got.Entries {
		if e.ID != got.Running.ID {
			continue
		}
		if e.Duration != got.Running.Duration {
			t.Errorf("Running.Duration = %s, but Entries shows %s", got.Running.Duration, e.Duration)
		}
		return
	}
	t.Errorf("Running entry %q missing from Entries", got.Running.ID)
}

func ids(entries []core.EntryView) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.ID)
	}
	return out
}

// TestReaderDayTagSharesExceedOne pins the deliberate consequence of tags not
// being a partition: an entry tagged N ways counts in full N times, so the
// shares in ByTag can sum past 1. A future "fix" that normalises them would
// break this test on purpose.
func TestReaderDayTagSharesExceedOne(t *testing.T) {
	t.Parallel()

	events := newBuilder(t).
		entry("e1", "acme", []string{"dev", "billable", "urgent"}, utc(2024, 3, 14, 9, 0), utc(2024, 3, 14, 12, 0)).
		done()

	r := NewReader(&stubLoader{events: events}, core.FixedClock(utc(2024, 3, 14, 18, 0)))
	got, err := r.Day(context.Background(), utc(2024, 3, 14, 12, 0))
	if err != nil {
		t.Fatalf("Day() error = %v", err)
	}

	if got.Total != 3*time.Hour {
		t.Fatalf("Total = %s, want 3h", got.Total)
	}

	var tagShare float64
	for _, b := range got.ByTag {
		if b.Duration != 3*time.Hour {
			t.Errorf("ByTag[%q].Duration = %s, want the entry's full 3h", b.Key, b.Duration)
		}
		tagShare += b.Share
	}
	if len(got.ByTag) != 3 {
		t.Fatalf("len(ByTag) = %d, want 3", len(got.ByTag))
	}
	if tagShare <= 1 {
		t.Errorf("sum of ByTag shares = %g, want > 1 (tags are not a partition)", tagShare)
	}
	if math.Abs(tagShare-3) > 1e-9 {
		t.Errorf("sum of ByTag shares = %g, want 3", tagShare)
	}

	// ByProject, by contrast, is a partition and must still sum to exactly 1.
	var projectShare float64
	for _, b := range got.ByProject {
		projectShare += b.Share
	}
	if math.Abs(projectShare-1) > 1e-9 {
		t.Errorf("sum of ByProject shares = %g, want 1", projectShare)
	}
}

// TestReaderDayMidnightSpanSplitsAcrossDays checks the halves of a clipped entry
// against each other: neither day may claim time the other already counted, and
// together they must account for the whole interval.
func TestReaderDayMidnightSpanSplitsAcrossDays(t *testing.T) {
	t.Parallel()

	start, end := utc(2024, 3, 14, 21, 15), utc(2024, 3, 15, 3, 45)
	events := newBuilder(t).entry("e1", "acme", []string{"dev"}, start, end).done()
	r := NewReader(&stubLoader{events: events}, core.FixedClock(utc(2024, 3, 15, 12, 0)))

	first, err := r.Day(context.Background(), utc(2024, 3, 14, 8, 0))
	if err != nil {
		t.Fatalf("Day(first) error = %v", err)
	}
	second, err := r.Day(context.Background(), utc(2024, 3, 15, 8, 0))
	if err != nil {
		t.Fatalf("Day(second) error = %v", err)
	}

	if want := 2*time.Hour + 45*time.Minute; first.Total != want {
		t.Errorf("first day Total = %s, want %s", first.Total, want)
	}
	if want := 3*time.Hour + 45*time.Minute; second.Total != want {
		t.Errorf("second day Total = %s, want %s", second.Total, want)
	}
	if got, want := first.Total+second.Total, end.Sub(start); got != want {
		t.Errorf("clipped halves sum to %s, want the full interval %s", got, want)
	}
	// Both days list the entry with its true, unclipped Start and End.
	for name, view := range map[string]core.DayView{"first": first, "second": second} {
		if len(view.Entries) != 1 {
			t.Fatalf("%s day: len(Entries) = %d, want 1", name, len(view.Entries))
		}
		if !view.Entries[0].Start.Equal(start) || !view.Entries[0].End.Equal(end) {
			t.Errorf("%s day: entry bounds = %s..%s, want %s..%s",
				name, view.Entries[0].Start, view.Entries[0].End, start, end)
		}
	}
}

// TestReaderDayReloadsEveryCall guards the rule that makes this a projection:
// the whole log is re-read on every query, so an event appended between two
// calls shows up in the second. A cache here would be the stale-read failure
// mode CQRS exists to avoid.
func TestReaderDayReloadsEveryCall(t *testing.T) {
	t.Parallel()

	loader := &stubLoader{events: newBuilder(t).
		entry("e1", "acme", nil, utc(2024, 3, 14, 9, 0), utc(2024, 3, 14, 10, 0)).
		done()}
	r := NewReader(loader, core.FixedClock(utc(2024, 3, 14, 18, 0)))
	day := utc(2024, 3, 14, 12, 0)

	first, err := r.Day(context.Background(), day)
	if err != nil {
		t.Fatalf("Day() error = %v", err)
	}
	if first.Total != time.Hour {
		t.Fatalf("Total = %s, want 1h", first.Total)
	}

	loader.events = newBuilder(t).
		entry("e1", "acme", nil, utc(2024, 3, 14, 9, 0), utc(2024, 3, 14, 10, 0)).
		entry("e2", "acme", nil, utc(2024, 3, 14, 10, 0), utc(2024, 3, 14, 11, 30)).
		done()

	second, err := r.Day(context.Background(), day)
	if err != nil {
		t.Fatalf("Day() error = %v", err)
	}
	if want := 150 * time.Minute; second.Total != want {
		t.Errorf("Total after append = %s, want %s", second.Total, want)
	}
	if loader.loads != 2 {
		t.Errorf("Load called %d times, want 2 (one per Day call)", loader.loads)
	}
	for i, since := range loader.since {
		if since != 0 {
			t.Errorf("Load call %d used sinceSeq %d, want 0 (full replay)", i, since)
		}
	}
}

// TestReaderDayLoadError checks that a store failure reaches the caller wrapped
// rather than swallowed or flattened into a string.
func TestReaderDayLoadError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("boom")
	r := NewReader(&stubLoader{err: sentinel}, core.FixedClock(utc(2024, 3, 14, 12, 0)))

	got, err := r.Day(context.Background(), utc(2024, 3, 14, 12, 0))
	if !errors.Is(err, sentinel) {
		t.Fatalf("Day() error = %v, want one wrapping %v", err, sentinel)
	}
	if !got.Date.IsZero() || got.Total != 0 {
		t.Errorf("Day() view = %+v, want the zero DayView on error", got)
	}
}

// TestReaderDayReplayError checks that an unfoldable log — here an event kind
// this binary does not know — surfaces as a wrapped core.ErrInvalid instead of a
// quietly wrong total.
func TestReaderDayReplayError(t *testing.T) {
	t.Parallel()

	b := newBuilder(t)
	b.append(core.Kind("entry.teleported"), core.EntryAggregate("e1"), utc(2024, 3, 14, 9, 0), struct{}{})

	r := NewReader(&stubLoader{events: b.done()}, core.FixedClock(utc(2024, 3, 14, 12, 0)))
	if _, err := r.Day(context.Background(), utc(2024, 3, 14, 12, 0)); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("Day() error = %v, want one wrapping core.ErrInvalid", err)
	}
}
