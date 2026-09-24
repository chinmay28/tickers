package archive

import (
	"database/sql"
	"fmt"
	"time"
)

// migration is one forward-only schema step, under the same rules as the main
// store's: THE LISTS ARE APPEND-ONLY, and every step must be additive, because
// a binary rolled back onto a newer archive has to keep collecting into it.
type migration struct {
	ID  string
	SQL string
}

// migrate applies every migration db hasn't recorded, each in its own
// transaction.
func migrate(db *sql.DB, list []migration) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
	  id         TEXT PRIMARY KEY,
	  applied_at TEXT NOT NULL
	)`); err != nil {
		return err
	}
	for _, m := range list {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE id = ?`, m.ID).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(m.SQL); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", m.ID, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (id, applied_at) VALUES (?, ?)`,
			m.ID, time.Now().UTC().Format(time.RFC3339)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// catalogMigrations shape catalog.sqlite: everything that isn't a bar.
//
// Times are unix seconds rather than the main store's RFC3339 text. The main
// store has hundreds of rows and is read by people; this one has millions and
// is read by programs.
var catalogMigrations = []migration{
	{
		// A symbol is tracked while it belongs to at least one list and isn't
		// excluded. Membership is a set of flags rather than one "origin"
		// because the lists overlap — GLD is listed on NYSE Arca *and* on the
		// watchlist — and leaving one list must not stop a symbol another list
		// still wants. A symbol on no list is retired: never fetched again,
		// never deleted, its history kept for analysis free of survivorship
		// bias.
		//
		// Cursors are per source. Yahoo walking back its 29 days of minute
		// bars and a paid feed walking back twenty years are two independent
		// walks over the same series, and each needs to know how far *it* has
		// got.
		//
		// `days` is the ledger: one row per symbol, intraday interval and UTC
		// day with the number of bars stored and a bitmask of the sources they
		// came from. It is what a gap-filling source consults to skip days
		// already held, what the coverage timeline is drawn from, and how the
		// archive counts its bars without scanning a billion rows.
		//
		// A split is recorded as pending before any bar is rescaled and marked
		// applied after every partition has been, so a crash half way through
		// resumes rather than rescaling twice.
		ID: "001_initial",
		SQL: `
CREATE TABLE symbols (
  id          INTEGER PRIMARY KEY,
  symbol      TEXT NOT NULL UNIQUE,
  name        TEXT NOT NULL DEFAULT '',
  exchange    TEXT NOT NULL DEFAULT '',
  kind        TEXT NOT NULL DEFAULT '',
  listed      INTEGER NOT NULL DEFAULT 0,
  extra       INTEGER NOT NULL DEFAULT 0,
  watchlist   INTEGER NOT NULL DEFAULT 0,
  user        INTEGER NOT NULL DEFAULT 0,
  excluded    INTEGER NOT NULL DEFAULT 0,
  priority    INTEGER NOT NULL DEFAULT 0,
  first_seen  INTEGER NOT NULL,
  last_seen   INTEGER NOT NULL,
  first_trade INTEGER
);

CREATE TABLE sources (
  id   INTEGER PRIMARY KEY,
  name TEXT NOT NULL UNIQUE
);

CREATE TABLE cursors (
  symbol_id    INTEGER NOT NULL,
  interval     TEXT NOT NULL,
  source_id    INTEGER NOT NULL,
  oldest       INTEGER,
  newest       INTEGER,
  complete     INTEGER NOT NULL DEFAULT 0,
  failures     INTEGER NOT NULL DEFAULT 0,
  next_attempt INTEGER NOT NULL DEFAULT 0,
  last_error   TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (symbol_id, interval, source_id)
);

CREATE TABLE days (
  symbol_id INTEGER NOT NULL,
  interval  TEXT NOT NULL,
  day       INTEGER NOT NULL,
  bars      INTEGER NOT NULL,
  sources   INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (symbol_id, interval, day)
) WITHOUT ROWID;

CREATE TABLE splits (
  symbol_id   INTEGER NOT NULL,
  ts          INTEGER NOT NULL,
  numerator   REAL NOT NULL,
  denominator REAL NOT NULL,
  state       TEXT NOT NULL DEFAULT 'pending',
  PRIMARY KEY (symbol_id, ts)
) WITHOUT ROWID;

CREATE TABLE dividends (
  symbol_id INTEGER NOT NULL,
  ts        INTEGER NOT NULL,
  amount    REAL NOT NULL,
  PRIMARY KEY (symbol_id, ts)
) WITHOUT ROWID;

CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`,
	},
	{
		// Renames. A company that changes its symbol — FB to META — leaves
		// its history under the old symbol's row; an alias says that row's
		// bars before `until` are the new symbol's too, so a read of META
		// includes the minute bars collected while it was still FB. It is a
		// link, not a merge: nothing is rewritten, and a later company given
		// the freed symbol keeps its own bars, which are all after `until`.
		ID: "002_aliases",
		SQL: `
CREATE TABLE aliases (
  symbol_id INTEGER NOT NULL,
  former    TEXT NOT NULL,
  until     INTEGER NOT NULL,
  PRIMARY KEY (symbol_id, former)
) WITHOUT ROWID;
`,
	},
}

// partitionMigrations shape each bars/<interval>/<year>.sqlite.
//
// The interval is the file, so it is not a column; the source is a one-byte
// ID. WITHOUT ROWID on (symbol_id, ts) makes the primary key the table: one
// symbol's year is contiguous on disk and reading it is one range scan.
//
// applied_splits records which splits this file's bars have been rescaled
// for, so a rescale interrupted between two files resumes on the files it
// hadn't reached and leaves the ones it had alone.
var partitionMigrations = []migration{
	{
		ID: "001_initial",
		SQL: `
CREATE TABLE bars (
  symbol_id INTEGER NOT NULL,
  ts        INTEGER NOT NULL,
  open      REAL NOT NULL,
  high      REAL NOT NULL,
  low       REAL NOT NULL,
  close     REAL NOT NULL,
  volume    INTEGER NOT NULL DEFAULT 0,
  source_id INTEGER NOT NULL,
  PRIMARY KEY (symbol_id, ts)
) WITHOUT ROWID;

CREATE TABLE applied_splits (
  symbol_id INTEGER NOT NULL,
  ts        INTEGER NOT NULL,
  PRIMARY KEY (symbol_id, ts)
) WITHOUT ROWID;
`,
	},
	{
		// What a bar says besides its OHLCV. `vwap` and `trades` are the
		// source's own, from every trade — NULL where it gave none, which
		// for Yahoo is always, so a NULL costs a byte rather than a zero
		// that reads as "no trades". `session` is 0 for the regular session,
		// which every existing bar is, 1 before the open and 2 after the
		// close; reads skip the others unless asked.
		ID: "002_vwap_trades_session",
		SQL: `
ALTER TABLE bars ADD COLUMN vwap REAL;
ALTER TABLE bars ADD COLUMN trades INTEGER;
ALTER TABLE bars ADD COLUMN session INTEGER NOT NULL DEFAULT 0;
`,
	},
}
