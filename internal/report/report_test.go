package report

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/random-skill-tests/internal/core"
)

// plus5 is a fixed non-UTC zone. A fixed offset keeps the tests independent of
// the machine's tzdata while still moving local midnight well off 00:00 UTC,
// which is the only property the window logic cares about.
var plus5 = time.FixedZone("UTC+5", 5*60*60)

// fakeLoader is an in-memory core.Loader. It counts Load calls so a test can
// prove the reader re-reads the log rather than caching a projection.
type fakeLoader struct {
	events []core.Event
	err    error
	loads  int
}

func (f *fakeLoader) Load(ctx context.Context, sinceSeq int64) ([]core.Event, error) {
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

// emit appends an event, assigning the Seq the store would have assigned.
func (f *fakeLoader) emit(t *testing.T, kind core.Kind, aggregate string, at time.Time, payload any) {
	t.Helper()
	e, err := core.NewEvent(kind, aggregate, at, payload)
	if err != nil {
		t.Fatalf("core.NewEvent(%s): %v", kind, err)
	}
	e.Seq = int64(len(f.events)) + 1
	f.events = append(f.events, e)
}

// addEntry records a complete interval.
func (f *fakeLoader) addEntry(t *testing.T, id, project string, tags []string, start, end time.Time) {
	t.Helper()
	f.emit(t, core.KindEntryAdded, core.EntryAggregate(id), end, core.EntryAdded{
		EntryID: id, Project: project, Tags: tags, Start: start, End: end,
	})
}

// startTimer opens an interval and leaves it running.
func (f *fakeLoader) startTimer(t *testing.T, id, project string, tags []string, start time.Time) {
	t.Helper()
	f.emit(t, core.KindTimerStarted, core.TimerAggregate, start, core.TimerStarted{
		EntryID: id, Project: project, Tags: tags, StartedAt: start,
	})
}

// utc is a terse constructor for an instant in the log's storage zone.
func utc(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, time.UTC)
}

// local is a terse constructor for a wall-clock instant in loc.
func local(loc *time.Location, y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, loc)
}

func sumDays(days []core.DayTotal) time.Duration {
	var total time.Duration
	for _, d := range days {
		total += d.Duration
	}
	return total
}

