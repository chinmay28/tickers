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
}
