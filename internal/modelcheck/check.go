package modelcheck

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/discovery"
)

// Verdict statuses.
const (
	StatusOK         = "ok"
	StatusMissing    = "missing"
	StatusUnchecked  = "unchecked"
	StatusIneligible = "ineligible"
)

// ErrClassPending marks an account discovery polls but has not polled yet.
// The other error classes are discovery's (discovery.ErrClass*).
const ErrClassPending = "pending"

// ErrClassTruncated marks an expected id absent from a listing that dropped
// ids (over the id cap, or invalid): the id may be among the dropped ones, so
// the verdict is unchecked rather than missing.
const ErrClassTruncated = "truncated"

// MaxSuggestions bounds Verdict.Suggestions.
const MaxSuggestions = 3

// DefaultConcurrency bounds concurrent listing fetches.
const DefaultConcurrency = 4

// Expectation is one backend id a static route expects one candidate account
// to list.
type Expectation struct {
	Route    string
	Account  string
	Provider string
	Expected string
}

// Expectations lists, in route config order, candidate first-seen order
// (interactive then background) and models order, every backend id the static
// routes expect: per candidate, upstreams[account].upstream_model, else the
// route upstream_model, else every id in models verbatim. Discovering routes
// and accounts missing from accounts are skipped.
func Expectations(accounts map[string]core.Account, routes []core.Route) []Expectation {
	var out []Expectation
	for _, rt := range routes {
		if rt.DiscoverModels {
			continue
		}
		seen := map[string]bool{}
		for _, id := range slices.Concat(rt.Interactive, rt.Background) {
			acct, ok := accounts[id]
			if seen[id] || !ok {
				continue
			}
			seen[id] = true
			want := rt.Models
			if m := rt.Upstreams[id].UpstreamModel; m != "" {
				want = []string{m}
			} else if rt.UpstreamModel != "" {
				want = []string{rt.UpstreamModel}
			}
			for _, m := range want {
				out = append(out, Expectation{Route: rt.Name, Account: id, Provider: acct.Provider, Expected: m})
			}
		}
	}
	return out
}

// Listing is one account's /models result as the check sees it: the sorted
// ids of a successful fetch and the count of ids discovery's parse dropped,
// or a non-empty ErrorClass.
type Listing struct {
	IDs        []string
	Dropped    int
	ErrorClass string
}

// Verdict is the check result for one (route, account, expected id).
type Verdict struct {
	Route    string `json:"route"`
	Account  string `json:"account"`
	Expected string `json:"expected"`
	Status   string `json:"status"`
	// ErrorClass is the fetch failure class of an unchecked verdict.
	ErrorClass string `json:"error_class,omitempty"`
	// Provider is set on an ineligible verdict.
	Provider string `json:"provider,omitempty"`
	// Suggestions are listed ids close to a missing expected id.
	Suggestions []string `json:"suggestions,omitempty"`
	// SingleModel reports a missing id on an account that lists exactly one
	// other id, named by Loaded (a swapped single-model server).
	SingleModel bool   `json:"single_model,omitempty"`
	Loaded      string `json:"loaded,omitempty"`
}

// Evaluate computes one verdict per expectation, in order. An eligible
// account without an entry in listings is unchecked with ErrClassPending; an
// expected id absent from a listing with dropped ids is unchecked with
// ErrClassTruncated, never missing.
func Evaluate(exps []Expectation, listings map[string]Listing) []Verdict {
	out := make([]Verdict, 0, len(exps))
	for _, e := range exps {
		v := Verdict{Route: e.Route, Account: e.Account, Expected: e.Expected}
		l, ok := listings[e.Account]
		switch {
		case !discovery.Eligible(e.Provider):
			v.Status, v.Provider = StatusIneligible, e.Provider
		case !ok:
			v.Status, v.ErrorClass = StatusUnchecked, ErrClassPending
		case l.ErrorClass != "":
			v.Status, v.ErrorClass = StatusUnchecked, l.ErrorClass
		case listed(e.Provider, e.Expected, l.IDs):
			v.Status = StatusOK
		case l.Dropped > 0:
			v.Status, v.ErrorClass = StatusUnchecked, ErrClassTruncated
		default:
			v.Status = StatusMissing
			v.Suggestions = Suggest(e.Expected, l.IDs)
			if len(l.IDs) == 1 {
				v.SingleModel, v.Loaded = true, l.IDs[0]
			}
		}
		out = append(out, v)
	}
	return out
}

// listed reports whether ids lists expected. For provider ollama, which
// resolves an untagged name to name:latest, a bare name (no ":" after the last
// "/") and its name:latest form are the same id.
func listed(provider, expected string, ids []string) bool {
	if slices.Contains(ids, expected) {
		return true
	}
	if provider != core.ProviderOllama {
		return false
	}
	if base, ok := strings.CutSuffix(expected, ":latest"); ok && stripSuffix(base) == base {
		return slices.Contains(ids, base)
	}
	return stripSuffix(expected) == expected && slices.Contains(ids, expected+":latest")
}

