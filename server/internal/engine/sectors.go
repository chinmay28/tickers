package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/store"
)

// The sector card: where an allocation's money actually is, and how that
// compares with the funds a reader names.
//
// It is a look-through, not a lookup. A portfolio holds funds, and a fund holds
// companies, so "what is this portfolio invested in" is only answerable one
// level down: each holding's own breakdown, scaled by what it is held at, added
// up. Two 60/40s built from different index funds can differ by ten points of
// technology, and nothing else on the page would ever say so.
//
// Three things keep it honest, and each is the reason for a field below.
//
//   - A breakdown is what a source says *today*. Same problem the fund
//     look-through has, and the same answer: this card never feeds a return.
//     Nothing here is measured over time, so there is nothing for today's
//     composition to retroactively misdescribe.
//   - The slices do not add up to 100, and pretending otherwise is the easy
//     lie. A bond fund's equity sectors cover almost none of it; a stock fund's
//     usually cover 98%. Covered is that number, and the client draws the
//     remainder rather than rescaling the slices to fill the circle.
//   - A holding nothing can be said about is named, not dropped. Gold, cash and
//     a currency pair are all genuinely sectorless, and a pie that quietly
//     omitted them would describe a portfolio nobody holds.

// maxSectorPeers caps how many comparisons one card will draw.
//
// Four because each is an upstream request against the crumbed endpoint — the
// fragile one — and because a row of pies stops being comparable long before it
// stops fitting. Anything past it is dropped with a note rather than silently,
// so a reader who typed six symbols can see that two of them went.
const maxSectorPeers = 4

// sectorsTTL is how long a symbol's breakdown is reused.
//
// As long as a fund's holdings list, and for the same reason: a sector mix
// moves when a fund reconstitutes, which is quarterly at best, and the feeds
// reporting it are weeks behind. A day means a reader flipping between two
// portfolios pays for each symbol once.
const sectorsTTL = 24 * time.Hour

// SectorSpec is one card's worth of question: an allocation, and what to put it
// beside.
type SectorSpec struct {
	// Holdings is the basket to look through. It is store.Holding rather than
	// the run's own HoldingResult so an unsaved allocation, a saved one and a
	// fund all reach this the same way — a fund being one holding at 100%.
	Holdings []store.Holding
	// Label names the subject in the UI. Empty gets a generic one; nothing here
	// depends on it.
	Label string
	// Peers are the funds to compare against, in the order they were asked for.
	// Empty is fine — the allocation on its own is still worth seeing.
	Peers []string
}

// SectorSlice is one sector's share of a basket, as a percentage of the whole
// basket rather than of the part that could be classified. That is what lets
// two baskets be read against each other when one is better covered than the
// other.
type SectorSlice struct {
	Sector string  `json:"sector"`
	Weight float64 `json:"weight"`
}

// SectorAllocation is one basket's exposure.
type SectorAllocation struct {
	Label string `json:"label"`
	// Symbol is the fund this is, empty for an allocation that is not one.
	Symbol string        `json:"symbol"`
	Slices []SectorSlice `json:"slices"`
	// Covered is how much of the basket got a sector at all, as a percentage.
	// The slices sum to it, not to 100 — see the note at the top of this file.
	Covered float64 `json:"covered"`
	// Unclassified names the holdings the source would not place. Reported
	// rather than dropped: "40% of this is gold" is the single most important
	// thing a sector card can say about a portfolio that holds gold.
	Unclassified []string `json:"unclassified"`
}

// SectorReport is the card.
type SectorReport struct {
	Subject SectorAllocation `json:"subject"`
	// Peers is what it is being compared against, in the order asked for. A
	// peer the source will not classify is still here, with no slices and its
	// own symbol under Unclassified — a missing pie beside a named comparison
	// says something a silently shortened row does not.
	Peers []SectorAllocation `json:"peers"`
	Notes []string           `json:"notes"`
}

// Sectors assembles the card for one allocation.
func (e *Engine) Sectors(ctx context.Context, spec SectorSpec) (SectorReport, error) {
	classifier, ok := e.provider.(quotes.Classifier)
	if !ok {
		return SectorReport{}, quotes.ErrNoSectors
	}
	// The same reason every other upstream path calls this: a base URL or
	// timeout edited in Settings has to be in force now, not after a restart.
	e.syncProvider()

	// Renormalised by the same function the backtest uses, so the weights this
	// card is built from are the weights that produced the curve above it. A
	// second implementation of "three thirds of 33.33" would eventually
	// disagree with the first.
	holdings, err := weights(spec.Holdings)
	if err != nil {
		return SectorReport{}, err
	}

	label := strings.TrimSpace(spec.Label)
	if label == "" {
		label = "Portfolio"
	}
	subject, err := e.lookThrough(ctx, classifier, label, "", holdings)
	if err != nil {
		return SectorReport{}, err
	}

	out := SectorReport{Subject: subject, Peers: []SectorAllocation{}, Notes: []string{}}

	peers, dropped := sectorPeers(spec.Peers, holdings)
	for _, peer := range peers {
		// A peer is one holding at 100%, which makes it the same code path as
		// the allocation — the reason a fund and a portfolio can share this
		// card at all.
		allocation, err := e.lookThrough(ctx, classifier, peer, peer,
			[]HoldingResult{{Symbol: peer, Weight: 100}})
		if err != nil {
			return SectorReport{}, err
		}
		out.Peers = append(out.Peers, allocation)
	}
	if dropped > 0 {
		out.Notes = append(out.Notes, fmt.Sprintf(
			"%d more comparison%s asked for and not drawn — a card holds %d.",
			dropped, plural(dropped), maxSectorPeers))
	}
	return out, nil
}

