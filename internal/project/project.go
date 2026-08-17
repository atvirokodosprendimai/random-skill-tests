// Package project is the write model for the project aggregate: the single
// writer that turns project commands into events.
//
// Nothing here stores state. A command validates its arguments, folds the log
// with core.BuildTimeline to learn what currently exists, refuses the command
// if it would break an invariant, and otherwise appends exactly one event. The
// reads inside a command are part of the command, not a cache: because this is
// the only writer for the aggregate, the state it just read is still the state
// it is about to change.
//
// Every command that reaches the log follows one ordering rule — persist, then
// publish. See Service.commit.
package project

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/atvirokodosprendimai/random-skill-tests/internal/core"
)

// maxSlugLen bounds a slug so it stays usable as a CLI argument and a column in
// a rendered table. Valid slugs are ASCII by construction, so a byte count and
// a character count are the same number here.
const maxSlugLen = 40

// slugPattern is the whole vocabulary of a slug: lowercase alphanumerics and
// hyphens, never leading with a hyphen. Slugs are the stable key that entries,
// rates and reports all reference, so the shape is fixed once, at the only
// place that can mint one.
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Service is the single writer for the project aggregate.
//
// It is safe for concurrent use: it holds no mutable state of its own, deriving
// everything it needs from the log on each call. Serializing conflicting writes
// is the log's job, not a lock's.
type Service struct {
	log   core.Log
	pub   core.Publisher
	clock core.Clock
}

// NewService returns a Service backed by log, publishing to pub, timestamping
// with clock.
//
// pub may be nil: a one-shot CLI run has no bus to notify, and that is a normal
// configuration rather than an error.
func NewService(log core.Log, pub core.Publisher, clock core.Clock) *Service {
	return &Service{log: log, pub: pub, clock: clock}
}

// Create records a new project.
//
// slug is trimmed and must match `^[a-z0-9][a-z0-9-]*$` within maxSlugLen
// characters; anything else wraps core.ErrInvalid. name is display-only and
// defaults to slug when blank — a missing label is not a reason to refuse a
// well-formed command. A slug already in the log, archived or not, wraps
// core.ErrConflict: archiving hides a project, it does not free its key.
func (s *Service) Create(ctx context.Context, slug, name string) (core.Project, error) {
	slug, err := normalizeSlug(slug)
	if err != nil {
		return core.Project{}, err
	}

	name = strings.TrimSpace(name)
	if name == "" {
		name = slug
	}

	tl, err := s.timeline(ctx)
	if err != nil {
		return core.Project{}, fmt.Errorf("project: create %q: %w", slug, err)
	}
	if _, ok := tl.Projects[slug]; ok {
		return core.Project{}, fmt.Errorf("project: create %q: slug already taken: %w", slug, core.ErrConflict)
	}

	ev, err := core.NewEvent(core.KindProjectCreated, core.ProjectAggregate(slug), s.clock(), core.ProjectCreated{
		Slug: slug,
		Name: name,
	})
	if err != nil {
		return core.Project{}, fmt.Errorf("project: create %q: %w", slug, err)
	}
	if err := s.commit(ctx, ev, slug); err != nil {
		return core.Project{}, fmt.Errorf("project: create %q: %w", slug, err)
	}

	return core.Project{Slug: slug, Name: name}, nil
}

// Archive hides a project without deleting its history.
//
// Past entries keep referencing the slug and must keep resolving, so archiving
// is an event like any other rather than a removal. An unknown slug wraps
// core.ErrNotFound; a project already archived wraps core.ErrConflict, because
// a second archival would record a state change that did not happen.
func (s *Service) Archive(ctx context.Context, slug string) error {
	slug, err := normalizeSlug(slug)
	if err != nil {
		return err
	}

	tl, err := s.timeline(ctx)
	if err != nil {
		return fmt.Errorf("project: archive %q: %w", slug, err)
	}
	p, ok := tl.Projects[slug]
	if !ok {
		return fmt.Errorf("project: archive %q: %w", slug, core.ErrNotFound)
	}
	if p.Archived {
		return fmt.Errorf("project: archive %q: already archived: %w", slug, core.ErrConflict)
	}

	ev, err := core.NewEvent(core.KindProjectArchived, core.ProjectAggregate(slug), s.clock(), core.ProjectArchived{
		Slug: slug,
	})
	if err != nil {
		return fmt.Errorf("project: archive %q: %w", slug, err)
	}
	if err := s.commit(ctx, ev, slug); err != nil {
		return fmt.Errorf("project: archive %q: %w", slug, err)
	}

	return nil
}

