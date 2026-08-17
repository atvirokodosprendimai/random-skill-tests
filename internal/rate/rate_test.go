package rate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/random-skill-tests/internal/core"
)

// testAt is the instant every test's FixedClock reports, so event timestamps
// are deterministic and no test depends on wall-clock ordering.
var testAt = time.Date(2026, 3, 14, 9, 26, 53, 0, time.UTC)

// fakeLog is an in-memory core.Log that assigns Seq on append, plus injectable
// failures so the persist-then-publish ordering can be tested from the error
// side as well as the happy side.
type fakeLog struct {
	events    []core.Event
	appendErr error
	loadErr   error
}

func (f *fakeLog) Append(_ context.Context, events ...core.Event) error {
	if f.appendErr != nil {
		return f.appendErr
	}
	for _, e := range events {
		e.Seq = int64(len(f.events)) + 1
		f.events = append(f.events, e)
	}
	return nil
}

func (f *fakeLog) Load(_ context.Context, sinceSeq int64) ([]core.Event, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	out := make([]core.Event, 0, len(f.events))
	for _, e := range f.events {
		if e.Seq > sinceSeq {
			out = append(out, e)
		}
	}
	return out, nil
}

// publishCall records one core.Publisher invocation.
type publishCall struct {
	subject     string
	aggregateID string
}

// fakePublisher records every Publish call so a test can assert on both what
// was published and, more importantly, that nothing was.
type fakePublisher struct {
	calls []publishCall
}

func (f *fakePublisher) Publish(subject, aggregateID string) {
	f.calls = append(f.calls, publishCall{subject: subject, aggregateID: aggregateID})
}

// newTestService wires a Service over fresh fakes and returns all three.
func newTestService(t *testing.T) (*Service, *fakeLog, *fakePublisher) {
	t.Helper()
	log := &fakeLog{}
	pub := &fakePublisher{}
	return NewService(log, pub, core.FixedClock(testAt)), log, pub
}

func TestSetGetRoundTrip(t *testing.T) {
	svc, log, pub := newTestService(t)
	ctx := context.Background()

	set, err := svc.Set(ctx, "acme", 12_500, "eur")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	want := Rate{Project: "acme", CentsPerHour: 12_500, Currency: "EUR"}
	if set != want {
		t.Errorf("Set returned %+v, want %+v", set, want)
	}

	if n := len(log.events); n != 1 {
		t.Fatalf("appended %d events, want 1", n)
	}
	ev := log.events[0]
	if ev.Kind != core.KindRateSet {
		t.Errorf("event kind = %q, want %q", ev.Kind, core.KindRateSet)
	}
	if ev.Aggregate != core.RatesAggregate {
		t.Errorf("event aggregate = %q, want %q", ev.Aggregate, core.RatesAggregate)
	}
	if !ev.At.Equal(testAt) {
		t.Errorf("event at = %v, want %v", ev.At, testAt)
	}
	payload, err := core.DecodePayload[core.RateSet](ev)
	if err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if payload != (core.RateSet{Project: "acme", CentsPerHour: 12_500, Currency: "EUR"}) {
		t.Errorf("payload = %+v, want normalised rate", payload)
	}

	wantCalls := []publishCall{{subject: core.SubjectRates, aggregateID: "acme"}}
	if len(pub.calls) != 1 || pub.calls[0] != wantCalls[0] {
		t.Errorf("publisher calls = %+v, want %+v", pub.calls, wantCalls)
	}

	got, err := svc.Get(ctx, "acme")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != want {
		t.Errorf("Get returned %+v, want %+v", got, want)
	}
}

