package engine

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/store"
)

// A fund page is a backtest of one symbol, with the holding-performance card
// pointed at what the fund holds instead of at the allocation.
//
// That is the whole design, and it is why this file is short. The chart, the
// summary table, the calendar years and the period card are all the portfolio
// path's, unchanged and unaware — a fund is not a new kind of row, a new kind
// of quote or a new kind of maths. It is an existing run with a different set
// of names measured beside it.
//
// The one thing that is genuinely new is the honesty problem. A source can only
// say what a fund holds *now*, so a look-through measured over ten years is ten
// years of today's constituents — which is survivorship bias with a chart on
// top. Two things keep that from being a lie: the fund's own numbers come from
// the fund's own series and never from its holdings, and the holdings are
// labelled with the day they were read. Everything else here follows from
// those.

// maxConstituents caps how many holdings a fund page measures.
//
// Yahoo answers with ten, so today this cap never binds. It is here because the
// cost of the card is one history fetch per holding, and a source that started
// answering with a fund's whole book would otherwise turn one page load into
// five hundred upstream requests.
const maxConstituents = 20

// constituentsTTL is how long a fund's composition is reused.
//
// Far longer than a price series, because it moves far more slowly: an index
// fund reconstitutes quarterly, and the feeds that report it are themselves
// weeks behind. A day means browsing back and forth between funds costs one
// request each, and it means the crumbed endpoint — the fragile one — is asked
// as rarely as the feature allows.
const constituentsTTL = 24 * time.Hour

// Fund is a fund page.
//
// It embeds Backtest rather than copying its fields, so the payload the client
// receives is the backtest payload plus a few facts about the fund, and the
// client renders it with the components it already has.
type Fund struct {
	Backtest
	Symbol string `json:"symbol"`
	// Name is the fund's own, as the source gives it. Empty when the source
	// would not say, which is not worth failing a page over.
	Name string `json:"name"`
	// AsOf is when the composition was read, RFC3339. It is the fetch time
	// rather than an as-of date from the source, because no free source
	// publishes one — so this says what can honestly be said, which is when we
	// asked.
	AsOf string `json:"asOf"`
	// Holdings is what the fund holds, largest first, with the weight the source
	// reported. It is the whole list this page knows about; Performance says
	// what each of them did.
	Constituents []Constituent `json:"constituents"`
	// Covered is the share of the fund the listed holdings add up to. It is the
	// number that stops the table reading as the fund: 65% means a third of what
	// moved the chart is not on this page at all.
	Covered float64 `json:"covered"`
}

// Constituent is one holding of a fund, as the page lists it.
type Constituent struct {
	Symbol string  `json:"symbol"`
	Name   string  `json:"name"`
	Weight float64 `json:"weight"`
	// Priced is false when the quote source has no history for this symbol at
	// all — a holding that is in the fund and cannot be measured. It is reported
	// rather than dropped, because "the fund holds this and we can't chart it"
	// is a different statement from "the fund doesn't hold it".
	Priced bool `json:"priced"`
}