func TestRangeWindow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		from, to time.Time
		wantFrom string // RFC3339, so the resolved location is checked too
		wantTo   string
		wantDays int
		wantErr  bool
	}{
		{
			name:     "single day",
			from:     utc(2024, time.January, 15, 0, 0),
			to:       utc(2024, time.January, 16, 0, 0),
			wantFrom: "2024-01-15T00:00:00Z",
			wantTo:   "2024-01-16T00:00:00Z",
			wantDays: 1,
		},
		{
			name:     "instants snap outward to whole days",
			from:     utc(2024, time.January, 15, 9, 30),
			to:       utc(2024, time.January, 18, 17, 45),
			wantFrom: "2024-01-15T00:00:00Z",
			wantTo:   "2024-01-18T00:00:00Z",
			wantDays: 3,
		},
		{
			name:     "days are taken in from's location",
			from:     local(plus5, 2024, time.March, 10, 6, 0),
			to:       local(plus5, 2024, time.March, 13, 6, 0),
			wantFrom: "2024-03-10T00:00:00+05:00",
			wantTo:   "2024-03-13T00:00:00+05:00",
			wantDays: 3,
		},
		{
			name:     "to in another zone is converted before truncation",
			from:     local(plus5, 2024, time.March, 10, 0, 0),
			to:       utc(2024, time.March, 11, 20, 0), // 2024-03-12 01:00 +05
			wantFrom: "2024-03-10T00:00:00+05:00",
			wantTo:   "2024-03-12T00:00:00+05:00",
			wantDays: 2,
		},
		{
			name:     "widest accepted window",
			from:     utc(2000, time.January, 1, 0, 0),
			to:       utc(2000, time.January, 1, 0, 0).AddDate(0, 0, maxWindowDays),
			wantFrom: "2000-01-01T00:00:00Z",
			wantTo:   "2010-01-08T00:00:00Z",
			wantDays: maxWindowDays,
		},
		{
			name:    "absurd window is refused",
			from:    utc(2000, time.January, 1, 0, 0),
			to:      utc(2000, time.January, 1, 0, 0).AddDate(0, 0, maxWindowDays+1),
			wantErr: true,
		},
		{
			name:    "to equal to from",
			from:    utc(2024, time.January, 15, 9, 0),
			to:      utc(2024, time.January, 15, 9, 0),
			wantErr: true,
		},
		{
			name:    "to before from",
			from:    utc(2024, time.January, 15, 0, 0),
			to:      utc(2024, time.January, 14, 0, 0),
			wantErr: true,
		},
		{
			// Not an error: the clause every read model shares refuses only
			// to <= from, so a sub-day span must collapse to an empty window
			// here exactly as it does in internal/invoice.
			name:     "sub-day span collapses to an empty window",
			from:     utc(2024, time.January, 15, 9, 0),
			to:       utc(2024, time.January, 15, 17, 0),
			wantFrom: "2024-01-15T00:00:00Z",
			wantTo:   "2024-01-15T00:00:00Z",
			wantDays: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := NewReader(&fakeLoader{}, core.FixedClock(utc(2024, time.January, 20, 12, 0)))
			got, err := r.Range(context.Background(), tc.from, tc.to)
			if tc.wantErr {
				if !errors.Is(err, core.ErrInvalid) {
					t.Fatalf("Range(%s, %s) error = %v, want core.ErrInvalid", tc.from, tc.to, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Range(%s, %s) unexpected error: %v", tc.from, tc.to, err)
			}
			if gotFrom := got.From.Format(time.RFC3339); gotFrom != tc.wantFrom {
				t.Errorf("From = %s, want %s", gotFrom, tc.wantFrom)
			}
			if gotTo := got.To.Format(time.RFC3339); gotTo != tc.wantTo {
				t.Errorf("To = %s, want %s", gotTo, tc.wantTo)
			}
			if len(got.Days) != tc.wantDays {
				t.Errorf("len(Days) = %d, want %d", len(got.Days), tc.wantDays)
			}
		})
	}
}

