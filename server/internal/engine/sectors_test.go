package engine

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/store"
)

// fakeClassifier is a provider that can say what sectors a symbol is in. It is
// deliberately not a historian: nothing on the sector card reads a price, and a
// test that needed one would be proving something about the wrong file.
type fakeClassifier struct {
	*fakeProvider
	sectors map[string][]quotes.SectorWeight
	calls   map[string]int
}

func newFakeClassifier(sectors map[string][]quotes.SectorWeight) *fakeClassifier {
	return &fakeClassifier{
		fakeProvider: &fakeProvider{prices: map[string]float64{}},
		sectors:      sectors,
		calls:        map[string]int{},
	}
}

func (f *fakeClassifier) Sectors(_ context.Context, symbol string) ([]quotes.SectorWeight, error) {
	f.calls[symbol]++
	weights, ok := f.sectors[symbol]
	if !ok {
		return nil, quotes.ErrUnclassified
	}
	return weights, nil
}

func sectorEngine(t *testing.T, sectors map[string][]quotes.SectorWeight) (*Engine, *fakeClassifier) {
	t.Helper()
	provider := newFakeClassifier(sectors)
	eng, _ := newTestEngine(t, provider)
	return eng, provider
}

// weightOf is one sector's share of a basket, or 0 where it has none.
func weightOf(allocation SectorAllocation, sector string) float64 {
	for _, s := range allocation.Slices {
		if s.Sector == sector {
			return s.Weight
		}
	}
	return 0
}

func TestSectorsScaleEachHoldingByWhatItIsHeldAt(t *testing.T) {
	// The whole point of a look-through: a fund that is 80% technology, held at
	// a quarter of the portfolio, is 20% of the portfolio's technology — not
	// 80% and not a vote of one out of two.
	eng, _ := sectorEngine(t, map[string][]quotes.SectorWeight{
		"QQQ": {{Sector: "Technology", Weight: 80}, {Sector: "Healthcare", Weight: 20}},
		"XLF": {{Sector: "Financial Services", Weight: 100}},
	})

	report, err := eng.Sectors(context.Background(), SectorSpec{
		Holdings: []store.Holding{{Symbol: "QQQ", Weight: 25}, {Symbol: "XLF", Weight: 75}},
	})
	if err != nil {
		t.Fatalf("looking through an allocation: %v", err)
	}

	if got := weightOf(report.Subject, "Technology"); math.Abs(got-20) > 0.001 {
		t.Errorf("technology came back at %.2f%%; QQQ is 80%% technology and is held at 25%%, which is 20%%", got)
	}
	if got := weightOf(report.Subject, "Financial Services"); math.Abs(got-75) > 0.001 {
		t.Errorf("financials came back at %.2f%%; XLF is all of it and is held at 75%%", got)
	}
	if math.Abs(report.Subject.Covered-100) > 0.001 {
		t.Errorf("coverage came back %.2f%%; both holdings were fully classified", report.Subject.Covered)
	}
}

func TestSectorsRenormaliseTheWeightsTheRunUsed(t *testing.T) {
	// Two holdings at 30 are two halves, exactly as the backtest simulates them
	// — the card has to agree with the curve above it.
	eng, _ := sectorEngine(t, map[string][]quotes.SectorWeight{
		"AAA": {{Sector: "Energy", Weight: 100}},
		"BBB": {{Sector: "Utilities", Weight: 100}},
	})

	report, err := eng.Sectors(context.Background(), SectorSpec{
		Holdings: []store.Holding{{Symbol: "AAA", Weight: 30}, {Symbol: "BBB", Weight: 30}},
	})
	if err != nil {
		t.Fatalf("looking through an allocation: %v", err)
	}
	if got := weightOf(report.Subject, "Energy"); math.Abs(got-50) > 0.001 {
		t.Errorf("energy came back at %.2f%%; two holdings at 30 are two halves, not two thirtieths", got)
	}
}

