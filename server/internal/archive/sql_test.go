package archive

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chinmay28/tickers/server/internal/quotes"
)

func TestSQLReadsBarsAcrossYearsWithSymbols(t *testing.T) {
	a := newTestArchive(t)
	track(t, a, User, "AAA", "BBB")
	// Minute bars either side of new year, so two year files are attached.
	eve := time.Date(2025, 12, 31, 14, 30, 0, 0, time.UTC)
	record(t, a, Batch{SymbolID: idOf(t, a, "AAA"), Interval: quotes.OneMinute, Source: "yahoo",
		Series: quotes.CandleSeries{Candles: append(minutes(eve, 3, 10), minutes(eve.AddDate(0, 0, 2), 2, 11)...)}})
	record(t, a, Batch{SymbolID: idOf(t, a, "BBB"), Interval: quotes.Daily, Source: "yahoo",
		Series: quotes.CandleSeries{Candles: []quotes.Candle{candle(day(0), 5, 100), candle(day(1), 6, 100)},
			Dividends: []quotes.Dividend{{Time: day(1), Amount: 0.25}}}})

	ctx := context.Background()
	res, err := a.SQL(ctx, SQLQuery{SQL: "SELECT symbol, time, close FROM bars ORDER BY ts", Interval: quotes.OneMinute,
		From: eve.AddDate(0, 0, -1), To: eve.AddDate(0, 0, 5)})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 5 || res.Rows[0][0] != "AAA" || res.Rows[0][1] != "2025-12-31T14:30Z" || res.Rows[4][2] != 11.0 {
		t.Errorf("rows = %v, want AAA's five minute bars across both years, oldest first", res.Rows)
	}
	if strings.Join(res.Columns, ",") != "symbol,time,close" {
		t.Errorf("columns = %v", res.Columns)
	}

	res, err = a.SQL(ctx, SQLQuery{SQL: `SELECT b.symbol, b.time, d.amount FROM bars b JOIN dividends d ON d.symbol = b.symbol AND d.ts = b.ts;`,
		Interval: quotes.Daily, MaxRows: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || res.Rows[0][1] != day(1).Format(time.DateOnly) || res.Rows[0][2] != 0.25 {
		t.Errorf("a join to dividends = %v, want BBB's one ex-date", res.Rows)
	}

	res, err = a.SQL(ctx, SQLQuery{SQL: "SELECT * FROM bars", Interval: quotes.Daily, MaxRows: 1})
	if err != nil || len(res.Rows) != 1 || !res.Truncated {
		t.Errorf("a capped read = %v %v, want one row and truncation said", res, err)
	}
	res, err = a.SQL(ctx, SQLQuery{SQL: "SELECT count(*) FROM bars", Interval: quotes.Hourly, From: day(0), To: day(1)})
	if err != nil || res.Rows[0][0] != int64(0) {
		t.Errorf("an interval with no files = %v %v, want an empty bars view, not an error", res, err)
	}
}

func TestSQLCannotWriteOrEscape(t *testing.T) {
	a := newTestArchive(t)
	track(t, a, User, "AAA")
	ctx := context.Background()
	for _, stmt := range []string{
		"DELETE FROM symbols",
		"ATTACH '/etc/passwd' AS x",
		"SELECT 1; DELETE FROM symbols",
		"PRAGMA query_only = 0",
		"",
	} {
		if _, err := a.SQL(ctx, SQLQuery{SQL: stmt, Interval: quotes.Daily}); !errors.Is(err, ErrBadSQL) {
			t.Errorf("%q gave %v, want ErrBadSQL", stmt, err)
		}
	}
	// Past the keyword check, the connection itself refuses to write.
	if _, err := a.SQL(ctx, SQLQuery{SQL: "WITH x AS (SELECT 1) INSERT INTO symbols (symbol, first_seen, last_seen) SELECT 'ZZZ', 0, 0 FROM x", Interval: quotes.Daily}); err == nil {
		t.Error("a WITH … INSERT wrote to the catalog")
	}
	if _, err := a.Lookup("ZZZ"); !errors.Is(err, ErrUnknownSymbol) {
		t.Error("ZZZ was written")
	}

	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	_, err := a.SQL(short, SQLQuery{SQL: "WITH RECURSIVE r(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM r) SELECT count(*) FROM r", Interval: quotes.Daily})
	if err == nil || !strings.Contains(err.Error(), "ran out of time") {
		t.Errorf("a runaway query gave %v, want it stopped and said so", err)
	}
	if _, err := a.SQL(ctx, SQLQuery{SQL: "SELECT 1", Interval: quotes.OneMinute, From: day(0).AddDate(-20, 0, 0), To: day(0)}); err != nil {
		t.Errorf("years with no files attached nothing and shouldn't count against the limit: %v", err)
	}
}
