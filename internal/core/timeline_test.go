package core

import (
	"errors"
	"testing"
	"time"
)

// base is a fixed instant every test builds from, so durations are exact.
var base = time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)

// ev builds an event at seq with the given kind and payload. Tests construct
// logs by hand rather than through the write models, because the whole point
// of a fold is that it depends on the log and nothing else.
func ev(t *testing.T, seq int64, kind Kind, aggregate string, payload any) Event {
	t.Helper()
	e, err := NewEvent(kind, aggregate, base, payload)
	if err != nil {
		t.Fatalf("NewEvent(%s): %v", kind, err)
	}
	e.Seq = seq
	return e
}

func TestBuildTimelineFoldsEntries(t *testing.T) {
	log := []Event{
		ev(t, 1, KindEntryAdded, EntryAggregate("a"), EntryAdded{
			EntryID: "a", Project: "web", Note: "layout", Tags: []string{"ui"},
			Start: base, End: base.Add(90 * time.Minute),
		}),
		ev(t, 2, KindEntryAdded, EntryAggregate("b"), EntryAdded{
			EntryID: "b", Project: "api", Start: base.Add(-time.Hour), End: base,
		}),
	}

	tl, err := BuildTimeline(log, base)
	if err != nil {
		t.Fatalf("BuildTimeline: %v", err)
	}
	if got, want := len(tl.Entries), 2; got != want {
		t.Fatalf("entries = %d, want %d", got, want)
	}
	// Sorted by Start, so the earlier "api" entry comes first regardless of
	// the order the events were appended in.
	if got, want := tl.Entries[0].ID, "b"; got != want {
		t.Errorf("first entry = %q, want %q (sorted by start)", got, want)
	}
	if got, want := tl.Entries[1].Duration, 90*time.Minute; got != want {
		t.Errorf("duration = %v, want %v", got, want)
	}
	if got, want := tl.Seq, int64(2); got != want {
		t.Errorf("seq = %d, want %d", got, want)
	}
}

func TestBuildTimelineRunningEntryMeasuredToNow(t *testing.T) {
	log := []Event{
		ev(t, 1, KindTimerStarted, EntryAggregate("a"), TimerStarted{
			EntryID: "a", Project: "web", StartedAt: base,
		}),
	}

	tl, err := BuildTimeline(log, base.Add(25*time.Minute))
	if err != nil {
		t.Fatalf("BuildTimeline: %v", err)
	}
	if tl.Running == nil {
		t.Fatal("Running = nil, want the open entry")
	}
	if got, want := tl.Running.Duration, 25*time.Minute; got != want {
		t.Errorf("running duration = %v, want %v", got, want)
	}
	if !tl.Running.End.IsZero() {
		t.Errorf("running End = %v, want zero — a running entry has no end yet", tl.Running.End)
	}
	// Running must be a view of what is in Entries, not a separate reality.
	if got, want := tl.Entries[0].Duration, tl.Running.Duration; got != want {
		t.Errorf("entries[0].Duration = %v, running = %v; they must agree", got, want)
	}
}

func TestBuildTimelineStopClosesTheInterval(t *testing.T) {
	log := []Event{
		ev(t, 1, KindTimerStarted, EntryAggregate("a"), TimerStarted{EntryID: "a", Project: "web", StartedAt: base}),
		ev(t, 2, KindTimerStopped, EntryAggregate("a"), TimerStopped{EntryID: "a", StoppedAt: base.Add(time.Hour)}),
	}

	tl, err := BuildTimeline(log, base.Add(5*time.Hour))
	if err != nil {
		t.Fatalf("BuildTimeline: %v", err)
	}
	if tl.Running != nil {
		t.Fatalf("Running = %+v, want nil after a stop", tl.Running)
	}
	// The clock is five hours past the start; a stopped entry must ignore it.
	if got, want := tl.Entries[0].Duration, time.Hour; got != want {
		t.Errorf("duration = %v, want %v — a stopped entry does not keep counting", got, want)
	}
}