func TestSetSupersedesPreviousRate(t *testing.T) {
	svc, log, _ := newTestService(t)
	ctx := context.Background()

	if _, err := svc.Set(ctx, "acme", 10_000, "EUR"); err != nil {
		t.Fatalf("first Set: %v", err)
	}
	// Re-rating a project is legal: it supersedes, it does not conflict.
	if _, err := svc.Set(ctx, "acme", 15_000, "USD"); err != nil {
		t.Fatalf("second Set: %v", err)
	}

	// Both events survive — the old rate is what lets a past invoice reproduce.
	if n := len(log.events); n != 2 {
		t.Fatalf("appended %d events, want 2", n)
	}

	got, err := svc.Get(ctx, "acme")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := Rate{Project: "acme", CentsPerHour: 15_000, Currency: "USD"}
	if got != want {
		t.Errorf("Get returned %+v, want the later rate %+v", got, want)
	}
}

func TestGetUnknownProject(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()

	if _, err := svc.Set(ctx, "acme", 10_000, "EUR"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	_, err := svc.Get(ctx, "globex")
	if !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("Get unknown project: err = %v, want core.ErrNotFound", err)
	}
}

func TestSetValidation(t *testing.T) {
	tests := []struct {
		name         string
		project      string
		centsPerHour int64
		currency     string
		wantErr      error
		want         Rate
	}{
		{
			name:     "empty project",
			project:  "",
			currency: "EUR",
			wantErr:  core.ErrInvalid,
		},
		{
			name:     "blank project",
			project:  "   ",
			currency: "EUR",
			wantErr:  core.ErrInvalid,
		},
		{
			name:         "negative cents",
			project:      "acme",
			centsPerHour: -1,
			currency:     "EUR",
			wantErr:      core.ErrInvalid,
		},
		{
			name:         "two letter currency",
			project:      "acme",
			centsPerHour: 100,
			currency:     "EU",
			wantErr:      core.ErrInvalid,
		},
		{
			name:         "four letter currency",
			project:      "acme",
			centsPerHour: 100,
			currency:     "EURO",
			wantErr:      core.ErrInvalid,
		},
		{
			name:         "non-ascii currency",
			project:      "acme",
			centsPerHour: 100,
			currency:     "€UR",
			wantErr:      core.ErrInvalid,
		},
		{
			name:         "three byte non-ascii currency",
			project:      "acme",
			centsPerHour: 100,
			currency:     "ÜR",
			wantErr:      core.ErrInvalid,
		},
		{
			name:         "digits in currency",
			project:      "acme",
			centsPerHour: 100,
			currency:     "EU1",
			wantErr:      core.ErrInvalid,
		},
		{
			name:         "lowercase currency is upper-cased",
			project:      "acme",
			centsPerHour: 100,
			currency:     "usd",
			want:         Rate{Project: "acme", CentsPerHour: 100, Currency: "USD"},
		},
		{
			name:         "empty currency defaults to EUR",
			project:      "acme",
			centsPerHour: 100,
			currency:     "",
			want:         Rate{Project: "acme", CentsPerHour: 100, Currency: "EUR"},
		},
		{
			name:         "blank currency defaults to EUR",
			project:      "acme",
			centsPerHour: 100,
			currency:     "  ",
			want:         Rate{Project: "acme", CentsPerHour: 100, Currency: "EUR"},
		},
		{
			name:         "zero cents is pro-bono, not invalid",
			project:      "acme",
			centsPerHour: 0,
			currency:     "EUR",
			want:         Rate{Project: "acme", CentsPerHour: 0, Currency: "EUR"},
		},
		{
			name:         "project and currency are trimmed",
			project:      "  acme  ",
			centsPerHour: 100,
			currency:     " chf ",
			want:         Rate{Project: "acme", CentsPerHour: 100, Currency: "CHF"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, log, pub := newTestService(t)

			got, err := svc.Set(context.Background(), tt.project, tt.centsPerHour, tt.currency)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Set err = %v, want %v", err, tt.wantErr)
				}
				// A rejected command must reach neither the log nor the bus.
				if len(log.events) != 0 {
					t.Errorf("appended %d events on validation failure, want 0", len(log.events))
				}
				if len(pub.calls) != 0 {
					t.Errorf("published %d times on validation failure, want 0", len(pub.calls))
				}
				return
			}
			if err != nil {
				t.Fatalf("Set: %v", err)
			}
			if got != tt.want {
				t.Errorf("Set returned %+v, want %+v", got, tt.want)
			}

			stored, err := svc.Get(context.Background(), tt.want.Project)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if stored != tt.want {
				t.Errorf("Get returned %+v, want %+v", stored, tt.want)
			}
		})
	}
}

