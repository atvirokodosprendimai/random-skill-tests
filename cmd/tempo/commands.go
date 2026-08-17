package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/atvirokodosprendimai/random-skill-tests/internal/core"
	"github.com/atvirokodosprendimai/random-skill-tests/internal/rate"
	"github.com/atvirokodosprendimai/random-skill-tests/internal/worklog"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

// newRoot builds the command tree.
//
// The tree mirrors the architecture on purpose: commands that change something
// (start, stop, add, edit, rm, projects add, rate set) go through a write
// model, and commands that show something (today, week, month, report,
// invoice, status) go through a read model. No command does both.
func newRoot() *cli.Command {
	return &cli.Command{
		Name:                   "tempo",
		Usage:                  "event-sourced time tracking for the terminal",
		Version:                version,
		UseShortOptionHandling: true,
		Suggest:                true,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "db",
				Usage:   "path to the event log (default: $XDG_DATA_HOME/tempo/tempo.db)",
				Sources: cli.EnvVars("TEMPO_DB"),
			},
			&cli.BoolFlag{Name: "no-color", Usage: "disable ANSI colour"},
			&cli.IntFlag{Name: "width", Usage: "output width in columns (0 = 80)"},
		},
		Commands: []*cli.Command{
			startCmd(), stopCmd(), cancelCmd(), statusCmd(),
			addCmd(), editCmd(), rmCmd(), listCmd(),
			todayCmd(), weekCmd(), monthCmd(), reportCmd(),
			projectsCmd(), rateCmd(), invoiceCmd(),
		},
	}
}

// --- write side ------------------------------------------------------------

func startCmd() *cli.Command {
	return &cli.Command{
		Name:      "start",
		Usage:     "start the timer on a project",
		ArgsUsage: "<project>",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "note", Aliases: []string{"m"}, Usage: "what you are working on"},
			&cli.StringSliceFlag{Name: "tag", Aliases: []string{"t"}, Usage: "tag (repeatable)"},
		},
		Action: withApp(func(ctx context.Context, cmd *cli.Command, a *app) error {
			slug := cmd.Args().First()
			if slug == "" {
				return fmt.Errorf("start: a project is required: %w", core.ErrInvalid)
			}
			e, err := a.timers.Start(ctx, slug, cmd.String("note"), cmd.StringSlice("tag"))
			if err != nil {
				return err
			}
			return a.out.Entries([]core.EntryView{e})
		}),
	}
}

func stopCmd() *cli.Command {
	return &cli.Command{
		Name:  "stop",
		Usage: "stop the running timer",
		Action: withApp(func(ctx context.Context, cmd *cli.Command, a *app) error {
			e, err := a.timers.Stop(ctx)
			if err != nil {
				return err
			}
			return a.out.Entries([]core.EntryView{e})
		}),
	}
}

func cancelCmd() *cli.Command {
	return &cli.Command{
		Name:  "cancel",
		Usage: "discard the running timer without recording time",
		Action: withApp(func(ctx context.Context, cmd *cli.Command, a *app) error {
			return a.timers.Cancel(ctx)
		}),
	}
}

func statusCmd() *cli.Command {
	return &cli.Command{
		Name:  "status",
		Usage: "show the running timer, if any",
		Action: withApp(func(ctx context.Context, cmd *cli.Command, a *app) error {
			e, ok, err := a.timers.Running(ctx)
			if err != nil {
				return err
			}
			if !ok {
				// An empty list is the renderer's cue to draw its designed
				// empty state, which names the command that fixes it.
				return a.out.Entries(nil)
			}
			return a.out.Entries([]core.EntryView{e})
		}),
	}
}

func addCmd() *cli.Command {
	return &cli.Command{
		Name:      "add",
		Usage:     "log time after the fact",
		ArgsUsage: "<project>",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "from", Usage: "start time, e.g. 09:00 or 2026-08-17 09:00", Required: true},
			&cli.StringFlag{Name: "to", Usage: "end time (mutually exclusive with --for)"},
			&cli.StringFlag{Name: "for", Usage: "duration, e.g. 90m (mutually exclusive with --to)"},
			&cli.StringFlag{Name: "note", Aliases: []string{"m"}, Usage: "what you worked on"},
			&cli.StringSliceFlag{Name: "tag", Aliases: []string{"t"}, Usage: "tag (repeatable)"},
		},
		Action: withApp(func(ctx context.Context, cmd *cli.Command, a *app) error {
			slug := cmd.Args().First()
			if slug == "" {
				return fmt.Errorf("add: a project is required: %w", core.ErrInvalid)
			}
			now := time.Now()
			start, err := parseWhen(cmd.String("from"), now)
			if err != nil {
				return fmt.Errorf("add: --from: %w", err)
			}
			end, err := resolveEnd(cmd.String("to"), cmd.String("for"), start, now)
			if err != nil {
				return fmt.Errorf("add: %w", err)
			}
			e, err := a.entries.Add(ctx, slug, cmd.String("note"), cmd.StringSlice("tag"), start, end)
			if err != nil {
				return err
			}
			return a.out.Entries([]core.EntryView{e})
		}),
	}
}

