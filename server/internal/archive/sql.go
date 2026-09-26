package archive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chinmay28/tickers/server/internal/quotes"
)

// SQLQuery is one read-only SQL statement over the archive, for a caller
// that would rather ask its own question than find a tool that asks it.
type SQLQuery struct {
	SQL string
	// Interval picks the bars the `bars` view reads, and From and To pick
	// which of its year files are attached — an intraday interval is a file
	// a year, and only so many can be attached at once.
	Interval quotes.Interval
	From, To time.Time
	// MaxRows bounds the answer; rows past it are dropped and Truncated set.
	MaxRows int
}

// SQLResult is what a query returned.
type SQLResult struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	Truncated bool     `json:"truncated,omitempty"`
}

// Bounds on a SQL query.
const (
	MaxSQLRows = 10000
	// maxSQLYears keeps the attached files under SQLite's limit of ten,
	// with the catalog as main.
	maxSQLYears = 9
)

// SQLSchema describes what a query can read, for whoever writes one.
const SQLSchema = `bars(symbol, ts, time, open, high, low, close, volume, vwap, trades, session, source_id)
  — the chosen interval's bars. ts is the bar's open in unix seconds (a daily bar's is midnight UTC of its date);
  time is that as text (YYYY-MM-DD for daily bars, YYYY-MM-DDTHH:MMZ intraday). session: 0 regular, 1 pre-market,
  2 after hours. vwap and trades are NULL where the source didn't give them. Prices are split-adjusted, not
  dividend-adjusted. A renamed symbol's bars before the rename are under its former symbol (see aliases).
dividends(symbol, ts, date, amount) and splits(symbol, ts, date, numerator, denominator) — per share, by ex-date.
symbols(id, symbol, name, exchange, kind, listed, extra, watchlist, user, excluded, priority, first_seen, last_seen, first_trade)
  — every symbol the archive has known, including delisted ones (listed = 0): use it to avoid survivorship bias.
cursors(symbol_id, interval, source_id, oldest, newest, complete, failures, next_attempt, last_error) — how far each
  series has been collected. aliases(symbol_id, former, until) — renames. days(symbol_id, interval, day, bars, sources)
  — which days are held (day is unix days).`

// ErrBadSQL is a statement that isn't a single read-only query.
var ErrBadSQL = errors.New("the statement must be one SELECT (or WITH … SELECT) query")

