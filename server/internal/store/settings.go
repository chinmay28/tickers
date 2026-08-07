package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Setting keys. Settings live in a key/value table rather than columns on a
// singleton row so adding one never needs a migration — which matters for a
// service whose upgrade story is "an older binary might be rolled back onto
// this database and has to keep running".
const (
	SettingRefreshSeconds   = "refresh_interval_seconds"
	SettingPublishOnRefresh = "publish_on_refresh"
	SettingHistoryHours     = "history_retention_hours"
	SettingQuoteBaseURL     = "quote_base_url"
	SettingQuoteTimeout     = "quote_timeout_seconds"
	SettingQuoteUserAgent   = "quote_user_agent"
	SettingPinnedSymbols    = "pinned_symbols"
	SettingSeeded           = "seeded"
)

// Bounds on the tunables. The refresh floor is the important one: Yahoo's
// endpoint is free and unauthenticated, and hammering it every few seconds is
// how a self-hosted watchlist gets its host rate-limited.
const (
	MinRefreshSeconds = 30
	MinQuoteTimeout   = 5
	MaxQuoteTimeout   = 120
	// A user agent long enough to be a paste accident rather than a browser
	// string; the field is free text otherwise.
	MaxUserAgentLen = 512
	// MaxPinnedSymbols bounds the pinned list. Pinning is a way to keep a
	// handful of symbols above the fold; pinning everything pins nothing, and
	// the cap is what stops a paste from turning the setting into a second
	// watchlist.
	MaxPinnedSymbols = 50
)

// Config is the tunable behaviour of the refresh loop and the quote source, as
// one value. Everything here is editable from the Settings page and takes
// effect on the next cycle — nothing in it needs a restart.
type Config struct {
	RefreshSeconds   int  `json:"refreshSeconds"`
	PublishOnRefresh bool `json:"publishOnRefresh"`
	HistoryHours     int  `json:"historyHours"`

	// QuoteBaseURL points the provider somewhere other than its default — a
	// caching proxy, a mirror, or a stand-in during testing. Empty means "use
	// whatever the server was started with", which is itself usually the
	// provider's own default.
	QuoteBaseURL string `json:"quoteBaseUrl"`
	// QuoteTimeoutSeconds bounds one upstream request. 0 means the default.
	QuoteTimeoutSeconds int `json:"quoteTimeoutSeconds"`
	// QuoteUserAgent overrides what the provider identifies itself as. Empty
	// means the default. Exposed because the string Yahoo accepts is a moving
	// target, and being able to change it without a redeploy is the difference
	// between a five-second fix and a rebuild.
	QuoteUserAgent string `json:"quoteUserAgent"`

	// PinnedSymbols are the symbols that sort above everything else on the
	// watchlist. It is a set, not an ordering — the watchlist's own order still
	// decides the sequence within the pinned group, so pinning never takes
	// drag-to-reorder away from a row.
	//
	// It holds *symbols* rather than ticker IDs on purpose: it survives
	// removing and re-adding a symbol, and it is something a person can read
	// and edit in a text field.
	//
	// A pinned symbol that isn't on the watchlist is simply inert, so deleting
	// a ticker never has to reach into settings to keep them consistent.
	PinnedSymbols []string `json:"pinnedSymbols"`
}

// DefaultConfig is what a fresh install runs with: a five-minute poll (the
// cadence the original script's cron entry used), publishing every cycle,
// three days of history behind the sparklines, and the provider's own
// defaults for everything about the upstream connection.
// The pinned list defaults to empty rather than to SeedSymbols: an empty
// stored value has to mean "nothing is pinned", or unpinning everything would
// read back as the shipped defaults on the next load. Fresh installs get their
// seeded symbols pinned by seed(); existing ones by migration 002.
func DefaultConfig() Config {
	return Config{RefreshSeconds: 300, PublishOnRefresh: true, HistoryHours: 72, PinnedSymbols: []string{}}
}