func TestGetEmptyProject(t *testing.T) {
	svc, _, _ := newTestService(t)

	if _, err := svc.Get(context.Background(), "  "); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("Get blank project: err = %v, want core.ErrInvalid", err)
	}
}

func TestList(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()

	empty, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List on empty log: %v", err)
	}
	if empty == nil {
		t.Error("List returned nil, want non-nil empty slice")
	}
	if len(empty) != 0 {
		t.Errorf("List on empty log returned %d rates, want 0", len(empty))
	}

	// Written out of order, and with acme re-rated, so both the sort and the
	// supersede rule are exercised at once.
	for _, r := range []Rate{
		{Project: "globex", CentsPerHour: 9_000, Currency: "USD"},
		{Project: "acme", CentsPerHour: 10_000, Currency: "EUR"},
		{Project: "initech", CentsPerHour: 0, Currency: "GBP"},
		{Project: "acme", CentsPerHour: 11_000, Currency: "EUR"},
	} {
		if _, err := svc.Set(ctx, r.Project, r.CentsPerHour, r.Currency); err != nil {
			t.Fatalf("Set %q: %v", r.Project, err)
		}
	}

	got, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []Rate{
		{Project: "acme", CentsPerHour: 11_000, Currency: "EUR"},
		{Project: "globex", CentsPerHour: 9_000, Currency: "USD"},
		{Project: "initech", CentsPerHour: 0, Currency: "GBP"},
	}
	if len(got) != len(want) {
		t.Fatalf("List returned %d rates (%+v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("List[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestSetAppendErrorPublishesNothing pins the single most important rule in the
// codebase: the bus never announces an event the log refused.
func TestSetAppendErrorPublishesNothing(t *testing.T) {
	wantErr := errors.New("disk on fire")
	log := &fakeLog{appendErr: wantErr}
	pub := &fakePublisher{}
	svc := NewService(log, pub, core.FixedClock(testAt))

	_, err := svc.Set(context.Background(), "acme", 10_000, "EUR")
	if !errors.Is(err, wantErr) {
		t.Fatalf("Set err = %v, want it to wrap %v", err, wantErr)
	}
	if len(pub.calls) != 0 {
		t.Fatalf("published %d times after a failed append, want 0: %+v", len(pub.calls), pub.calls)
	}
	if len(log.events) != 0 {
		t.Errorf("log holds %d events after a failed append, want 0", len(log.events))
	}
}

func TestSetNilPublisherIsNotAnError(t *testing.T) {
	log := &fakeLog{}
	svc := NewService(log, nil, core.FixedClock(testAt))

	got, err := svc.Set(context.Background(), "acme", 10_000, "EUR")
	if err != nil {
		t.Fatalf("Set with nil publisher: %v", err)
	}
	if want := (Rate{Project: "acme", CentsPerHour: 10_000, Currency: "EUR"}); got != want {
		t.Errorf("Set returned %+v, want %+v", got, want)
	}
	if len(log.events) != 1 {
		t.Errorf("appended %d events, want 1", len(log.events))
	}
}

func TestReadsPropagateLoadError(t *testing.T) {
	wantErr := errors.New("log unreadable")
	svc := NewService(&fakeLog{loadErr: wantErr}, &fakePublisher{}, core.FixedClock(testAt))
	ctx := context.Background()

	if _, err := svc.Get(ctx, "acme"); !errors.Is(err, wantErr) {
		t.Errorf("Get err = %v, want it to wrap %v", err, wantErr)
	}
	if _, err := svc.List(ctx); !errors.Is(err, wantErr) {
		t.Errorf("List err = %v, want it to wrap %v", err, wantErr)
	}
}
