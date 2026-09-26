package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"time"
)

// Trial is one strategy an agent tested, as the record of research keeps it.
//
// The record exists because the best of many backtests looks good by
// construction. How good it has to look before it means anything depends on
// how many were tried on the same data, and only a record kept outside the
// agent — which may not remember, or may not say — can answer that.
type Trial struct {
	Fingerprint string          `json:"fingerprint"`
	Symbol      string          `json:"symbol"`
	Interval    string          `json:"interval"`
	Origin      string          `json:"origin"`
	Definition  json.RawMessage `json:"definition"`
	// SharpePerBar and Periods are the backtest's per-bar Sharpe ratio and
	// how many returns it came from, as the last run found them.
	SharpePerBar float64   `json:"sharpePerBar"`
	Periods      int       `json:"periods"`
	TotalReturn  float64   `json:"totalReturn"`
	Trades       int       `json:"trades"`
	Runs         int       `json:"runs"`
	FirstAt      time.Time `json:"firstAt"`
	LastAt       time.Time `json:"lastAt"`
}

// TrialFamily is every trial on one symbol at one interval — the trials
// that competed over the same data, and so the count a result is deflated by.
type TrialFamily struct {
	Symbol   string `json:"symbol"`
	Interval string `json:"interval"`
	Trials   int    `json:"trials"`
	// SharpeVariance is the sample variance of the trials' per-bar Sharpe
	// ratios: how widely luck alone spread them.
	SharpeVariance float64 `json:"sharpeVariance"`
	BestSharpe     float64 `json:"bestSharpePerBar"`
	LastAt         time.Time
}

// Fingerprint is a trial's key in the record: the same rules over the same
// series and window are one trial however often they run. The symbol and
// interval are part of it even though a definition usually names them too —
// the record shouldn't depend on every caller's definitions doing so.
func Fingerprint(symbol, interval string, def json.RawMessage) string {
	h := sha256.New()
	h.Write([]byte(NormalizeSymbol(symbol) + "\x00" + interval + "\x00"))
	h.Write(compact(def))
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// RecordTrials adds trials to the record, or counts another run of ones
// already in it, in one transaction. A rerun takes the new numbers — a
// window ending today covers more bars each day.
func (s *Store) RecordTrials(trials []Trial) error {
	if len(trials) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := nowRFC3339()
	stmt, err := tx.Prepare(`INSERT INTO research_trials
		(fingerprint, symbol, interval, origin, definition, sharpe_per_bar, periods, total_return, trades, first_at, last_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (fingerprint) DO UPDATE SET runs = runs + 1, last_at = excluded.last_at,
		  sharpe_per_bar = excluded.sharpe_per_bar, periods = excluded.periods,
		  total_return = excluded.total_return, trades = excluded.trades`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, t := range trials {
		if t.Symbol == "" || t.Interval == "" || len(t.Definition) == 0 {
			return errors.New("a trial needs a symbol, an interval and its definition")
		}
		sharpe := t.SharpePerBar
		if math.IsNaN(sharpe) || math.IsInf(sharpe, 0) {
			sharpe = 0
		}
		if _, err := stmt.Exec(Fingerprint(t.Symbol, t.Interval, t.Definition), NormalizeSymbol(t.Symbol), t.Interval, t.Origin, string(compact(t.Definition)),
			sharpe, t.Periods, t.TotalReturn, t.Trades, now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Family summarises the trials on one symbol at one interval.
func (s *Store) Family(symbol, interval string) (TrialFamily, error) {
	f := TrialFamily{Symbol: NormalizeSymbol(symbol), Interval: interval}
	var mean, meanSq, best float64
	var last string
	err := s.db.QueryRow(`SELECT count(*), COALESCE(avg(sharpe_per_bar), 0), COALESCE(avg(sharpe_per_bar * sharpe_per_bar), 0),
		COALESCE(max(sharpe_per_bar), 0), COALESCE(max(last_at), '') FROM research_trials WHERE symbol = ? AND interval = ?`,
		f.Symbol, interval).Scan(&f.Trials, &mean, &meanSq, &best, &last)
	if err != nil {
		return f, err
	}
	f.BestSharpe, f.LastAt = best, parseTime(last)
	if f.Trials > 1 {
		n := float64(f.Trials)
		f.SharpeVariance = math.Max(0, (meanSq-mean*mean)*n/(n-1))
	}
	return f, nil
}

// Families summarises the whole record, the most recently tried first.
func (s *Store) Families() ([]TrialFamily, error) {
	rows, err := s.db.Query(`SELECT symbol, interval FROM research_trials GROUP BY symbol, interval ORDER BY max(last_at) DESC`)
	if err != nil {
		return nil, err
	}
	var keys [][2]string
	for rows.Next() {
		var k [2]string
		if err := rows.Scan(&k[0], &k[1]); err != nil {
			rows.Close()
			return nil, err
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []TrialFamily{}
	for _, k := range keys {
		f, err := s.Family(k[0], k[1])
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// Trials lists the record's trials, the most recently run first; an empty
// symbol or interval matches any.
func (s *Store) Trials(symbol, interval string, limit int) ([]Trial, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT fingerprint, symbol, interval, origin, definition, sharpe_per_bar, periods, total_return, trades,
		runs, first_at, last_at FROM research_trials
		WHERE (? = '' OR symbol = ?) AND (? = '' OR interval = ?) ORDER BY last_at DESC, fingerprint LIMIT ?`,
		NormalizeSymbol(symbol), NormalizeSymbol(symbol), interval, interval, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Trial{}
	for rows.Next() {
		var t Trial
		var def, first, last string
		if err := rows.Scan(&t.Fingerprint, &t.Symbol, &t.Interval, &t.Origin, &def, &t.SharpePerBar, &t.Periods, &t.TotalReturn,
			&t.Trades, &t.Runs, &first, &last); err != nil {
			return nil, err
		}
		t.Definition, t.FirstAt, t.LastAt = json.RawMessage(def), parseTime(first), parseTime(last)
		out = append(out, t)
	}
	return out, rows.Err()
}