func TestSectorsNameWhatTheSourceWillNotClassify(t *testing.T) {
	// Gold is the case this exists for. Dropping it would describe a portfolio
	// as all equities when 40% of it is a metal, and rescaling the slices to
	// fill the circle would do it while looking tidy.
	eng, _ := sectorEngine(t, map[string][]quotes.SectorWeight{
		"VTI": {{Sector: "Technology", Weight: 50}, {Sector: "Industrials", Weight: 50}},
	})

	report, err := eng.Sectors(context.Background(), SectorSpec{
		Holdings: []store.Holding{{Symbol: "VTI", Weight: 60}, {Symbol: "GLD", Weight: 40}},
	})
	if err != nil {
		t.Fatalf("looking through an allocation: %v", err)
	}

	if math.Abs(report.Subject.Covered-60) > 0.001 {
		t.Errorf("coverage came back %.2f%%; 40%% of this portfolio is in something the source will not place", report.Subject.Covered)
	}
	if !contains(report.Subject.Unclassified, "GLD") {
		t.Errorf("the unclassified list is %v; GLD is in the portfolio and has to be named, not folded into the gap", report.Subject.Unclassified)
	}
	if got := weightOf(report.Subject, "Technology"); math.Abs(got-30) > 0.001 {
		t.Errorf("technology came back at %.2f%%; it is half of a holding held at 60%%, and must not be scaled up to fill the circle", got)
	}
}

func TestSectorsDrawEveryPieTheSameWayRound(t *testing.T) {
	// The comparison mechanism: the client colours a slice by which sector it
	// is, so every basket has to be ordered the same way. Sorting by size would
	// put the same sector somewhere different in each pie.
	eng, _ := sectorEngine(t, map[string][]quotes.SectorWeight{
		"AAA": {{Sector: "Utilities", Weight: 70}, {Sector: "Energy", Weight: 30}},
		"BBB": {{Sector: "Energy", Weight: 90}, {Sector: "Utilities", Weight: 10}},
	})

	report, err := eng.Sectors(context.Background(), SectorSpec{
		Holdings: []store.Holding{{Symbol: "AAA", Weight: 100}},
		Peers:    []string{"BBB"},
	})
	if err != nil {
		t.Fatalf("looking through an allocation: %v", err)
	}
	if len(report.Peers) != 1 {
		t.Fatalf("%d comparisons came back for one asked for", len(report.Peers))
	}
	for _, basket := range []SectorAllocation{report.Subject, report.Peers[0]} {
		if len(basket.Slices) != 2 || basket.Slices[0].Sector != "Energy" {
			t.Errorf("%s is ordered %v; every basket is sorted into the canonical sector order, whatever the sizes are",
				basket.Label, basket.Slices)
		}
	}
}

func TestSectorsReportAPeerTheSourceCannotPlace(t *testing.T) {
	// A named comparison that came back empty is information. Dropping it would
	// leave the reader believing they had asked for something they hadn't.
	eng, _ := sectorEngine(t, map[string][]quotes.SectorWeight{
		"AAA": {{Sector: "Energy", Weight: 100}},
	})

	report, err := eng.Sectors(context.Background(), SectorSpec{
		Holdings: []store.Holding{{Symbol: "AAA", Weight: 100}},
		Peers:    []string{"BTC-USD"},
	})
	if err != nil {
		t.Fatalf("looking through an allocation: %v", err)
	}
	if len(report.Peers) != 1 {
		t.Fatalf("%d comparisons came back; a peer with no breakdown is still a peer that was asked for", len(report.Peers))
	}
	if report.Peers[0].Covered != 0 || !contains(report.Peers[0].Unclassified, "BTC-USD") {
		t.Errorf("BTC-USD came back covered %.2f%% and unclassified %v; it should be a named comparison with nothing in it",
			report.Peers[0].Covered, report.Peers[0].Unclassified)
	}
}

func TestSectorsDropAPeerThatIsTheSubject(t *testing.T) {
	// The fund page's case: two identical pies side by side read as a bug
	// rather than as the tautology they are.
	eng, _ := sectorEngine(t, map[string][]quotes.SectorWeight{
		"QQQ": {{Sector: "Technology", Weight: 100}},
	})

	report, err := eng.Sectors(context.Background(), SectorSpec{
		Holdings: []store.Holding{{Symbol: "QQQ", Weight: 100}},
		Peers:    []string{"qqq", "QQQ"},
	})
	if err != nil {
		t.Fatalf("looking through a fund: %v", err)
	}
	if len(report.Peers) != 0 {
		t.Errorf("%d comparisons came back; a fund compared against itself draws the same pie twice", len(report.Peers))
	}
}