// QuoteTimeout is QuoteTimeoutSeconds as a duration; zero means "the
// provider's default", which is what a zero duration signals downstream too.
func (c Config) QuoteTimeout() time.Duration {
	return time.Duration(c.QuoteTimeoutSeconds) * time.Second
}

// RefreshInterval is RefreshSeconds as a duration.
func (c Config) RefreshInterval() time.Duration {
	return time.Duration(c.RefreshSeconds) * time.Second
}

// HistoryRetention is HistoryHours as a duration.
func (c Config) HistoryRetention() time.Duration {
	return time.Duration(c.HistoryHours) * time.Hour
}

// Config reads the stored configuration, falling back to the default for any
// key that isn't set — so a database written by an older version, which never
// knew about a newer key, reads back as the default rather than as zero.
func (s *Store) Config() (Config, error) {
	cfg := DefaultConfig()

	if v, err := s.Setting(SettingRefreshSeconds); err != nil {
		return cfg, err
	} else if v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= MinRefreshSeconds {
			cfg.RefreshSeconds = n
		}
	}
	if v, err := s.Setting(SettingPublishOnRefresh); err != nil {
		return cfg, err
	} else if v != "" {
		cfg.PublishOnRefresh = v == "true"
	}
	if v, err := s.Setting(SettingHistoryHours); err != nil {
		return cfg, err
	} else if v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.HistoryHours = n
		}
	}
	if v, err := s.Setting(SettingQuoteBaseURL); err != nil {
		return cfg, err
	} else {
		cfg.QuoteBaseURL = v
	}
	if v, err := s.Setting(SettingQuoteTimeout); err != nil {
		return cfg, err
	} else if v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= MinQuoteTimeout && n <= MaxQuoteTimeout {
			cfg.QuoteTimeoutSeconds = n
		}
	}
	if v, err := s.Setting(SettingQuoteUserAgent); err != nil {
		return cfg, err
	} else {
		cfg.QuoteUserAgent = v
	}
	if v, err := s.Setting(SettingPinnedSymbols); err != nil {
		return cfg, err
	} else {
		cfg.PinnedSymbols = ParsePinnedSymbols(v)
	}
	return cfg, nil
}

// PinnedSymbols reads just the pinned list. The ticker queries need it on
// every read to order their results, and going through Config() for it would
// mean six extra lookups per watchlist render.
func (s *Store) PinnedSymbols() ([]string, error) {
	v, err := s.Setting(SettingPinnedSymbols)
	if err != nil {
		return nil, err
	}
	return ParsePinnedSymbols(v), nil
}

// ParsePinnedSymbols turns the stored comma-separated list into normalised
// symbols. Blanks and duplicates are dropped, so a value hand-edited in the
// database ("vti, , VTI") still reads back as something the watchlist can use.
func ParsePinnedSymbols(raw string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		sym := NormalizeSymbol(part)
		if sym == "" || seen[sym] {
			continue
		}
		seen[sym] = true
		out = append(out, sym)
	}
	return out
}

// ConfigPatch is a partial configuration update; a nil field is left alone.
// The string fields are pointers for the same reason: "" is a meaningful value
// here (it means "go back to the default"), so it has to be distinguishable
// from "not mentioned".
type ConfigPatch struct {
	RefreshSeconds      *int      `json:"refreshSeconds"`
	PublishOnRefresh    *bool     `json:"publishOnRefresh"`
	HistoryHours        *int      `json:"historyHours"`
	QuoteBaseURL        *string   `json:"quoteBaseUrl"`
	QuoteTimeoutSeconds *int      `json:"quoteTimeoutSeconds"`
	QuoteUserAgent      *string   `json:"quoteUserAgent"`
	PinnedSymbols       *[]string `json:"pinnedSymbols"`
}