func TestBuildTimelineAppliesSparseEdits(t *testing.T) {
	note := "revised"
	log := []Event{
		ev(t, 1, KindEntryAdded, EntryAggregate("a"), EntryAdded{
			EntryID: "a", Project: "web", Note: "original", Tags: []string{"ui"},
			Start: base, End: base.Add(time.Hour),
		}),
		ev(t, 2, KindEntryEdited, EntryAggregate("a"), EntryEdited{EntryID: "a", Note: &note}),
	}

	tl, err := BuildTimeline(log, base)
	if err != nil {
		t.Fatalf("BuildTimeline: %v", err)
	}
	got := tl.Entries[0]
	if got.Note != "revised" {
		t.Errorf("note = %q, want %q", got.Note, "revised")
	}
	// A nil patch field means "leave alone" — the untouched fields must survive.
	if got.Project != "web" {
		t.Errorf("project = %q, want %q — a sparse edit must not clear unset fields", got.Project, "web")
	}
	if got.Duration != time.Hour {
		t.Errorf("duration = %v, want %v", got.Duration, time.Hour)
	}
}

func TestBuildTimelineDeleteIsATombstone(t *testing.T) {
	note := "too late"
	log := []Event{
		ev(t, 1, KindEntryAdded, EntryAggregate("a"), EntryAdded{
			EntryID: "a", Project: "web", Start: base, End: base.Add(time.Hour),
		}),
		ev(t, 2, KindEntryDeleted, EntryAggregate("a"), EntryDeleted{EntryID: "a"}),
		// Editing a deleted entry must be a no-op, not a resurrection and not
		// an error: replay has to stay total over any log the writers produced.
		ev(t, 3, KindEntryEdited, EntryAggregate("a"), EntryEdited{EntryID: "a", Note: &note}),
	}

	tl, err := BuildTimeline(log, base)
	if err != nil {
		t.Fatalf("BuildTimeline: %v", err)
	}
	if len(tl.Entries) != 0 {
		t.Fatalf("entries = %+v, want none after a delete", tl.Entries)
	}
}

func TestBuildTimelineProjectsAndRates(t *testing.T) {
	log := []Event{
		ev(t, 1, KindProjectCreated, ProjectAggregate("web"), ProjectCreated{Slug: "web", Name: "Web"}),
		ev(t, 2, KindProjectCreated, ProjectAggregate("api"), ProjectCreated{Slug: "api", Name: "API"}),
		ev(t, 3, KindProjectArchived, ProjectAggregate("api"), ProjectArchived{Slug: "api"}),
		ev(t, 4, KindRateSet, RatesAggregate, RateSet{Project: "web", CentsPerHour: 8000, Currency: "EUR"}),
		// A later rate supersedes rather than conflicts; the log keeps both so
		// a past invoice stays reproducible.
		ev(t, 5, KindRateSet, RatesAggregate, RateSet{Project: "web", CentsPerHour: 9500, Currency: "EUR"}),
	}

	tl, err := BuildTimeline(log, base)
	if err != nil {
		t.Fatalf("BuildTimeline: %v", err)
	}
	if !tl.Projects["api"].Archived {
		t.Error("api should be archived")
	}
	if tl.Projects["web"].Archived {
		t.Error("web should not be archived")
	}
	if got, want := tl.Rates["web"], int64(9500); got != want {
		t.Errorf("rate = %d, want %d — the later RateSet wins", got, want)
	}
	if got, want := tl.Currencies["web"], "EUR"; got != want {
		t.Errorf("currency = %q, want %q", got, want)
	}
}

func TestBuildTimelineArchivingAnUnknownProjectIsIgnored(t *testing.T) {
	log := []Event{
		ev(t, 1, KindProjectArchived, ProjectAggregate("ghost"), ProjectArchived{Slug: "ghost"}),
	}

	tl, err := BuildTimeline(log, base)
	if err != nil {
		t.Fatalf("BuildTimeline: %v", err)
	}
	if _, ok := tl.Projects["ghost"]; ok {
		t.Error("archiving an uncreated slug must not conjure a nameless project")
	}
}

