// Package eventstore is tempo's durable event log, backed by SQLite.
//
// It is the only package in the system that writes persistent state, and it
// implements exactly one contract: core.Log. Everything else — timers, reports,
// invoices — is a fold over what this package hands back, so the guarantees
// here are the guarantees the whole system rests on:
//
//   - Append is atomic. A command that emits three events either contributes
//     all three to the log or none of them, never a prefix.
//   - Seq is assigned here and only here, by SQLite's AUTOINCREMENT. Callers
//     cannot supply one, because only the single writer knows what comes next.
//   - The log is append-only. There is no Update and no Delete, by design: a
//     correction is a new event, which is what lets a read model rebuild any
//     past state by replaying a prefix.
//
// The driver is modernc.org/sqlite, a pure-Go translation of SQLite, so tempo
// builds and cross-compiles without cgo.
package eventstore

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pressly/goose/v3"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/atvirokodosprendimai/random-skill-tests/internal/core"
)

// migrationsFS carries the schema into the binary so that a freshly installed
// tempo can create its log without shipping loose .sql files alongside it.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

const (
	// driverName is the name modernc.org/sqlite registers itself under.
	driverName = "sqlite"

	// migrationsDir is the path of the embedded migrations inside migrationsFS.
	migrationsDir = "migrations"

	// memoryPath is SQLite's private, non-persistent database. Open accepts it
	// verbatim for tests: an in-memory database lives exactly as long as the
	// connection holding it, which is why Store pins the pool to one connection.
	memoryPath = ":memory:"

	// dsnPragmas configures every connection at open time.
	//
	// journal_mode(WAL) lets readers proceed while the writer holds the log;
	// busy_timeout(5000) makes a contended lock wait rather than fail instantly
	// when a second tempo process is mid-append; foreign_keys(1) is on because
	// SQLite defaults it off and a schema that grows references later should not
	// silently lose their enforcement.
	dsnPragmas = "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
)

// insertEvent writes one event. seq is absent on purpose: it is AUTOINCREMENT's
// to assign, and letting a caller pick one would let two writers collide.
const insertEvent = `INSERT INTO events (id, kind, aggregate, at, payload) VALUES (?, ?, ?, ?, ?)`

// selectEvents reads the log forward from an exclusive lower bound.
const selectEvents = `SELECT seq, id, kind, aggregate, at, payload FROM events WHERE seq > ? ORDER BY seq ASC`

// Store is the single writer to the event log.
type Store struct {
	db *sql.DB
}

// Compile-time proof that Store satisfies the kernel's port. If the interface
// in internal/core changes, this line fails the build here rather than at some
// distant call site that happened to be the first to notice.
var _ core.Log = (*Store)(nil)

// gooseMu serialises goose's process-global configuration.
//
// SetBaseFS, SetDialect and SetLogger all write package-level variables that Up
// then reads, so two concurrent Open calls would race on them (and could apply
// migrations with another store's base FS). The lock covers configure-and-run as
// a single unit, which is the only way to make that global state safe.
var gooseMu sync.Mutex

// Open opens (creating if absent) the SQLite event log at path, applies
// migrations, and returns a ready Store.
//
// Parent directories of path are created if missing, so a first run against
// ~/.local/share/tempo/log.db needs no setup step. Pass ":memory:" for a
// throwaway in-memory log.
//
// The returned Store owns a database handle; call Close when done.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("eventstore: empty database path: %w", core.ErrInvalid)
	}

	// ":memory:" is not a filesystem path — creating a directory for it would
	// litter the working directory with a ":memory:" folder.
	if path != memoryPath {
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("eventstore: create directory %q: %w", dir, err)
			}
		}
	}

	db, err := sql.Open(driverName, path+dsnPragmas)
	if err != nil {
		return nil, fmt.Errorf("eventstore: open %q: %w", path, err)
	}

	// One connection, deliberately. SQLite in WAL mode admits exactly one
	// writer at a time; with a larger pool a second connection's write would
	// spin on the lock until busy_timeout expired and then fail with SQLITE_BUSY.
	// Capping the pool at one turns that lock contention into in-process
	// queueing inside database/sql, which waits on the caller's context instead
	// of erroring. It is also what keeps a MemoryPath log coherent: every
	// connection to memoryPath would otherwise be its own empty database.
	db.SetMaxOpenConns(1)

	// sql.Open is lazy; ping so a bad path or an unwritable directory fails here
	// rather than on the first append.
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("eventstore: connect to %q: %w", path, err)
	}

	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}

	return &Store{db: db}, nil
}

// migrate brings the schema up to date from the embedded migrations.
func migrate(db *sql.DB) error {
	gooseMu.Lock()
	defer gooseMu.Unlock()

	// tempo is a CLI whose stdout is a data channel (piped into jq, redirected
	// to a file). goose's default logger writes "OK 00001_events.sql" there,
	// which would corrupt that output on any run that happens to migrate.
	goose.SetLogger(goose.NopLogger())
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("eventstore: set goose dialect: %w", err)
	}
	if err := goose.Up(db, migrationsDir); err != nil {
		return fmt.Errorf("eventstore: apply migrations: %w", err)
	}
	return nil
}

