package archive

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// List is one of the reasons a symbol is tracked.
type List string

const (
	// Listed symbols come from the exchange directories.
	Listed List = "listed"
	// Extra symbols come from configuration: crypto, indices, anything no
	// US exchange lists.
	Extra List = "extra"
	// Watchlist symbols are what the app itself prices: watchlist rows,
	// composite legs, portfolio holdings. They are always fetched first.
	Watchlist List = "watchlist"
	// User symbols were added by hand on the Data page, and are never
	// retired by a list changing under them.
	User List = "user"
)

func (l List) column() (string, error) {
	switch l {
	case Listed, Extra, Watchlist, User:
		return string(l), nil
	}
	return "", fmt.Errorf("unknown symbol list %q", l)
}

// Kinds of symbol. Informational: nothing schedules differently by kind.
const (
	KindStock = "stock"
	KindETF   = "etf"
	KindOther = "other"
)

// Entry is a symbol as a list hands it over.
type Entry struct {
	Symbol   string
	Name     string
	Exchange string
	Kind     string
}

// Symbol is a tracked, retired or excluded symbol.
type Symbol struct {
	ID       int64  `json:"id"`
	Symbol   string `json:"symbol"`
	Name     string `json:"name"`
	Exchange string `json:"exchange"`
	Kind     string `json:"kind"`
	Lists    []List `json:"lists"`
	Excluded bool   `json:"excluded"`
	// Marked is the hand-set priority flag; Priority is whether the symbol
	// goes first for any reason — that, or being on the watchlist or added
	// by hand.
	Marked    bool      `json:"marked"`
	Priority  bool      `json:"priority"`
	Active    bool      `json:"active"`
	FirstSeen time.Time `json:"firstSeen"`
	// FirstTrade is the provider's earliest date for it, or zero until a
	// fetch has said.
	FirstTrade time.Time `json:"firstTrade"`
}

// activeSQL is the one definition of "tracked". Everything that asks goes
// through it, so "on a list and not excluded" cannot drift between queries.
const activeSQL = `(excluded = 0 AND (listed + extra + watchlist + user) > 0)`

// priorityExpr is who goes first: what the app itself uses, what somebody
// added by hand, and what somebody marked.
const priorityExpr = `(watchlist + user + priority) > 0`

