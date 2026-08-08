package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/chinmay28/tickers/server/internal/expr"
)

// tickerColumns is the one list of columns every ticker query selects, kept in
// one place so adding a field can't leave a scan and a select disagreeing.
const tickerColumns = `id, symbol, expression, portfolio_id, label, position, enabled, origin, created_at, updated_at`

// Tickers lists the whole watchlist in display order — pinned symbols first.
func (s *Store) Tickers() ([]Ticker, error) {
	rows, err := s.db.Query(`
		SELECT ` + tickerColumns + `
		FROM tickers ORDER BY position, symbol`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ts, err := scanTickers(rows)
	if err != nil {
		return nil, err
	}
	return s.pinFirst(ts)
}

// EnabledTickers is the watchlist the refresh loop actually fetches, in the
// same display order — which is also the payload's order.
func (s *Store) EnabledTickers() ([]Ticker, error) {
	rows, err := s.db.Query(`
		SELECT ` + tickerColumns + `
		FROM tickers WHERE enabled = 1 ORDER BY position, symbol`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ts, err := scanTickers(rows)
	if err != nil {
		return nil, err
	}
	return s.pinFirst(ts)
}

// Ticker looks one up by ID.
func (s *Store) Ticker(id string) (Ticker, error) {
	row := s.db.QueryRow(`
		SELECT `+tickerColumns+`
		FROM tickers WHERE id = ?`, id)
	t, err := scanTicker(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Ticker{}, ErrNotFound
	}
	if err != nil {
		return Ticker{}, err
	}
	pinned, err := s.PinnedSymbols()
	if err != nil {
		return Ticker{}, err
	}
	return applyPins([]Ticker{t}, pinned)[0], nil
}

// pinFirst stamps Pinned and lifts the pinned rows to the top.
func (s *Store) pinFirst(ts []Ticker) ([]Ticker, error) {
	pinned, err := s.PinnedSymbols()
	if err != nil {
		return nil, err
	}
	return applyPins(ts, pinned), nil
}

// applyPins marks every ticker whose symbol is pinned and moves those to the
// front.
//
// The sort is stable and keys on nothing but pinned-ness, so `position` still
// decides the order *within* each group: dragging a row still reorders it, and
// unpinning a symbol drops it straight back into the slot it would otherwise
// have had. That is why the setting is a set of symbols rather than an
// ordering — pinning answers "above the fold or not", and the watchlist
// answers "in what order".
func applyPins(ts []Ticker, pinned []string) []Ticker {
	isPinned := make(map[string]bool, len(pinned))
	for _, sym := range pinned {
		isPinned[sym] = true
	}
	for i := range ts {
		ts[i].Pinned = isPinned[ts[i].Symbol]
	}
	sort.SliceStable(ts, func(i, j int) bool { return ts[i].Pinned && !ts[j].Pinned })
	return ts
}

// NewTicker is the input for adding a row to the watchlist.
//
// Symbol and Expression are alternatives: give one or the other. Giving a
// Symbol that reads as a formula ("VTI/GLD") is the same as giving an
// Expression, which is what lets the add box take either without a mode switch.
type NewTicker struct {
	Symbol     string
	Expression string
	Label      string
	Enabled    *bool // nil means enabled
}

// CreateTicker appends a row to the end of the watchlist.
func (s *Store) CreateTicker(in NewTicker) (Ticker, error) {
	sym, formula, err := resolveIdentity(in.Symbol, in.Expression)
	if err != nil {
		return Ticker{}, err
	}

	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	now := nowRFC3339()
	id := newID()

	// Positions are dense-ish but never renumbered on insert: append past the
	// current maximum and let ReorderTickers be the only thing that rewrites
	// the sequence. COALESCE handles the empty-table case.
	_, err = s.db.Exec(`
		INSERT INTO tickers (id, symbol, expression, label, position, enabled, origin, created_at, updated_at)
		VALUES (?, ?, ?, ?, (SELECT COALESCE(MAX(position), -1) + 1 FROM tickers), ?, ?, ?, ?)`,
		id, sym, formula, strings.TrimSpace(in.Label), boolInt(enabled), OriginUser, now, now)
	if isUniqueViolation(err) {
		return Ticker{}, fmt.Errorf("%w: %s", ErrDuplicateSymbol, sym)
	}
	if err != nil {
		return Ticker{}, err
	}
	return s.Ticker(id)
}

// TickerPatch is a partial update; a nil field is left alone.
type TickerPatch struct {
	Symbol     *string
	Expression *string
	Label      *string
	Enabled    *bool
}

// UpdateTicker applies a patch.
//
// Changing what the row *is* — its symbol or its formula — does two extra
// things: it promotes the row out of `seed` origin (it is the user's choice
// now, not the shipped default), and it drops the stale quote, because a row
// showing the old symbol's price under the new symbol's name for the seconds
// until the next refresh is worse than showing nothing.
//
// It deliberately leaves the pinned list alone. Pins are configured in
// Settings and keyed by symbol, so retyping a pinned row's symbol unpins it as
// a side effect of the symbol no longer matching — which is the same thing
// that happens if you delete the row and add a different one.
func (s *Store) UpdateTicker(id string, patch TickerPatch) (Ticker, error) {
	existing, err := s.Ticker(id)
	if err != nil {
		return Ticker{}, err
	}

	// A portfolio's row is not the user's to re-aim: its symbol is the
	// portfolio's name and its value comes from the allocation, so retyping
	// either here would leave a row that says one thing and prices another.
	// The label is still theirs.
	if existing.IsPortfolio() && (patch.Symbol != nil || patch.Expression != nil) {
		return Ticker{}, errors.New("a portfolio's row cannot be re-pointed; edit the portfolio instead")
	}

	// Symbol and formula are resolved as a pair, because either one can decide
	// what the other becomes: clearing the formula turns a composite back into
	// a plain symbol, and typing a formula into the symbol field turns a plain
	// symbol into a composite.
	symbolIn, formulaIn := existing.Symbol, existing.Expression
	if patch.Symbol != nil {
		symbolIn = *patch.Symbol
	}
	if patch.Expression != nil {
		formulaIn = *patch.Expression
		if strings.TrimSpace(formulaIn) == "" && existing.IsComposite() && patch.Symbol == nil {
			// Otherwise the composite's own symbol ("VTI/GLD") would be re-read
			// as a formula and nothing would change — silently, which is worse
			// than saying what is missing.
			return Ticker{}, errors.New("a symbol is required to turn a composite back into an ordinary ticker")
		}
	}
	sym, formula, err := resolveIdentity(symbolIn, formulaIn)
	if err != nil {
		return Ticker{}, err
	}

	sets := []string{"updated_at = ?"}
	args := []any{nowRFC3339()}

	identityChanged := sym != existing.Symbol || formula != existing.Expression
	if identityChanged {
		sets = append(sets, "symbol = ?", "expression = ?", "origin = ?")
		args = append(args, sym, formula, OriginUser)
	}
	if patch.Label != nil {
		sets = append(sets, "label = ?")
		args = append(args, strings.TrimSpace(*patch.Label))
	}
	if patch.Enabled != nil {
		sets = append(sets, "enabled = ?")
		args = append(args, boolInt(*patch.Enabled))
	}

	args = append(args, id)
	_, err = s.db.Exec(`UPDATE tickers SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if isUniqueViolation(err) {
		return Ticker{}, fmt.Errorf("%w: %s", ErrDuplicateSymbol, sym)
	}
	if err != nil {
		return Ticker{}, err
	}

	if identityChanged {
		if _, err := s.db.Exec(`DELETE FROM quotes WHERE ticker_id = ?`, id); err != nil {
			return Ticker{}, err
		}
	}
	return s.Ticker(id)
}

// resolveIdentity works out what a row is from the two fields that can say so,
// and returns its stored (symbol, expression) pair.
//
// An empty expression whose symbol reads as a formula is promoted to a
// composite: someone typing "VTI/GLD" into the symbol box means the ratio, and
// there is no other thing they could mean — no provider has a symbol with a
// slash in it.
func resolveIdentity(symbol, expression string) (sym string, formula string, err error) {
	expression = strings.TrimSpace(expression)
	if expression == "" && expr.Looks(symbol) {
		expression = strings.TrimSpace(symbol)
	}

	if expression == "" {
		sym = NormalizeSymbol(symbol)
		if sym == "" {
			return "", "", errors.New("symbol is required")
		}
		return sym, "", nil
	}

	parsed, err := expr.Parse(expression)
	if err != nil {
		return "", "", fmt.Errorf("%w: %s", ErrInvalidExpression, err)
	}
	if parsed.Operators() == 0 {
		return "", "", fmt.Errorf("%w: a composite has to combine values, for example VTI/GLD", ErrInvalidExpression)
	}
	return parsed.Key(), parsed.String(), nil
}

// DeleteTicker removes a symbol from the watchlist. Its quote goes with it via
// ON DELETE CASCADE; the history rows are keyed by symbol and deliberately
// survive, so re-adding a symbol you removed by accident brings its chart back.
func (s *Store) DeleteTicker(id string) error {
	res, err := s.db.Exec(`DELETE FROM tickers WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ReorderTickers rewrites display positions to match the given ID order. IDs
// not mentioned keep their relative order after the ones that are, so a
// partial list (a drag on a filtered view) can't scramble the rest.
func (s *Store) ReorderTickers(ids []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := nowRFC3339()
	seen := map[string]bool{}
	pos := 0
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		res, err := tx.Exec(`UPDATE tickers SET position = ?, updated_at = ? WHERE id = ?`, pos, now, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: ticker %s", ErrNotFound, id)
		}
		pos++
	}

	rows, err := tx.Query(`SELECT id FROM tickers ORDER BY position, symbol`)
	if err != nil {
		return err
	}
	var rest []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		if !seen[id] {
			rest = append(rest, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range rest {
		if _, err := tx.Exec(`UPDATE tickers SET position = ? WHERE id = ?`, pos, id); err != nil {
			return err
		}
		pos++
	}
	return tx.Commit()
}

// ---------------------------------------------------------------------------

type scannable interface{ Scan(dest ...any) error }

func scanTicker(row scannable) (Ticker, error) {
	var (
		t                    Ticker
		enabled              int
		createdAt, updatedAt string
	)
	if err := row.Scan(&t.ID, &t.Symbol, &t.Expression, &t.PortfolioID, &t.Label, &t.Position,
		&enabled, &t.Origin, &createdAt, &updatedAt); err != nil {
		return Ticker{}, err
	}
	t.Enabled = enabled != 0
	t.CreatedAt = parseTime(createdAt)
	t.UpdatedAt = parseTime(updatedAt)
	return t, nil
}

func scanTickers(rows *sql.Rows) ([]Ticker, error) {
	out := []Ticker{}
	for rows.Next() {
		t, err := scanTicker(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