// Close releases the database handle. A Store is unusable afterwards.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("eventstore: close: %w", err)
	}
	return nil
}

// Append persists events atomically in order, assigning each a Seq.
//
// Either every event lands or none does: the whole batch runs in one
// transaction, so a command that emits two events is never observable
// half-applied. Appending zero events is a no-op.
//
// Seq is assigned by the store, not the caller. The events passed in are left
// untouched — Append never writes back into the caller's values, because an
// Event is immutable by the kernel's contract.
//
// A duplicate event ID is reported as core.ErrConflict; a structurally
// incomplete event as core.ErrInvalid. In both cases the log is unchanged.
func (s *Store) Append(ctx context.Context, events ...core.Event) error {
	if len(events) == 0 {
		return nil
	}

	// Validate before opening a transaction: a batch that cannot possibly land
	// should not take the write lock at all.
	for i, e := range events {
		if err := validate(e); err != nil {
			return fmt.Errorf("eventstore: append event %d of %d: %w", i+1, len(events), err)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("eventstore: begin transaction: %w", err)
	}
	// Rollback after a successful Commit returns sql.ErrTxDone. The append has
	// already landed by then, so that error is meaningless and dropped; on every
	// failure path this is what guarantees the log is left untouched.
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, insertEvent)
	if err != nil {
		return fmt.Errorf("eventstore: prepare insert: %w", err)
	}
	defer stmt.Close()

	for _, e := range events {
		// e is a copy and stays one: nothing here assigns back into events.
		//
		// At is stored as RFC3339Nano UTC text so the log is readable with the
		// sqlite3 shell alone. RFC3339Nano trims trailing zeros from the
		// fraction, so the text is not perfectly lexically ordered — that is
		// harmless, because seq, not at, is the ordering authority.
		if _, err := stmt.ExecContext(ctx, e.ID, string(e.Kind), e.Aggregate, e.At.UTC().Format(time.RFC3339Nano), []byte(e.Payload)); err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("eventstore: event %q already in log: %w: %w", e.ID, core.ErrConflict, err)
			}
			return fmt.Errorf("eventstore: insert event %q: %w", e.ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("eventstore: commit %d events: %w", len(events), err)
	}
	return nil
}

// Load returns every event with Seq > sinceSeq, in Seq order. Pass 0 to replay
// the whole log.
//
// The returned slice is never nil, so a caller can fold it unconditionally: an
// empty result means "nothing new", and a failure is always an error instead.
func (s *Store) Load(ctx context.Context, sinceSeq int64) ([]core.Event, error) {
	rows, err := s.db.QueryContext(ctx, selectEvents, sinceSeq)
	if err != nil {
		return nil, fmt.Errorf("eventstore: query events since seq %d: %w", sinceSeq, err)
	}
	defer rows.Close()

	events := make([]core.Event, 0)
	for rows.Next() {
		var (
			e       core.Event
			kind    string
			at      string
			payload []byte
		)
		if err := rows.Scan(&e.Seq, &e.ID, &kind, &e.Aggregate, &at, &payload); err != nil {
			return nil, fmt.Errorf("eventstore: scan event: %w", err)
		}
		ts, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			// A timestamp this package cannot parse means the row was not
			// written by this package. Refusing beats folding a zero time into
			// somebody's timesheet.
			return nil, fmt.Errorf("eventstore: parse timestamp %q at seq %d: %w", at, e.Seq, core.ErrInvalid)
		}
		e.Kind = core.Kind(kind)
		e.At = ts.UTC()
		e.Payload = payload
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("eventstore: iterate events since seq %d: %w", sinceSeq, err)
	}
	return events, nil
}

// validate rejects an event the schema would refuse or, worse, silently accept.
// NOT NULL does not catch an empty string, and an event with no ID or no kind is
// unreplayable — better to name the problem than to store it.
func validate(e core.Event) error {
	switch {
	case e.ID == "":
		return fmt.Errorf("empty event id: %w", core.ErrInvalid)
	case e.Kind == "":
		return fmt.Errorf("event %q has empty kind: %w", e.ID, core.ErrInvalid)
	case e.Aggregate == "":
		return fmt.Errorf("event %q has empty aggregate: %w", e.ID, core.ErrInvalid)
	case len(e.Payload) == 0:
		return fmt.Errorf("event %q has empty payload: %w", e.ID, core.ErrInvalid)
	}
	return nil
}

// isUniqueViolation reports whether err is SQLite refusing a duplicate key.
//
// Event IDs are UNIQUE in the schema, so this is the log rejecting an event it
// already holds — a conflict with existing state rather than a malformed call,
// which is why Append maps it to core.ErrConflict.
func isUniqueViolation(err error) bool {
	var serr *sqlite.Error
	if !errors.As(err, &serr) {
		return false
	}
	// The driver enables extended result codes, so Code returns the specific
	// constraint that failed rather than a bare SQLITE_CONSTRAINT.
	switch serr.Code() {
	case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
		return true
	}
	return false
}
