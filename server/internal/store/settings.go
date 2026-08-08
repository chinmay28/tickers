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
	SettingLogos            = "logos_enabled"
	SettingLogoURL          = "logo_url_template"
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
	// MaxLogoURLLen bounds the logo template. Long enough for a service URL
	// with a token in it, short enough that the field is not a paste target.
	MaxLogoURLLen = 512
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

	// Logos turns on fetching a real logo per symbol from the quote source and
	// caching it here. It is off by default and stays that way until somebody
	// turns it on, because it is the one setting that makes this install talk
	// to a host it otherwise wouldn't, about the symbols on its watchlist.
	// Every symbol without one — every ETF, every crypto pair, every composite
	// and portfolio — keeps the mark drawn from its name either way.
	Logos bool `json:"logos"`

	// LogoURLTemplate is where a logo comes from, with `{symbol}` standing in
	// for the ticker. Empty means "let the quote source work it out", which
	// for Yahoo means the `logoUrl` its search results sometimes carry.
	//
	// It is configurable for the same reason the user agent is: there is no
	// standard way to get a picture from a ticker, what works is a moving
	// target of free services, and an install that can reach one should not
	// have to wait for a release to use it. Changing it clears the cache, so
	// the next cycles ask the new source rather than serving the old one's
	// answers.
	LogoURLTemplate string `json:"logoUrlTemplate"`
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
	if v, err := s.Setting(SettingLogos); err != nil {
		return cfg, err
	} else if v != "" {
		cfg.Logos = v == "true"
	}
	if v, err := s.Setting(SettingLogoURL); err != nil {
		return cfg, err
	} else {
		cfg.LogoURLTemplate = v
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
	Logos               *bool     `json:"logos"`
	LogoURLTemplate     *string   `json:"logoUrlTemplate"`
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
	if patch.LogoURLTemplate != nil {
		v := strings.TrimSpace(*patch.LogoURLTemplate)
		if err := validateLogoURLTemplate(v); err != nil {
			return cfg, err
		}
		// A new source means the old source's answers — including everything
		// it said had no logo — are worthless. Keeping them would make a
		// working template look broken for as long as the cache lived.
		if v != cfg.LogoURLTemplate {
			if _, err := s.ForgetLogos(); err != nil {
				return cfg, err
			}
		}
		cfg.LogoURLTemplate = v
	}
	if patch.Logos != nil {
		// Turning it off empties the cache. A drawer of third-party images
		// nobody wants any more should not outlive the setting that filled it,
		// and it also makes turning the feature back on mean "go and look
		// again" rather than "show me what you kept".
		if !*patch.Logos && cfg.Logos {
			if _, err := s.ForgetLogos(); err != nil {
				return cfg, err
			}
		}
		cfg.Logos = *patch.Logos
	}

	if err := s.SetSettings(map[string]string{
		SettingRefreshSeconds:   strconv.Itoa(cfg.RefreshSeconds),
		SettingPublishOnRefresh: strconv.FormatBool(cfg.PublishOnRefresh),
		SettingHistoryHours:     strconv.Itoa(cfg.HistoryHours),
		SettingQuoteBaseURL:     cfg.QuoteBaseURL,
		SettingQuoteTimeout:     strconv.Itoa(cfg.QuoteTimeoutSeconds),
		SettingQuoteUserAgent:   cfg.QuoteUserAgent,
		SettingPinnedSymbols:    strings.Join(cfg.PinnedSymbols, ","),
		SettingLogos:            strconv.FormatBool(cfg.Logos),
		SettingLogoURL:          cfg.LogoURLTemplate,
	}); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// The placeholders a logo template may carry. They mirror the constants in
// `quotes`, which is what expands them — repeated rather than imported because
// `store` sits below `quotes` in the dependency order and reaching up would
// invert it. The client mirrors them a third time, for the same reason it
// mirrors expr.Looks.
const (
	logoSymbolToken      = "{symbol}"
	logoSymbolLowerToken = "{symbol_lower}"
)

// validateLogoURLTemplate accepts an empty value (meaning "let the provider
// decide") or an http/https URL carrying a symbol placeholder.
//
// The placeholder is required rather than optional: a template without one
// resolves to the same picture for every symbol, which is not a logo feature,
// it is a wallpaper. The scheme check matters for the same reason it does on
// the quote base URL — the server fetches whatever is configured here.
func validateLogoURLTemplate(raw string) error {
	if raw == "" {
		return nil
	}
	if len(raw) > MaxLogoURLLen {
		return fmt.Errorf("the logo URL cannot be longer than %d characters", MaxLogoURLLen)
	}
	if strings.ContainsAny(raw, "\r\n") {
		return errors.New("the logo URL cannot contain line breaks")
	}
	if !strings.Contains(raw, logoSymbolToken) && !strings.Contains(raw, logoSymbolLowerToken) {
		return fmt.Errorf("the logo URL has to contain %s (or %s), or every symbol gets the same picture",
			logoSymbolToken, logoSymbolLowerToken)
	}
	// Checked as it will actually be used. A template is only a URL once the
	// placeholder is gone — `{symbol}` in a host would parse as nothing useful.
	filled := strings.ReplaceAll(raw, logoSymbolToken, "TEST")
	filled = strings.ReplaceAll(filled, logoSymbolLowerToken, "test")
	parsed, err := url.Parse(filled)
	if err != nil {
		return errors.New("that logo URL is not a URL")
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return errors.New("the logo URL has to be http:// or https:// with a host")
	}
	return nil
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
