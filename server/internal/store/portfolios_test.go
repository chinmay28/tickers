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
