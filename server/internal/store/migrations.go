package store

// migration is one forward-only schema step. Steps are applied in order and
// recorded in `schema_migrations`, so applying them twice is a no-op.
//
// THE LIST IS APPEND-ONLY. Never edit or reorder a migration that has shipped:
// a deployed database has already recorded it as applied and will skip it
// forever, so an edit only changes what *fresh* installs get — which is how
// two installs of the same version end up with different schemas. To change
// something, append a new migration that changes it.
//
// Every step must also be *additive* where it can be: an older binary rolled
// back onto a newer database (scripts/quickstart.sh does exactly that when an
// upgrade fails its health check) has to keep working, and it only will if the
// new schema is a superset of the old one.
type migration struct {
	ID  string
	SQL string
}

var migrations = []migration{
	{
		ID: "001_initial",
		SQL: `
CREATE TABLE tickers (
  id          TEXT PRIMARY KEY,
  symbol      TEXT NOT NULL,
  label       TEXT NOT NULL DEFAULT '',
  position    INTEGER NOT NULL,
  enabled     INTEGER NOT NULL DEFAULT 1,
  origin      TEXT NOT NULL DEFAULT 'user',
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);
CREATE UNIQUE INDEX tickers_symbol_idx ON tickers (symbol);

CREATE TABLE quotes (
  ticker_id      TEXT PRIMARY KEY REFERENCES tickers (id) ON DELETE CASCADE,
  symbol         TEXT NOT NULL,
  price          REAL,
  previous_close REAL,
  currency       TEXT NOT NULL DEFAULT '',
  short_name     TEXT NOT NULL DEFAULT '',
  market_state   TEXT NOT NULL DEFAULT '',
  status         TEXT NOT NULL,
  error          TEXT NOT NULL DEFAULT '',
  fetched_at     TEXT NOT NULL
);

CREATE TABLE quote_history (
  id     INTEGER PRIMARY KEY AUTOINCREMENT,
  symbol TEXT NOT NULL,
  price  REAL NOT NULL,
  at     TEXT NOT NULL
);
CREATE INDEX quote_history_symbol_at_idx ON quote_history (symbol, at);

CREATE TABLE sinks (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  base_url   TEXT NOT NULL,
  key        TEXT NOT NULL,
  category   TEXT NOT NULL DEFAULT '',
  format     TEXT NOT NULL DEFAULT 'minion',
  enabled    INTEGER NOT NULL DEFAULT 1,
  timeout_ms INTEGER NOT NULL DEFAULT 10000,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE runs (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  started_at   TEXT NOT NULL,
  finished_at  TEXT NOT NULL,
  trigger      TEXT NOT NULL,
  ok_count     INTEGER NOT NULL DEFAULT 0,
  error_count  INTEGER NOT NULL DEFAULT 0,
  publishes    TEXT NOT NULL DEFAULT '[]',
  error        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX runs_started_idx ON runs (started_at DESC);

CREATE TABLE settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`,
	},
	{
		// Pinning replaced the old "placeholder" chip: the seeded symbols used
		// to be flagged by `origin = 'seed'`, and are now the default contents
		// of the pinned list in Settings. This carries an existing install over
		// — whatever of the shipped watchlist it still has, in its current
		// order, becomes its initial pinned list.
		//
		// INSERT OR IGNORE, so an install that somehow already has the key
		// keeps it. The HAVING clause is what stops a watchlist with no seeded
		// rows left from writing a NULL: the aggregate always produces one row,
		// and this drops it when there was nothing to concatenate.
		ID: "002_pin_seeded_symbols",
		SQL: `
INSERT OR IGNORE INTO settings (key, value)
SELECT 'pinned_symbols', group_concat(symbol, ',')
  FROM (SELECT symbol FROM tickers WHERE origin = 'seed' ORDER BY position, symbol)
HAVING count(*) > 0;
`,
	},
	{
		// Composite tickers: a row whose price is computed from a formula over
		// other symbols ("VTI/GLD") rather than fetched. The column defaults to
		// the empty string, which is what every existing row means — "this is an
		// ordinary symbol" — so an older binary rolled back onto this database
		// still reads and writes the table happily; it just never sets the
		// column, and its composites-that-aren't stay plain tickers.
		ID: "003_composite_expression",
		SQL: `
ALTER TABLE tickers ADD COLUMN expression TEXT NOT NULL DEFAULT '';
`,
	},
	{
		// Portfolios: a saved allocation to backtest. A whole new table, so an
		// older binary rolled back onto this database simply never reads it —
		// the watchlist, the quotes and the published payload are untouched.
		//
		// The allocation is one JSON column rather than a child table. Nothing
		// joins a holding to anything (a portfolio leg is fetched like a
		// composite's leg, and need not be on the watchlist), nothing queries
		// across portfolios by symbol, and the whole list is written and read
		// as a unit — which is exactly the shape `runs.publishes` already is.
		ID: "004_portfolios",
		SQL: `
CREATE TABLE portfolios (
  id             TEXT PRIMARY KEY,
  name           TEXT NOT NULL,
  allocations    TEXT NOT NULL DEFAULT '[]',
  initial_amount REAL NOT NULL DEFAULT 10000,
  start_year     INTEGER NOT NULL DEFAULT 0,
  end_year       INTEGER NOT NULL DEFAULT 0,
  rebalance      TEXT NOT NULL DEFAULT 'annually',
  benchmark      TEXT NOT NULL DEFAULT '',
  position       INTEGER NOT NULL,
  created_at     TEXT NOT NULL,
  updated_at     TEXT NOT NULL
);
`,
	},
	{
		// Periodic contributions. Both columns default to "nothing is paid in",
		// which is what every existing portfolio means, so a binary rolled back
		// onto this database reads and writes the table exactly as before — it
		// just never sets them, and its portfolios stay lump-sum ones.
		ID: "005_portfolio_contributions",
		SQL: `
ALTER TABLE portfolios ADD COLUMN contribution REAL NOT NULL DEFAULT 0;
ALTER TABLE portfolios ADD COLUMN contribution_frequency TEXT NOT NULL DEFAULT 'none';
`,
	},
}
