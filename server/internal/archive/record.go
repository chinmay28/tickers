package archive

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/chinmay28/tickers/server/internal/quotes"
)

// Cursor is how far one source has walked one symbol's series at one
// interval, and how its last attempt went.
//
// Oldest and Newest bound the *windows asked for*, not the bars that came
// back: a weekend has no bars and is still covered. Zero means nothing yet.
type Cursor struct {
	SymbolID int64           `json:"-"`
	Interval quotes.Interval `json:"interval"`
	Source   string          `json:"source"`
	Oldest   time.Time       `json:"oldest"`
	Newest   time.Time       `json:"newest"`
	// Complete means this source has nothing older to give: it reached the
	// listing, or the edge of how far back it keeps this interval.
	Complete bool `json:"complete"`
	// Failures counts consecutive failed attempts, and NextAttempt is when
	// the collector may try again. Both reset on a success.
	Failures    int       `json:"failures"`
	NextAttempt time.Time `json:"nextAttempt"`
	LastError   string    `json:"lastError"`
}

// Cursors lists every cursor of every active symbol.
func (a *Archive) Cursors() ([]Cursor, error) {
	return a.queryCursors(`WHERE c.symbol_id IN (SELECT id FROM symbols WHERE ` + activeSQL + `)`)
}

// SymbolCursors lists one symbol's cursors.
func (a *Archive) SymbolCursors(id int64) ([]Cursor, error) {
	return a.queryCursors(`WHERE c.symbol_id = ?`, id)
}

