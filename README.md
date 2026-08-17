# tempo

An event-sourced time tracker for the terminal, built as a test of a
**CQRS agent fan-out**: one main agent owning the shared kernel and the wiring,
ten writer subagents each owning exactly one `internal/` package, written
concurrently against contracts published before any of them were spawned.

```
tempo start web -m "layout pass" -t ui
tempo stop
tempo today
tempo week
tempo invoice web --from 2026-08-01 --to 2026-09-01
```

## The idea

Nothing here stores current state. Every command appends an immutable event to
a log; every view is a fresh fold over that log. `tempo today` and
`tempo report` cannot disagree, because they are two projections of one truth.

```
command ──▶ write model ──▶ append event ──▶ publish(aggregate id)
                                  │
                                  ▼
                            the event log
                                  │
query  ──▶ read model  ◀──── replay + project ──▶ render
```

Corrections are events too. `edit` writes a sparse patch, `rm` writes a
tombstone, `cancel` deletes the interval a running timer would have become —
the original events stay in the log, so any past state is still reproducible.

## Layout

| Package | Role |
|---|---|
| `internal/core` | shared kernel: event envelope, typed payloads, ports, view structs, and `BuildTimeline` — the one canonical replay |
| `internal/eventstore` | append-only SQLite log (modernc, WAL, `MaxOpenConns=1`, goose migrations) |
| `internal/bus` | in-process fan-out of aggregate ids, buffered-1 drop-oldest |
| `internal/project` | **write** — projects |
| `internal/timer` | **write** — the running timer, and its at-most-one invariant |
| `internal/worklog` | **write** — manually logged entries |
| `internal/rate` | **write** — billing rates |
| `internal/summary` | **read** — `(day) → DayView` |
| `internal/report` | **read** — `(range) → RangeReport` |
| `internal/invoice` | **read** — `(project, range) → Invoice`, enriched with money |
| `internal/render` | terminal presentation of the view structs |
| `cmd/tempo` | the wiring, and nothing else |

The write models take a `core.Log` and a `core.Publisher`. The read models take
only a `core.Loader`. A read model cannot write because it was never handed
anything that can — the separation is enforced by the type system, not by
convention.

Every package depends on `internal/core` and the standard library, and never on
a sibling. That is what made ten of them safe to write concurrently.

## Rules the code actually follows

- **Single writer per aggregate.** One path writes a project, one writes the
  timer, one writes entries, one writes rates.
- **Persist, then publish.** A publish is unreachable from an append's error
  path, so the UI can never show time that failed to save. Every write model
  has a test asserting zero publishes on a write error.
- **The payload is the id, never the state.** A subscriber re-reads the log, so
  a superseded notification is always safe to drop.
- **Money is integer cents.** Invoice line amounts round half-up per line, and
  the total is the sum of the lines — so the column a client adds up matches
  the footer.
- **Replay is written once.** `core.BuildTimeline` is the only fold; the three
  read models start from it and differ only in how they project it.

## Building

```sh
go build ./...
go test ./... -race
go run ./cmd/tempo --help
```

The log lives at `$XDG_DATA_HOME/tempo/tempo.db`, or `~/.local/share/tempo/tempo.db`,
or wherever `--db` / `$TEMPO_DB` points.

## Where this stops

The bus is in-process, so a second `tempo` process does not see the first one's
writes live. That seam is deliberate and is where NATS would attach: publish the
same aggregate id to a subject, subscribe in a long-lived process, and the read
models need no changes at all — they already re-read on every query.