func TestRangeDays(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		now      time.Time
		seed     func(t *testing.T, f *fakeLoader)
		from, to time.Time
		want     []time.Duration
	}{
		{
			name: "single day with two entries",
			now:  utc(2024, time.January, 1, 23, 0),
			seed: func(t *testing.T, f *fakeLoader) {
				f.addEntry(t, "a", "alpha", nil, utc(2024, time.January, 1, 9, 0), utc(2024, time.January, 1, 11, 0))
				f.addEntry(t, "b", "beta", nil, utc(2024, time.January, 1, 13, 0), utc(2024, time.January, 1, 14, 30))
			},
			from: utc(2024, time.January, 1, 0, 0),
			to:   utc(2024, time.January, 2, 0, 0),
			want: []time.Duration{3*time.Hour + 30*time.Minute},
		},
		{
			name: "idle day is present as a zero total",
			now:  utc(2024, time.January, 4, 0, 0),
			seed: func(t *testing.T, f *fakeLoader) {
				f.addEntry(t, "a", "alpha", nil, utc(2024, time.January, 1, 9, 0), utc(2024, time.January, 1, 11, 0))
				f.addEntry(t, "b", "alpha", nil, utc(2024, time.January, 3, 13, 0), utc(2024, time.January, 3, 14, 30))
			},
			from: utc(2024, time.January, 1, 0, 0),
			to:   utc(2024, time.January, 4, 0, 0),
			want: []time.Duration{2 * time.Hour, 0, 90 * time.Minute},
		},
		{
			name: "entry spanning midnight splits in proportion",
			now:  utc(2024, time.January, 3, 0, 0),
			seed: func(t *testing.T, f *fakeLoader) {
				// 23:00 → 01:30 is one hour on the 1st and ninety minutes on
				// the 2nd, not two and a half hours on the 1st.
				f.addEntry(t, "a", "alpha", nil, utc(2024, time.January, 1, 23, 0), utc(2024, time.January, 2, 1, 30))
			},
			from: utc(2024, time.January, 1, 0, 0),
			to:   utc(2024, time.January, 3, 0, 0),
			want: []time.Duration{time.Hour, 90 * time.Minute},
		},
		{
			name: "entry spanning three days splits across all of them",
			now:  utc(2024, time.January, 5, 0, 0),
			seed: func(t *testing.T, f *fakeLoader) {
				f.addEntry(t, "a", "alpha", nil, utc(2024, time.January, 1, 22, 0), utc(2024, time.January, 3, 2, 0))
			},
			from: utc(2024, time.January, 1, 0, 0),
			to:   utc(2024, time.January, 5, 0, 0),
			want: []time.Duration{2 * time.Hour, 24 * time.Hour, 2 * time.Hour, 0},
		},
		{
			name: "midnight split follows the caller's zone, not UTC",
			now:  utc(2024, time.March, 12, 0, 0),
			seed: func(t *testing.T, f *fakeLoader) {
				// 18:00Z → 21:00Z is 23:00 → 02:00 in +05: one hour on the
				// 10th, two on the 11th. Comparing in UTC would put all three
				// on the 10th.
				f.addEntry(t, "a", "alpha", nil, utc(2024, time.March, 10, 18, 0), utc(2024, time.March, 10, 21, 0))
			},
			from: local(plus5, 2024, time.March, 10, 0, 0),
			to:   local(plus5, 2024, time.March, 12, 0, 0),
			want: []time.Duration{time.Hour, 2 * time.Hour},
		},
		{
			name: "entries are clipped to the window edges",
			now:  utc(2024, time.January, 13, 0, 0),
			seed: func(t *testing.T, f *fakeLoader) {
				f.addEntry(t, "before", "alpha", nil, utc(2024, time.January, 9, 22, 0), utc(2024, time.January, 10, 2, 0))
				f.addEntry(t, "after", "alpha", nil, utc(2024, time.January, 11, 23, 0), utc(2024, time.January, 12, 5, 0))
				f.addEntry(t, "outside", "alpha", nil, utc(2024, time.January, 5, 9, 0), utc(2024, time.January, 5, 17, 0))
			},
			from: utc(2024, time.January, 10, 0, 0),
			to:   utc(2024, time.January, 12, 0, 0),
			want: []time.Duration{2 * time.Hour, time.Hour},
		},
		{
			name: "running entry counts up to the clock",
			now:  utc(2024, time.January, 2, 11, 30),
			seed: func(t *testing.T, f *fakeLoader) {
				f.startTimer(t, "live", "alpha", nil, utc(2024, time.January, 2, 9, 0))
			},
			from: utc(2024, time.January, 1, 0, 0),
			to:   utc(2024, time.January, 3, 0, 0),
			want: []time.Duration{0, 150 * time.Minute},
		},
		{
			name: "running entry is clipped like any other",
			now:  utc(2024, time.January, 2, 0, 30),
			seed: func(t *testing.T, f *fakeLoader) {
				f.startTimer(t, "live", "alpha", nil, utc(2024, time.January, 1, 23, 0))
			},
			from: utc(2024, time.January, 1, 0, 0),
			to:   utc(2024, time.January, 2, 0, 0),
			want: []time.Duration{time.Hour},
		},
		{
			name: "edits and tombstones are honoured by the canonical replay",
			now:  utc(2024, time.January, 3, 0, 0),
			seed: func(t *testing.T, f *fakeLoader) {
				f.addEntry(t, "a", "alpha", nil, utc(2024, time.January, 1, 9, 0), utc(2024, time.January, 1, 10, 0))
				f.addEntry(t, "b", "beta", nil, utc(2024, time.January, 1, 13, 0), utc(2024, time.January, 1, 17, 0))
				end := utc(2024, time.January, 1, 12, 0)
				f.emit(t, core.KindEntryEdited, core.EntryAggregate("a"), end, core.EntryEdited{
					EntryID: "a", End: &end,
				})
				f.emit(t, core.KindEntryDeleted, core.EntryAggregate("b"), utc(2024, time.January, 1, 18, 0), core.EntryDeleted{
					EntryID: "b",
				})
			},
			from: utc(2024, time.January, 1, 0, 0),
			to:   utc(2024, time.January, 2, 0, 0),
			want: []time.Duration{3 * time.Hour},
		},
		{
			name: "empty log yields a dense zero-filled report",
			now:  utc(2024, time.January, 4, 0, 0),
			seed: func(t *testing.T, f *fakeLoader) {},
			from: utc(2024, time.January, 1, 0, 0),
			to:   utc(2024, time.January, 4, 0, 0),
			want: []time.Duration{0, 0, 0},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := &fakeLoader{}
			tc.seed(t, f)
			r := NewReader(f, core.FixedClock(tc.now))

			got, err := r.Range(context.Background(), tc.from, tc.to)
			if err != nil {
				t.Fatalf("Range: %v", err)
			}
			if len(got.Days) != len(tc.want) {
				t.Fatalf("len(Days) = %d, want %d (%v)", len(got.Days), len(tc.want), got.Days)
			}

			// Days must be dense and ascending: day i is exactly i calendar
			// days after the resolved window start.
			day := core.Day(tc.from)
			for i, want := range tc.want {
				if wantDate := day.AddDate(0, 0, i); !got.Days[i].Date.Equal(wantDate) {
					t.Errorf("Days[%d].Date = %s, want %s",
						i, got.Days[i].Date.Format(time.RFC3339), wantDate.Format(time.RFC3339))
				}
				if got.Days[i].Duration != want {
					t.Errorf("Days[%d].Duration = %s, want %s", i, got.Days[i].Duration, want)
				}
			}

			// The whole point of splitting by overlap: the parts tile the
			// window, so the days must reconstruct the total exactly.
			if sum := sumDays(got.Days); sum != got.Total {
				t.Errorf("sum(Days) = %s, Total = %s: they must agree exactly", sum, got.Total)
			}
			if got.ByProject == nil {
				t.Error("ByProject is nil; the renderer needs an empty slice to draw an empty state")
			}
			if got.ByTag == nil {
				t.Error("ByTag is nil; the renderer needs an empty slice to draw an empty state")
			}
		})
	}
}

