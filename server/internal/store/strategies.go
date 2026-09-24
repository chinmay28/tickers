package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Strategy is a saved trading strategy: a name and its rules.
//
// The rules are kept as the JSON the editor sent. What they *mean* — whether
// "rsi:14 crosses_above 70" is a condition that can run — is the strategy
// package's to decide, and engine asks it before anything is saved; store
// sits below the packages that know about indicators, so it checks what it
// can without them: that there is a name, and that the rules are a JSON
// object of a sensible size.
type Strategy struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Definition json.RawMessage `json:"definition"`
	CreatedAt  time.Time       `json:"createdAt"`
	UpdatedAt  time.Time       `json:"updatedAt"`
}

// Bounds on a saved strategy.
const (
	MaxStrategyName       = 80
	MaxStrategyDefinition = 16 << 10
	MaxStrategies         = 200
)

func validStrategy(name string, def json.RawMessage) error {
	if name == "" {
		return errors.New("a strategy name is required")
	}
	if len(name) > MaxStrategyName {
		return fmt.Errorf("a strategy name cannot be longer than %d characters", MaxStrategyName)
	}
	if len(def) > MaxStrategyDefinition {
		return fmt.Errorf("a strategy cannot be larger than %d KB", MaxStrategyDefinition>>10)
	}
	var obj map[string]any
	if err := json.Unmarshal(def, &obj); err != nil || obj == nil {
		return errors.New("a strategy's rules must be a JSON object")
	}
	return nil
}

// Strategies lists every saved strategy, alphabetically.
func (s *Store) Strategies() ([]Strategy, error) {
	rows, err := s.db.Query(`SELECT id, name, definition, created_at, updated_at FROM strategies ORDER BY name COLLATE NOCASE, created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Strategy{}
	for rows.Next() {
		var st Strategy
		var def, created, updated string
		if err := rows.Scan(&st.ID, &st.Name, &def, &created, &updated); err != nil {
			return nil, err
		}
		st.Definition = json.RawMessage(def)
		st.CreatedAt, st.UpdatedAt = parseTime(created), parseTime(updated)
		out = append(out, st)
	}
	return out, rows.Err()
}

// CreateStrategy saves a new strategy.
func (s *Store) CreateStrategy(name string, def json.RawMessage) (Strategy, error) {
	name = strings.TrimSpace(name)
	if err := validStrategy(name, def); err != nil {
		return Strategy{}, err
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM strategies`).Scan(&n); err != nil {
		return Strategy{}, err
	}
	if n >= MaxStrategies {
		return Strategy{}, fmt.Errorf("there cannot be more than %d saved strategies", MaxStrategies)
	}
	now := nowRFC3339()
	st := Strategy{ID: newID(), Name: name, Definition: compact(def)}
	if _, err := s.db.Exec(`INSERT INTO strategies (id, name, definition, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		st.ID, st.Name, string(st.Definition), now, now); err != nil {
		return Strategy{}, err
	}
	st.CreatedAt, st.UpdatedAt = parseTime(now), parseTime(now)
	return st, nil
}

// UpdateStrategy replaces a strategy's name and rules.
func (s *Store) UpdateStrategy(id, name string, def json.RawMessage) (Strategy, error) {
	name = strings.TrimSpace(name)
	if err := validStrategy(name, def); err != nil {
		return Strategy{}, err
	}
	now := nowRFC3339()
	res, err := s.db.Exec(`UPDATE strategies SET name = ?, definition = ?, updated_at = ? WHERE id = ?`,
		name, string(compact(def)), now, id)
	if err != nil {
		return Strategy{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Strategy{}, ErrNotFound
	}
	all, err := s.Strategies()
	if err != nil {
		return Strategy{}, err
	}
	for _, st := range all {
		if st.ID == id {
			return st, nil
		}
	}
	return Strategy{}, ErrNotFound
}

// DeleteStrategy removes a strategy.
func (s *Store) DeleteStrategy(id string) error {
	res, err := s.db.Exec(`DELETE FROM strategies WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func compact(raw json.RawMessage) json.RawMessage {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return raw
	}
	return buf.Bytes()
}
