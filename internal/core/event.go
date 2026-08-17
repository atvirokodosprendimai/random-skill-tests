package core

import (
	"encoding/json"
	"fmt"
	"time"
)

// Kind identifies a domain event type. Kinds are strings rather than integers
// because they are persisted: a stored log must stay readable after a refactor
// that reorders the constants.
type Kind string

// The complete set of events tempo can record. Adding a Kind is an append-only
// change — existing values must never be renamed or reused, since old rows in
// the log still carry them.
const (
	KindProjectCreated  Kind = "project.created"
	KindProjectArchived Kind = "project.archived"
	KindTimerStarted    Kind = "timer.started"
	KindTimerStopped    Kind = "timer.stopped"
	KindEntryAdded      Kind = "entry.added"
	KindEntryEdited     Kind = "entry.edited"
	KindEntryDeleted    Kind = "entry.deleted"
	KindRateSet         Kind = "rate.set"
)

// Event is the envelope every write model appends and every read model folds.
//
// It is immutable by contract: once appended, no field is ever updated. A
// correction is a new event (EntryEdited, EntryDeleted), never a mutation of an
// old one, which is what lets a read model rebuild any past state by replaying
// a prefix of the log.
type Event struct {
	// Seq is the log position, assigned by the store on append and strictly
	// increasing. It is the only ordering authority in the system — At comes
	// from a Clock and may repeat or, under a clock adjustment, go backwards.
	// It carries no JSON tag because it is storage metadata, not payload.
	Seq int64 `json:"-"`

	// ID uniquely identifies this event.
	ID string `json:"id"`

	// Kind selects which payload type Payload decodes into.
	Kind Kind `json:"kind"`

	// Aggregate names the thing this event is about, so a reader can filter
	// without decoding payloads. Build it with the *Aggregate helpers.
	Aggregate string `json:"aggregate"`

	// At is when the event happened, in UTC, sourced from a Clock.
	At time.Time `json:"at"`

	// Payload is the Kind-specific body, held raw so that folding code decodes
	// only the events it actually cares about.
	Payload json.RawMessage `json:"payload"`
}

// Aggregate identifiers. Prefixing keeps namespaces from colliding once the log
// holds every domain in one table.
const (
	// TimerAggregate is a singleton: there is one running-timer slot, which is
	// precisely the invariant internal/timer enforces.
	TimerAggregate = "timer"

	// RatesAggregate is a singleton holding all per-project billing rates.
	RatesAggregate = "rates"
)

// ProjectAggregate returns the aggregate identifier for a project slug.
func ProjectAggregate(slug string) string { return "project:" + slug }

// EntryAggregate returns the aggregate identifier for a work entry.
func EntryAggregate(id string) string { return "entry:" + id }

// Subjects published on the bus after a successful append. The value on the
// wire is the aggregate id, never rendered state — a subscriber re-reads the
// log itself, so a superseded notification is always safe to drop.
const (
	SubjectProjects = "tempo.projects"
	SubjectTimer    = "tempo.timer"
	SubjectEntries  = "tempo.entries"
	SubjectRates    = "tempo.rates"
)

// ProjectCreated records a new project. Slug is the stable key used everywhere
// else; Name is display-only and may change without breaking references.
type ProjectCreated struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// ProjectArchived hides a project from pickers without deleting its history —
// past entries still reference the slug and must keep resolving.
type ProjectArchived struct {
	Slug string `json:"slug"`
}

// TimerStarted opens an interval. The entry it will become already has its ID
// here, so the matching TimerStopped needs no lookup to close it.
type TimerStarted struct {
	EntryID   string    `json:"entry_id"`
	Project   string    `json:"project"`
	Note      string    `json:"note"`
	Tags      []string  `json:"tags,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

// TimerStopped closes the interval opened by TimerStarted for EntryID.
type TimerStopped struct {
	EntryID   string    `json:"entry_id"`
	StoppedAt time.Time `json:"stopped_at"`
}

// EntryAdded records a complete interval logged after the fact, bypassing the
// timer entirely. It is a peer of TimerStarted/TimerStopped, not a special
// case: replay treats both as the same kind of entry.
type EntryAdded struct {
	EntryID string    `json:"entry_id"`
	Project string    `json:"project"`
	Note    string    `json:"note"`
	Tags    []string  `json:"tags,omitempty"`
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
}

// EntryEdited is a sparse patch: a nil field means "leave alone", which is what
// distinguishes "clear the note" (pointer to "") from "do not touch the note"
// (nil) when the two are replayed months apart.
type EntryEdited struct {
	EntryID string     `json:"entry_id"`
	Project *string    `json:"project,omitempty"`
	Note    *string    `json:"note,omitempty"`
	Tags    *[]string  `json:"tags,omitempty"`
	Start   *time.Time `json:"start,omitempty"`
	End     *time.Time `json:"end,omitempty"`
}

// EntryDeleted tombstones an entry. Replay drops it from every view, but the
// original events stay in the log — deletion is an assertion, not an erasure.
type EntryDeleted struct {
	EntryID string `json:"entry_id"`
}

// RateSet records the billing rate for a project from this point forward.
// Money is integer cents; float currency arithmetic is a bug waiting for a
// long enough invoice.
type RateSet struct {
	Project      string `json:"project"`
	CentsPerHour int64  `json:"cents_per_hour"`
	Currency     string `json:"currency"`
}

// NewEvent marshals payload and returns a ready-to-append Event.
//
// Seq is left zero: assigning it is the store's job, and a write model that
// tried to pick one would be guessing at a value only the single writer knows.
func NewEvent(kind Kind, aggregate string, at time.Time, payload any) (Event, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Event{}, fmt.Errorf("core: marshal %s payload: %w", kind, err)
	}
	id, err := NewID()
	if err != nil {
		return Event{}, err
	}
	return Event{
		ID:        id,
		Kind:      kind,
		Aggregate: aggregate,
		At:        at.UTC(),
		Payload:   raw,
	}, nil
}

// DecodePayload decodes e's payload into T. The caller is expected to have
// already switched on e.Kind; a mismatch surfaces as a decode error rather
// than silently yielding a zero value.
func DecodePayload[T any](e Event) (T, error) {
	var v T
	if err := json.Unmarshal(e.Payload, &v); err != nil {
		return v, fmt.Errorf("core: decode %s payload: %w", e.Kind, err)
	}
	return v, nil
}