// TestRangeSubDaySpanIsZeroReport pins the cross-read-model agreement: a span
// that starts and ends inside one calendar day is answered, not refused, even
// when that day is full of work. The report must still be structurally valid so
// the renderer can draw an empty state.
func TestRangeSubDaySpanIsZeroReport(t *testing.T) {
	t.Parallel()

	f := &fakeLoader{}
	f.addEntry(t, "a", "alpha", []string{"dev"}, utc(2024, time.January, 15, 9, 0), utc(2024, time.January, 15, 17, 0))

	r := NewReader(f, core.FixedClock(utc(2024, time.January, 16, 0, 0)))
	got, err := r.Range(context.Background(), utc(2024, time.January, 15, 10, 0), utc(2024, time.January, 15, 14, 0))
	if err != nil {
		t.Fatalf("Range over a sub-day span: unexpected error %v", err)
	}
	if got.Total != 0 {
		t.Errorf("Total = %s, want 0", got.Total)
	}
	if got.Days == nil || len(got.Days) != 0 {
		t.Errorf("Days = %v, want a non-nil empty slice", got.Days)
	}
	if got.ByProject == nil || len(got.ByProject) != 0 {
		t.Errorf("ByProject = %v, want a non-nil empty slice", got.ByProject)
	}
	if got.ByTag == nil || len(got.ByTag) != 0 {
		t.Errorf("ByTag = %v, want a non-nil empty slice", got.ByTag)
	}
	if !got.From.Equal(got.To) {
		t.Errorf("From = %s, To = %s, want an empty window", got.From, got.To)
	}
}

