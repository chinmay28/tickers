package store

import (
	"errors"
	"strings"
	"testing"
)

func sixtyForty() []Holding {
	return []Holding{{Symbol: "VTI", Weight: 60}, {Symbol: "BND", Weight: 40}}
}

func TestCreatePortfolioStoresTheAllocationAndItsDefaults(t *testing.T) {
	st := newTestStore(t)

	p, err := st.CreatePortfolio(NewPortfolio{
		Name:     "Two fund",
		Holdings: []Holding{{Symbol: " vti ", Weight: 60}, {Symbol: "bnd", Weight: 40}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if p.Holdings[0].Symbol != "VTI" || p.Holdings[1].Symbol != "BND" {
		t.Errorf("stored %v; symbols have to be normalised on the way in, as tickers are", p.Holdings)
	}
	// Every field the form can leave alone has to come back as something a
	// backtest can run, not as a zero.
	if p.Rebalance != RebalanceAnnually {
		t.Errorf("rebalance defaulted to %q, want %q", p.Rebalance, RebalanceAnnually)
	}
	if p.InitialAmount != 10000 {
		t.Errorf("initial amount defaulted to %v, want 10000", p.InitialAmount)
	}

	// And it survives the round trip through the JSON column.
	read, err := st.Portfolio(p.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(read.Holdings) != 2 || read.Holdings[0].Weight != 60 {
		t.Errorf("read back %+v, want the allocation that went in", read.Holdings)
	}
}

func TestCreatePortfolioNamesAnUnnamedOneAfterWhatItHolds(t *testing.T) {
	st := newTestStore(t)

	p, err := st.CreatePortfolio(NewPortfolio{Holdings: sixtyForty()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.Name != "VTI / BND" {
		t.Errorf("name = %q, want it derived from the holdings rather than left blank", p.Name)
	}
}

func TestCreatePortfolioDropsTheEditorsEmptyTrailingRow(t *testing.T) {
	st := newTestStore(t)

	// The allocation editor keeps a blank row at the bottom for the next entry.
	// Submitting with it still there means "done", not "a holding called nothing".
	p, err := st.CreatePortfolio(NewPortfolio{
		Name:     "Two fund",
		Holdings: append(sixtyForty(), Holding{}),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(p.Holdings) != 2 {
		t.Errorf("stored %d holdings, want 2 — the blank row is not one", len(p.Holdings))
	}
}

func TestPortfolioValidationRejectsWhatCannotBeSimulated(t *testing.T) {
	st := newTestStore(t)

	cases := []struct {
		name string
		in   NewPortfolio
		want string
	}{
		{
			// The one that earns its keep. Weights summing to 90 are not a
			// portfolio 10% in cash — nothing here models cash — they are a typo,
			// and simulating them would answer a different question.
			name: "weights that do not add up",
			in:   NewPortfolio{Name: "Short", Holdings: []Holding{{Symbol: "VTI", Weight: 60}, {Symbol: "BND", Weight: 30}}},
			want: "100",
		},
		{
			name: "no holdings at all",
			in:   NewPortfolio{Name: "Empty"},
			want: "at least one holding",
		},
		{
			name: "a holding with no weight",
			in:   NewPortfolio{Name: "Zero", Holdings: []Holding{{Symbol: "VTI", Weight: 100}, {Symbol: "BND"}}},
			want: "BND",
		},
		{
			name: "the same symbol twice",
			in:   NewPortfolio{Name: "Doubled", Holdings: []Holding{{Symbol: "VTI", Weight: 50}, {Symbol: "vti", Weight: 50}}},
			want: "twice",
		},
		{
			name: "a cadence nothing implements",
			in:   NewPortfolio{Name: "Odd", Holdings: sixtyForty(), Rebalance: "fortnightly"},
			want: "rebalance",
		},
		{
			name: "an end year before the start",
			in:   NewPortfolio{Name: "Backwards", Holdings: sixtyForty(), StartYear: 2020, EndYear: 2010},
			want: "before the start year",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := st.CreatePortfolio(tc.in)
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			// The API answers 400 for the whole family off this sentinel, so a
			// message that isn't wrapped becomes a 500 for a form error.
			if !errors.Is(err, ErrInvalidPortfolio) {
				t.Errorf("error %v is not an ErrInvalidPortfolio; the API will call it a server fault", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q, so the form cannot point at what is wrong", err, tc.want)
			}
		})
	}
}

func TestPortfolioAcceptsThirdsThatDoNotQuiteMakeAHundred(t *testing.T) {
	st := newTestStore(t)

	// 33.33 three times is 99.99. Rejecting it would be arithmetic pedantry
	// aimed at the one person who typed the honest thing.
	if _, err := st.CreatePortfolio(NewPortfolio{
		Name: "Thirds",
		Holdings: []Holding{
			{Symbol: "VTI", Weight: 33.33},
			{Symbol: "BND", Weight: 33.33},
			{Symbol: "GLD", Weight: 33.33},
		},
	}); err != nil {
		t.Errorf("three equal thirds were rejected: %v", err)
	}
}

func TestHoldingsCarryTheirReplacementThroughTheJSONColumn(t *testing.T) {
	st := newTestStore(t)

	p, err := st.CreatePortfolio(NewPortfolio{
		Name: "Recent listing",
		Holdings: []Holding{
			{Symbol: "HOOD", Weight: 20, Replacement: " qqq "},
			{Symbol: "VTI", Weight: 80},
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	read, err := st.Portfolio(p.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if read.Holdings[0].Replacement != "QQQ" {
		t.Errorf("replacement = %q, want QQQ normalised and stored", read.Holdings[0].Replacement)
	}
	if read.Holdings[1].Replacement != "" {
		t.Errorf("a holding with no stand-in came back with %q", read.Holdings[1].Replacement)
	}

	// A symbol standing in for itself is a typo that would otherwise look like
	// a working configuration and change nothing.
	_, err = st.CreatePortfolio(NewPortfolio{
		Name:     "Circular",
		Holdings: []Holding{{Symbol: "VTI", Weight: 100, Replacement: "vti"}},
	})
	if err == nil {
		t.Fatal("a holding standing in for itself was accepted")
	}
	if !errors.Is(err, ErrInvalidPortfolio) {
		t.Errorf("error %v is not an ErrInvalidPortfolio", err)
	}
}

func TestContributionAndItsCadenceOnlyMeanAnythingTogether(t *testing.T) {
	st := newTestStore(t)

	// An amount with no cadence has no moment to be paid at, and a cadence with
	// no amount pays nothing at it. Half a pair reads as neither, rather than as
	// a portfolio that quietly contributes on some schedule nobody picked.
	amountOnly, err := st.CreatePortfolio(NewPortfolio{
		Name: "Amount only", Holdings: sixtyForty(), Contribution: 500,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if amountOnly.Contribution != 0 || amountOnly.ContributionFrequency != RebalanceNone {
		t.Errorf("stored %v %q, want it neutralised", amountOnly.Contribution, amountOnly.ContributionFrequency)
	}

	cadenceOnly, err := st.CreatePortfolio(NewPortfolio{
		Name: "Cadence only", Holdings: sixtyForty(), ContributionFrequency: RebalanceMonthly,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if cadenceOnly.Contribution != 0 || cadenceOnly.ContributionFrequency != RebalanceNone {
		t.Errorf("stored %v %q, want it neutralised", cadenceOnly.Contribution, cadenceOnly.ContributionFrequency)
	}

	both, err := st.CreatePortfolio(NewPortfolio{
		Name: "Both", Holdings: sixtyForty(), Contribution: 500, ContributionFrequency: RebalanceMonthly,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if both.Contribution != 500 || both.ContributionFrequency != RebalanceMonthly {
		t.Errorf("stored %v %q, want them kept", both.Contribution, both.ContributionFrequency)
	}

	if _, err := st.CreatePortfolio(NewPortfolio{
		Name: "Odd", Holdings: sixtyForty(), Contribution: 500, ContributionFrequency: "fortnightly",
	}); err == nil {
		t.Error("a cadence nothing implements was accepted")
	}
}

func TestUpdatePortfolioValidatesTheWholeAllocationNotThePatch(t *testing.T) {
	st := newTestStore(t)
	p, err := st.CreatePortfolio(NewPortfolio{Name: "Two fund", Holdings: sixtyForty()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Replacing one holding's weight leaves the set summing to 70. Checking the
	// patched field alone would let that through.
	broken := []Holding{{Symbol: "VTI", Weight: 30}, {Symbol: "BND", Weight: 40}}
	if _, err := st.UpdatePortfolio(p.ID, PortfolioPatch{Holdings: &broken}); err == nil {
		t.Error("an update that left the weights at 70% was accepted")
	}

	name := "Renamed"
	updated, err := st.UpdatePortfolio(p.ID, PortfolioPatch{Name: &name})
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if updated.Name != "Renamed" || len(updated.Holdings) != 2 {
		t.Errorf("renaming changed more than the name: %+v", updated)
	}
}

func TestPortfoliosAreListedInDisplayOrderAndDeletable(t *testing.T) {
	st := newTestStore(t)
	for _, name := range []string{"First", "Second", "Third"} {
		if _, err := st.CreatePortfolio(NewPortfolio{Name: name, Holdings: sixtyForty()}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	all, err := st.Portfolios()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 3 || all[0].Name != "First" || all[2].Name != "Third" {
		t.Fatalf("listed %v, want them in the order they were added", all)
	}

	if err := st.DeletePortfolio(all[1].ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := st.DeletePortfolio(all[1].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting twice gave %v, want ErrNotFound", err)
	}
	if _, err := st.Portfolio("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("reading a missing portfolio gave %v, want ErrNotFound", err)
	}
}

func TestPortfolioSymbolIsPublishable(t *testing.T) {
	cases := map[string]string{
		"Four fund":        "FOUR-FUND",
		"  60/40  ":        "60-40",
		"Rainy-day fund!!": "RAINY-DAY-FUND",
		"VTI":              "VTI",
		"!!!":              "",
	}
	for name, want := range cases {
		// A space survives SQLite and JSON and then breaks the first consumer
		// that splits on one, so it never reaches a published key.
		if got := PortfolioSymbol(name); got != want {
			t.Errorf("PortfolioSymbol(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestSyncPortfolioTickerCreatesRenamesAndStoresUnits(t *testing.T) {
	st := newTestStore(t)
	p, err := st.CreatePortfolio(NewPortfolio{Name: "Two fund", Holdings: sixtyForty()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	row, err := st.SyncPortfolioTicker(p, map[string]float64{"VTI": 20, "BND": 57.14})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if row.Symbol != "TWO-FUND" || !row.IsPortfolio() || row.PortfolioID != p.ID {
		t.Fatalf("row = %+v, want a portfolio row keyed TWO-FUND", row)
	}
	if row.IsComposite() {
		t.Error("a portfolio's row is not a composite; its symbol is a name, not a formula")
	}

	// The units are what the refresh cycle prices the row from, so they have to
	// survive the round trip through the allocations column.
	saved, err := st.Portfolio(p.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if saved.Holdings[0].Units != 20 {
		t.Errorf("units = %v, want 20", saved.Holdings[0].Units)
	}

	// Renaming the portfolio renames the key it publishes under, and does not
	// leave a second row behind.
	renamed := "Three fund"
	p, err = st.UpdatePortfolio(p.ID, PortfolioPatch{Name: &renamed})
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := st.SyncPortfolioTicker(p, map[string]float64{"VTI": 20, "BND": 57.14}); err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	tickers, err := st.Tickers()
	if err != nil {
		t.Fatalf("tickers: %v", err)
	}
	rows := 0
	for _, ticker := range tickers {
		if ticker.IsPortfolio() {
			rows++
			if ticker.Symbol != "THREE-FUND" {
				t.Errorf("row symbol = %q, want THREE-FUND", ticker.Symbol)
			}
		}
	}
	if rows != 1 {
		t.Errorf("found %d portfolio rows, want 1 — a rename moves the row, it does not add one", rows)
	}
}

func TestDeletingAPortfolioTakesItsRowWithIt(t *testing.T) {
	st := newTestStore(t)
	p, err := st.CreatePortfolio(NewPortfolio{Name: "Two fund", Holdings: sixtyForty()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.SyncPortfolioTicker(p, map[string]float64{"VTI": 1, "BND": 1}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if err := st.DeletePortfolio(p.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Left behind, the row is an unpriceable symbol nothing knows how to value
	// — and an older binary would try to fetch "TWO-FUND" from the provider
	// every cycle forever.
	if _, err := st.PortfolioTicker(p.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("the row outlived its portfolio: %v", err)
	}
}

func TestAPortfolioRowCannotBeRePointedFromTheWatchlist(t *testing.T) {
	st := newTestStore(t)
	p, err := st.CreatePortfolio(NewPortfolio{Name: "Two fund", Holdings: sixtyForty()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	row, err := st.SyncPortfolioTicker(p, map[string]float64{"VTI": 1, "BND": 1})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}

	// Its symbol is the portfolio's name and its value comes from the
	// allocation; retyping either here would leave a row saying one thing and
	// pricing another.
	symbol := "AAPL"
	if _, err := st.UpdateTicker(row.ID, TickerPatch{Symbol: &symbol}); err == nil {
		t.Error("a portfolio's row was re-pointed at a symbol")
	}

	// The label is still the user's.
	label := "Retirement"
	updated, err := st.UpdateTicker(row.ID, TickerPatch{Label: &label})
	if err != nil {
		t.Fatalf("relabel: %v", err)
	}
	if updated.Label != "Retirement" || updated.Symbol != "TWO-FUND" {
		t.Errorf("row = %+v after relabelling", updated)
	}
}