func TestBuildTimelineRejectsUnknownKind(t *testing.T) {
	log := []Event{ev(t, 1, Kind("entry.teleported"), "x", struct{}{})}

	_, err := BuildTimeline(log, base)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid — an unreadable log must fail loudly, not render a quietly wrong total", err)
	}
}

func TestBuildTimelineClampsNegativeDuration(t *testing.T) {
	log := []Event{
		ev(t, 1, KindEntryAdded, EntryAggregate("a"), EntryAdded{
			EntryID: "a", Project: "web", Start: base, End: base.Add(-time.Hour),
		}),
	}

	tl, err := BuildTimeline(log, base)
	if err != nil {
		t.Fatalf("BuildTimeline: %v", err)
	}
	// A backwards interval must not subtract from anybody's day.
	if got := tl.Entries[0].Duration; got != 0 {
		t.Errorf("duration = %v, want 0", got)
	}
}

func TestBuildTimelineEmptyLog(t *testing.T) {
	tl, err := BuildTimeline(nil, base)
	if err != nil {
		t.Fatalf("BuildTimeline: %v", err)
	}
	if tl.Running != nil || len(tl.Entries) != 0 {
		t.Errorf("empty log produced %+v, want an empty timeline", tl)
	}
	// Maps must be usable, not nil — every read model indexes them directly.
	if tl.Projects == nil || tl.Rates == nil || tl.Currencies == nil {
		t.Error("maps must be non-nil so callers can index without a guard")
	}
}

func TestBucketize(t *testing.T) {
	got := Bucketize(map[string]time.Duration{
		"web":  3 * time.Hour,
		"api":  time.Hour,
		"docs": 3 * time.Hour, // ties with web, broken by key
	}, 7*time.Hour)

	want := []string{"docs", "web", "api"}
	for i, key := range want {
		if got[i].Key != key {
			t.Errorf("bucket[%d] = %q, want %q (longest first, ties by key)", i, got[i].Key, key)
		}
	}
	if s := got[0].Share; s < 0.42 || s > 0.43 {
		t.Errorf("share = %v, want ~3/7", s)
	}
}

func TestBucketizeZeroTotal(t *testing.T) {
	// A zero total must not divide by zero; shares collapse to 0.
	got := Bucketize(map[string]time.Duration{"web": 0}, 0)
	if len(got) != 1 || got[0].Share != 0 {
		t.Errorf("got %+v, want a single zero-share bucket", got)
	}
}

func TestDayTruncatesInLocation(t *testing.T) {
	zone := time.FixedZone("UTC+3", 3*60*60)
	// 01:00 local on the 18th is still 22:00 UTC on the 17th; Day must follow
	// the caller's location, not UTC, or everyone's day boundary shifts.
	got := Day(time.Date(2026, 8, 18, 1, 0, 0, 0, zone))
	want := time.Date(2026, 8, 18, 0, 0, 0, 0, zone)
	if !got.Equal(want) {
		t.Errorf("Day = %v, want %v", got, want)
	}
}

func TestDecodePayloadRoundTrip(t *testing.T) {
	e := ev(t, 1, KindProjectCreated, ProjectAggregate("web"), ProjectCreated{Slug: "web", Name: "Web"})

	got, err := DecodePayload[ProjectCreated](e)
	if err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if got.Slug != "web" || got.Name != "Web" {
		t.Errorf("got %+v, want {web Web}", got)
	}
}

func TestNewEventNormalisesToUTC(t *testing.T) {
	zone := time.FixedZone("UTC+3", 3*60*60)
	e, err := NewEvent(KindProjectCreated, "x", time.Date(2026, 8, 17, 12, 0, 0, 0, zone), ProjectCreated{})
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	if e.At.Location() != time.UTC {
		t.Errorf("At location = %v, want UTC — the log must not carry a machine's offset", e.At.Location())
	}
	if e.ID == "" {
		t.Error("ID must be generated")
	}
	if e.Seq != 0 {
		t.Errorf("Seq = %d, want 0 — only the store assigns positions", e.Seq)
	}
}