// Suggest returns at most MaxSuggestions listed ids close to expected: first
// those equal to it after stripping a ":<suffix>" from either side (sorted),
// then those whose repo path (the id without its suffix) equals expected's
// case-insensitively (sorted). expected itself is never suggested.
func Suggest(expected string, listed []string) []string {
	base := stripSuffix(expected)
	var exact, fold []string
	for _, id := range listed {
		switch b := stripSuffix(id); {
		case id == expected:
		case b == base:
			exact = append(exact, id)
		case strings.EqualFold(b, base):
			fold = append(fold, id)
		}
	}
	slices.Sort(exact)
	slices.Sort(fold)
	out := append(exact, fold...)
	if len(out) > MaxSuggestions {
		out = out[:MaxSuggestions]
	}
	return out
}

// stripSuffix removes a trailing ":<suffix>" (a llama.cpp quant or Ollama
// tag) found after the last "/".
func stripSuffix(id string) string {
	slash := strings.LastIndex(id, "/")
	if i := strings.LastIndex(id, ":"); i > slash && i > 0 {
		return id[:i]
	}
	return id
}

// AccountResult describes one account's listing in a Result. Times are UTC;
// FetchedAt is null when the account was never fetched. Source is "fetch"
// (fetched by the check) or "discovery" (read from discovery's shared
// listing).
type AccountResult struct {
	Account    string     `json:"account"`
	Source     string     `json:"source"`
	FetchedAt  *time.Time `json:"fetched_at"`
	ModelCount int        `json:"model_count"`
	ErrorClass string     `json:"error_class,omitempty"`
}

// Summary counts verdicts by status; SingleModel counts missing verdicts with
// single_model set.
type Summary struct {
	OK          int `json:"ok"`
	Missing     int `json:"missing"`
	SingleModel int `json:"single_model"`
	Unchecked   int `json:"unchecked"`
	Ineligible  int `json:"ineligible"`
}

// Result is one complete check. It never carries URLs, credentials or
// upstream bodies.
type Result struct {
	CheckedAt *time.Time      `json:"checked_at"`
	Accounts  []AccountResult `json:"accounts"`
	Verdicts  []Verdict       `json:"verdicts"`
	Summary   Summary         `json:"summary"`
}

func summarize(vs []Verdict) Summary {
	var s Summary
	for _, v := range vs {
		switch v.Status {
		case StatusOK:
			s.OK++
		case StatusMissing:
			s.Missing++
			if v.SingleModel {
				s.SingleModel++
			}
		case StatusUnchecked:
			s.Unchecked++
		case StatusIneligible:
			s.Ineligible++
		}
	}
	return s
}

// Options tune Probe and Checker. Zero values select the defaults.
type Options struct {
	// Fetch tunes the listing fetch exactly as for discovery (HTTPClient,
	// Timeout, MaxBodyBytes, MaxIDs); its defaults are discovery's.
	Fetch discovery.Options
	// Concurrency bounds concurrent fetches (default DefaultConcurrency).
	Concurrency int
	// Clock stamps results; default core.SystemClock.
	Clock core.Clock
}

func (o Options) withDefaults() Options {
	if o.Fetch.HTTPClient == nil {
		o.Fetch.HTTPClient = discovery.NewHTTPClient()
	}
	if o.Concurrency <= 0 {
		o.Concurrency = DefaultConcurrency
	}
	if o.Clock == nil {
		o.Clock = core.SystemClock{}
	}
	return o
}

// eligibleAccounts lists, sorted, the accounts with at least one eligible
// expectation.
func eligibleAccounts(exps []Expectation) []string {
	var out []string
	for _, e := range exps {
		if discovery.Eligible(e.Provider) && !slices.Contains(out, e.Account) {
			out = append(out, e.Account)
		}
	}
	slices.Sort(out)
	return out
}

// fetchAll fetches each of ids once, at most concurrency at a time.
func fetchAll(ctx context.Context, creds core.CredentialSource, accounts map[string]core.Account, ids []string, opts Options) map[string]Listing {
	out := make(map[string]Listing, len(ids))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, opts.Concurrency)
	for _, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			got, dropped, class := discovery.Fetch(ctx, creds, accounts[id], opts.Fetch)
			mu.Lock()
			out[id] = Listing{IDs: got, Dropped: dropped, ErrorClass: class}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// Probe runs one offline-wiring check: it fetches each eligible account's
// listing once (bounded concurrency) and evaluates every static route. A
// default HTTP client it builds has its idle connections closed on return.
func Probe(ctx context.Context, creds core.CredentialSource, accounts map[string]core.Account, routes []core.Route, opts Options) Result {
	if opts.Fetch.HTTPClient == nil {
		opts.Fetch.HTTPClient = discovery.NewHTTPClient()
		defer opts.Fetch.HTTPClient.CloseIdleConnections()
	}
	opts = opts.withDefaults()
	exps := Expectations(accounts, routes)
	ids := eligibleAccounts(exps)
	listings := fetchAll(ctx, creds, accounts, ids, opts)
	now := opts.Clock.Now().UTC()
	res := Result{CheckedAt: &now, Accounts: []AccountResult{}}
	for _, id := range ids {
		l := listings[id]
		res.Accounts = append(res.Accounts, AccountResult{Account: id, Source: "fetch", FetchedAt: &now, ModelCount: len(l.IDs), ErrorClass: l.ErrorClass})
	}
	res.Verdicts = Evaluate(exps, listings)
	res.Summary = summarize(res.Verdicts)
	return res
}
