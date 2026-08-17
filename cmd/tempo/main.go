// Command tempo is an event-sourced time tracker for the terminal.
//
// Nothing here stores current state. Every command appends an immutable event
// to a log, and every view is a fresh fold over that log — which is why
// `tempo today` and `tempo report` can disagree about nothing: they are two
// projections of one source of truth.
//
// This file is the wiring, and deliberately holds no domain logic. Each
// internal package owns exactly one concern; main's job is to construct them,
// hand them their ports, and map errors onto exit codes.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/mattn/go-isatty"
	"github.com/urfave/cli/v3"

	"github.com/atvirokodosprendimai/random-skill-tests/internal/bus"
	"github.com/atvirokodosprendimai/random-skill-tests/internal/core"
	"github.com/atvirokodosprendimai/random-skill-tests/internal/eventstore"
	"github.com/atvirokodosprendimai/random-skill-tests/internal/invoice"
	"github.com/atvirokodosprendimai/random-skill-tests/internal/project"
	"github.com/atvirokodosprendimai/random-skill-tests/internal/rate"
	"github.com/atvirokodosprendimai/random-skill-tests/internal/render"
	"github.com/atvirokodosprendimai/random-skill-tests/internal/report"
	"github.com/atvirokodosprendimai/random-skill-tests/internal/summary"
	"github.com/atvirokodosprendimai/random-skill-tests/internal/timer"
	"github.com/atvirokodosprendimai/random-skill-tests/internal/worklog"
)

// Exit codes. Distinguishing them lets a shell script tell "you asked for
// something impossible" from "that does not exist" without parsing stderr.
const (
	exitOK       = 0
	exitError    = 1
	exitInvalid  = 2
	exitNotFound = 3
	exitConflict = 4
)

func main() {
	// Ctrl-C cancels the context rather than killing the process outright, so
	// an in-flight Append gets to finish its transaction instead of leaving a
	// hot journal behind.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := newRoot().Run(ctx, os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "tempo:", err)
		os.Exit(exitCode(err))
	}
	os.Exit(exitOK)
}

// exitCode maps a kernel sentinel onto a process exit code. Errors are wrapped
// all the way up from the packages that produced them, so errors.Is works here
// without main knowing which package failed.
func exitCode(err error) int {
	switch {
	case errors.Is(err, core.ErrInvalid):
		return exitInvalid
	case errors.Is(err, core.ErrNotFound):
		return exitNotFound
	case errors.Is(err, core.ErrConflict):
		return exitConflict
	default:
		return exitError
	}
}

// app holds the constructed object graph for one CLI invocation.
//
// The split visible in these fields is the whole architecture in miniature:
// the write models take a core.Log and a core.Publisher, the read models take
// only a core.Loader. A read model literally cannot write, because it was
// never handed anything that can.
type app struct {
	store *eventstore.Store
	bus   *bus.Bus

	// Write side — one service per aggregate, each the sole writer of its own.
	projects *project.Service
	timers   *timer.Service
	entries  *worklog.Service
	rates    *rate.Service

	// Read side — projections, all of them pure functions of the log.
	days     *summary.Reader
	reports  *report.Reader
	invoices *invoice.Reader

	out *render.Renderer
}

// open constructs the graph from the root flags.
func open(ctx context.Context, cmd *cli.Command) (*app, error) {
	path, err := dbPath(cmd.String("db"))
	if err != nil {
		return nil, err
	}
	store, err := eventstore.Open(ctx, path)
	if err != nil {
		return nil, err
	}

	// The bus makes every write announce itself. In a one-shot CLI process
	// nobody is subscribed, so this is a no-op today — it is wired anyway
	// because it is the seam a daemon or a NATS bridge would attach to, and
	// retrofitting the publish calls later means touching every write path.
	b := bus.New()
	clock := core.SystemClock

	return &app{
		store:    store,
		bus:      b,
		projects: project.NewService(store, b, clock),
		timers:   timer.NewService(store, b, clock),
		entries:  worklog.NewService(store, b, clock),
		rates:    rate.NewService(store, b, clock),
		days:     summary.NewReader(store, clock),
		reports:  report.NewReader(store, clock),
		invoices: invoice.NewReader(store, clock),
		out: render.New(os.Stdout, render.Options{
			Color: useColor(cmd),
			Width: cmd.Int("width"),
		}),
	}, nil
}

// close releases the store. Errors are returned rather than logged so that a
// failed WAL checkpoint does not silently look like success.
func (a *app) close() error {
	if a == nil {
		return nil
	}
	a.bus.Close()
	return a.store.Close()
}

// useColor decides whether to emit ANSI escapes. Explicit --no-color wins,
// then the NO_COLOR convention, then whether stdout is actually a terminal —
// piping to a file must never embed escape sequences.
func useColor(cmd *cli.Command) bool {
	if cmd.Bool("no-color") {
		return false
	}
	if _, set := os.LookupEnv("NO_COLOR"); set {
		return false
	}
	return isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd())
}

// dbPath resolves the log location, honouring an explicit --db, then
// XDG_DATA_HOME, then ~/.local/share.
func dbPath(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return filepath.Join(dir, "tempo", "tempo.db"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "tempo", "tempo.db"), nil
}

// withApp adapts a handler that needs the object graph into a cli.ActionFunc,
// constructing the graph and tearing it down around the call.
//
// It exists so that no command body repeats open/close bookkeeping, and so
// that a store left open by an early return is impossible.
func withApp(fn func(context.Context, *cli.Command, *app) error) cli.ActionFunc {
	return func(ctx context.Context, cmd *cli.Command) (err error) {
		a, err := open(ctx, cmd)
		if err != nil {
			return err
		}
		defer func() {
			// A close error must not mask the real failure, but it must not
			// vanish either when the command itself succeeded.
			if cerr := a.close(); cerr != nil && err == nil {
				err = cerr
			}
		}()
		return fn(ctx, cmd, a)
	}
}
