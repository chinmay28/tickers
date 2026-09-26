package archive

import (
	"context"
	"sort"
	"time"

	"github.com/chinmay28/tickers/server/internal/quotes"
)

// Traded is a symbol and how much of it traded in a window.
type Traded struct {
	Symbol Symbol
	// DollarVolume is the average close times volume over the days it
	// traded in the window.
	DollarVolume float64
	Days         int
}

// MostTraded lists the symbols with daily bars in [from, to) by average
// dollar volume, most first: the liquid universe a cross-sectional study
// ranks. Kind, when set, keeps only that kind. Symbols no longer listed are
// included — a universe of today's survivors is the bias this is meant to
// avoid — as are excluded ones, whose bars are history like any other's.
//
// It scans the daily file once, grouped by symbol: seconds for the whole
// market on a Pi, which is why the caller caps what it loads afterwards
// rather than asking per symbol.
func (a *Archive) MostTraded(ctx context.Context, from, to time.Time, kind string, limit int) ([]Traded, error) {
	db, err := a.partition(partitionKey(quotes.Daily, from), false)
	if err != nil || db == nil {
		return nil, err
	}
	rows, err := db.read.QueryContext(ctx, `SELECT symbol_id, avg(close * volume), count(*) FROM bars
		WHERE ts >= ? AND ts < ? AND session = 0 GROUP BY symbol_id`, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	byID := map[int64]Traded{}
	for rows.Next() {
		var id int64
		var t Traded
		if err := rows.Scan(&id, &t.DollarVolume, &t.Days); err != nil {
			rows.Close()
			return nil, err
		}
		byID[id] = t
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	srows, err := a.catalog.read.QueryContext(ctx, `SELECT `+symbolColumns+` FROM symbols`)
	if err != nil {
		return nil, err
	}
	defer srows.Close()
	var out []Traded
	for srows.Next() {
		s, err := scanSymbol(srows)
		if err != nil {
			return nil, err
		}
		t, ok := byID[s.ID]
		if !ok || (kind != "" && s.Kind != kind) {
			continue
		}
		t.Symbol = s
		out = append(out, t)
	}
	if err := srows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DollarVolume != out[j].DollarVolume {
			return out[i].DollarVolume > out[j].DollarVolume
		}
		return out[i].Symbol.Symbol < out[j].Symbol.Symbol
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
