// Package rate is tempo's write model for per-project billing rates.
//
// It is the single writer for core.RatesAggregate: every rate change in the
// system enters the log through Service.Set and through nothing else. The reads
// offered here (Get, List) are folds over that same log via core.BuildTimeline,
// not a cache — the log is the truth, and a rate held in memory would be the
// exact drift CQRS exists to avoid.
//
// Money is integer cents throughout. A float rate looks harmless at one hour
// and quietly produces a wrong invoice total once the hours get large enough,
// so no float ever enters, leaves, or is computed inside this package.
package rate

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/atvirokodosprendimai/random-skill-tests/internal/core"
)

// defaultCurrency is applied when a caller sets a rate without naming a
// currency. Rejecting the common case would make the command bureaucratic for
// no gain, and a stored default is reproducible where an implicit one is not.
const defaultCurrency = "EUR"

// Rate is a project's billing rate as of the end of the log.
type Rate struct {
	// Project is the project slug the rate applies to.
	Project string

	// CentsPerHour is the hourly rate in integer minor currency units. Cents,
	// never a float — see the package comment.
	CentsPerHour int64

	// Currency is an upper-cased three-letter code, e.g. "EUR".
	Currency string
}

// Service is the single writer for per-project billing rates.
type Service struct {
	log   core.Log
	pub   core.Publisher
	clock core.Clock
}

// NewService returns a Service backed by log, publishing to pub, timestamping
// with clock.
//
// pub may be nil: a caller with no bus (a one-shot CLI invocation, a test) still
// gets a fully functional writer, since publishing is a notification and not
// part of the write itself.
func NewService(log core.Log, pub core.Publisher, clock core.Clock) *Service {
	return &Service{log: log, pub: pub, clock: clock}
}

// Set records the rate that applies to project from now on.
//
// Setting a rate for a project that already has one is expected and legal: the
// new event supersedes the old one from this point forward, it does not
// conflict with it. The superseded event stays in the log on purpose — an
// invoice raised last quarter must still reproduce at last quarter's rate, and
// it only can because the old rate was never overwritten.
//
// Set does not verify that project exists. Projects are a separate aggregate
// with a separate writer, so checking here would couple this writer to that
// one's current state and make a rate command fail on a race it has no part in.
// A rate for an unknown project is a read-side reconciliation concern.
func (s *Service) Set(ctx context.Context, project string, centsPerHour int64, currency string) (Rate, error) {
	r, err := newRate(project, centsPerHour, currency)
	if err != nil {
		return Rate{}, err
	}

	ev, err := core.NewEvent(core.KindRateSet, core.RatesAggregate, s.clock(), core.RateSet{
		Project:      r.Project,
		CentsPerHour: r.CentsPerHour,
		Currency:     r.Currency,
	})
	if err != nil {
		return Rate{}, fmt.Errorf("rate: set %q: %w", r.Project, err)
	}

	// Persist, then publish — never the reverse. A notification that outran its
	// event would send every subscriber to re-read a log that does not yet
	// contain the change, and they would cache the previous rate as current
	// with nothing left to correct them. So a failed append publishes nothing
	// at all: the bus only ever announces facts already in the log.
	if err := s.log.Append(ctx, ev); err != nil {
		return Rate{}, fmt.Errorf("rate: set %q: %w", r.Project, err)
	}
	if s.pub != nil {
		s.pub.Publish(core.SubjectRates, r.Project)
	}
	return r, nil
}

// Get returns the rate currently in force for project: the most recent
// core.KindRateSet the log holds for it.
//
// A project that has never had a rate set wraps core.ErrNotFound. That is
// distinct from a rate of zero, which is a deliberate pro-bono rate and is
// returned like any other.
func (s *Service) Get(ctx context.Context, project string) (Rate, error) {
	// Trimmed to match Set, which stores the trimmed slug; otherwise " acme"
	// would write a rate that "acme" could never read back.
	project = strings.TrimSpace(project)
	if project == "" {
		return Rate{}, fmt.Errorf("rate: get %q: project is empty: %w", project, core.ErrInvalid)
	}

	tl, err := s.timeline(ctx)
	if err != nil {
		return Rate{}, fmt.Errorf("rate: get %q: %w", project, err)
	}
	cents, ok := tl.Rates[project]
	if !ok {
		return Rate{}, fmt.Errorf("rate: get %q: %w", project, core.ErrNotFound)
	}
	return Rate{Project: project, CentsPerHour: cents, Currency: tl.Currencies[project]}, nil
}

// List returns every project's current rate, sorted by project.
//
// The result is always non-nil, so a caller can range over it and report an
// empty rate table without a nil check.
func (s *Service) List(ctx context.Context) ([]Rate, error) {
	tl, err := s.timeline(ctx)
	if err != nil {
		return nil, fmt.Errorf("rate: list: %w", err)
	}

	rates := make([]Rate, 0, len(tl.Rates))
	for project, cents := range tl.Rates {
		rates = append(rates, Rate{
			Project:      project,
			CentsPerHour: cents,
			Currency:     tl.Currencies[project],
		})
	}
	// Map iteration order is random; a rate table that reshuffles between runs
	// is unreadable, so ordering is imposed here rather than left to callers.
	sort.Slice(rates, func(i, j int) bool { return rates[i].Project < rates[j].Project })
	return rates, nil
}

// timeline replays the whole log into the canonical fold.
//
// It replays from zero every call instead of keeping a snapshot: other writers
// append to the same log concurrently, so any cached position here would need
// invalidation this package cannot observe.
func (s *Service) timeline(ctx context.Context) (core.Timeline, error) {
	events, err := s.log.Load(ctx, 0)
	if err != nil {
		return core.Timeline{}, fmt.Errorf("load log: %w", err)
	}
	return core.BuildTimeline(events, s.clock())
}

// newRate validates a Set command and normalises it into the value that is both
// written to the log and handed back to the caller, so the echoed rate and the
// stored rate cannot disagree.
func newRate(project string, centsPerHour int64, currency string) (Rate, error) {
	project = strings.TrimSpace(project)
	if project == "" {
		return Rate{}, fmt.Errorf("rate: set %q: project is empty: %w", project, core.ErrInvalid)
	}
	// Zero is legal — pro-bono work still gets tracked and simply bills at
	// nothing. Negative is not: it would credit the client for hours worked.
	if centsPerHour < 0 {
		return Rate{}, fmt.Errorf("rate: set %q: cents per hour %d is negative: %w", project, centsPerHour, core.ErrInvalid)
	}
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return Rate{}, fmt.Errorf("rate: set %q: %w", project, err)
	}
	return Rate{Project: project, CentsPerHour: centsPerHour, Currency: cur}, nil
}

// normalizeCurrency trims, validates and upper-cases a currency code, defaulting
// an empty one to defaultCurrency.
//
// Codes are stored upper-cased so that "eur" and "EUR" fold to one currency in
// the log rather than reading back as two.
func normalizeCurrency(currency string) (string, error) {
	currency = strings.TrimSpace(currency)
	if currency == "" {
		return defaultCurrency, nil
	}
	// Length is measured in bytes deliberately: a three-letter code is three
	// ASCII bytes, so any multi-byte rune fails this check or the letter check
	// below instead of sneaking through as "three characters".
	if len(currency) != 3 {
		return "", fmt.Errorf("currency %q must be three ASCII letters: %w", currency, core.ErrInvalid)
	}
	for i := range len(currency) {
		if c := currency[i]; (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return "", fmt.Errorf("currency %q must be three ASCII letters: %w", currency, core.ErrInvalid)
		}
	}
	return strings.ToUpper(currency), nil
}
