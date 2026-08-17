-- +goose Up
-- The event log. Rows are append-only: nothing in tempo ever updates or
-- deletes one, which is why the table carries no updated_at and no soft-delete
-- flag. A correction is a later event, never a rewrite of an earlier row.
CREATE TABLE events (
    -- seq is the ordering authority for the whole system. AUTOINCREMENT (not
    -- plain INTEGER PRIMARY KEY) guarantees a value is never reused after a
    -- delete, so a reader that remembers "I have seen up to seq N" can never be
    -- shown a different event at a seq it already consumed.
    seq       INTEGER PRIMARY KEY AUTOINCREMENT,
    -- id is UNIQUE so that a retried command cannot land twice.
    id        TEXT NOT NULL UNIQUE,
    kind      TEXT NOT NULL,
    aggregate TEXT NOT NULL,
    -- at is RFC3339Nano UTC text: the log stays readable with sqlite3 alone.
    at        TEXT NOT NULL,
    payload   BLOB NOT NULL
);

-- Read models filter the log by aggregate ("everything about project:acme") and
-- by kind ("every rate.set"). Both are full-table scans without these.
CREATE INDEX events_aggregate_idx ON events (aggregate);
CREATE INDEX events_kind_idx ON events (kind);

-- +goose Down
DROP INDEX events_kind_idx;
DROP INDEX events_aggregate_idx;
DROP TABLE events;