func (a *Archive) queryCursors(where string, args ...any) ([]Cursor, error) {
	rows, err := a.catalog.read.Query(`SELECT c.symbol_id, c.interval, s.name, c.oldest, c.newest, c.complete,
		  c.failures, c.next_attempt, c.last_error
		FROM cursors c JOIN sources s ON s.id = c.source_id `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Cursor
	for rows.Next() {
		var c Cursor
		var interval string
		var oldest, newest sql.NullInt64
		var next int64
		if err := rows.Scan(&c.SymbolID, &interval, &c.Source, &oldest, &newest, &c.Complete,
			&c.Failures, &next, &c.LastError); err != nil {
			return nil, err
		}
		c.Interval = quotes.Interval(interval)
		c.Oldest, c.Newest = fromNull(oldest), fromNull(newest)
		if next != 0 {
			c.NextAttempt = time.Unix(next, 0).UTC()
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SaveCursor writes a cursor on its own — what a failed attempt records.
func (a *Archive) SaveCursor(c Cursor) error {
	if err := validCursor(c); err != nil {
		return err
	}
	sid, err := a.sourceID(c.Source)
	if err != nil {
		return err
	}
	return saveCursor(a.catalog.write, c, sid)
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func saveCursor(db execer, c Cursor, sourceID int64) error {
	var next int64
	if !c.NextAttempt.IsZero() {
		next = c.NextAttempt.Unix()
	}
	_, err := db.Exec(`INSERT INTO cursors (symbol_id, interval, source_id, oldest, newest, complete, failures, next_attempt, last_error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (symbol_id, interval, source_id) DO UPDATE SET
		  oldest = excluded.oldest, newest = excluded.newest, complete = excluded.complete,
		  failures = excluded.failures, next_attempt = excluded.next_attempt, last_error = excluded.last_error`,
		c.SymbolID, string(c.Interval), sourceID, toNull(c.Oldest), toNull(c.Newest), c.Complete, c.Failures, next, c.LastError)
	return err
}

func validCursor(c Cursor) error {
	if c.SymbolID <= 0 {
		return errors.New("a cursor needs a symbol")
	}
	if c.Interval.Step() == 0 {
		return fmt.Errorf("unknown interval %q", c.Interval)
	}
	if !c.Oldest.IsZero() && !c.Newest.IsZero() && c.Newest.Before(c.Oldest) {
		return errors.New("a cursor cannot end before it starts")
	}
	return nil
}

// ResetCursors forgets every cursor a symbol has at an interval (every
// interval when interval is empty), so each source walks it again from the
// start. Bars are kept; a walk that finds them held skips them.
func (a *Archive) ResetCursors(id int64, interval quotes.Interval) error {
	if interval == "" {
		_, err := a.catalog.write.Exec(`DELETE FROM cursors WHERE symbol_id = ?`, id)
		return err
	}
	_, err := a.catalog.write.Exec(`DELETE FROM cursors WHERE symbol_id = ? AND interval = ?`, id, string(interval))
	return err
}

// ---------------------------------------------------------------------------
// Recording
// ---------------------------------------------------------------------------

// Batch is one successful fetch from one source.
type Batch struct {
	SymbolID int64
	Interval quotes.Interval
	Source   string
	// Replace lets these bars overwrite bars another source already stored.
	// Without it a source fills gaps and revises only its own bars — the
	// first source to store a bar keeps it until somebody asks otherwise.
	Replace bool
	Series  quotes.CandleSeries
	// Cursor, when set, is the source's cursor after this fetch.
	Cursor *Cursor
}

// Record writes a fetch.
//
// The steps are ordered so that a crash at any point leaves the archive
// consistent, and the next attempt repeats work rather than skipping it:
//
//  1. New splits are applied — see applySplits.
//  2. Bars are written, one transaction per partition file.
//  3. The ledger, the dividends and the cursor are written in one catalog
//     transaction.
//
// A crash after 2 leaves bars the cursor doesn't claim; the fetch is repeated
// and its upserts rewrite the same rows. There is no order of these steps in
// which a cursor claims bars that aren't there.
func (a *Archive) Record(b Batch) error {
	if b.SymbolID <= 0 {
		return errors.New("a batch needs a symbol")
	}
	if b.Interval.Step() == 0 {
		return fmt.Errorf("unknown interval %q", b.Interval)
	}
	if b.Cursor != nil {
		c := *b.Cursor
		c.SymbolID, c.Interval, c.Source = b.SymbolID, b.Interval, b.Source
		if err := validCursor(c); err != nil {
			return err
		}
		b.Cursor = &c
	}
	sid, err := a.sourceID(b.Source)
	if err != nil {
		return err
	}
	if sid > 62 {
		// The ledger's source mask is a signed 64-bit integer.
		return errors.New("the archive supports at most 62 sources")
	}

	if err := a.applySplits(b.SymbolID, b.Series.Splits); err != nil {
		return err
	}
	ledger, err := a.writeBars(b, sid)
	if err != nil {
		return err
	}

	tx, err := a.catalog.write.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for day, d := range ledger {
		if _, err := tx.Exec(`INSERT INTO days (symbol_id, interval, day, bars, sources) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (symbol_id, interval, day) DO UPDATE SET bars = excluded.bars, sources = excluded.sources`,
			b.SymbolID, string(b.Interval), day, d.bars, d.sources); err != nil {
			return fmt.Errorf("record ledger: %w", err)
		}
	}
	for _, d := range b.Series.Dividends {
		if _, err := tx.Exec(`INSERT INTO dividends (symbol_id, ts, amount) VALUES (?, ?, ?)
			ON CONFLICT (symbol_id, ts) DO UPDATE SET amount = excluded.amount`, b.SymbolID, d.Time.Unix(), d.Amount); err != nil {
			return fmt.Errorf("record dividend: %w", err)
		}
	}
	if !b.Series.FirstTrade.IsZero() {
		if _, err := tx.Exec(`UPDATE symbols SET first_trade = ? WHERE id = ?`, b.Series.FirstTrade.Unix(), b.SymbolID); err != nil {
			return err
		}
	}
	if b.Cursor != nil {
		if err := saveCursor(tx, *b.Cursor, sid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type dayCount struct {
	bars    int64
	sources int64
}

// writeBars upserts a batch's bars into their partitions and, for an intraday
// interval, recounts every day it touched for the ledger.
func (a *Archive) writeBars(b Batch, sid int64) (map[int64]dayCount, error) {
	byKey := map[string][]quotes.Candle{}
	for _, c := range b.Series.Candles {
		// A non-finite price is a parse accident upstream, not a price.
		// Skipped singly, so one bad bar doesn't cost a whole window.
		if !finite(c.Open, c.High, c.Low, c.Close, c.VWAP) {
			continue
		}
		key := partitionKey(b.Interval, c.Time)
		byKey[key] = append(byKey[key], c)
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	ledger := map[int64]dayCount{}
	for _, key := range keys {
		db, err := a.partition(key, true)
		if err != nil {
			return nil, err
		}
		tx, err := db.write.Begin()
		if err != nil {
			return nil, err
		}
		// A conflicting bar is overwritten only by its own source — a
		// provider revising a bar it printed — or on an explicit replace.
		stmt, err := tx.Prepare(`INSERT INTO bars (symbol_id, ts, open, high, low, close, volume, source_id, vwap, trades, session)
			VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?10, ?11, ?12)
			ON CONFLICT (symbol_id, ts) DO UPDATE SET
			  open = excluded.open, high = excluded.high, low = excluded.low, close = excluded.close,
			  volume = excluded.volume, source_id = excluded.source_id,
			  vwap = excluded.vwap, trades = excluded.trades, session = excluded.session
			WHERE bars.source_id = excluded.source_id OR ?9`)
		if err != nil {
			tx.Rollback()
			return nil, err
		}
		days := map[int64]bool{}
		for _, c := range byKey[key] {
			if _, err := stmt.Exec(b.SymbolID, c.Time.Unix(), c.Open, c.High, c.Low, c.Close, c.Volume, sid, b.Replace,
				nullIfZero(c.VWAP), nullIfZero(float64(c.Trades)), int(c.Session)); err != nil {
				stmt.Close()
				tx.Rollback()
				return nil, fmt.Errorf("record bar: %w", err)
			}
			days[dayOf(c.Time)] = true
		}
		stmt.Close()
		if b.Interval.Intraday() {
			for day := range days {
				d, err := countDay(tx, b.SymbolID, day)
				if err != nil {
					tx.Rollback()
					return nil, err
				}
				ledger[day] = d
			}
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
	}
	return ledger, nil
}

// countDay counts one symbol's bars on one UTC day, and which sources they
// came from.
func countDay(tx *sql.Tx, id, day int64) (dayCount, error) {
	rows, err := tx.Query(`SELECT source_id, count(*) FROM bars WHERE symbol_id = ? AND ts >= ? AND ts < ? GROUP BY source_id`,
		id, day, day+86400)
	if err != nil {
		return dayCount{}, err
	}
	defer rows.Close()
	var d dayCount
	for rows.Next() {
		var sid, n int64
		if err := rows.Scan(&sid, &n); err != nil {
			return d, err
		}
		d.bars += n
		d.sources |= 1 << sid
	}
	return d, rows.Err()
}

// dayOf is the UTC day an instant falls on, as the unix second of its
// midnight. A US session runs 13:30–21:00 UTC at the latest, so a US
// trading day is always one UTC day.
func dayOf(t time.Time) int64 {
	s := t.Unix()
	return s - ((s%86400)+86400)%86400
}

// ---------------------------------------------------------------------------
// Splits
// ---------------------------------------------------------------------------

// applySplits brings every stored bar onto the basis of any split it hasn't
// seen yet.
//
// Every source serves prices split-adjusted as of the moment it is asked — it
// is Archivist's contract — so once a split happens, every bar stored before
// it is on the old basis. Any response holding an adjusted pre-split bar also
// holds the split, because its window spans both; so the first response that
// reports a split is the moment to rescale, and it has to happen before that
// response's own bars (already on the new basis) are written.
//
// The split is marked pending first and applied last, with each partition
// recording that it has been rescaled. A crash in between resumes at the next
// Open on the partitions it hadn't reached. The collector is the archive's one
// writer and a pending split is finished before anything else is written, so
// a partition created after a split cannot hold bars on the old basis.
func (a *Archive) applySplits(id int64, splits []quotes.Split) error {
	for _, s := range splits {
		ratio := s.Ratio()
		if ratio == 0 || ratio == 1 {
			continue
		}
		var state string
		err := a.catalog.write.QueryRow(`SELECT state FROM splits WHERE symbol_id = ? AND ts = ?`, id, s.Time.Unix()).Scan(&state)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := a.catalog.write.Exec(`INSERT INTO splits (symbol_id, ts, numerator, denominator, state) VALUES (?, ?, ?, ?, 'pending')`,
				id, s.Time.Unix(), s.Numerator, s.Denominator); err != nil {
				return fmt.Errorf("record split: %w", err)
			}
		case err != nil:
			return err
		case state == "applied":
			continue
		}
		if err := a.rescale(id, s.Time, ratio); err != nil {
			return err
		}
		// Dividends are quoted per share, so a split changes them too; they
		// live in the catalog, which makes this and marking the split applied
		// one transaction.
		tx, err := a.catalog.write.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE dividends SET amount = amount * ? WHERE symbol_id = ? AND ts < ?`, ratio, id, s.Time.Unix()); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.Exec(`UPDATE splits SET state = 'applied' WHERE symbol_id = ? AND ts = ?`, id, s.Time.Unix()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// rescale multiplies one symbol's bars before at by ratio, in every partition
// that could hold one and hasn't been rescaled for this split already.
func (a *Archive) rescale(id int64, at time.Time, ratio float64) error {
	keys, err := a.existingPartitions()
	if err != nil {
		return err
	}
	for _, key := range keys {
		var year int
		if _, err := fmt.Sscanf(key[len(key)-4:], "%04d", &year); err == nil && year > at.UTC().Year() {
			continue
		}
		db, err := a.partition(key, false)
		if err != nil || db == nil {
			return err
		}
		tx, err := db.write.Begin()
		if err != nil {
			return err
		}
		res, err := tx.Exec(`INSERT OR IGNORE INTO applied_splits (symbol_id, ts) VALUES (?, ?)`, id, at.Unix())
		if err != nil {
			tx.Rollback()
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			tx.Rollback()
			continue
		}
		if _, err := tx.Exec(`UPDATE bars SET open = open * ?1, high = high * ?1, low = low * ?1, close = close * ?1,
			  vwap = vwap * ?1, volume = CAST(round(volume / ?1) AS INTEGER)
			WHERE symbol_id = ?2 AND ts < ?3`, ratio, id, at.Unix()); err != nil {
			tx.Rollback()
			return fmt.Errorf("rescale %s for a split: %w", key, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// resumeSplits finishes any split a crash left pending.
func (a *Archive) resumeSplits() error {
	rows, err := a.catalog.write.Query(`SELECT symbol_id, ts, numerator, denominator FROM splits WHERE state = 'pending'`)
	if err != nil {
		return err
	}
	type pending struct {
		id int64
		s  quotes.Split
	}
	var todo []pending
	for rows.Next() {
		var p pending
		var ts int64
		if err := rows.Scan(&p.id, &ts, &p.s.Numerator, &p.s.Denominator); err != nil {
			rows.Close()
			return err
		}
		p.s.Time = time.Unix(ts, 0).UTC()
		todo = append(todo, p)
	}
	rows.Close()
	for _, p := range todo {
		if err := a.applySplits(p.id, []quotes.Split{p.s}); err != nil {
			return fmt.Errorf("resume split: %w", err)
		}
	}
	return rows.Err()
}

// nullIfZero stores a source's silence as NULL rather than as a zero that
// would read as a measurement.
func nullIfZero(v float64) any {
	if v == 0 {
		return nil
	}
	return v
}

func finite(values ...float64) bool {
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}