// SQL runs one read-only query over the archive's catalog and the bars of
// one interval.
//
// It gets a connection of its own rather than a reader from the pools: the
// bar files have to be ATTACHed to it, and views made over them, which is
// state no pooled connection should keep. Every file is opened read-only and
// the connection is switched to query_only before the caller's statement
// runs, so the one statement that gets through the keyword check still
// can't write. The caller's context bounds it; SQLite is interrupted when it
// ends.
func (a *Archive) SQL(ctx context.Context, q SQLQuery) (SQLResult, error) {
	stmt, err := checkSQL(q.SQL)
	if err != nil {
		return SQLResult{}, err
	}
	var keys []string
	for _, k := range partitionKeys(q.Interval, q.From, q.To) {
		if _, err := os.Stat(a.partitionPath(k)); err == nil {
			keys = append(keys, k)
		}
	}
	if len(keys) > maxSQLYears {
		return SQLResult{}, fmt.Errorf("that window spans %d years of %s files; SQL can read at most %d at once — narrow it", len(keys), q.Interval, maxSQLYears)
	}

	db, err := sql.Open("sqlite", "file:"+a.catalogPath()+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		return SQLResult{}, err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return SQLResult{}, err
	}
	defer conn.Close()

	var parts []string
	for i, k := range keys {
		alias := fmt.Sprintf("p%d", i)
		if _, err := conn.ExecContext(ctx, `ATTACH ? AS `+alias, "file:"+a.partitionPath(k)+"?mode=ro"); err != nil {
			return SQLResult{}, err
		}
		cols, err := tableColumns(ctx, conn, alias, "bars")
		if err != nil {
			return SQLResult{}, err
		}
		// A partition no writer has opened since a column was added lacks
		// it; the view reads NULL there rather than failing.
		pick := func(c string) string {
			if cols[c] {
				return c
			}
			return "NULL AS " + c
		}
		parts = append(parts, fmt.Sprintf(`SELECT symbol_id, ts, open, high, low, close, volume, %s, %s, %s, source_id FROM %s.bars`,
			pick("vwap"), pick("trades"), "COALESCE("+pick("session")+", 0) AS session", alias))
	}
	timeExpr := `date(b.ts, 'unixepoch')`
	if q.Interval.Intraday() {
		timeExpr = `strftime('%Y-%m-%dT%H:%MZ', b.ts, 'unixepoch')`
	}
	source := `SELECT NULL AS symbol_id, NULL AS ts, NULL AS open, NULL AS high, NULL AS low, NULL AS close, NULL AS volume,
		NULL AS vwap, NULL AS trades, NULL AS session, NULL AS source_id WHERE 0`
	if len(parts) > 0 {
		source = strings.Join(parts, " UNION ALL ")
	}
	views := []string{
		`CREATE TEMP VIEW bars AS SELECT s.symbol AS symbol, b.ts AS ts, ` + timeExpr + ` AS time, b.open AS open, b.high AS high,
			b.low AS low, b.close AS close, b.volume AS volume, b.vwap AS vwap, b.trades AS trades, b.session AS session,
			b.source_id AS source_id FROM (` + source + `) b JOIN main.symbols s ON s.id = b.symbol_id`,
		// Shadowing the catalog's own tables by name, so a query reads
		// symbols rather than joining ids back to them every time.
		`CREATE TEMP VIEW dividends AS SELECT s.symbol AS symbol, d.ts AS ts, date(d.ts, 'unixepoch') AS date, d.amount AS amount
			FROM main.dividends d JOIN main.symbols s ON s.id = d.symbol_id`,
		`CREATE TEMP VIEW splits AS SELECT s.symbol AS symbol, x.ts AS ts, date(x.ts, 'unixepoch') AS date,
			x.numerator AS numerator, x.denominator AS denominator FROM main.splits x JOIN main.symbols s ON s.id = x.symbol_id`,
		`PRAGMA query_only = 1`,
	}
	for _, v := range views {
		if _, err := conn.ExecContext(ctx, v); err != nil {
			return SQLResult{}, err
		}
	}

	rows, err := conn.QueryContext(ctx, stmt)
	if err != nil {
		return SQLResult{}, sqlError(ctx, err)
	}
	defer rows.Close()
	res := SQLResult{Rows: [][]any{}}
	if res.Columns, err = rows.Columns(); err != nil {
		return res, err
	}
	limit := q.MaxRows
	if limit <= 0 || limit > MaxSQLRows {
		limit = MaxSQLRows
	}
	for rows.Next() {
		if len(res.Rows) == limit {
			res.Truncated = true
			break
		}
		vals := make([]any, len(res.Columns))
		ptrs := make([]any, len(vals))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return res, err
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = string(b)
			}
		}
		res.Rows = append(res.Rows, vals)
	}
	if err := rows.Err(); err != nil {
		return res, sqlError(ctx, err)
	}
	return res, nil
}

func (a *Archive) catalogPath() string { return filepath.Join(a.root, "catalog.sqlite") }

// checkSQL admits one SELECT or WITH statement. It is a courtesy, not the
// guard — query_only and read-only files are — but it turns an ATTACH or a
// second statement into a sentence rather than a SQLite error.
func checkSQL(raw string) (string, error) {
	stmt := strings.TrimSpace(raw)
	stmt = strings.TrimSpace(strings.TrimSuffix(stmt, ";"))
	if stmt == "" {
		return "", ErrBadSQL
	}
	if strings.Contains(stmt, ";") {
		return "", fmt.Errorf("%w; it has more than one (or a semicolon in a string, which isn't allowed)", ErrBadSQL)
	}
	first := strings.ToLower(strings.Fields(stmt)[0])
	if first != "select" && first != "with" {
		return "", ErrBadSQL
	}
	if strings.Contains(strings.ToLower(stmt), "load_extension") {
		return "", ErrBadSQL
	}
	return stmt, nil
}

func tableColumns(ctx context.Context, conn *sql.Conn, schema, table string) (map[string]bool, error) {
	rows, err := conn.QueryContext(ctx, fmt.Sprintf(`SELECT name FROM pragma_table_info('%s', '%s')`, table, schema))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

// sqlError says a timeout is a timeout: SQLite reports an interrupt as a
// bare "interrupted", which reads as a fault rather than a limit.
func sqlError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("the query ran out of time (%v); narrow the window, filter by symbol, or aggregate", ctx.Err())
	}
	return err
}