func editCmd() *cli.Command {
	return &cli.Command{
		Name:      "edit",
		Usage:     "amend an existing entry",
		ArgsUsage: "<entry-id>",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "project", Usage: "move the entry to another project"},
			&cli.StringFlag{Name: "note", Aliases: []string{"m"}, Usage: "replace the note (empty clears it)"},
			&cli.StringSliceFlag{Name: "tag", Aliases: []string{"t"}, Usage: "replace all tags"},
			&cli.StringFlag{Name: "from", Usage: "move the start time"},
			&cli.StringFlag{Name: "to", Usage: "move the end time"},
		},
		Action: withApp(func(ctx context.Context, cmd *cli.Command, a *app) error {
			id := cmd.Args().First()
			if id == "" {
				return fmt.Errorf("edit: an entry id is required: %w", core.ErrInvalid)
			}
			// Only flags the user actually typed become patch fields. That is
			// what keeps "--note ''" (clear it) distinct from omitting --note
			// (leave it), all the way down into the stored event.
			var p worklog.Patch
			now := time.Now()
			if cmd.IsSet("project") {
				v := cmd.String("project")
				p.Project = &v
			}
			if cmd.IsSet("note") {
				v := cmd.String("note")
				p.Note = &v
			}
			if cmd.IsSet("tag") {
				v := cmd.StringSlice("tag")
				p.Tags = &v
			}
			if cmd.IsSet("from") {
				v, err := parseWhen(cmd.String("from"), now)
				if err != nil {
					return fmt.Errorf("edit: --from: %w", err)
				}
				p.Start = &v
			}
			if cmd.IsSet("to") {
				v, err := parseWhen(cmd.String("to"), now)
				if err != nil {
					return fmt.Errorf("edit: --to: %w", err)
				}
				p.End = &v
			}
			e, err := a.entries.Edit(ctx, id, p)
			if err != nil {
				return err
			}
			return a.out.Entries([]core.EntryView{e})
		}),
	}
}

func rmCmd() *cli.Command {
	return &cli.Command{
		Name:      "rm",
		Usage:     "delete an entry",
		ArgsUsage: "<entry-id>",
		Action: withApp(func(ctx context.Context, cmd *cli.Command, a *app) error {
			id := cmd.Args().First()
			if id == "" {
				return fmt.Errorf("rm: an entry id is required: %w", core.ErrInvalid)
			}
			return a.entries.Delete(ctx, id)
		}),
	}
}

// --- read side -------------------------------------------------------------

func listCmd() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "list entries in a range",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "from", Usage: "start of range (default: 7 days ago)"},
			&cli.StringFlag{Name: "to", Usage: "end of range, exclusive (default: tomorrow)"},
		},
		Action: withApp(func(ctx context.Context, cmd *cli.Command, a *app) error {
			now := time.Now()
			from, to, err := parseRange(cmd.String("from"), cmd.String("to"), now, -7)
			if err != nil {
				return err
			}
			es, err := a.entries.List(ctx, from, to)
			if err != nil {
				return err
			}
			return a.out.Entries(es)
		}),
	}
}

func todayCmd() *cli.Command {
	return &cli.Command{
		Name:      "today",
		Aliases:   []string{"day"},
		Usage:     "show one day",
		ArgsUsage: "[date]",
		Action: withApp(func(ctx context.Context, cmd *cli.Command, a *app) error {
			day := time.Now()
			if arg := cmd.Args().First(); arg != "" {
				var err error
				if day, err = parseWhen(arg, day); err != nil {
					return fmt.Errorf("today: %w", err)
				}
			}
			v, err := a.days.Day(ctx, day)
			if err != nil {
				return err
			}
			return a.out.Day(v)
		}),
	}
}

func weekCmd() *cli.Command {
	return &cli.Command{
		Name:      "week",
		Usage:     "show the week containing a day",
		ArgsUsage: "[date]",
		Action: rollup(func(ctx context.Context, a *app, day time.Time) (core.RangeReport, error) {
			return a.reports.Week(ctx, day)
		}),
	}
}

func monthCmd() *cli.Command {
	return &cli.Command{
		Name:      "month",
		Usage:     "show the month containing a day",
		ArgsUsage: "[date]",
		Action: rollup(func(ctx context.Context, a *app, day time.Time) (core.RangeReport, error) {
			return a.reports.Month(ctx, day)
		}),
	}
}

// rollup adapts the week/month readers, which differ only in which window they
// pick. Writing the argument parsing twice would be two chances to drift.
func rollup(fn func(context.Context, *app, time.Time) (core.RangeReport, error)) cli.ActionFunc {
	return withApp(func(ctx context.Context, cmd *cli.Command, a *app) error {
		day := time.Now()
		if arg := cmd.Args().First(); arg != "" {
			var err error
			if day, err = parseWhen(arg, day); err != nil {
				return err
			}
		}
		v, err := fn(ctx, a, day)
		if err != nil {
			return err
		}
		return a.out.Report(v)
	})
}