func TestRangeBuckets(t *testing.T) {
	t.Parallel()

	f := &fakeLoader{}
	f.addEntry(t, "a", "alpha", []string{"dev", "billable"}, utc(2024, time.January, 1, 9, 0), utc(2024, time.January, 1, 11, 0))
	f.addEntry(t, "b", "beta", []string{"dev"}, utc(2024, time.January, 1, 11, 0), utc(2024, time.January, 1, 12, 0))
	f.addEntry(t, "c", "alpha", nil, utc(2024, time.January, 1, 13, 0), utc(2024, time.January, 1, 14, 0))

	r := NewReader(f, core.FixedClock(utc(2024, time.January, 2, 0, 0)))
	got, err := r.Range(context.Background(), utc(2024, time.January, 1, 0, 0), utc(2024, time.January, 2, 0, 0))
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if want := 4 * time.Hour; got.Total != want {
		t.Fatalf("Total = %s, want %s", got.Total, want)
	}

	wantProjects := []core.Bucket{
		{Key: "alpha", Duration: 3 * time.Hour, Share: 0.75},
		{Key: "beta", Duration: time.Hour, Share: 0.25},
	}
	if len(got.ByProject) != len(wantProjects) {
		t.Fatalf("ByProject = %v, want %v", got.ByProject, wantProjects)
	}
	var projectShare float64
	for i, want := range wantProjects {
		if got.ByProject[i] != want {
			t.Errorf("ByProject[%d] = %+v, want %+v", i, got.ByProject[i], want)
		}
		projectShare += got.ByProject[i].Share
	}
	// Projects partition the total, so their shares must add up to exactly 1.
	if projectShare != 1 {
		t.Errorf("sum of ByProject shares = %v, want 1", projectShare)
	}

	// A two-tag entry contributes its full duration to both tags, so "dev" and
	// "billable" overlap and the shares deliberately exceed 1.
	wantTags := []core.Bucket{
		{Key: "dev", Duration: 3 * time.Hour, Share: 0.75},
		{Key: "billable", Duration: 2 * time.Hour, Share: 0.5},
	}
	if len(got.ByTag) != len(wantTags) {
		t.Fatalf("ByTag = %v, want %v", got.ByTag, wantTags)
	}
	var tagShare float64
	for i, want := range wantTags {
		if got.ByTag[i] != want {
			t.Errorf("ByTag[%d] = %+v, want %+v", i, got.ByTag[i], want)
		}
		tagShare += got.ByTag[i].Share
	}
	if tagShare <= 1 {
		t.Errorf("sum of ByTag shares = %v, want > 1: tags are labels, not a partition", tagShare)
	}
}

func TestWeek(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		day      time.Time
		wantFrom string
		wantTo   string
	}{
		{
			name:     "sunday closes the week that began six days earlier",
			day:      utc(2024, time.March, 17, 21, 45), // a Sunday
			wantFrom: "2024-03-11T00:00:00Z",
			wantTo:   "2024-03-18T00:00:00Z",
		},
		{
			name:     "monday opens its own week",
			day:      utc(2024, time.March, 11, 0, 0), // a Monday
			wantFrom: "2024-03-11T00:00:00Z",
			wantTo:   "2024-03-18T00:00:00Z",
		},
		{
			name:     "midweek day resolves to the same Monday",
			day:      utc(2024, time.March, 14, 13, 5), // a Thursday
			wantFrom: "2024-03-11T00:00:00Z",
			wantTo:   "2024-03-18T00:00:00Z",
		},
		{
			name:     "week is resolved in the day's own location",
			day:      local(plus5, 2024, time.March, 17, 2, 0), // Sunday local, Saturday in UTC
			wantFrom: "2024-03-11T00:00:00+05:00",
			wantTo:   "2024-03-18T00:00:00+05:00",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := NewReader(&fakeLoader{}, core.FixedClock(utc(2024, time.March, 20, 12, 0)))
			got, err := r.Week(context.Background(), tc.day)
			if err != nil {
				t.Fatalf("Week(%s): %v", tc.day, err)
			}
			if gotFrom := got.From.Format(time.RFC3339); gotFrom != tc.wantFrom {
				t.Errorf("From = %s, want %s", gotFrom, tc.wantFrom)
			}
			if gotTo := got.To.Format(time.RFC3339); gotTo != tc.wantTo {
				t.Errorf("To = %s, want %s", gotTo, tc.wantTo)
			}
			if got.From.Weekday() != time.Monday {
				t.Errorf("From falls on %s, want Monday", got.From.Weekday())
			}
			if len(got.Days) != 7 {
				t.Errorf("len(Days) = %d, want 7", len(got.Days))
			}
		})
	}
}