// List returns projects as of the end of the log, sorted by slug.
//
// Archived projects are omitted unless includeArchived is set. The sort is by
// slug rather than by name so that the order is stable under a rename.
func (s *Service) List(ctx context.Context, includeArchived bool) ([]core.Project, error) {
	tl, err := s.timeline(ctx)
	if err != nil {
		return nil, fmt.Errorf("project: list: %w", err)
	}

	out := make([]core.Project, 0, len(tl.Projects))
	for _, p := range tl.Projects {
		if p.Archived && !includeArchived {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })

	return out, nil
}

// Get returns one project by slug.
//
// The slug is normalized the same way a command's is, so a malformed argument
// wraps core.ErrInvalid before any lookup happens; a well-formed slug the log
// never created wraps core.ErrNotFound. Archived projects are returned — a
// caller resolving an entry's project needs it to resolve after archival.
func (s *Service) Get(ctx context.Context, slug string) (core.Project, error) {
	slug, err := normalizeSlug(slug)
	if err != nil {
		return core.Project{}, err
	}

	tl, err := s.timeline(ctx)
	if err != nil {
		return core.Project{}, fmt.Errorf("project: get %q: %w", slug, err)
	}
	p, ok := tl.Projects[slug]
	if !ok {
		return core.Project{}, fmt.Errorf("project: get %q: %w", slug, core.ErrNotFound)
	}

	return p, nil
}

// commit persists ev and, only if the log accepted it, announces the change.
//
// The ordering is the central rule of this codebase. A subscriber reacts to a
// notification by re-reading the log, so publishing before a successful append
// would advertise state that does not exist and may never exist. The publish
// call is therefore unreachable from the error path by construction, not by a
// caller remembering to skip it.
func (s *Service) commit(ctx context.Context, ev core.Event, slug string) error {
	if err := s.log.Append(ctx, ev); err != nil {
		return err
	}
	// A nil Publisher is a supported configuration (a CLI run with no bus),
	// not a missing dependency.
	if s.pub != nil {
		s.pub.Publish(core.SubjectProjects, slug)
	}
	return nil
}

// timeline replays the whole log into the canonical fold. Every read in this
// package — including the duplicate check inside a command — goes through here
// so that the write model can never disagree with a read model about what
// exists.
func (s *Service) timeline(ctx context.Context) (core.Timeline, error) {
	events, err := s.log.Load(ctx, 0)
	if err != nil {
		return core.Timeline{}, fmt.Errorf("load log: %w", err)
	}
	return core.BuildTimeline(events, s.clock())
}

// normalizeSlug trims surrounding whitespace and validates what is left,
// returning the canonical form callers should store and echo back. Validation
// happens before any log access: a malformed slug is the caller's mistake and
// costs nothing to reject.
func normalizeSlug(slug string) (string, error) {
	trimmed := strings.TrimSpace(slug)
	switch {
	case trimmed == "":
		return "", fmt.Errorf("project: slug %q: must not be empty: %w", slug, core.ErrInvalid)
	case len(trimmed) > maxSlugLen:
		return "", fmt.Errorf("project: slug %q: must be at most %d characters: %w", trimmed, maxSlugLen, core.ErrInvalid)
	case !slugPattern.MatchString(trimmed):
		return "", fmt.Errorf("project: slug %q: must match %s: %w", trimmed, slugPattern, core.ErrInvalid)
	}
	return trimmed, nil
}