func TestSectorsSayWhenTheyDroppedAComparison(t *testing.T) {
	// No silent caps: a reader who typed six symbols has to be able to see that
	// two of them are not on the card.
	sectors := map[string][]quotes.SectorWeight{"AAA": {{Sector: "Energy", Weight: 100}}}
	peers := []string{}
	for _, symbol := range []string{"P1", "P2", "P3", "P4", "P5", "P6"} {
		sectors[symbol] = []quotes.SectorWeight{{Sector: "Utilities", Weight: 100}}
		peers = append(peers, symbol)
	}
	eng, _ := sectorEngine(t, sectors)

	report, err := eng.Sectors(context.Background(), SectorSpec{
		Holdings: []store.Holding{{Symbol: "AAA", Weight: 100}},
		Peers:    peers,
	})
	if err != nil {
		t.Fatalf("looking through an allocation: %v", err)
	}
	if len(report.Peers) != maxSectorPeers {
		t.Errorf("%d comparisons were drawn; the card holds %d", len(report.Peers), maxSectorPeers)
	}
	if len(report.Notes) == 0 {
		t.Error("two comparisons were dropped and the card says nothing about it, which reads as though all six were drawn")
	}
}

func TestSectorsAskTheSourceOncePerSymbol(t *testing.T) {
	// The crumbed endpoint again. A benchmark that is also a holding, and a
	// second look at the same card, both have to be free.
	eng, provider := sectorEngine(t, map[string][]quotes.SectorWeight{
		"VTI": {{Sector: "Technology", Weight: 100}},
	})

	for i := 0; i < 3; i++ {
		if _, err := eng.Sectors(context.Background(), SectorSpec{
			Holdings: []store.Holding{{Symbol: "VTI", Weight: 100}},
			Peers:    []string{"VTI"},
		}); err != nil {
			t.Fatalf("looking through an allocation: %v", err)
		}
	}
	if provider.calls["VTI"] != 1 {
		t.Errorf("the breakdown was fetched %d times for three cards; it is cached for %v", provider.calls["VTI"], sectorsTTL)
	}
}

func TestSectorsRememberARefusal(t *testing.T) {
	// "This has no sectors" is as durable as "this is what it holds", and a
	// portfolio holding gold asks the question on every render.
	eng, provider := sectorEngine(t, map[string][]quotes.SectorWeight{
		"VTI": {{Sector: "Technology", Weight: 100}},
	})

	for i := 0; i < 3; i++ {
		report, err := eng.Sectors(context.Background(), SectorSpec{
			Holdings: []store.Holding{{Symbol: "VTI", Weight: 50}, {Symbol: "GLD", Weight: 50}},
		})
		if err != nil {
			t.Fatalf("looking through an allocation: %v", err)
		}
		if !contains(report.Subject.Unclassified, "GLD") {
			t.Fatalf("pass %d lost GLD from the unclassified list, so the cached refusal is not being reported", i+1)
		}
	}
	if provider.calls["GLD"] != 1 {
		t.Errorf("a symbol the source refuses to classify was asked about %d times; the refusal is durable", provider.calls["GLD"])
	}
}

func TestSectorsWithoutAClassifier(t *testing.T) {
	eng, _ := newTestEngine(t, &fakeProvider{prices: map[string]float64{"VTI": 300}})

	_, err := eng.Sectors(context.Background(), SectorSpec{
		Holdings: []store.Holding{{Symbol: "VTI", Weight: 100}},
	})
	if !errors.Is(err, quotes.ErrNoSectors) {
		t.Errorf("Sectors error = %v, want ErrNoSectors so the API answers 501 and the card disappears", err)
	}
}

func TestSectorsRefuseAnAllocationThatIsNotOne(t *testing.T) {
	eng, _ := sectorEngine(t, map[string][]quotes.SectorWeight{})

	_, err := eng.Sectors(context.Background(), SectorSpec{})
	if !errors.Is(err, ErrBadSpec) {
		t.Errorf("Sectors error = %v, want ErrBadSpec so the API answers 400 rather than 500", err)
	}
}
