package store

import (
	"database/sql"
	"errors"
	"net/url"
	"strings"
)

// Sinks lists every configured publish target, newest last.
func (s *Store) Sinks() ([]Sink, error) {
	rows, err := s.db.Query(`
		SELECT id, name, base_url, key, category, format, enabled, timeout_ms, created_at, updated_at
		FROM sinks ORDER BY created_at, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Sink{}
	for rows.Next() {
		sk, err := scanSink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sk)
	}
	return out, rows.Err()
}

// EnabledSinks is what a publish cycle actually posts to.
func (s *Store) EnabledSinks() ([]Sink, error) {
	all, err := s.Sinks()
	if err != nil {
		return nil, err
	}
	out := []Sink{}
	for _, sk := range all {
		if sk.Enabled {
			out = append(out, sk)
		}
	}
	return out, nil
}

// Sink looks one up by ID.
func (s *Store) Sink(id string) (Sink, error) {
	row := s.db.QueryRow(`
		SELECT id, name, base_url, key, category, format, enabled, timeout_ms, created_at, updated_at
		FROM sinks WHERE id = ?`, id)
	sk, err := scanSink(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Sink{}, ErrNotFound
	}
	return sk, err
}

// NewSink is the input for adding a publish target.
type NewSink struct {
	Name      string
	BaseURL   string
	Key       string
	Category  string
	Format    string
	Enabled   *bool
	TimeoutMS int
}

// CreateSink validates and stores a publish target.
func (s *Store) CreateSink(in NewSink) (Sink, error) {
	sk := Sink{
		ID:        newID(),
		Name:      strings.TrimSpace(in.Name),
		BaseURL:   strings.TrimRight(strings.TrimSpace(in.BaseURL), "/"),
		Key:       strings.TrimSpace(in.Key),
		Category:  strings.TrimSpace(in.Category),
		Format:    strings.TrimSpace(in.Format),
		Enabled:   in.Enabled == nil || *in.Enabled,
		TimeoutMS: in.TimeoutMS,
	}
	if sk.Format == "" {
		sk.Format = FormatMinion
	}
	if sk.TimeoutMS <= 0 {
		sk.TimeoutMS = 10000
	}
	if sk.Name == "" {
		sk.Name = sk.Key
	}
	if err := ValidateSink(sk); err != nil {
		return Sink{}, err
	}

	now := nowRFC3339()
	if _, err := s.db.Exec(`
		INSERT INTO sinks (id, name, base_url, key, category, format, enabled, timeout_ms, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sk.ID, sk.Name, sk.BaseURL, sk.Key, sk.Category, sk.Format,
		boolInt(sk.Enabled), sk.TimeoutMS, now, now); err != nil {
		return Sink{}, err
	}
	return s.Sink(sk.ID)
}

// SinkPatch is a partial update; a nil field is left alone.
type SinkPatch struct {
	Name      *string
	BaseURL   *string
	Key       *string
	Category  *string
	Format    *string
	Enabled   *bool
	TimeoutMS *int
}

// UpdateSink applies a patch, validating the result as a whole rather than
// field by field — a base URL and a key are only meaningful together.
func (s *Store) UpdateSink(id string, patch SinkPatch) (Sink, error) {
	sk, err := s.Sink(id)
	if err != nil {
		return Sink{}, err
	}
	if patch.Name != nil {
		sk.Name = strings.TrimSpace(*patch.Name)
	}
	if patch.BaseURL != nil {
		sk.BaseURL = strings.TrimRight(strings.TrimSpace(*patch.BaseURL), "/")
	}
	if patch.Key != nil {
		sk.Key = strings.TrimSpace(*patch.Key)
	}
	if patch.Category != nil {
		sk.Category = strings.TrimSpace(*patch.Category)
	}
	if patch.Format != nil {
		sk.Format = strings.TrimSpace(*patch.Format)
	}
	if patch.Enabled != nil {
		sk.Enabled = *patch.Enabled
	}
	if patch.TimeoutMS != nil && *patch.TimeoutMS > 0 {
		sk.TimeoutMS = *patch.TimeoutMS
	}
	if sk.Name == "" {
		sk.Name = sk.Key
	}
	if err := ValidateSink(sk); err != nil {
		return Sink{}, err
	}

	if _, err := s.db.Exec(`
		UPDATE sinks SET name = ?, base_url = ?, key = ?, category = ?, format = ?,
		                 enabled = ?, timeout_ms = ?, updated_at = ?
		WHERE id = ?`,
		sk.Name, sk.BaseURL, sk.Key, sk.Category, sk.Format,
		boolInt(sk.Enabled), sk.TimeoutMS, nowRFC3339(), id); err != nil {
		return Sink{}, err
	}
	return s.Sink(id)
}

// DeleteSink removes a publish target.
func (s *Store) DeleteSink(id string) error {
	res, err := s.db.Exec(`DELETE FROM sinks WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ValidateSink rejects a target that could not possibly work.
//
// The scheme check is the one that earns its keep: the server POSTs whatever
// is configured here, so restricting it to http/https keeps a typo (or a
// hostile settings payload) from turning the publisher into a file:// or
// gopher:// client.
func ValidateSink(sk Sink) error {
	if sk.BaseURL == "" {
		return errors.New("baseUrl is required (e.g. http://homeapi.local:9999/api/entries)")
	}
	u, err := url.Parse(sk.BaseURL)
	if err != nil {
		return errors.New("baseUrl is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("baseUrl must be an http:// or https:// URL")
	}
	if u.Host == "" {
		return errors.New("baseUrl needs a host")
	}
	if sk.Key == "" {
		return errors.New("key is required (the entry name to write, e.g. minion-quotes)")
	}
	if strings.ContainsAny(sk.Key, "/?#") {
		return errors.New("key cannot contain '/', '?' or '#'")
	}
	switch sk.Format {
	case FormatMinion, FormatDetailed:
	default:
		return errors.New(`format must be "minion" or "detailed"`)
	}
	return nil
}

func scanSink(row scannable) (Sink, error) {
	var (
		sk                   Sink
		enabled              int
		createdAt, updatedAt string
	)
	if err := row.Scan(&sk.ID, &sk.Name, &sk.BaseURL, &sk.Key, &sk.Category, &sk.Format,
		&enabled, &sk.TimeoutMS, &createdAt, &updatedAt); err != nil {
		return Sink{}, err
	}
	sk.Enabled = enabled != 0
	sk.CreatedAt = parseTime(createdAt)
	sk.UpdatedAt = parseTime(updatedAt)
	return sk, nil
}
