package universe

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const nasdaqListed = `Symbol|Security Name|Market Category|Test Issue|Financial Status|Round Lot Size|ETF|NextShares
AAPL|Apple Inc. - Common Stock|Q|N|N|100|N|N
QQQ|Invesco QQQ Trust, Series 1|G|N|N|100|Y|N
ZXZZT|NASDAQ TEST STOCK|G|Y|N|100|N|N
File Creation Time: 0924202617:00|||||||
`

const otherListed = `ACT Symbol|Security Name|Exchange|CQS Symbol|ETF|Round Lot Size|Test Issue|NASDAQ Symbol
BRK.B|Berkshire Hathaway Inc. Class B|N|BRK.B|N|100|N|BRK.B
GLD|SPDR Gold Trust|P|GLD|Y|100|N|GLD
ABR$D|Arbor Realty Trust 6.375% Preferred|N|ABRpD|N|100|N|ABR-D
ACHR.WS|Archer Aviation Warrants|N|ACHR.WS|N|100|N|ACHR=
AAPL|a duplicate the other file already listed|N|AAPL|N|100|N|AAPL
File Creation Time: 0924202617:00|||||||
`

func TestParseReadsBothFilesByHeaderName(t *testing.T) {
	nasdaq, err := Parse(strings.NewReader(nasdaqListed))
	if err != nil {
		t.Fatalf("parse nasdaqlisted: %v", err)
	}
	if len(nasdaq) != 2 {
		t.Fatalf("nasdaqlisted gave %d listings, want 2 — the test issue must be skipped: %+v", len(nasdaq), nasdaq)
	}
	if nasdaq[0] != (Listing{Symbol: "AAPL", Name: "Apple Inc. - Common Stock", Exchange: "NASDAQ"}) {
		t.Errorf("first listing = %+v", nasdaq[0])
	}
	if !nasdaq[1].ETF {
		t.Errorf("QQQ is not flagged as an ETF")
	}

	other, err := Parse(strings.NewReader(otherListed))
	if err != nil {
		t.Fatalf("parse otherlisted: %v", err)
	}
	got := map[string]Listing{}
	for _, l := range other {
		got[l.Symbol] = l
	}
	if l, ok := got["BRK-B"]; !ok || l.Exchange != "NYSE" {
		t.Errorf("BRK.B came back as %+v, want BRK-B on NYSE — Yahoo writes a share class with a hyphen", l)
	}
	if l := got["GLD"]; l.Exchange != "NYSE Arca" || !l.ETF {
		t.Errorf("GLD = %+v, want an ETF on NYSE Arca", l)
	}
	for _, skipped := range []string{"ABR$D", "ABR-PD", "ACHR.WS", "ACHR-WS"} {
		if _, ok := got[skipped]; ok {
			t.Errorf("%s was kept; preferreds and warrants have no reliable Yahoo spelling", skipped)
		}
	}
}

func TestParseRefusesATruncatedFile(t *testing.T) {
	cut := nasdaqListed[:strings.Index(nasdaqListed, "File Creation")]
	if _, err := Parse(strings.NewReader(cut)); !errors.Is(err, ErrTruncated) {
		t.Fatalf("a file without its trailer parsed with err %v, want ErrTruncated — a cut-off list reads as delistings", err)
	}
	if _, err := Parse(strings.NewReader("")); !errors.Is(err, ErrTruncated) {
		t.Fatalf("an empty file parsed with err %v, want ErrTruncated", err)
	}
}

func TestYahooSymbol(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"AAPL", "AAPL", true},
		{" brk.a ", "BRK-A", true},
		{"BF.B", "BF-B", true},
		{"ABC.U", "", false},
		{"ABC.WS", "", false},
		{"ABC.R", "", false},
		{"ABC$A", "", false},
		{"ABC=", "", false},
		{".A", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := YahooSymbol(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("YahooSymbol(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestListingsMergesBothFilesAndFailsWhole(t *testing.T) {
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/nasdaqlisted.txt":
			w.Write([]byte(nasdaqListed))
		case "/otherlisted.txt":
			if fail {
				http.Error(w, "down", http.StatusServiceUnavailable)
				return
			}
			w.Write([]byte(otherListed))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	src := Source{BaseURL: srv.URL + "/"}
	all, err := src.Listings(context.Background())
	if err != nil {
		t.Fatalf("listings: %v", err)
	}
	symbols := map[string]int{}
	for _, l := range all {
		symbols[l.Symbol]++
	}
	if len(all) != 4 || symbols["AAPL"] != 1 {
		t.Errorf("listings = %v, want AAPL, QQQ, BRK-B, GLD once each", symbols)
	}

	fail = true
	if got, err := src.Listings(context.Background()); err == nil {
		t.Fatalf("one file failing returned %d listings and no error — half a market would read as delistings", len(got))
	}
}
