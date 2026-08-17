package project

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/random-skill-tests/internal/core"
)

// fakeLog is an in-memory core.Log. It assigns Seq the way the real store does,
// because BuildTimeline uses Seq for ordering and Load's sinceSeq filter.
type fakeLog struct {
	events    []core.Event
	seq       int64
	appendErr error
	loadErr   error
}

func (f *fakeLog) Append(_ context.Context, events ...core.Event) error {
	if f.appendErr != nil {
		return f.appendErr
	}
	for _, e := range events {
		f.seq++
		e.Seq = f.seq
		f.events = append(f.events, e)
	}
	return nil
}

func (f *fakeLog) Load(_ context.Context, sinceSeq int64) ([]core.Event, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	var out []core.Event
	for _, e := range f.events {
		if e.Seq > sinceSeq {
			out = append(out, e)
		}
	}
	return out, nil
}

// publishCall records one Publisher invocation so tests can assert both that a
// publish happened and that one did not.
type publishCall struct {
	subject     string
	aggregateID string
}

type fakePublisher struct {
	calls []publishCall
}

func (f *fakePublisher) Publish(subject, aggregateID string) {
	f.calls = append(f.calls, publishCall{subject: subject, aggregateID: aggregateID})
}

var testNow = time.Date(2026, 3, 14, 9, 30, 0, 0, time.UTC)

// newTestService wires a Service over fresh fakes with a fixed clock.
func newTestService(t *testing.T) (*Service, *fakeLog, *fakePublisher) {
	t.Helper()
	log := &fakeLog{}
	pub := &fakePublisher{}
	return NewService(log, pub, core.FixedClock(testNow)), log, pub
}

func TestCreate(t *testing.T) {
	svc, log, pub := newTestService(t)

	got, err := svc.Create(context.Background(), "  web  ", "  Web Redesign  ")
	if err != nil {
		t.Fatalf("Create: unexpected error: %v", err)
	}

	want := core.Project{Slug: "web", Name: "Web Redesign"}
	if got != want {
		t.Errorf("Create returned %+v, want %+v", got, want)
	}

	if len(log.events) != 1 {
		t.Fatalf("appended %d events, want 1", len(log.events))
	}
	ev := log.events[0]
	if ev.Kind != core.KindProjectCreated {
		t.Errorf("event kind = %q, want %q", ev.Kind, core.KindProjectCreated)
	}
	if ev.Aggregate != core.ProjectAggregate("web") {
		t.Errorf("event aggregate = %q, want %q", ev.Aggregate, core.ProjectAggregate("web"))
	}
	if !ev.At.Equal(testNow) {
		t.Errorf("event At = %v, want %v", ev.At, testNow)
	}
	payload, err := core.DecodePayload[core.ProjectCreated](ev)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Slug != "web" || payload.Name != "Web Redesign" {
		t.Errorf("payload = %+v, want {web Web Redesign}", payload)
	}

	wantCalls := []publishCall{{subject: core.SubjectProjects, aggregateID: "web"}}
	if len(pub.calls) != 1 || pub.calls[0] != wantCalls[0] {
		t.Errorf("publish calls = %+v, want %+v", pub.calls, wantCalls)
	}
}

func TestCreateDefaultsNameToSlug(t *testing.T) {
	svc, _, _ := newTestService(t)

	got, err := svc.Create(context.Background(), "api", "   ")
	if err != nil {
		t.Fatalf("Create: unexpected error: %v", err)
	}
	if got.Name != "api" {
		t.Errorf("Name = %q, want %q", got.Name, "api")
	}
}

func TestCreateNilPublisher(t *testing.T) {
	// A CLI run has no bus; a nil Publisher must not be an error or a panic.
	svc := NewService(&fakeLog{}, nil, core.FixedClock(testNow))

	if _, err := svc.Create(context.Background(), "api", "API"); err != nil {
		t.Fatalf("Create with nil publisher: %v", err)
	}
}

func TestCreateDuplicate(t *testing.T) {
	tests := []struct {
		name    string
		archive bool
	}{
		{name: "active project"},
		{name: "archived project", archive: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _, pub := newTestService(t)
			ctx := context.Background()

			if _, err := svc.Create(ctx, "web", "Web"); err != nil {
				t.Fatalf("seed Create: %v", err)
			}
			if tt.archive {
				if err := svc.Archive(ctx, "web"); err != nil {
					t.Fatalf("seed Archive: %v", err)
				}
			}
			before := len(pub.calls)

			_, err := svc.Create(ctx, "web", "Web Again")
			if !errors.Is(err, core.ErrConflict) {
				t.Fatalf("Create duplicate error = %v, want core.ErrConflict", err)
			}
			// A refused command is not a change, so it must not notify.
			if len(pub.calls) != before {
				t.Errorf("publish calls = %d, want %d (no publish on conflict)", len(pub.calls), before)
			}
		})
	}
}

