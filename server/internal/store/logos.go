package store

import (
	"database/sql"
	"errors"
	"time"
)

// What a logo row records. A symbol is asked about once: either an image came
// back, or the source said there isn't one. Both are answers, and both are
// stored, because the alternative is asking again every cycle for the rest of
// the install's life.
const (
	LogoOK   = "ok"
	LogoNone = "none"
)

// MaxLogoBytes bounds one cached image. Logos are small — a 150px PNG is a few
// kilobytes — and the cap is what stops a source that answers a logo request
// with a video from filling a Raspberry Pi's SD card.
const MaxLogoBytes = 256 * 1024

// Logo is one cached image, or the record that a symbol hasn't got one.
type Logo struct {
	Symbol      string `json:"symbol"`
	Status      string `json:"status"`
	ContentType string `json:"contentType"`
	Bytes       []byte `json:"-"`
	Source      string `json:"source"`
	// Reason is why a symbol has no logo, in the provider's own words. It is
	// the difference between "this fund hasn't got one" and "the URL you
	// configured answers 404 for everything", which the Settings page shows
	// rather than leaving to be dug out of the database.
	Reason    string    `json:"reason"`
	FetchedAt time.Time `json:"fetchedAt"`
}

// SaveLogo records what the source said about a symbol, image or not.
func (s *Store) SaveLogo(l Logo) error {
	symbol := NormalizeSymbol(l.Symbol)
	if symbol == "" {
		return errors.New("a symbol is required")
	}
	if l.Status != LogoOK && l.Status != LogoNone {
		return errors.New(`logo status has to be "ok" or "none"`)
	}
	if l.Status == LogoOK {
		if len(l.Bytes) == 0 {
			return errors.New("a stored logo needs its image bytes")
		}
		if len(l.Bytes) > MaxLogoBytes {
			return errors.New("that image is too large to cache")
		}
		if l.ContentType == "" {
			return errors.New("a stored logo needs its content type")
		}
	} else {
		// A "none" row is a tombstone; keeping a stale image beside it would
		// let the API serve something the source has since disowned.
		l.Bytes, l.ContentType = nil, ""
	}
	if l.FetchedAt.IsZero() {
		l.FetchedAt = time.Now().UTC()
	}

	_, err := s.db.Exec(`
		INSERT INTO logos (symbol, status, content_type, bytes, source, reason, fetched_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (symbol) DO UPDATE SET
		  status = excluded.status,
		  content_type = excluded.content_type,
		  bytes = excluded.bytes,
		  source = excluded.source,
		  reason = excluded.reason,
		  fetched_at = excluded.fetched_at`,
		symbol, l.Status, l.ContentType, l.Bytes, l.Source, l.Reason,
		l.FetchedAt.UTC().Format(time.RFC3339Nano))
	return err
}

// Logo returns one symbol's cached image. A symbol that was asked about and
// hasn't got one is ErrNotFound, the same as one that was never asked about:
// the caller's next move is identical either way.
func (s *Store) Logo(symbol string) (Logo, error) {
	var (
		l  Logo
		at string
	)
	err := s.db.QueryRow(`
		SELECT symbol, status, content_type, bytes, source, reason, fetched_at
		FROM logos WHERE symbol = ? AND status = ?`,
		NormalizeSymbol(symbol), LogoOK).
		Scan(&l.Symbol, &l.Status, &l.ContentType, &l.Bytes, &l.Source, &l.Reason, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return Logo{}, ErrNotFound
	}
	if err != nil {
		return Logo{}, err
	}
	l.FetchedAt = parseTime(at)
	return l, nil
}

// LogoSymbols is every symbol that has an image, for the client to key off.
//
// The alternative — an <img> per row that 404s for everything without a logo —
// costs a request per uncovered symbol on every load and puts a column of
// failures in the console. The set is small enough to ride along with the
// state payload the client already fetches.
func (s *Store) LogoSymbols() ([]string, error) {
	rows, err := s.db.Query(`SELECT symbol FROM logos WHERE status = ? ORDER BY symbol`, LogoOK)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var symbol string
		if err := rows.Scan(&symbol); err != nil {
			return nil, err
		}
		out = append(out, symbol)
	}
	return out, rows.Err()
}

// AskedAboutLogos is every symbol already answered for, image or not. The
// refresh cycle subtracts it from the symbols it just fetched to find the ones
// still worth a request.
func (s *Store) AskedAboutLogos() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT symbol FROM logos`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	asked := map[string]bool{}
	for rows.Next() {
		var symbol string
		if err := rows.Scan(&symbol); err != nil {
			return nil, err
		}
		asked[symbol] = true
	}
	return asked, rows.Err()
}

// ForgetLogos empties the cache, so the next cycles ask again. It is what
// turning the setting off is for — a cache of third-party images nobody wants
// any more should not outlive the feature that filled it.
func (s *Store) ForgetLogos() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM logos`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// LogoStats is what the Settings page reports back: how many symbols have an
// image, how many were answered "no", and the reason most of the noes gave.
//
// The reason is the useful part. A feature that fetches pictures and shows
// none looks identical whether this symbol simply hasn't got one or the
// configured URL answers 404 for everything, and without saying which, the
// only way to tell them apart is to open the database.
type LogoStats struct {
	OK   int `json:"ok"`
	None int `json:"none"`
	// Reason is the most common explanation among the noes, empty if there
	// aren't any.
	Reason string `json:"reason"`
}

// LogoStats counts the cache.
func (s *Store) LogoStats() (LogoStats, error) {
	var stats LogoStats
	if err := s.db.QueryRow(`
		SELECT
		  count(*) FILTER (WHERE status = ?),
		  count(*) FILTER (WHERE status = ?)
		FROM logos`, LogoOK, LogoNone).Scan(&stats.OK, &stats.None); err != nil {
		return stats, err
	}
	if stats.None == 0 {
		return stats, nil
	}

	var reason sql.NullString
	err := s.db.QueryRow(`
		SELECT reason FROM logos
		WHERE status = ? AND reason <> ''
		GROUP BY reason ORDER BY count(*) DESC LIMIT 1`, LogoNone).Scan(&reason)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return stats, err
	}
	stats.Reason = reason.String
	return stats, nil
}
