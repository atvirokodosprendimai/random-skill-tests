package core

import "context"

// Appender is the write side of the log. It is the narrow interface every write
// model accepts, so a service can be tested against an in-memory slice without
// knowing SQLite exists.
type Appender interface {
	// Append persists events atomically and in order, assigning each a Seq.
	// Either every event lands or none does: a command that emits two events
	// must never be observable half-applied.
	Append(ctx context.Context, events ...Event) error
}

// Loader is the read side of the log.
type Loader interface {
	// Load returns every event with Seq greater than sinceSeq, in Seq order.
	// Pass 0 to replay the whole log. Read models call this on every query
	// rather than caching, because the log is the truth and a stale cache is
	// the failure mode CQRS exists to avoid.
	Load(ctx context.Context, sinceSeq int64) ([]Event, error)
}

// Log is the full event log. One implementation satisfies both halves, but
// callers should accept the narrowest interface they actually use: a read model
// that accepts Log instead of Loader has quietly granted itself write access.
type Log interface {
	Appender
	Loader
}

// Publisher fans out a change notification after a successful append.
//
// The published value is the aggregate id, never the new state. Two racing
// notifications for one aggregate therefore both cause a fresh read and the
// later read wins, which is correct. Pushing rendered state would let a stale
// render land last.
type Publisher interface {
	Publish(subject, aggregateID string)
}