// NormalizeSymbol is the one spelling the archive stores: upper case, no
// surrounding space.
func NormalizeSymbol(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

// SetList makes list's members exactly entries: every entry joins it, and
// every symbol on it that isn't in entries leaves it. It reports how many
// joined and left. A symbol that leaves its last list is retired, not
// deleted.
//
// The caller is trusted to pass the *whole* list. See the collector for the
// guard against believing a list that came back short.
func (a *Archive) SetList(list List, entries []Entry, now time.Time) (joined, left int, err error) {
	col, err := list.column()
	if err != nil {
		return 0, 0, err
	}
	tx, err := a.catalog.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`CREATE TEMP TABLE IF NOT EXISTS keep (symbol TEXT PRIMARY KEY)`); err != nil {
		return 0, 0, err
	}
	if _, err := tx.Exec(`DELETE FROM keep`); err != nil {
		return 0, 0, err
	}
	for _, e := range entries {
		symbol := NormalizeSymbol(e.Symbol)
		if symbol == "" {
			return 0, 0, errors.New("a tracked symbol cannot be blank")
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO keep (symbol) VALUES (?)`, symbol); err != nil {
			return 0, 0, err
		}
		n, err := upsertMember(tx, col, symbol, e, now)
		if err != nil {
			return 0, 0, err
		}
		joined += n
	}
	res, err := tx.Exec(`UPDATE symbols SET ` + col + ` = 0 WHERE ` + col + ` = 1 AND symbol NOT IN (SELECT symbol FROM keep)`)
	if err != nil {
		return 0, 0, err
	}
	n, _ := res.RowsAffected()
	return joined, int(n), tx.Commit()
}

// upsertMember puts a symbol on a list, reporting 1 if it wasn't on it.
func upsertMember(tx *sql.Tx, col, symbol string, e Entry, now time.Time) (int, error) {
	var was int
	err := tx.QueryRow(`SELECT `+col+` FROM symbols WHERE symbol = ?`, symbol).Scan(&was)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	kind := e.Kind
	if kind == "" {
		kind = KindOther
	}
	// A name or an exchange only ever improves: a list that doesn't know one
	// (the watchlist) never blanks what a list that does (the directory) said.
	if _, err := tx.Exec(`INSERT INTO symbols (symbol, name, exchange, kind, `+col+`, first_seen, last_seen)
		VALUES (?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT (symbol) DO UPDATE SET
		  name = CASE WHEN excluded.name <> '' THEN excluded.name ELSE symbols.name END,
		  exchange = CASE WHEN excluded.exchange <> '' THEN excluded.exchange ELSE symbols.exchange END,
		  kind = CASE WHEN excluded.kind <> 'other' OR symbols.kind = '' THEN excluded.kind ELSE symbols.kind END,
		  `+col+` = 1, last_seen = excluded.last_seen`,
		symbol, e.Name, e.Exchange, kind, now.Unix(), now.Unix()); err != nil {
		return 0, fmt.Errorf("track %s: %w", symbol, err)
	}
	if was == 1 {
		return 0, nil
	}
	return 1, nil
}

// Add puts one symbol on a list without touching the list's other members.
func (a *Archive) Add(list List, e Entry, now time.Time) error {
	col, err := list.column()
	if err != nil {
		return err
	}
	symbol := NormalizeSymbol(e.Symbol)
	if symbol == "" {
		return errors.New("a symbol is required")
	}
	tx, err := a.catalog.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := upsertMember(tx, col, symbol, e, now); err != nil {
		return err
	}
	return tx.Commit()
}

// Remove takes one symbol off a list. Removing a symbol that isn't on it is
// not an error.
func (a *Archive) Remove(list List, symbol string) error {
	col, err := list.column()
	if err != nil {
		return err
	}
	_, err = a.catalog.Exec(`UPDATE symbols SET `+col+` = 0 WHERE symbol = ?`, NormalizeSymbol(symbol))
	return err
}

// ErrUnknownSymbol is returned for a symbol the archive has never seen.
var ErrUnknownSymbol = errors.New("that symbol is not in the archive")

// SetExcluded stops (or resumes) collecting a symbol whatever lists it is on.
// Its history is kept either way.
func (a *Archive) SetExcluded(symbol string, excluded bool) error {
	return a.setFlag(symbol, "excluded", excluded)
}

// SetPriority moves a symbol to the front of the queue, or back.
func (a *Archive) SetPriority(symbol string, priority bool) error {
	return a.setFlag(symbol, "priority", priority)
}

func (a *Archive) setFlag(symbol, col string, on bool) error {
	res, err := a.catalog.Exec(`UPDATE symbols SET `+col+` = ? WHERE symbol = ?`, on, NormalizeSymbol(symbol))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrUnknownSymbol
	}
	return nil
}

const symbolColumns = `id, symbol, name, exchange, kind, listed, extra, watchlist, user, excluded, priority,
	` + activeSQL + `, ` + priorityExpr + `, first_seen, first_trade`

func scanSymbol(row interface{ Scan(...any) error }) (Symbol, error) {
	var s Symbol
	var listed, extra, watchlist, user bool
	var firstSeen int64
	var firstTrade sql.NullInt64
	if err := row.Scan(&s.ID, &s.Symbol, &s.Name, &s.Exchange, &s.Kind, &listed, &extra, &watchlist, &user,
		&s.Excluded, &s.Marked, &s.Active, &s.Priority, &firstSeen, &firstTrade); err != nil {
		return s, err
	}
	s.Lists = []List{}
	for _, m := range []struct {
		on   bool
		list List
	}{{listed, Listed}, {extra, Extra}, {watchlist, Watchlist}, {user, User}} {
		if m.on {
			s.Lists = append(s.Lists, m.list)
		}
	}
	s.FirstSeen = time.Unix(firstSeen, 0).UTC()
	s.FirstTrade = fromNull(firstTrade)
	return s, nil
}

// ActiveSymbols lists every symbol the collector should fetch.
func (a *Archive) ActiveSymbols() ([]Symbol, error) {
	rows, err := a.catalog.Query(`SELECT ` + symbolColumns + ` FROM symbols WHERE ` + activeSQL + ` ORDER BY symbol`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Symbol
	for rows.Next() {
		s, err := scanSymbol(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Lookup returns one symbol, or ErrUnknownSymbol.
func (a *Archive) Lookup(symbol string) (Symbol, error) {
	s, err := scanSymbol(a.catalog.QueryRow(`SELECT `+symbolColumns+` FROM symbols WHERE symbol = ?`, NormalizeSymbol(symbol)))
	if errors.Is(err, sql.ErrNoRows) {
		return s, ErrUnknownSymbol
	}
	return s, err
}

// Filters for SymbolQuery.
const (
	FilterAll      = ""
	FilterActive   = "active"
	FilterPriority = "priority"
	FilterFailing  = "failing"
	FilterRetired  = "retired"
	FilterExcluded = "excluded"
	FilterUser     = "user"
)

// SymbolQuery is a page of the symbol browser.
type SymbolQuery struct {
	// Text matches the start of a symbol or anywhere in a name.
	Text   string
	Filter string
	Kind   string
	Offset int
	Limit  int
}

// SymbolPage is one page of results and the total they came from.
type SymbolPage struct {
	Total   int      `json:"total"`
	Symbols []Symbol `json:"symbols"`
}

// MaxPage bounds a page of symbols.
const MaxPage = 200

// QuerySymbols pages through symbols for the browser.
func (a *Archive) QuerySymbols(q SymbolQuery) (SymbolPage, error) {
	var where []string
	var args []any
	if t := strings.TrimSpace(q.Text); t != "" {
		where = append(where, `(symbol LIKE ? ESCAPE '\' OR name LIKE ? ESCAPE '\')`)
		esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
		args = append(args, esc.Replace(strings.ToUpper(t))+"%", "%"+esc.Replace(t)+"%")
	}
	switch q.Filter {
	case FilterAll:
	case FilterActive:
		where = append(where, activeSQL)
	case FilterPriority:
		where = append(where, activeSQL+` AND `+priorityExpr)
	case FilterFailing:
		where = append(where, activeSQL+` AND id IN (SELECT symbol_id FROM cursors WHERE failures > 0)`)
	case FilterRetired:
		where = append(where, `excluded = 0 AND (listed + extra + watchlist + user) = 0`)
	case FilterExcluded:
		where = append(where, `excluded = 1`)
	case FilterUser:
		where = append(where, `user = 1`)
	default:
		return SymbolPage{}, fmt.Errorf("unknown filter %q", q.Filter)
	}
	if q.Kind != "" {
		where = append(where, `kind = ?`)
		args = append(args, q.Kind)
	}
	cond := ""
	if len(where) > 0 {
		cond = " WHERE " + strings.Join(where, " AND ")
	}
	limit := q.Limit
	if limit <= 0 || limit > MaxPage {
		limit = MaxPage
	}

	var page SymbolPage
	if err := a.catalog.QueryRow(`SELECT count(*) FROM symbols`+cond, args...).Scan(&page.Total); err != nil {
		return page, err
	}
	rows, err := a.catalog.Query(`SELECT `+symbolColumns+` FROM symbols`+cond+
		` ORDER BY `+priorityExpr+` DESC, symbol LIMIT ? OFFSET ?`, append(args, limit, max(q.Offset, 0))...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	page.Symbols = []Symbol{}
	for rows.Next() {
		s, err := scanSymbol(rows)
		if err != nil {
			return page, err
		}
		page.Symbols = append(page.Symbols, s)
	}
	return page, rows.Err()
}