func TestSlugValidation(t *testing.T) {
	tests := []struct {
		name string
		slug string
	}{
		{name: "empty", slug: ""},
		{name: "whitespace only", slug: "   "},
		{name: "uppercase", slug: "Web"},
		{name: "leading hyphen", slug: "-web"},
		{name: "inner spaces", slug: "web app"},
		{name: "underscore", slug: "web_app"},
		{name: "too long", slug: strings.Repeat("a", maxSlugLen+1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, log, pub := newTestService(t)
			ctx := context.Background()

			if _, err := svc.Create(ctx, tt.slug, "Name"); !errors.Is(err, core.ErrInvalid) {
				t.Errorf("Create(%q) error = %v, want core.ErrInvalid", tt.slug, err)
			}
			if err := svc.Archive(ctx, tt.slug); !errors.Is(err, core.ErrInvalid) {
				t.Errorf("Archive(%q) error = %v, want core.ErrInvalid", tt.slug, err)
			}
			if _, err := svc.Get(ctx, tt.slug); !errors.Is(err, core.ErrInvalid) {
				t.Errorf("Get(%q) error = %v, want core.ErrInvalid", tt.slug, err)
			}
			if len(log.events) != 0 {
				t.Errorf("appended %d events on invalid slug, want 0", len(log.events))
			}
			if len(pub.calls) != 0 {
				t.Errorf("published %d times on invalid slug, want 0", len(pub.calls))
			}
		})
	}
}

func TestSlugBoundaryLengthAccepted(t *testing.T) {
	svc, _, _ := newTestService(t)

	slug := strings.Repeat("a", maxSlugLen)
	if _, err := svc.Create(context.Background(), slug, ""); err != nil {
		t.Fatalf("Create with %d-char slug: %v", maxSlugLen, err)
	}
}

func TestArchive(t *testing.T) {
	svc, log, pub := newTestService(t)
	ctx := context.Background()

	if _, err := svc.Create(ctx, "web", "Web"); err != nil {
		t.Fatalf("seed Create: %v", err)
	}
	if err := svc.Archive(ctx, "web"); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	if len(log.events) != 2 {
		t.Fatalf("appended %d events, want 2", len(log.events))
	}
	ev := log.events[1]
	if ev.Kind != core.KindProjectArchived {
		t.Errorf("event kind = %q, want %q", ev.Kind, core.KindProjectArchived)
	}
	if ev.Aggregate != core.ProjectAggregate("web") {
		t.Errorf("event aggregate = %q, want %q", ev.Aggregate, core.ProjectAggregate("web"))
	}
	payload, err := core.DecodePayload[core.ProjectArchived](ev)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Slug != "web" {
		t.Errorf("payload slug = %q, want %q", payload.Slug, "web")
	}

	if len(pub.calls) != 2 {
		t.Fatalf("publish calls = %d, want 2", len(pub.calls))
	}
	want := publishCall{subject: core.SubjectProjects, aggregateID: "web"}
	if pub.calls[1] != want {
		t.Errorf("publish call = %+v, want %+v", pub.calls[1], want)
	}

	// History survives archival: the project is still resolvable by slug.
	got, err := svc.Get(ctx, "web")
	if err != nil {
		t.Fatalf("Get after archive: %v", err)
	}
	if !got.Archived {
		t.Errorf("Get(%q).Archived = false, want true", "web")
	}
}

func TestArchiveUnknown(t *testing.T) {
	svc, log, pub := newTestService(t)

	err := svc.Archive(context.Background(), "ghost")
	if !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("Archive unknown error = %v, want core.ErrNotFound", err)
	}
	if len(log.events) != 0 {
		t.Errorf("appended %d events, want 0", len(log.events))
	}
	if len(pub.calls) != 0 {
		t.Errorf("published %d times, want 0", len(pub.calls))
	}
}

