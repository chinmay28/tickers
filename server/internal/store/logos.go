package store

import (
	"database/sql"
	"errors"
	"time"
)

// What a logo row records. A symbol is asked about, and either an image came
// back or the source said there isn't one. Both are answers, and both are
// stored, because the alternative is asking again every cycle.
const (
	LogoOK   = "ok"
	LogoNone = "none"
)

// Who put a logo here. The two have opposite lifetimes: a fetched one is a
// cache entry — it goes stale, it is re-asked, and it is dropped when the
// feature that fetched it is turned off — while an uploaded one is a file
// somebody chose, and every one of those behaviours would be data loss.
const (
	LogoFetched = "fetched"
	LogoCustom  = "custom"
)

// MaxLogoBytes bounds one image. Logos are small — a 150px PNG is a few
// kilobytes — and the cap is what stops a source that answers a logo request
// with a video, or an upload of somebody's holiday photo, from filling a
// Raspberry Pi's SD card.
const MaxLogoBytes = 256 * 1024

// Logo is one image, or the record that a symbol hasn't got one.
type Logo struct {
	Symbol      string `json:"symbol"`
	Status      string `json:"status"`
	Origin      string `json:"origin"`
	ContentType string `json:"contentType"`
	Bytes       []byte `json:"-"`
	Source      string `json:"source"`
	// Reason is why a symbol has no logo, in the provider's own words. It is
	// the difference between "this fund hasn't got one" and "the URL you
	// configured answers 404 for everything", which the Settings page shows
	// rather than leaving to be dug out of the database.
	Reason string `json:"reason"`
	// ETag and LastModified are what the source said about this image, sent
	// back on the next check so it can answer "unchanged" instead of resending
	// the bytes. They are the source's own opaque strings; nothing here reads
	// them, it only hands them back.
	ETag         string `json:"etag"`
	LastModified string `json:"lastModified"`
	// FetchedAt is when the source was last asked; UpdatedAt is when the image
	// itself last changed. They are the same on a fresh store and drift apart
	// on every re-check that finds nothing new — which is nearly all of them,
	// and is exactly the case where the version the client sees must not move.
	FetchedAt time.Time `json:"fetchedAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// SaveLogo records what a source said about a symbol, or stores an upload.
func (s *Store) SaveLogo(l Logo) error {
	symbol := NormalizeSymbol(l.Symbol)
	if symbol == "" {
		return errors.New("a symbol is required")
	}
	if l.Status != LogoOK && l.Status != LogoNone {
		return errors.New(`logo status has to be "ok" or "none"`)
	}
	if l.Origin == "" {
		l.Origin = LogoFetched
	}
	if l.Origin != LogoFetched && l.Origin != LogoCustom {
		return errors.New(`logo origin has to be "fetched" or "custom"`)
	}
	if l.Origin == LogoCustom && l.Status != LogoOK {
		// There is no such thing as uploading the absence of a picture, and a
		// custom row is never re-asked — a "none" one would be a symbol
		// permanently marked as having no logo, with no way back.
		return errors.New("an uploaded logo needs an image")
	}
	if l.Status == LogoOK {
		if len(l.Bytes) == 0 {
			return errors.New("a stored logo needs its image bytes")
		}
		if len(l.Bytes) > MaxLogoBytes {
			return errors.New("that image is too large to store")
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
	// Storing an image *is* the image changing: every caller of this either has
	// new bytes or is replacing them. A re-check that found nothing new calls
	// TouchLogo instead, precisely so it does not land here.
	l.UpdatedAt = l.FetchedAt

	_, err := s.db.Exec(`
		INSERT INTO logos (symbol, status, origin, content_type, bytes, source, reason,
		                   etag, last_modified, fetched_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (symbol) DO UPDATE SET
		  status = excluded.status,
		  origin = excluded.origin,
		  content_type = excluded.content_type,
		  bytes = excluded.bytes,
		  source = excluded.source,
		  reason = excluded.reason,
		  etag = excluded.etag,
		  last_modified = excluded.last_modified,
		  fetched_at = excluded.fetched_at,
		  updated_at = excluded.updated_at`,
		symbol, l.Status, l.Origin, l.ContentType, l.Bytes, l.Source, l.Reason,
		l.ETag, l.LastModified,
		l.FetchedAt.UTC().Format(time.RFC3339Nano),
		l.UpdatedAt.UTC().Format(time.RFC3339Nano))
	return err
}

// Logo returns one symbol's image. A symbol that was asked about and hasn't got
// one is ErrNotFound, the same as one never asked about: the caller's next move
// is identical either way.
func (s *Store) Logo(symbol string) (Logo, error) {
	var (
		l           Logo
		at, updated string
	)
	err := s.db.QueryRow(`
		SELECT symbol, status, origin, content_type, bytes, source, reason,
		       etag, last_modified, fetched_at, updated_at
		FROM logos WHERE symbol = ? AND status = ?`,
		NormalizeSymbol(symbol), LogoOK).
		Scan(&l.Symbol, &l.Status, &l.Origin, &l.ContentType, &l.Bytes, &l.Source, &l.Reason,
			&l.ETag, &l.LastModified, &at, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Logo{}, ErrNotFound
	}
	if err != nil {
		return Logo{}, err
	}
	l.FetchedAt = parseTime(at)
	l.UpdatedAt = parseTime(updated)
	return l, nil
}

// LogoMark is what the client is told about one symbol's image.
type LogoMark struct {
	// Version is the second the image last *changed*, and it goes in the URL:
	// the bytes are served with a day of browser caching, so without it a
	// replaced logo would keep showing the old one until tomorrow — and if it
	// moved on every re-check instead, every browser would re-download an
	// unchanged image daily.
	Version int64 `json:"v"`
	// Custom marks an upload. The UI needs it to know whether "remove" means
	// "delete my picture" or "throw away a cached one that will be back
	// tomorrow" — two different promises to make to somebody pressing a button.
	Custom bool `json:"custom"`
}

// LogoVersions is every symbol that has an image.
//
// It rides along with the state payload because the client cannot guess it: an
// <img> per symbol would 404 for every fund and crypto pair on every load.
func (s *Store) LogoVersions() (map[string]LogoMark, error) {
	rows, err := s.db.Query(`SELECT symbol, origin, updated_at FROM logos WHERE status = ?`, LogoOK)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]LogoMark{}
	for rows.Next() {
		var symbol, origin, at string
		if err := rows.Scan(&symbol, &origin, &at); err != nil {
			return nil, err
		}
		out[symbol] = LogoMark{Version: parseTime(at).Unix(), Custom: origin == LogoCustom}
	}
	return out, rows.Err()
}

// SettledLogos is every symbol the refresh cycle should leave alone: the
// uploaded ones, which are nobody's business but the operator's, and the
// fetched ones that are still fresh.
//
// `since` is the oldest a fetched answer may be. Everything older is asked
// about again — including the noes, which is what lets a wrong URL or an
// expired key heal on its own instead of needing the cache cleared by hand.
func (s *Store) SettledLogos(since time.Time) (map[string]bool, error) {
	rows, err := s.db.Query(`
		SELECT symbol FROM logos
		WHERE origin = ? OR fetched_at >= ?`,
		LogoCustom, since.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	settled := map[string]bool{}
	for rows.Next() {
		var symbol string
		if err := rows.Scan(&symbol); err != nil {
			return nil, err
		}
		settled[symbol] = true
	}
	return settled, rows.Err()
}

// LogoChecks is what to send when each symbol is next re-checked: the
// validators the last fetch left behind, and the bytes it stored.
//
// The bytes are here because not every source offers a validator. Against one
// that doesn't, "has this changed?" can still be answered — by fetching it and
// comparing — and the answer is worth having either way: an unchanged image
// should not rewrite a row, and a changed one should not be missed.
type LogoCheck struct {
	ETag         string
	LastModified string
	Bytes        []byte
}

// LogoChecks returns one entry per fetched row. Uploads are left out: they are
// never re-checked, so there is nothing to send.
func (s *Store) LogoChecks() (map[string]LogoCheck, error) {
	rows, err := s.db.Query(`
		SELECT symbol, etag, last_modified, bytes FROM logos WHERE origin = ?`, LogoFetched)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]LogoCheck{}
	for rows.Next() {
		var (
			symbol string
			check  LogoCheck
		)
		if err := rows.Scan(&symbol, &check.ETag, &check.LastModified, &check.Bytes); err != nil {
			return nil, err
		}
		out[symbol] = check
	}
	return out, rows.Err()
}

// TouchLogo records that a row was re-checked and found current, without
// rewriting the image.
//
// It is what a 304 — or a byte-identical re-fetch — comes to. The row's age is
// the only thing that moved, and that is exactly what has to move: it is what
// stops the symbol being asked about again tomorrow.
func (s *Store) TouchLogo(symbol string, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	res, err := s.db.Exec(`UPDATE logos SET fetched_at = ? WHERE symbol = ? AND origin = ?`,
		at.UTC().Format(time.RFC3339Nano), NormalizeSymbol(symbol), LogoFetched)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteLogo drops one symbol's row, whoever put it there. Removing an upload
// leaves the symbol to be fetched again, or drawn if fetching is off.
func (s *Store) DeleteLogo(symbol string) error {
	res, err := s.db.Exec(`DELETE FROM logos WHERE symbol = ?`, NormalizeSymbol(symbol))
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// ForgetLogos empties the *fetched* half of the cache, so the next cycles ask
// again. It is what turning the setting off, or changing the source, is for.
//
// Uploads are left alone deliberately. The argument for clearing is that a
// drawer of third-party images should not outlive the feature that filled it —
// an uploaded file is not a third party's, it is the operator's, and deleting
// it here would mean that turning a setting off silently destroyed their work.
func (s *Store) ForgetLogos() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM logos WHERE origin = ?`, LogoFetched)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// LogoStats is what the Settings page reports back: how many symbols have an
// image, how many were answered "no", how many were uploaded, and the reason
// most of the noes gave.
//
// The reason is the useful part. A feature that fetches pictures and shows
// none looks identical whether this symbol simply hasn't got one or the
// configured URL answers 404 for everything, and without saying which, the
// only way to tell them apart is to open the database.
type LogoStats struct {
	OK     int `json:"ok"`
	None   int `json:"none"`
	Custom int `json:"custom"`
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
		  count(*) FILTER (WHERE status = ?),
		  count(*) FILTER (WHERE origin = ?)
		FROM logos`, LogoOK, LogoNone, LogoCustom).
		Scan(&stats.OK, &stats.None, &stats.Custom); err != nil {
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
