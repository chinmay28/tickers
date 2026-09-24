// Package universe lists every symbol traded on a US exchange, for the archive
// to collect.
//
// The list comes from Nasdaq Trader's symbol directory — the same two files
// every exchange-listed security is published in each day, free and without a
// key. nasdaqlisted.txt covers Nasdaq's own listings; otherlisted.txt covers
// NYSE, NYSE American, NYSE Arca, Cboe and IEX. Together they are the listed
// US market: stocks, ETFs, and the odd closed-end fund. OTC names are not in
// them, and are not in the archive.
package universe

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// DefaultBaseURL is where Nasdaq Trader serves the directory.
const DefaultBaseURL = "https://www.nasdaqtrader.com/dynamic/SymDir"

// Listing is one tradable symbol, spelled the way Yahoo spells it.
type Listing struct {
	Symbol   string
	Name     string
	Exchange string
	ETF      bool
}

// Source fetches the directory. The zero value talks to Nasdaq Trader with
// the default HTTP client.
type Source struct {
	BaseURL string
	Client  *http.Client
}

// Listings fetches both files and returns every listing, deduplicated.
//
// It fails if either file fails, rather than returning the half it got: a
// caller that retires symbols missing from the list would otherwise retire
// half the market because one request timed out.
func (s Source) Listings(ctx context.Context) ([]Listing, error) {
	base := strings.TrimRight(s.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}

	seen := map[string]bool{}
	var out []Listing
	for _, file := range []string{"nasdaqlisted.txt", "otherlisted.txt"} {
		listings, err := fetch(ctx, client, base+"/"+file)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		for _, l := range listings {
			if !seen[l.Symbol] {
				seen[l.Symbol] = true
				out = append(out, l)
			}
		}
	}
	return out, nil
}

func fetch(ctx context.Context, client *http.Client, url string) ([]Listing, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return Parse(resp.Body)
}

// ErrTruncated means a directory file ended without its trailer. Every file
// Nasdaq publishes closes with a "File Creation Time" line, so a file without
// one was cut off in transit — and a list missing its tail would read as
// hundreds of delistings.
var ErrTruncated = errors.New("symbol directory is truncated")

// exchanges names otherlisted.txt's one-letter exchange codes.
var exchanges = map[string]string{
	"A": "NYSE American",
	"N": "NYSE",
	"P": "NYSE Arca",
	"Z": "Cboe BZX",
	"V": "IEX",
}

// Parse reads either directory file. The columns are found by header name,
// not position — the two files order them differently, and Nasdaq has added
// columns before.
func Parse(r io.Reader) ([]Listing, error) {
	scanner := bufio.NewScanner(r)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return nil, err
		}
		return nil, ErrTruncated
	}
	col := map[string]int{}
	for i, name := range strings.Split(strings.TrimSpace(scanner.Text()), "|") {
		col[name] = i
	}
	// nasdaqlisted.txt calls it "Symbol"; otherlisted.txt's "ACT Symbol" is
	// the one in the same (Nasdaq) symbology as the other file.
	symbolCol, ok := col["Symbol"]
	if !ok {
		if symbolCol, ok = col["ACT Symbol"]; !ok {
			return nil, errors.New("symbol directory has no symbol column")
		}
	}
	field := func(fields []string, name string) string {
		if i, ok := col[name]; ok && i < len(fields) {
			return strings.TrimSpace(fields[i])
		}
		return ""
	}

	var out []Listing
	complete := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "File Creation Time") {
			complete = true
			break
		}
		if line == "" {
			continue
		}
		fields := strings.Split(line, "|")
		if symbolCol >= len(fields) {
			continue
		}
		if field(fields, "Test Issue") == "Y" || field(fields, "NextShares") == "Y" {
			continue
		}
		symbol, ok := YahooSymbol(fields[symbolCol])
		if !ok {
			continue
		}
		exchange := "NASDAQ"
		if code := field(fields, "Exchange"); code != "" {
			exchange = exchanges[code]
			if exchange == "" {
				exchange = code
			}
		}
		out = append(out, Listing{
			Symbol:   symbol,
			Name:     field(fields, "Security Name"),
			Exchange: exchange,
			ETF:      field(fields, "ETF") == "Y",
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if !complete {
		return nil, ErrTruncated
	}
	return out, nil
}

// YahooSymbol converts a Nasdaq-symbology symbol to Yahoo's, reporting false
// for the kinds the archive skips.
//
// A share class is the one conversion that matters: Nasdaq writes BRK.B and
// Yahoo writes BRK-B. Preferreds ("$"), warrants (".WS"), units (".U") and
// rights (".R") are skipped rather than translated — Yahoo's spellings for
// them are inconsistent enough that a guess is wrong as often as right, and
// a wrong guess is a symbol that fails every day forever.
func YahooSymbol(s string) (string, bool) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" {
		return "", false
	}
	for _, r := range s {
		if !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.') {
			return "", false
		}
	}
	root, class, dotted := strings.Cut(s, ".")
	if !dotted {
		return s, true
	}
	if root == "" || len(class) != 1 || strings.ContainsAny(class, "UWR") {
		return "", false
	}
	return root + "-" + class, true
}
