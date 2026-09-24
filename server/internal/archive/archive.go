// Package archive owns the market-data archive: OHLCV bars for every symbol
// the collector tracks, the corporate actions that explain them, and a record
// of how much of each series has been fetched.
//
// It is a second SQLite file, not tables in the main database, and that is
// deliberate. The archive grows by gigabytes a year where the main database
// stays kilobytes; the upgrade rollback snapshots and health-checks the main
// database, and neither should wait on, or fail because of, the archive. It
// follows the main store's rules all the same: this package is the only thing
// that opens the file, its migrations are append-only, and it validates what
// it is given while the collector decides what to ask for.
package archive

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chinmay28/tickers/server/internal/quotes"
	_ "modernc.org/sqlite"
)

// Archive is a handle on the archive database. It is safe for concurrent use.
type Archive struct {
	db *sql.DB
}

// Open opens (creating if needed) the archive at path and applies any pending
// migrations. The pragmas are the main store's, for the same reasons: a reader
// — `tickers coverage` in another process — never blocks the collector, and a
// concurrent writer waits instead of failing.
func Open(path string) (*Archive, error) {
	if path == "" {
		return nil, errors.New("archive: empty database path")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("archive: create data dir: %w", err)
		}
	}
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("archive: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("archive: ping %s: %w", path, err)
	}
	a := &Archive{db: db}
	if err := a.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return a, nil
}

// Close releases the database handle.
func (a *Archive) Close() error { return a.db.Close() }

