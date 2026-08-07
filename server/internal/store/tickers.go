package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Tickers lists the whole watchlist in display order — pinned symbols first.
func (s *Store) Tickers() ([]Ticker, error) {
	rows, err := s.db.Query(`
		SELECT id, symbol, label, position, enabled, origin, created_at, updated_at
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
		SELECT id, symbol, label, position, enabled, origin, created_at, updated_at
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
		SELECT id, symbol, label, position, enabled, origin, created_at, updated_at
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

// NewTicker is the input for adding a symbol to the watchlist.
type NewTicker struct {
	Symbol  string
	Label   string
	Enabled *bool // nil means enabled
}

// CreateTicker appends a symbol to the end of the watchlist.
func (s *Store) CreateTicker(in NewTicker) (Ticker, error) {
	sym := NormalizeSymbol(in.Symbol)
	if sym == "" {
		return Ticker{}, errors.New("symbol is required")
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
	_, err := s.db.Exec(`
		INSERT INTO tickers (id, symbol, label, position, enabled, origin, created_at, updated_at)
		VALUES (?, ?, ?, (SELECT COALESCE(MAX(position), -1) + 1 FROM tickers), ?, ?, ?, ?)`,
		id, sym, strings.TrimSpace(in.Label), boolInt(enabled), OriginUser, now, now)
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
	Symbol  *string
	Label   *string
	Enabled *bool
}

// UpdateTicker applies a patch.
//
// Changing the symbol does two extra things: it promotes the row out of `seed`
// origin (it is the user's choice now, not the shipped default), and it drops
// the stale quote, because a row showing the old symbol's price under the new
// symbol's name for the seconds until the next refresh is worse than showing
// nothing.
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

	sets := []string{"updated_at = ?"}
	args := []any{nowRFC3339()}
	symbolChanged := false

	if patch.Symbol != nil {
		sym := NormalizeSymbol(*patch.Symbol)
		if sym == "" {
			return Ticker{}, errors.New("symbol is required")
		}
		if sym != existing.Symbol {
			symbolChanged = true
			sets = append(sets, "symbol = ?", "origin = ?")
			args = append(args, sym, OriginUser)
		}
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
		return Ticker{}, fmt.Errorf("%w: %s", ErrDuplicateSymbol, NormalizeSymbol(*patch.Symbol))
	}
	if err != nil {
		return Ticker{}, err
	}

	if symbolChanged {
		if _, err := s.db.Exec(`DELETE FROM quotes WHERE ticker_id = ?`, id); err != nil {
			return Ticker{}, err
		}
	}
	return s.Ticker(id)
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
	if err := row.Scan(&t.ID, &t.Symbol, &t.Label, &t.Position, &enabled, &t.Origin, &createdAt, &updatedAt); err != nil {
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