// Fund assembles the look-through page for one symbol.
//
// benchmark is optional and is passed straight through to the run, so a fund
// gets compared against whatever the reader asked for by exactly the code a
// portfolio does.
func (e *Engine) Fund(ctx context.Context, symbol, benchmark string) (Fund, error) {
	compositor, ok := e.provider.(quotes.Compositor)
	if !ok {
		return Fund{}, quotes.ErrNoConstituents
	}
	symbol = store.NormalizeSymbol(symbol)
	if symbol == "" {
		return Fund{}, fmt.Errorf("%w: a symbol is required", ErrBadSpec)
	}
	// A fund benchmarked against itself would draw the same line twice and
	// report a gap of zero on every row, which reads as a bug rather than as
	// the tautology it is.
	if store.NormalizeSymbol(benchmark) == symbol {
		benchmark = ""
	}

	// The composition first, because it is the one part that can say "this is
	// not a fund" — and doing several years of history fetches before finding
	// that out would make a typo cost ten seconds.
	composition, asOf, err := e.constituents(ctx, compositor, symbol)
	if err != nil {
		return Fund{}, err
	}

	// The fund's own run. One holding at 100%, no contributions, no rebalancing
	// — which makes every number in the summary and the calendar years the
	// fund's own total return, with nothing of its holdings in it.
	run, detail, err := e.simulateSpec(ctx, BacktestSpec{
		Holdings:  []store.Holding{{Symbol: symbol, Weight: 100}},
		Benchmark: benchmark,
	})
	if err != nil {
		return Fund{}, err
	}

	out := Fund{
		Backtest: run,
		Symbol:   symbol,
		Name:     composition.Name,
		AsOf:     asOf.UTC().Format(time.RFC3339),
	}
	// The summary's column head. simulateSpec calls every run "Portfolio",
	// which is right for one and wrong here — a fund's column is the fund, and
	// it sits next to a benchmark column already labelled with its symbol.
	out.Portfolio.Label = symbol

	// Each holding's monthly series, on the same cache the run itself used — so
	// a fund holding something already on the watchlist costs nothing for that
	// leg, and two funds over the same mega-caps cost one fetch between them.
	//
	// Deliberately *not* intersected into the run's months. commonMonths is
	// right for a portfolio, whose legs are all held at once; here it would
	// truncate a twenty-five year fund to the listing date of whatever it most
	// recently started holding.
	historian, _ := e.provider.(quotes.Historian)
	series := map[string]map[string]float64{}
	raw := map[string]map[string]float64{}
	first := map[string]string{}

	legs := make([]HoldingResult, 0, len(composition.Holdings))
	for _, c := range composition.Holdings {
		if len(out.Constituents) >= maxConstituents {
			break
		}
		listed := Constituent{Symbol: c.Symbol, Name: c.Name, Weight: c.Weight}
		// Best-effort per holding. A constituent the source cannot price is a
		// row without a number, not a failed page — funds hold cash lines,
		// foreign listings and things that delisted last week.
		if err := e.loadMonthly(ctx, historian, c.Symbol, series, raw, first); err == nil {
			listed.Priced = true
			legs = append(legs, HoldingResult{Symbol: c.Symbol, Weight: c.Weight})
		} else {
			e.log.Debug("a fund holding could not be priced",
				"fund", symbol, "holding", c.Symbol, "error", err)
		}
		out.Constituents = append(out.Constituents, listed)
		out.Covered += c.Weight
	}

	// The card the whole page is for. Same function the portfolio path calls,
	// with the fund's own run as the line every holding is measured against.
	out.Performance = holdingPerformance(detail.months, legs, series, detail.run, detail.bench)

	// A holding with no history at all is missing from every period, and
	// holdingPerformance never saw it to say so.
	unpriced := make([]string, 0)
	for _, c := range out.Constituents {
		if !c.Priced {
			unpriced = append(unpriced, c.Symbol)
		}
	}
	if len(unpriced) > 0 {
		for i := range out.Performance {
			out.Performance[i].Missing = append(out.Performance[i].Missing, unpriced...)
			sort.Strings(out.Performance[i].Missing)
		}
	}

	out.Notes = append(out.Notes, fundNote(len(out.Constituents), out.Covered))
	return out, nil
}

// fundNote is the sentence that keeps the table from reading as the fund.
//
// It is prose rather than a chip because it is the one thing on the page a
// reader has to take away with them, and because it is different on every fund:
// twenty holdings covering 65% of QQQ and ten covering 24% of a total-market
// fund are not the same claim.
func fundNote(count int, covered float64) string {
	return fmt.Sprintf(
		"These %d holdings are %.1f%% of the fund, and are what it holds today — "+
			"the returns beside them are those companies' own, not the fund's history of holding them.",
		count, covered)
}

// fundEntry is one fund's cached constituent list.
type fundEntry struct {
	value   quotes.Composition
	fetched time.Time
}

// constituents reads a fund's composition, or returns the cached one along with
// when it was read.
//
// Cached by symbol on its own TTL rather than the price series' — see
// constituentsTTL — and cached *only on success*: a crumb that expired mid-page
// must not leave a fund remembered as "not a fund" for a day.
func (e *Engine) constituents(ctx context.Context, c quotes.Compositor, symbol string) (quotes.Composition, time.Time, error) {
	e.mu.Lock()
	entry, ok := e.fundCache[symbol]
	e.mu.Unlock()
	if ok && time.Since(entry.fetched) <= constituentsTTL {
		return entry.value, entry.fetched, nil
	}

	value, err := c.Constituents(ctx, symbol)
	if err != nil {
		// Unwrapped. The sentinels the source raises — ErrNotFund, ErrNotFound —
		// are what the API turns into a 400, and re-wrapping them here would put
		// this package between the two for no reason.
		return quotes.Composition{}, time.Time{}, err
	}

	now := time.Now()
	e.mu.Lock()
	if e.fundCache == nil {
		e.fundCache = map[string]fundEntry{}
	}
	e.fundCache[symbol] = fundEntry{value: value, fetched: now}
	e.mu.Unlock()
	return value, now, nil
}