// UpdateConfig validates and persists a patch, returning the config as it now
// stands.
func (s *Store) UpdateConfig(patch ConfigPatch) (Config, error) {
	cfg, err := s.Config()
	if err != nil {
		return cfg, err
	}

	if patch.RefreshSeconds != nil {
		if *patch.RefreshSeconds < MinRefreshSeconds {
			return cfg, fmt.Errorf("refreshSeconds must be at least %d", MinRefreshSeconds)
		}
		cfg.RefreshSeconds = *patch.RefreshSeconds
	}
	if patch.PublishOnRefresh != nil {
		cfg.PublishOnRefresh = *patch.PublishOnRefresh
	}
	if patch.HistoryHours != nil {
		if *patch.HistoryHours < 0 {
			return cfg, errors.New("historyHours cannot be negative")
		}
		cfg.HistoryHours = *patch.HistoryHours
	}
	if patch.QuoteBaseURL != nil {
		v := strings.TrimRight(strings.TrimSpace(*patch.QuoteBaseURL), "/")
		if err := validateQuoteBaseURL(v); err != nil {
			return cfg, err
		}
		cfg.QuoteBaseURL = v
	}
	if patch.QuoteTimeoutSeconds != nil {
		v := *patch.QuoteTimeoutSeconds
		// 0 is the escape hatch back to the provider's default, so it is valid
		// even though it is below the floor.
		if v != 0 && (v < MinQuoteTimeout || v > MaxQuoteTimeout) {
			return cfg, fmt.Errorf("quoteTimeoutSeconds must be 0 (default) or between %d and %d",
				MinQuoteTimeout, MaxQuoteTimeout)
		}
		cfg.QuoteTimeoutSeconds = v
	}
	if patch.QuoteUserAgent != nil {
		v := strings.TrimSpace(*patch.QuoteUserAgent)
		if len(v) > MaxUserAgentLen {
			return cfg, fmt.Errorf("quoteUserAgent cannot be longer than %d characters", MaxUserAgentLen)
		}
		if strings.ContainsAny(v, "\r\n") {
			// A newline here would let the value inject extra request headers.
			return cfg, errors.New("quoteUserAgent cannot contain line breaks")
		}
		cfg.QuoteUserAgent = v
	}
	if patch.PinnedSymbols != nil {
		// Each entry is itself split on commas, so a client that sends the raw
		// text of the settings field as one string gets the same result as one
		// that sends a proper list.
		pinned := ParsePinnedSymbols(strings.Join(*patch.PinnedSymbols, ","))
		if len(pinned) > MaxPinnedSymbols {
			return cfg, fmt.Errorf("pinnedSymbols must be a list of at most %d symbols", MaxPinnedSymbols)
		}
		cfg.PinnedSymbols = pinned
	}

	if err := s.SetSettings(map[string]string{
		SettingRefreshSeconds:   strconv.Itoa(cfg.RefreshSeconds),
		SettingPublishOnRefresh: strconv.FormatBool(cfg.PublishOnRefresh),
		SettingHistoryHours:     strconv.Itoa(cfg.HistoryHours),
		SettingQuoteBaseURL:     cfg.QuoteBaseURL,
		SettingQuoteTimeout:     strconv.Itoa(cfg.QuoteTimeoutSeconds),
		SettingQuoteUserAgent:   cfg.QuoteUserAgent,
		SettingPinnedSymbols:    strings.Join(cfg.PinnedSymbols, ","),
	}); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// validateQuoteBaseURL accepts an empty value (meaning "use the default") or
// an http/https URL with a host. The scheme check matters: the server fetches
// whatever is configured here, and allowing file:// or gopher:// would turn a
// settings field into an arbitrary-read primitive.
func validateQuoteBaseURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("quoteBaseUrl is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("quoteBaseUrl must be an http:// or https:// URL")
	}
	if u.Host == "" {
		return errors.New("quoteBaseUrl needs a host")
	}
	return nil
}

// Setting reads one raw setting; an unset key reads as "".
func (s *Store) Setting(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetSettings writes several settings in one transaction.
func (s *Store) SetSettings(kv map[string]string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, v := range kv {
		if _, err := tx.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
			ON CONFLICT (key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}