func TestArchiveTwice(t *testing.T) {
	svc, log, pub := newTestService(t)
	ctx := context.Background()

	if _, err := svc.Create(ctx, "web", "Web"); err != nil {
		t.Fatalf("seed Create: %v", err)
	}
	if err := svc.Archive(ctx, "web"); err != nil {
		t.Fatalf("first Archive: %v", err)
	}

	err := svc.Archive(ctx, "web")
	if !errors.Is(err, core.ErrConflict) {
		t.Fatalf("second Archive error = %v, want core.ErrConflict", err)
	}
	if len(log.events) != 2 {
		t.Errorf("appended %d events, want 2 (no event for the refused command)", len(log.events))
	}
	if len(pub.calls) != 2 {
		t.Errorf("publish calls = %d, want 2 (no publish for the refused command)", len(pub.calls))
	}
}

func TestList(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()

	for _, p := range []struct{ slug, name string }{
		{"web", "Web"},
		{"api", "API"},
		{"ops", "Ops"},
	} {
		if _, err := svc.Create(ctx, p.slug, p.name); err != nil {
			t.Fatalf("seed Create %q: %v", p.slug, err)
		}
	}
	if err := svc.Archive(ctx, "ops"); err != nil {
		t.Fatalf("seed Archive: %v", err)
	}

	tests := []struct {
		name            string
		includeArchived bool
		want            []core.Project
	}{
		{
			name: "active only, sorted by slug",
			want: []core.Project{
				{Slug: "api", Name: "API"},
				{Slug: "web", Name: "Web"},
			},
		},
		{
			name:            "including archived",
			includeArchived: true,
			want: []core.Project{
				{Slug: "api", Name: "API"},
				{Slug: "ops", Name: "Ops", Archived: true},
				{Slug: "web", Name: "Web"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := svc.List(ctx, tt.includeArchived)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("List returned %+v, want %+v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("List[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestListEmpty(t *testing.T) {
	svc, _, _ := newTestService(t)

	got, err := svc.List(context.Background(), true)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List returned %+v, want empty", got)
	}
}

func TestGetUnknown(t *testing.T) {
	svc, _, _ := newTestService(t)

	if _, err := svc.Get(context.Background(), "ghost"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("Get unknown error = %v, want core.ErrNotFound", err)
	}
}

// TestNoPublishOnAppendError pins the ordering rule the whole codebase depends
// on: a failed append must leave the bus silent, because a subscriber would
// react by reading state that was never written.
func TestNoPublishOnAppendError(t *testing.T) {
	appendErr := errors.New("disk on fire")

	tests := []struct {
		name string
		// seed runs against a healthy log before Append starts failing.
		seed func(t *testing.T, svc *Service)
		call func(svc *Service) error
	}{
		{
			name: "create",
			call: func(svc *Service) error {
				_, err := svc.Create(context.Background(), "web", "Web")
				return err
			},
		},
		{
			name: "archive",
			seed: func(t *testing.T, svc *Service) {
				t.Helper()
				if _, err := svc.Create(context.Background(), "web", "Web"); err != nil {
					t.Fatalf("seed Create: %v", err)
				}
			},
			call: func(svc *Service) error {
				return svc.Archive(context.Background(), "web")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := &fakeLog{}
			pub := &fakePublisher{}
			svc := NewService(log, pub, core.FixedClock(testNow))

			if tt.seed != nil {
				tt.seed(t, svc)
			}
			pub.calls = nil
			log.appendErr = appendErr

			err := tt.call(svc)
			if !errors.Is(err, appendErr) {
				t.Fatalf("error = %v, want it to wrap %v", err, appendErr)
			}
			if len(pub.calls) != 0 {
				t.Fatalf("publisher recorded %d calls after a failed append, want 0", len(pub.calls))
			}
		})
	}
}

func TestLoadErrorPropagates(t *testing.T) {
	loadErr := errors.New("log unreadable")
	log := &fakeLog{loadErr: loadErr}
	pub := &fakePublisher{}
	svc := NewService(log, pub, core.FixedClock(testNow))
	ctx := context.Background()

	tests := []struct {
		name string
		call func() error
	}{
		{name: "create", call: func() error { _, err := svc.Create(ctx, "web", "Web"); return err }},
		{name: "archive", call: func() error { return svc.Archive(ctx, "web") }},
		{name: "list", call: func() error { _, err := svc.List(ctx, false); return err }},
		{name: "get", call: func() error { _, err := svc.Get(ctx, "web"); return err }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, loadErr) {
				t.Fatalf("error = %v, want it to wrap %v", err, loadErr)
			}
			if len(pub.calls) != 0 {
				t.Fatalf("publisher recorded %d calls, want 0", len(pub.calls))
			}
		})
	}
}
