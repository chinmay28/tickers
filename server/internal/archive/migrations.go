package archive

// migration is one forward-only schema step, under the same rules as the main
// store's: THE LIST IS APPEND-ONLY, and every step must be additive, because a
// binary rolled back onto a newer archive has to keep collecting into it.
type migration struct {
	ID  string
	SQL string
}

var migrations = []migration{
	{
		// Times are unix seconds rather than the main store's RFC3339 text.
		// The main store has hundreds of rows and is read by people; this one
		// has hundreds of millions and is read by programs, and an integer is
		// a fifth of the bytes in both the row and the index.
		//
		// Bars are keyed by a small integer symbol ID for the same reason, and
		// are WITHOUT ROWID so the primary key *is* the table: one symbol's
		// series at one interval is contiguous on disk, and reading a year of
		// it is one range scan.
		ID: "001_initial",
		SQL: `
CREATE TABLE symbols (
  id          INTEGER PRIMARY KEY,
  symbol      TEXT NOT NULL UNIQUE,
  name        TEXT NOT NULL DEFAULT '',
  exchange    TEXT NOT NULL DEFAULT '',
  kind        TEXT NOT NULL DEFAULT '',
  active      INTEGER NOT NULL DEFAULT 1,
  first_seen  INTEGER NOT NULL,
  last_seen   INTEGER NOT NULL,
  first_trade INTEGER
);

CREATE TABLE bars (
  symbol_id INTEGER NOT NULL,
  interval  TEXT NOT NULL,
  ts        INTEGER NOT NULL,
  open      REAL NOT NULL,
  high      REAL NOT NULL,
  low       REAL NOT NULL,
  close     REAL NOT NULL,
  volume    INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (symbol_id, interval, ts)
) WITHOUT ROWID;

CREATE TABLE splits (
  symbol_id   INTEGER NOT NULL,
  ts          INTEGER NOT NULL,
  numerator   REAL NOT NULL,
  denominator REAL NOT NULL,
  PRIMARY KEY (symbol_id, ts)
) WITHOUT ROWID;

CREATE TABLE dividends (
  symbol_id INTEGER NOT NULL,
  ts        INTEGER NOT NULL,
  amount    REAL NOT NULL,
  PRIMARY KEY (symbol_id, ts)
) WITHOUT ROWID;

CREATE TABLE coverage (
  symbol_id    INTEGER NOT NULL,
  interval     TEXT NOT NULL,
  oldest       INTEGER,
  newest       INTEGER,
  complete     INTEGER NOT NULL DEFAULT 0,
  failures     INTEGER NOT NULL DEFAULT 0,
  next_attempt INTEGER NOT NULL DEFAULT 0,
  last_error   TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (symbol_id, interval)
);

CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`,
	},
}