// lookThrough adds up one basket's holdings, each scaled by what it is held at.
func (e *Engine) lookThrough(ctx context.Context, c quotes.Classifier,
	label, symbol string, holdings []HoldingResult) (SectorAllocation, error) {
	out := SectorAllocation{Label: label, Symbol: symbol, Slices: []SectorSlice{}, Unclassified: []string{}}

	totals := map[string]float64{}
	for _, h := range holdings {
		sectors, err := e.symbolSectors(ctx, c, h.Symbol)
		switch {
		case errors.Is(err, quotes.ErrUnclassified), errors.Is(err, quotes.ErrNotFound):
			// Both are durable answers about this holding rather than faults,
			// and neither is worth failing a card over — one gold ETF must not
			// cost a reader the other nine slices.
			out.Unclassified = append(out.Unclassified, h.Symbol)
			continue
		case err != nil:
			return SectorAllocation{}, fmt.Errorf("%s: %w", h.Symbol, err)
		}
		for _, s := range sectors {
			// h.Weight is a percentage of the basket and s.Weight a percentage
			// of the holding, so their product is a percentage twice over.
			totals[s.Sector] += h.Weight * s.Weight / 100
		}
	}

	for sector, weight := range totals {
		out.Slices = append(out.Slices, SectorSlice{Sector: sector, Weight: weight})
		out.Covered += weight
	}
	// Canonical order, not largest first: the client colours a slice by which
	// sector it is, and two pies are only comparable if they are drawn round
	// the circle the same way. Sorting by size would put the same sector in a
	// different place in every pie on the card.
	sort.Slice(out.Slices, func(i, j int) bool {
		return sectorOrder(out.Slices[i].Sector) < sectorOrder(out.Slices[j].Sector)
	})
	sort.Strings(out.Unclassified)
	return out, nil
}

// sectorRank is the position of each canonical sector, built once.
var sectorRank = func() map[string]int {
	rank := make(map[string]int, len(quotes.SectorNames))
	for i, name := range quotes.SectorNames {
		rank[name] = i
	}
	return rank
}()

// sectorOrder places a sector in the canonical order. A name this build has
// never heard of sorts after all of them rather than being dropped — it is
// drawn without a colour of its own, which is the visible version of the same
// admission.
func sectorOrder(name string) int {
	if rank, ok := sectorRank[name]; ok {
		return rank
	}
	return len(quotes.SectorNames)
}

// sectorPeers cleans up the comparison list and says how many it had to drop.
//
// A peer that is the subject's only holding is dropped for the reason the fund
// page drops a fund benchmarked against itself: two identical pies side by side
// read as a bug rather than as the tautology they are.
func sectorPeers(asked []string, holdings []HoldingResult) ([]string, int) {
	subject := ""
	if len(holdings) == 1 {
		subject = holdings[0].Symbol
	}

	out := make([]string, 0, len(asked))
	seen := map[string]bool{}
	dropped := 0
	for _, raw := range asked {
		peer := store.NormalizeSymbol(raw)
		if peer == "" || peer == subject || seen[peer] {
			continue
		}
		seen[peer] = true
		if len(out) >= maxSectorPeers {
			dropped++
			continue
		}
		out = append(out, peer)
	}
	return out, dropped
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// ---------------------------------------------------------------------------
// The sector cache
// ---------------------------------------------------------------------------

// sectorEntry is one symbol's cached breakdown.
type sectorEntry struct {
	weights []quotes.SectorWeight
	// unclassified records a durable "the source will not place this". It is
	// cached alongside the successes, unlike a fund's composition, because a
	// portfolio holding gold asks this question on every render of the card and
	// the answer is never going to change. Only this one refusal is remembered
	// — a handshake that failed is a fault, and re-asking is exactly right.
	unclassified bool
	fetched      time.Time
}

// symbolSectors reads one symbol's breakdown, or returns the cached one.
//
// By symbol rather than by basket, which is what makes a comparison cheap: a
// portfolio benchmarked against VTI and a portfolio that holds VTI pay for it
// once between them, exactly as the history cache does for prices.
func (e *Engine) symbolSectors(ctx context.Context, c quotes.Classifier, symbol string) ([]quotes.SectorWeight, error) {
	e.mu.Lock()
	entry, ok := e.sectorCache[symbol]
	e.mu.Unlock()
	if ok && time.Since(entry.fetched) <= sectorsTTL {
		if entry.unclassified {
			return nil, fmt.Errorf("%w: %s", quotes.ErrUnclassified, symbol)
		}
		return entry.weights, nil
	}

	weights, err := c.Sectors(ctx, symbol)
	if err != nil && !errors.Is(err, quotes.ErrUnclassified) {
		return nil, err
	}

	e.mu.Lock()
	if e.sectorCache == nil {
		e.sectorCache = map[string]sectorEntry{}
	}
	e.sectorCache[symbol] = sectorEntry{weights: weights, unclassified: err != nil, fetched: time.Now()}
	e.mu.Unlock()
	return weights, err
}
