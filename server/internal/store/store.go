// Package store owns the SQLite database: the schema, the migration runner,
// and every query the rest of the server makes. Nothing else opens the file.
//
// The driver is modernc.org/sqlite — a pure-Go translation of SQLite with no
// cgo — which is what lets the whole server compile to one static binary
// (CGO_ENABLED=0) that cross-compiles to a Raspberry Pi from any machine.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned by the lookup/update/delete helpers when the row
// isn't there. The API layer turns it into a 404.
var ErrNotFound = errors.New("not found")

// ErrDuplicateSymbol is returned when a ticker would collide with one already
// on the watchlist. Symbols are the user-facing identity of a row, so this is
// a 409, not a 500.
var ErrDuplicateSymbol = errors.New("symbol already on the watchlist")

// ErrInvalidExpression wraps everything a composite's formula can be wrong
// about — a stray character, an unclosed bracket, a formula that combines
// nothing. It has a sentinel of its own because the API has to answer 400 for
// all of them, and the underlying messages are the parser's rather than a fixed
// set this package could match on.
var ErrInvalidExpression = errors.New("invalid composite formula")

// Store is a handle on the database. It is safe for concurrent use.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path, applies any
// pending migrations, and seeds first-run defaults.
//
// The pragmas are the ones that matter for a small always-on service:
// WAL so a reader never blocks the refresh loop, busy_timeout so a concurrent
// writer waits instead of failing, and foreign_keys because the schema relies
// on ON DELETE CASCADE to keep quotes with their tickers.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: empty database path")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("store: create data dir: %w", err)
		}
	}

	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// One writer at a time. SQLite serialises writes anyway; capping the pool
	// keeps that serialisation in Go, where it waits politely, rather than in
	// the driver, where it surfaces as SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: ping %s: %w", path, err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.seed(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// OpenMemory opens a private in-memory database — used by the tests, and by
// `tickers serve --db :memory:` for a throwaway instance.
func OpenMemory() (*Store, error) {
	return Open(":memory:")
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for the few callers that need it (the health check's
// liveness ping). Prefer adding a method here over reaching through this.
func (s *Store) DB() *sql.DB { return s.db }

// migrate applies every migration the database hasn't recorded yet, each in
// its own transaction so a failure leaves the schema at the last good step.
func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
	  id         TEXT PRIMARY KEY,
	  applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}

	applied := map[string]bool{}
	rows, err := s.db.Query(`SELECT id FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("store: read schema_migrations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		applied[id] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, m := range migrations {
		if applied[m.ID] {
			continue
		}
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("store: begin %s: %w", m.ID, err)
		}
		if _, err := tx.Exec(m.SQL); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: apply migration %s: %w", m.ID, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (id, applied_at) VALUES (?, ?)`,
			m.ID, nowRFC3339()); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: record migration %s: %w", m.ID, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: commit %s: %w", m.ID, err)
		}
	}
	return nil
}

// AppliedMigrations lists the migration IDs this database has recorded, oldest
// first. /api/health reports it so an operator can see, from the outside,
// exactly what schema a running instance is on.
func (s *Store) AppliedMigrations() ([]string, error) {
	rows, err := s.db.Query(`SELECT id FROM schema_migrations ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SeedSymbols is the watchlist a brand-new install starts with: the hardcoded
// list from the original update_minion_quotes.py script this app grew out of.
//
// They are seeded *pinned*, so a fresh install shows something real above the
// fold immediately and the pinned list starts out as a worked example of what
// the setting does. Unpinning them in Settings is a one-field edit, and they
// are ordinary tickers otherwise — nothing about them is special to the
// refresh loop.
var SeedSymbols = []string{"VTI", "GLD", "P", "ORCL", "STRC", "IBIT", "BTC-USD"}

// seed populates first-run defaults. It is keyed off a settings flag rather
// than "is the table empty", so deleting every ticker stays deleted instead of
// resurrecting the author's watchlist on the next restart.
func (s *Store) seed() error {
	done, err := s.Setting(SettingSeeded)
	if err != nil {
		return err
	}
	if done == "true" {
		return nil
	}

	now := nowRFC3339()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for i, sym := range SeedSymbols {
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO tickers (id, symbol, label, position, enabled, origin, created_at, updated_at)
			 VALUES (?, ?, '', ?, 1, 'seed', ?, ?)`,
			newID(), sym, i, now, now); err != nil {
			return fmt.Errorf("store: seed ticker %s: %w", sym, err)
		}
	}
	// Pin what we just seeded. This is the only place the default pinned list
	// is written: Config() falls back to *empty*, so that unpinning everything
	// stays unpinned rather than reading back as the shipped defaults.
	if _, err := tx.Exec(`INSERT OR IGNORE INTO settings (key, value) VALUES (?, ?)`,
		SettingPinnedSymbols, strings.Join(SeedSymbols, ",")); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO settings (key, value) VALUES (?, 'true')`, SettingSeeded); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------------------------------------------------------------------------
// Time
// ---------------------------------------------------------------------------

// Timestamps are stored as RFC3339 in UTC: sortable as text, unambiguous
// across the DST boundary, and readable when you open the database by hand.
func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// NormalizeSymbol renders a symbol the way every provider and every downstream
// consumer expects to see it: upper case, no surrounding space. Yahoo tickers
// are case-insensitive but the published payload keys are not, so normalising
// on the way in is what keeps `btc-usd` and `BTC-USD` from becoming two
// entries that overwrite each other downstream.
func NormalizeSymbol(sym string) string {
	return strings.ToUpper(strings.TrimSpace(sym))
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