func (a *Archive) migrate() error {
	if _, err := a.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
	  id         TEXT PRIMARY KEY,
	  applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("archive: create schema_migrations: %w", err)
	}
	for _, m := range migrations {
		var n int
		if err := a.db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE id = ?`, m.ID).Scan(&n); err != nil {
			return fmt.Errorf("archive: read schema_migrations: %w", err)
		}
		if n > 0 {
			continue
		}
		tx, err := a.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(m.SQL); err != nil {
			tx.Rollback()
			return fmt.Errorf("archive: apply migration %s: %w", m.ID, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (id, applied_at) VALUES (?, ?)`,
			m.ID, time.Now().UTC().Format(time.RFC3339)); err != nil {
			tx.Rollback()
			return fmt.Errorf("archive: record migration %s: %w", m.ID, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("archive: commit %s: %w", m.ID, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Symbols
// ---------------------------------------------------------------------------

// Kinds of symbol. Informational: nothing schedules differently by kind.
const (
	KindStock = "stock"
	KindETF   = "etf"
	KindExtra = "extra"
)

// Entry is a symbol to track, as the collector hands it over.
type Entry struct {
	Symbol   string
	Name     string
	Exchange string
	Kind     string
}

// Symbol is a tracked symbol.
type Symbol struct {
	ID       int64
	Symbol   string
	Name     string
	Exchange string
	Kind     string
	// Active is whether the collector still fetches it. A symbol that drops
	// off the exchange lists is retired, not deleted: its history is exactly
	// what an analysis free of survivorship bias needs.
	Active bool
	// FirstTrade is the provider's earliest date for it, or zero until a
	// fetch has said.
	FirstTrade time.Time
}

// Track upserts entries as active symbols. It never retires anything, so it
// is safe to call with a partial list — the extras on a day the exchange
// directory could not be fetched.
func (a *Archive) Track(entries []Entry, now time.Time) error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO symbols (symbol, name, exchange, kind, active, first_seen, last_seen)
		VALUES (?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT (symbol) DO UPDATE SET
		  name = CASE WHEN excluded.name <> '' THEN excluded.name ELSE symbols.name END,
		  exchange = CASE WHEN excluded.exchange <> '' THEN excluded.exchange ELSE symbols.exchange END,
		  kind = excluded.kind, active = 1, last_seen = excluded.last_seen`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, e := range entries {
		symbol := strings.ToUpper(strings.TrimSpace(e.Symbol))
		if symbol == "" {
			return errors.New("a tracked symbol cannot be blank")
		}
		if _, err := stmt.Exec(symbol, e.Name, e.Exchange, e.Kind, now.Unix(), now.Unix()); err != nil {
			return fmt.Errorf("track %s: %w", symbol, err)
		}
	}
	return tx.Commit()
}

// RetireAllExcept marks every active symbol not in keep as retired, and
// reports how many it retired. The caller is trusted to pass a *complete*
// list; see universe.Source.Listings for why that is its job to guarantee.
func (a *Archive) RetireAllExcept(keep []string) (int, error) {
	tx, err := a.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`CREATE TEMP TABLE IF NOT EXISTS keep (symbol TEXT PRIMARY KEY)`); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM keep`); err != nil {
		return 0, err
	}
	for _, s := range keep {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO keep (symbol) VALUES (?)`, strings.ToUpper(strings.TrimSpace(s))); err != nil {
			return 0, err
		}
	}
	res, err := tx.Exec(`UPDATE symbols SET active = 0 WHERE active = 1 AND symbol NOT IN (SELECT symbol FROM keep)`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), tx.Commit()
}

// ActiveSymbols lists every symbol the collector should fetch.
func (a *Archive) ActiveSymbols() ([]Symbol, error) {
	rows, err := a.db.Query(`SELECT id, symbol, name, exchange, kind, active, first_trade
		FROM symbols WHERE active = 1 ORDER BY symbol`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Symbol
	for rows.Next() {
		var s Symbol
		var firstTrade sql.NullInt64
		if err := rows.Scan(&s.ID, &s.Symbol, &s.Name, &s.Exchange, &s.Kind, &s.Active, &firstTrade); err != nil {
			return nil, err
		}
		if firstTrade.Valid {
			s.FirstTrade = time.Unix(firstTrade.Int64, 0).UTC()
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Coverage
// ---------------------------------------------------------------------------

// Coverage is how much of one symbol's series at one interval is on disk, and
// how the last attempt to extend it went.
//
// Oldest and Newest bound the *window asked for*, not the bars that came back:
// a weekend has no bars and is still covered. Zero means nothing yet.
type Coverage struct {
	SymbolID int64
	Interval quotes.Interval
	Oldest   time.Time
	Newest   time.Time
	// Complete means there is nothing older left to fetch — the provider's
	// beginning for this symbol, or the edge of how far back it keeps this
	// interval.
	Complete bool
	// Failures counts consecutive failed attempts, and NextAttempt is when
	// the collector may try again. Both reset on a success.
	Failures    int
	NextAttempt time.Time
	LastError   string
}

// Coverages lists every coverage row for active symbols. A symbol × interval
// with no row has never been fetched.
func (a *Archive) Coverages() ([]Coverage, error) {
	rows, err := a.db.Query(`SELECT c.symbol_id, c.interval, c.oldest, c.newest, c.complete, c.failures, c.next_attempt, c.last_error
		FROM coverage c JOIN symbols s ON s.id = c.symbol_id WHERE s.active = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Coverage
	for rows.Next() {
		var c Coverage
		var interval string
		var oldest, newest sql.NullInt64
		var next int64
		if err := rows.Scan(&c.SymbolID, &interval, &oldest, &newest, &c.Complete, &c.Failures, &next, &c.LastError); err != nil {
			return nil, err
		}
		c.Interval = quotes.Interval(interval)
		c.Oldest = fromNull(oldest)
		c.Newest = fromNull(newest)
		if next != 0 {
			c.NextAttempt = time.Unix(next, 0).UTC()
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SaveCoverage writes a coverage row on its own — what a failed attempt
// records.
func (a *Archive) SaveCoverage(c Coverage) error {
	if err := validCoverage(c); err != nil {
		return err
	}
	return saveCoverage(a.db, c)
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func saveCoverage(db execer, c Coverage) error {
	var next int64
	if !c.NextAttempt.IsZero() {
		next = c.NextAttempt.Unix()
	}
	_, err := db.Exec(`INSERT INTO coverage (symbol_id, interval, oldest, newest, complete, failures, next_attempt, last_error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (symbol_id, interval) DO UPDATE SET
		  oldest = excluded.oldest, newest = excluded.newest, complete = excluded.complete,
		  failures = excluded.failures, next_attempt = excluded.next_attempt, last_error = excluded.last_error`,
		c.SymbolID, string(c.Interval), toNull(c.Oldest), toNull(c.Newest), c.Complete, c.Failures, next, c.LastError)
	return err
}

func validCoverage(c Coverage) error {
	if c.SymbolID <= 0 {
		return errors.New("coverage needs a symbol")
	}
	if c.Interval.Step() == 0 {
		return fmt.Errorf("unknown interval %q", c.Interval)
	}
	if !c.Oldest.IsZero() && !c.Newest.IsZero() && c.Newest.Before(c.Oldest) {
		return errors.New("coverage ends before it starts")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Recording
// ---------------------------------------------------------------------------

// Batch is one successful fetch: what came back, and the coverage it leaves
// behind.
type Batch struct {
	Coverage Coverage
	Series   quotes.CandleSeries
}

// Record writes a fetch in one transaction: its corporate actions, its bars,
// and its coverage. Either all of it lands or none does, so a crash can never
// leave coverage claiming bars that aren't there.
//
// A split is the one thing that rewrites history. Yahoo serves every price
// split-adjusted as of the moment it is asked, so once a split happens every
// bar stored before it is on the old basis. The first response to mention a
// split rescales the symbol's stored bars dated before it — every interval —
// and only then are the response's own bars written, which are already on the
// new basis and overwrite whatever they overlap. A split already on record is
// never applied twice.
func (a *Archive) Record(b Batch) error {
	if err := validCoverage(b.Coverage); err != nil {
		return err
	}
	id := b.Coverage.SymbolID

	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, s := range b.Series.Splits {
		ratio := s.Ratio()
		if ratio == 0 || ratio == 1 {
			continue
		}
		res, err := tx.Exec(`INSERT OR IGNORE INTO splits (symbol_id, ts, numerator, denominator) VALUES (?, ?, ?, ?)`,
			id, s.Time.Unix(), s.Numerator, s.Denominator)
		if err != nil {
			return fmt.Errorf("record split: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		if _, err := tx.Exec(`UPDATE bars SET open = open * ?1, high = high * ?1, low = low * ?1, close = close * ?1,
			  volume = CAST(round(volume / ?1) AS INTEGER)
			WHERE symbol_id = ?2 AND ts < ?3`, ratio, id, s.Time.Unix()); err != nil {
			return fmt.Errorf("rescale for split: %w", err)
		}
	}

	for _, d := range b.Series.Dividends {
		if _, err := tx.Exec(`INSERT INTO dividends (symbol_id, ts, amount) VALUES (?, ?, ?)
			ON CONFLICT (symbol_id, ts) DO UPDATE SET amount = excluded.amount`, id, d.Time.Unix(), d.Amount); err != nil {
			return fmt.Errorf("record dividend: %w", err)
		}
	}

	stmt, err := tx.Prepare(`INSERT INTO bars (symbol_id, interval, ts, open, high, low, close, volume)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (symbol_id, interval, ts) DO UPDATE SET
		  open = excluded.open, high = excluded.high, low = excluded.low, close = excluded.close, volume = excluded.volume`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, c := range b.Series.Candles {
		// A non-finite price is a parse accident upstream, not a price.
		// Skipped singly, so one bad bar doesn't cost a whole window.
		if !finite(c.Open, c.High, c.Low, c.Close) {
			continue
		}
		if _, err := stmt.Exec(id, string(b.Coverage.Interval), c.Time.Unix(), c.Open, c.High, c.Low, c.Close, c.Volume); err != nil {
			return fmt.Errorf("record bar: %w", err)
		}
	}

	if !b.Series.FirstTrade.IsZero() {
		if _, err := tx.Exec(`UPDATE symbols SET first_trade = ? WHERE id = ?`, b.Series.FirstTrade.Unix(), id); err != nil {
			return err
		}
	}
	if err := saveCoverage(tx, b.Coverage); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------------------------------------------------------------------------
// Reading
// ---------------------------------------------------------------------------

// Candles returns a symbol's stored bars in [from, to), oldest first.
func (a *Archive) Candles(symbol string, interval quotes.Interval, from, to time.Time) ([]quotes.Candle, error) {
	rows, err := a.db.Query(`SELECT b.ts, b.open, b.high, b.low, b.close, b.volume
		FROM bars b JOIN symbols s ON s.id = b.symbol_id
		WHERE s.symbol = ? AND b.interval = ? AND b.ts >= ? AND b.ts < ?
		ORDER BY b.ts`, strings.ToUpper(strings.TrimSpace(symbol)), string(interval), from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []quotes.Candle
	for rows.Next() {
		var c quotes.Candle
		var ts int64
		if err := rows.Scan(&ts, &c.Open, &c.High, &c.Low, &c.Close, &c.Volume); err != nil {
			return nil, err
		}
		c.Time = time.Unix(ts, 0).UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

// IntervalStats summarises one interval across every active symbol.
type IntervalStats struct {
	Interval quotes.Interval
	// Started counts symbols with anything fetched, Complete those whose
	// backfill has reached the beginning, Failing those whose last attempt
	// failed — including ones that have never once succeeded, which are
	// exactly the ones worth seeing.
	Started  int
	Complete int
	Failing  int
	// Oldest is the deepest any series reaches, Newest the freshest; Laggard
	// is the stalest series' newest point — the one that says how far behind
	// the forward pass is.
	Oldest  time.Time
	Newest  time.Time
	Laggard time.Time
}

// Stats is the archive at a glance.
type Stats struct {
	Active    int
	Retired   int
	Intervals []IntervalStats
}

// Stats summarises coverage. It reads the coverage table only — counting
// bars would scan a table of hundreds of millions of rows.
func (a *Archive) Stats() (Stats, error) {
	var st Stats
	if err := a.db.QueryRow(`SELECT coalesce(sum(active), 0), coalesce(sum(1 - active), 0) FROM symbols`).Scan(&st.Active, &st.Retired); err != nil {
		return st, err
	}
	rows, err := a.db.Query(`SELECT c.interval, count(c.newest), coalesce(sum(c.complete), 0), coalesce(sum(c.failures > 0), 0),
		  min(c.oldest), max(c.newest), min(c.newest)
		FROM coverage c JOIN symbols s ON s.id = c.symbol_id
		WHERE s.active = 1
		GROUP BY c.interval`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	byInterval := map[quotes.Interval]IntervalStats{}
	for rows.Next() {
		var s IntervalStats
		var interval string
		var oldest, newest, laggard sql.NullInt64
		if err := rows.Scan(&interval, &s.Started, &s.Complete, &s.Failing, &oldest, &newest, &laggard); err != nil {
			return st, err
		}
		s.Interval = quotes.Interval(interval)
		s.Oldest, s.Newest, s.Laggard = fromNull(oldest), fromNull(newest), fromNull(laggard)
		byInterval[s.Interval] = s
	}
	if err := rows.Err(); err != nil {
		return st, err
	}
	for _, i := range quotes.Intervals {
		if s, ok := byInterval[i]; ok {
			st.Intervals = append(st.Intervals, s)
		}
	}
	return st, nil
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

// MarkUniverseSynced and UniverseSyncedAt remember when the exchange lists
// were last read, so a restart doesn't re-read them.
func (a *Archive) MarkUniverseSynced(at time.Time) error {
	_, err := a.db.Exec(`INSERT INTO meta (key, value) VALUES ('universe_synced_at', ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, at.UTC().Format(time.RFC3339))
	return err
}

func (a *Archive) UniverseSyncedAt() (time.Time, error) {
	var v string
	err := a.db.QueryRow(`SELECT value FROM meta WHERE key = 'universe_synced_at'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	t, _ := time.Parse(time.RFC3339, v)
	return t, nil
}

func fromNull(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.Unix(v.Int64, 0).UTC()
}

func toNull(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Unix()
}

func finite(values ...float64) bool {
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}