func reportCmd() *cli.Command {
	return &cli.Command{
		Name:  "report",
		Usage: "roll up an arbitrary range",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "from", Usage: "start of range (default: 30 days ago)"},
			&cli.StringFlag{Name: "to", Usage: "end of range, exclusive (default: tomorrow)"},
		},
		Action: withApp(func(ctx context.Context, cmd *cli.Command, a *app) error {
			now := time.Now()
			from, to, err := parseRange(cmd.String("from"), cmd.String("to"), now, -30)
			if err != nil {
				return err
			}
			v, err := a.reports.Range(ctx, from, to)
			if err != nil {
				return err
			}
			return a.out.Report(v)
		}),
	}
}

func invoiceCmd() *cli.Command {
	return &cli.Command{
		Name:      "invoice",
		Usage:     "bill a project over a range",
		ArgsUsage: "<project>",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "from", Usage: "start of range (default: 30 days ago)"},
			&cli.StringFlag{Name: "to", Usage: "end of range, exclusive (default: tomorrow)"},
		},
		Action: withApp(func(ctx context.Context, cmd *cli.Command, a *app) error {
			slug := cmd.Args().First()
			if slug == "" {
				return fmt.Errorf("invoice: a project is required: %w", core.ErrInvalid)
			}
			now := time.Now()
			from, to, err := parseRange(cmd.String("from"), cmd.String("to"), now, -30)
			if err != nil {
				return err
			}
			v, err := a.invoices.For(ctx, slug, from, to)
			if err != nil {
				return err
			}
			return a.out.Invoice(v)
		}),
	}
}

// --- projects and rates ----------------------------------------------------

func projectsCmd() *cli.Command {
	return &cli.Command{
		Name:  "projects",
		Usage: "manage projects",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "all", Aliases: []string{"a"}, Usage: "include archived projects"},
		},
		Action: withApp(func(ctx context.Context, cmd *cli.Command, a *app) error {
			ps, err := a.projects.List(ctx, cmd.Bool("all"))
			if err != nil {
				return err
			}
			return a.out.Projects(ps)
		}),
		Commands: []*cli.Command{
			{
				Name:      "add",
				Usage:     "create a project",
				ArgsUsage: "<slug> [name]",
				Action: withApp(func(ctx context.Context, cmd *cli.Command, a *app) error {
					slug := cmd.Args().First()
					name := strings.Join(cmd.Args().Tail(), " ")
					p, err := a.projects.Create(ctx, slug, name)
					if err != nil {
						return err
					}
					return a.out.Projects([]core.Project{p})
				}),
			},
			{
				Name:      "archive",
				Usage:     "hide a project without deleting its history",
				ArgsUsage: "<slug>",
				Action: withApp(func(ctx context.Context, cmd *cli.Command, a *app) error {
					return a.projects.Archive(ctx, cmd.Args().First())
				}),
			},
		},
	}
}

func rateCmd() *cli.Command {
	return &cli.Command{
		Name:  "rate",
		Usage: "manage billing rates",
		Action: withApp(func(ctx context.Context, cmd *cli.Command, a *app) error {
			rs, err := a.rates.List(ctx)
			if err != nil {
				return err
			}
			return printRates(a, rs)
		}),
		Commands: []*cli.Command{
			{
				Name:      "set",
				Usage:     "set a project's hourly rate",
				ArgsUsage: "<project> <amount>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "currency", Aliases: []string{"c"}, Value: "EUR"},
				},
				Action: withApp(func(ctx context.Context, cmd *cli.Command, a *app) error {
					slug := cmd.Args().First()
					cents, err := parseAmount(cmd.Args().Get(1))
					if err != nil {
						return fmt.Errorf("rate set: %w", err)
					}
					r, err := a.rates.Set(ctx, slug, cents, cmd.String("currency"))
					if err != nil {
						return err
					}
					return printRates(a, []rate.Rate{r})
				}),
			},
		},
	}
}

// parseAmount reads a major-unit amount ("85", "85.50") as integer cents.
//
// It parses the string by hand rather than going through float64: 85.10 is not
// representable in binary floating point, and an invoice that is one cent out
// is a support ticket.
func parseAmount(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("an amount is required: %w", core.ErrInvalid)
	}
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")

	whole, frac, hasFrac := strings.Cut(s, ".")
	units, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not an amount: %w", s, core.ErrInvalid)
	}
	cents := units * 100
	if hasFrac {
		if len(frac) > 2 {
			return 0, fmt.Errorf("%q has more than two decimal places: %w", s, core.ErrInvalid)
		}
		// Pad so "5" reads as 50 cents, not 5.
		for len(frac) < 2 {
			frac += "0"
		}
		sub, err := strconv.ParseInt(frac, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%q is not an amount: %w", s, core.ErrInvalid)
		}
		cents += sub
	}
	if neg {
		cents = -cents
	}
	return cents, nil
}
