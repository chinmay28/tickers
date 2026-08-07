package store

import (
	"database/sql"
	"time"
)

// SaveQuote upserts the latest reading for a ticker and, when the reading is a
// real price, appends a history point.
//
// Only successful reads make it into history: a chart that plots gaps as if
// they were data is a chart that lies, and the failure is already recorded on
// the quote row where the UI shows it.
func (s *Store) SaveQuote(q Quote) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
		INSERT INTO quotes (ticker_id, symbol, price, previous_close, currency, short_name,
		                    market_state, status, error, fetched_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (ticker_id) DO UPDATE SET
		  symbol = excluded.symbol,
		  price = excluded.price,
		  previous_close = excluded.previous_close,
		  currency = excluded.currency,
		  short_name = excluded.short_name,
		  market_state = excluded.market_state,
		  status = excluded.status,
		  error = excluded.error,
		  fetched_at = excluded.fetched_at`,
		q.TickerID, q.Symbol, q.Price, q.PreviousClose, q.Currency, q.ShortName,
		q.MarketState, q.Status, q.Error, q.FetchedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}

	if q.Status == StatusOK && q.Price != nil {
		if _, err := tx.Exec(`INSERT INTO quote_history (symbol, price, at) VALUES (?, ?, ?)`,
			q.Symbol, *q.Price, q.FetchedAt.UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Quotes returns the latest reading for every ticker, keyed by ticker ID.
func (s *Store) Quotes() (map[string]Quote, error) {
	rows, err := s.db.Query(`
		SELECT ticker_id, symbol, price, previous_close, currency, short_name,
		       market_state, status, error, fetched_at
		FROM quotes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]Quote{}
	for rows.Next() {
		var (
			q                    Quote
			price, previousClose sql.NullFloat64
			fetchedAt            string
		)
		if err := rows.Scan(&q.TickerID, &q.Symbol, &price, &previousClose, &q.Currency,
			&q.ShortName, &q.MarketState, &q.Status, &q.Error, &fetchedAt); err != nil {
			return nil, err
		}
		if price.Valid {
			v := price.Float64
			q.Price = &v
		}
		if previousClose.Valid {
			v := previousClose.Float64
			q.PreviousClose = &v
		}
		q.FetchedAt = parseTime(fetchedAt)
		out[q.TickerID] = q
	}
	return out, rows.Err()
}

// History returns up to limit recent points for a symbol, oldest first — the
// order a sparkline wants to draw them in.
func (s *Store) History(symbol string, limit int) ([]HistoryPoint, error) {
	if limit <= 0 {
		limit = 120
	}
	// Newest-first with a LIMIT so the index does the work, then reversed in
	// Go. Ordering ascending would make SQLite scan the whole symbol.
	rows, err := s.db.Query(`
		SELECT symbol, price, at FROM quote_history
		WHERE symbol = ? ORDER BY at DESC LIMIT ?`, NormalizeSymbol(symbol), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	points := []HistoryPoint{}
	for rows.Next() {
		var (
			p  HistoryPoint
			at string
		)
		if err := rows.Scan(&p.Symbol, &p.Price, &at); err != nil {
			return nil, err
		}
		p.At = parseTime(at)
		points = append(points, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(points)-1; i < j; i, j = i+1, j-1 {
		points[i], points[j] = points[j], points[i]
	}
	return points, nil
}

// PruneHistory drops history older than the retention window and reports how
// many rows went. A zero or negative window disables pruning — the escape
// hatch for anyone who wants to keep the whole series.
func (s *Store) PruneHistory(retention time.Duration) (int64, error) {
	if retention <= 0 {
		return 0, nil
	}
	cutoff := time.Now().UTC().Add(-retention).Format(time.RFC3339Nano)
	res, err := s.db.Exec(`DELETE FROM quote_history WHERE at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
