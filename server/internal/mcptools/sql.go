package mcptools

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/chinmay28/tickers/server/internal/archive"
	"github.com/chinmay28/tickers/server/internal/mcp"
	"github.com/chinmay28/tickers/server/internal/quotes"
)

// Bounds on a SQL query from an agent.
const (
	defaultSQLRows = 200
	sqlTimeout     = 30 * time.Second
)

func (t *tools) registerSQL(s *mcp.Server) {
	s.AddTool(mcp.Tool{
		Name:  "query_sql",
		Title: "Query the archive with SQL",
		Description: "Run one read-only SQLite SELECT over the archive, for any question the other tools don't ask: cross-sectional " +
			"aggregates, custom statistics, distributions, joins with dividends. Window functions (LAG, AVG() OVER …) and CTEs work. " +
			"interval picks which bars the bars view holds; from and to only pick which year files are attached for intraday " +
			"intervals (at most 9 years) — filter rows with WHERE. Aggregate in SQL rather than pulling raw rows: at most 10000 rows " +
			"come back, and a query gets 30 seconds.\n\nSchema:\n" + archive.SQLSchema,
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"sql":{"type":"string","description":"One SELECT or WITH … SELECT statement."},
			"interval":{"type":"string","enum":["1d","1h","5m","1m"],"description":"Default 1d."},
			"from":{"type":"string","description":"YYYY-MM-DD; intraday only. Default: the start of last year."},
			"to":{"type":"string","description":"YYYY-MM-DD; intraday only. Default: today."},
			"maxRows":{"type":"integer","minimum":1,"maximum":10000,"description":"Default 200."}
		},"required":["sql"],"additionalProperties":false}`),
		ReadOnly: true,
		Handler:  handler(t.sql),
	})
}

type sqlArgs struct {
	SQL      string `json:"sql"`
	Interval string `json:"interval"`
	From     string `json:"from"`
	To       string `json:"to"`
	MaxRows  int    `json:"maxRows"`
}

func (t *tools) sql(ctx context.Context, in sqlArgs) (any, error) {
	interval, err := parseInterval(in.Interval)
	if err != nil {
		return nil, err
	}
	now := t.now().UTC()
	from, to := time.Date(now.Year()-1, 1, 1, 0, 0, 0, 0, time.UTC), now
	if in.From != "" {
		if from, err = time.Parse(time.DateOnly, in.From); err != nil {
			return nil, errors.New("from must be a date like 2024-01-31")
		}
	}
	if in.To != "" {
		if to, err = time.Parse(time.DateOnly, in.To); err != nil {
			return nil, errors.New("to must be a date like 2024-12-31")
		}
	}
	if interval == quotes.Daily {
		from, to = now, now // one file whatever the window
	}
	ctx, cancel := context.WithTimeout(ctx, sqlTimeout)
	defer cancel()
	var res archive.SQLResult
	err = t.read(func(a *archive.Archive) (err error) {
		res, err = a.SQL(ctx, archive.SQLQuery{SQL: in.SQL, Interval: interval, From: from, To: to, MaxRows: clamp(in.MaxRows, defaultSQLRows, archive.MaxSQLRows)})
		return err
	})
	if err != nil {
		return nil, err
	}
	// Floats come back as SQLite computed them; a ratio of two prices has
	// sixteen digits, and none past the sixth say anything.
	for _, row := range res.Rows {
		for i, v := range row {
			if f, ok := v.(float64); ok {
				row[i] = round(f, 6)
			}
		}
	}
	out := struct {
		archive.SQLResult
		Count int    `json:"count"`
		Note  string `json:"note,omitempty"`
	}{SQLResult: res, Count: len(res.Rows)}
	if res.Truncated {
		out.Note = "more rows matched than were returned; aggregate in SQL or raise maxRows"
	}
	return out, nil
}