func TestMonth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		day      time.Time
		wantFrom string
		wantTo   string
		wantDays int
	}{
		{
			name:     "february in a leap year",
			day:      utc(2024, time.February, 15, 10, 0),
			wantFrom: "2024-02-01T00:00:00Z",
			wantTo:   "2024-03-01T00:00:00Z",
			wantDays: 29,
		},
		{
			name:     "february in a common year",
			day:      utc(2023, time.February, 15, 10, 0),
			wantFrom: "2023-02-01T00:00:00Z",
			wantTo:   "2023-03-01T00:00:00Z",
			wantDays: 28,
		},
		{
			name:     "thirty day month",
			day:      utc(2024, time.April, 10, 10, 0),
			wantFrom: "2024-04-01T00:00:00Z",
			wantTo:   "2024-05-01T00:00:00Z",
			wantDays: 30,
		},
		{
			name:     "last day of a thirty-one day month",
			day:      utc(2024, time.January, 31, 23, 59),
			wantFrom: "2024-01-01T00:00:00Z",
			wantTo:   "2024-02-01T00:00:00Z",
			wantDays: 31,
		},
		{
			name:     "december rolls into the next year",
			day:      utc(2024, time.December, 15, 10, 0),
			wantFrom: "2024-12-01T00:00:00Z",
			wantTo:   "2025-01-01T00:00:00Z",
			wantDays: 31,
		},
		{
			name:     "month is resolved in the day's own location",
			day:      local(plus5, 2024, time.April, 1, 2, 0), // still March in UTC
			wantFrom: "2024-04-01T00:00:00+05:00",
			wantTo:   "2024-05-01T00:00:00+05:00",
			wantDays: 30,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := NewReader(&fakeLoader{}, core.FixedClock(utc(2025, time.January, 5, 12, 0)))
			got, err := r.Month(context.Background(), tc.day)
			if err != nil {
				t.Fatalf("Month(%s): %v", tc.day, err)
			}
			if gotFrom := got.From.Format(time.RFC3339); gotFrom != tc.wantFrom {
				t.Errorf("From = %s, want %s", gotFrom, tc.wantFrom)
			}
			if gotTo := got.To.Format(time.RFC3339); gotTo != tc.wantTo {
				t.Errorf("To = %s, want %s", gotTo, tc.wantTo)
			}
			if len(got.Days) != tc.wantDays {
				t.Errorf("len(Days) = %d, want %d", len(got.Days), tc.wantDays)
			}
		})
	}
}

// TestRangeRereadsLogEveryCall pins the no-caching rule: a second call must see
// events appended after the first one answered.
func TestRangeRereadsLogEveryCall(t *testing.T) {
	t.Parallel()

	f := &fakeLoader{}
	f.addEntry(t, "a", "alpha", nil, utc(2024, time.January, 1, 9, 0), utc(2024, time.January, 1, 10, 0))

	r := NewReader(f, core.FixedClock(utc(2024, time.January, 2, 0, 0)))
	from, to := utc(2024, time.January, 1, 0, 0), utc(2024, time.January, 2, 0, 0)

	first, err := r.Range(context.Background(), from, to)
	if err != nil {
		t.Fatalf("first Range: %v", err)
	}
	if want := time.Hour; first.Total != want {
		t.Fatalf("first Total = %s, want %s", first.Total, want)
	}

	f.addEntry(t, "b", "beta", nil, utc(2024, time.January, 1, 14, 0), utc(2024, time.January, 1, 16, 0))

	second, err := r.Range(context.Background(), from, to)
	if err != nil {
		t.Fatalf("second Range: %v", err)
	}
	if want := 3 * time.Hour; second.Total != want {
		t.Errorf("second Total = %s, want %s (a cached projection would still report %s)", second.Total, want, first.Total)
	}
	if f.loads != 2 {
		t.Errorf("Load called %d times, want 2: every query re-reads the log", f.loads)
	}
}

// TestRangeLoadErrorWraps checks the loader's failure reaches the caller intact
// rather than being flattened into a new sentinel.
func TestRangeLoadErrorWraps(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("store offline")
	r := NewReader(&fakeLoader{err: sentinel}, core.FixedClock(utc(2024, time.January, 2, 0, 0)))

	_, err := r.Range(context.Background(), utc(2024, time.January, 1, 0, 0), utc(2024, time.January, 2, 0, 0))
	if !errors.Is(err, sentinel) {
		t.Fatalf("Range error = %v, want it to wrap %v", err, sentinel)
	}
}
