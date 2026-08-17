// Package core is tempo's shared kernel: the single contract every other
// package compiles against.
//
// tempo is an event-sourced time tracker. Nothing in the system stores current
// state — the event log is the only truth, and every view is a fold over it.
// That split is why this package exists: the write models append events, the
// read models replay them, and neither imports the other.
//
// The kernel deliberately holds no behaviour beyond encoding and the canonical
// replay (see BuildTimeline). It exists so that internal/timer, internal/report,
// internal/render and the rest depend on exactly one package plus the standard
// library, never on each other.
package core

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Sentinel errors callers are expected to match with errors.Is. Every package
// wraps these rather than inventing its own vocabulary, so the CLI can map a
// failure to an exit code without knowing which package produced it.
var (
	// ErrInvalid reports an argument the caller could have gotten right:
	// an empty slug, an end before a start, a negative rate.
	ErrInvalid = errors.New("tempo: invalid argument")

	// ErrNotFound reports that an aggregate does not exist in the log.
	ErrNotFound = errors.New("tempo: not found")

	// ErrConflict reports a command refused because it would break an
	// invariant of current state — starting a second concurrent timer,
	// creating a project slug that is already taken.
	ErrConflict = errors.New("tempo: conflicting state")
)

// Clock returns the current time. Write models take one instead of calling
// time.Now so that event timestamps are injectable and tests are deterministic;
// an event's At field must always come from here.
type Clock func() time.Time

// SystemClock is the production Clock. It returns UTC because the log is
// canonical storage and must not carry the machine's local offset; formatting
// back into local time is the render layer's job.
func SystemClock() time.Time { return time.Now().UTC() }

// FixedClock returns a Clock that always reports at, for use in tests.
func FixedClock(at time.Time) Clock {
	return func() time.Time { return at }
}

// NewID returns a random 24-character hex identifier for an event or an entry.
//
// It is deliberately not sequential: ordering in this system comes from the
// store-assigned Event.Seq, so an ID only ever needs to be unique, and using
// crypto/rand avoids any illusion that IDs sort meaningfully.
func NewID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("core: generate id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// Day truncates t to midnight in t's own location. Day boundaries are a
// presentation concern that all three read models share, so the rule lives in
// the kernel rather than being re-derived (slightly differently) in each.
func Day(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}
