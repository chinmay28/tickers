package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// Archive setting keys. They live in the same key/value table as every other
// setting, so adding one never needs a migration, but they are read and
// written as their own value — ArchiveConfig — rather than as fields on
// Config. Config is served to every browser in /api/state; the archive's
// settings are only ever needed by the Data page, and carry a credential.
const (
	SettingArchiveEnabled   = "archive_enabled"
	SettingArchivePath      = "archive_path"
	SettingArchivePaused    = "archive_paused"
	SettingArchiveIntervals = "archive_intervals"
	SettingArchiveListed    = "archive_listed"
	SettingArchiveExtras    = "archive_extras"
	SettingArchiveSpacingMS = "archive_spacing_ms"
	SettingArchiveMinFreeGB = "archive_min_free_gb"
	SettingPolygonKey       = "archive_polygon_key"
	SettingPolygonYears     = "archive_polygon_years"
	SettingPolygonPerMinute = "archive_polygon_per_minute"
	SettingPolygonBaseURL   = "archive_polygon_base_url"
)

// ArchiveIntervals are the bar widths the archive can collect, finest last.
// They mirror quotes.Intervals, repeated rather than imported because store
// sits below quotes in the dependency order.
var ArchiveIntervals = []string{"1d", "1h", "5m", "1m"}

// DefaultArchiveExtras are collected besides the exchange lists: the two
// largest cryptocurrencies and the indices every analysis wants a benchmark
// from. None of them is listed on a US exchange, which is why they need
// naming.
var DefaultArchiveExtras = []string{"BTC-USD", "ETH-USD", "^GSPC", "^DJI", "^IXIC", "^RUT", "^VIX"}

// Bounds on the archive's tunables.
const (
	// MinArchiveSpacingMS is the floor on the gap between two requests to
	// Yahoo — four a second, which is already more than it should be asked.
	MinArchiveSpacingMS = 250
	MaxArchiveSpacingMS = 60_000
	MaxArchiveMinFreeGB = 10_000
	MaxArchiveExtras    = 200
	MaxPolygonYears     = 30
	MaxPolygonPerMinute = 6000
	MaxArchivePathLen   = 1024
)

// ArchiveConfig is how the market-data archive is collected. Every field is
// editable on the Data page and takes effect without a restart.
type ArchiveConfig struct {
	// Enabled is the archive's on/off switch. Off closes the archive and
	// stops collecting; nothing stored is touched, and the app reads from the
	// quote source as it did before the archive existed. It defaults to on:
	// an install with a folder configured — which the quick start sets up —
	// collects without anyone having to find the switch.
	Enabled bool `json:"enabled"`
	// Path is the archive folder. Empty means "whatever the server was
	// started with" (--archive / TICKERS_ARCHIVE), and if that is empty too,
	// there is no archive.
	Path string `json:"path"`
	// Paused stops fetching without forgetting anything.
	Paused bool `json:"paused"`
	// Intervals to collect.
	Intervals []string `json:"intervals"`
	// Listed collects every symbol listed on a US exchange.
	Listed bool `json:"listed"`
	// Extras are collected besides the exchange lists.
	Extras []string `json:"extras"`
	// SpacingMS is the gap between two requests to Yahoo.
	SpacingMS int `json:"spacingMs"`
	// MinFreeGB pauses collection when the archive's disk has less free.
	// Zero never pauses.
	MinFreeGB int `json:"minFreeGb"`

	// PolygonKey enables Polygon as a gap-filling source. Like LogoKey it is
	// never served; PolygonKeySet says whether one is stored.
	PolygonKey    string `json:"-"`
	PolygonKeySet bool   `json:"polygonKeySet"`
	// PolygonYears is how far back the subscription reaches. It is the
	// plan's, not the API's, so only the person who bought it can say.
	PolygonYears int `json:"polygonYears"`
	// PolygonPerMinute is the plan's request limit.
	PolygonPerMinute int    `json:"polygonPerMinute"`
	PolygonBaseURL   string `json:"polygonBaseUrl"`
}

// DefaultArchiveConfig is what an install that has never opened the Data page
// runs with. Polygon's defaults are its free tier: two years, five requests a
// minute.
func DefaultArchiveConfig() ArchiveConfig {
	return ArchiveConfig{
		Enabled:          true,
		Intervals:        append([]string(nil), ArchiveIntervals...),
		Listed:           true,
		Extras:           append([]string(nil), DefaultArchiveExtras...),
		SpacingMS:        2000,
		MinFreeGB:        10,
		PolygonYears:     2,
		PolygonPerMinute: 5,
	}
}

// ArchiveConfig reads the stored archive settings, falling back to the default
// for anything unset.
func (s *Store) ArchiveConfig() (ArchiveConfig, error) {
	cfg := DefaultArchiveConfig()
	read := func(key string) (string, bool, error) {
		v, err := s.Setting(key)
		return v, v != "", err
	}
	var err error
	var v string
	var ok bool
	if v, ok, err = read(SettingArchiveEnabled); err != nil {
		return cfg, err
	} else if ok {
		cfg.Enabled = v == "true"
	}
	if cfg.Path, _, err = read(SettingArchivePath); err != nil {
		return cfg, err
	}
	if v, ok, err = read(SettingArchivePaused); err != nil {
		return cfg, err
	} else if ok {
		cfg.Paused = v == "true"
	}
	if v, ok, err = read(SettingArchiveIntervals); err != nil {
		return cfg, err
	} else if ok {
		if list, err := parseIntervals(v); err == nil {
			cfg.Intervals = list
		}
	}
	if v, ok, err = read(SettingArchiveListed); err != nil {
		return cfg, err
	} else if ok {
		cfg.Listed = v == "true"
	}
	if v, ok, err = read(SettingArchiveExtras); err != nil {
		return cfg, err
	} else if ok {
		// Stored as "-" when emptied on purpose: an empty value would read
		// back as "never set" and bring the defaults back.
		cfg.Extras = ParsePinnedSymbols(strings.TrimPrefix(v, "-"))
	}
	if v, ok, err = read(SettingArchiveSpacingMS); err != nil {
		return cfg, err
	} else if ok {
		if n, err := strconv.Atoi(v); err == nil && n >= MinArchiveSpacingMS && n <= MaxArchiveSpacingMS {
			cfg.SpacingMS = n
		}
	}
	if v, ok, err = read(SettingArchiveMinFreeGB); err != nil {
		return cfg, err
	} else if ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= MaxArchiveMinFreeGB {
			cfg.MinFreeGB = n
		}
	}
	if cfg.PolygonKey, _, err = read(SettingPolygonKey); err != nil {
		return cfg, err
	}
	cfg.PolygonKeySet = cfg.PolygonKey != ""
	if v, ok, err = read(SettingPolygonYears); err != nil {
		return cfg, err
	} else if ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= MaxPolygonYears {
			cfg.PolygonYears = n
		}
	}
	if v, ok, err = read(SettingPolygonPerMinute); err != nil {
		return cfg, err
	} else if ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= MaxPolygonPerMinute {
			cfg.PolygonPerMinute = n
		}
	}
	if cfg.PolygonBaseURL, _, err = read(SettingPolygonBaseURL); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func parseIntervals(raw string) ([]string, error) {
	want := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		known := false
		for _, i := range ArchiveIntervals {
			if i == part {
				known = true
			}
		}
		if !known {
			return nil, fmt.Errorf("unknown interval %q (want any of %s)", part, strings.Join(ArchiveIntervals, ", "))
		}
		want[part] = true
	}
	if len(want) == 0 {
		return nil, errors.New("collect at least one interval, or pause the collector instead")
	}
	// Always in the canonical order, which is also the order first fetches
	// are made in: daily history first.
	var out []string
	for _, i := range ArchiveIntervals {
		if want[i] {
			out = append(out, i)
		}
	}
	return out, nil
}

// ArchivePatch is a partial update to ArchiveConfig; a nil field is left
// alone. The path is not in it — moving the archive is an operation, not a
// field, and goes through SetArchivePath.
type ArchivePatch struct {
	Enabled          *bool     `json:"enabled"`
	Paused           *bool     `json:"paused"`
	Intervals        *[]string `json:"intervals"`
	Listed           *bool     `json:"listed"`
	Extras           *[]string `json:"extras"`
	SpacingMS        *int      `json:"spacingMs"`
	MinFreeGB        *int      `json:"minFreeGb"`
	PolygonKey       *string   `json:"polygonKey"`
	PolygonYears     *int      `json:"polygonYears"`
	PolygonPerMinute *int      `json:"polygonPerMinute"`
	PolygonBaseURL   *string   `json:"polygonBaseUrl"`
}

// UpdateArchiveConfig validates and persists a patch.
func (s *Store) UpdateArchiveConfig(p ArchivePatch) (ArchiveConfig, error) {
	cfg, err := s.ArchiveConfig()
	if err != nil {
		return cfg, err
	}
	kv := map[string]string{}
	if p.Enabled != nil {
		cfg.Enabled = *p.Enabled
		kv[SettingArchiveEnabled] = strconv.FormatBool(cfg.Enabled)
	}
	if p.Paused != nil {
		cfg.Paused = *p.Paused
		kv[SettingArchivePaused] = strconv.FormatBool(cfg.Paused)
	}
	if p.Intervals != nil {
		list, err := parseIntervals(strings.Join(*p.Intervals, ","))
		if err != nil {
			return cfg, err
		}
		cfg.Intervals = list
		kv[SettingArchiveIntervals] = strings.Join(list, ",")
	}
	if p.Listed != nil {
		cfg.Listed = *p.Listed
		kv[SettingArchiveListed] = strconv.FormatBool(cfg.Listed)
	}
	if p.Extras != nil {
		extras := ParsePinnedSymbols(strings.Join(*p.Extras, ","))
		if len(extras) > MaxArchiveExtras {
			return cfg, fmt.Errorf("extras must be a list of at most %d symbols; add more on the Data page one at a time", MaxArchiveExtras)
		}
		cfg.Extras = extras
		kv[SettingArchiveExtras] = "-" + strings.Join(extras, ",")
	}
	if p.SpacingMS != nil {
		if *p.SpacingMS < MinArchiveSpacingMS || *p.SpacingMS > MaxArchiveSpacingMS {
			return cfg, fmt.Errorf("the request spacing must be between %d and %d ms", MinArchiveSpacingMS, MaxArchiveSpacingMS)
		}
		cfg.SpacingMS = *p.SpacingMS
		kv[SettingArchiveSpacingMS] = strconv.Itoa(cfg.SpacingMS)
	}
	if p.MinFreeGB != nil {
		if *p.MinFreeGB < 0 || *p.MinFreeGB > MaxArchiveMinFreeGB {
			return cfg, fmt.Errorf("the free-space floor must be between 0 and %d GB", MaxArchiveMinFreeGB)
		}
		cfg.MinFreeGB = *p.MinFreeGB
		kv[SettingArchiveMinFreeGB] = strconv.Itoa(cfg.MinFreeGB)
	}
	if p.PolygonKey != nil {
		v := strings.TrimSpace(*p.PolygonKey)
		if len(v) > MaxLogoKeyLen {
			return cfg, fmt.Errorf("the Polygon key cannot be longer than %d characters", MaxLogoKeyLen)
		}
		if strings.ContainsAny(v, "\r\n") {
			// It goes into a request header.
			return cfg, errors.New("the Polygon key cannot contain line breaks")
		}
		cfg.PolygonKey, cfg.PolygonKeySet = v, v != ""
		kv[SettingPolygonKey] = v
	}
	if p.PolygonYears != nil {
		if *p.PolygonYears < 1 || *p.PolygonYears > MaxPolygonYears {
			return cfg, fmt.Errorf("Polygon's history must be between 1 and %d years", MaxPolygonYears)
		}
		cfg.PolygonYears = *p.PolygonYears
		kv[SettingPolygonYears] = strconv.Itoa(cfg.PolygonYears)
	}
	if p.PolygonPerMinute != nil {
		if *p.PolygonPerMinute < 1 || *p.PolygonPerMinute > MaxPolygonPerMinute {
			return cfg, fmt.Errorf("Polygon's request limit must be between 1 and %d a minute", MaxPolygonPerMinute)
		}
		cfg.PolygonPerMinute = *p.PolygonPerMinute
		kv[SettingPolygonPerMinute] = strconv.Itoa(cfg.PolygonPerMinute)
	}
	if p.PolygonBaseURL != nil {
		v := strings.TrimRight(strings.TrimSpace(*p.PolygonBaseURL), "/")
		if err := validateQuoteBaseURL(v); err != nil {
			return cfg, errors.New(strings.Replace(err.Error(), "quoteBaseUrl", "the Polygon URL", 1))
		}
		cfg.PolygonBaseURL = v
		kv[SettingPolygonBaseURL] = v
	}
	if len(kv) == 0 {
		return cfg, nil
	}
	return cfg, s.SetSettings(kv)
}

// SetArchivePath stores the archive folder. The path must be absolute — a
// relative one would resolve against wherever systemd happened to start the
// process — and must not be a place the rest of the system lives. Whether it
// exists and is an archive is the caller's business: it may be about to be
// initialised or moved into.
func (s *Store) SetArchivePath(path string) error {
	path = strings.TrimSpace(path)
	if path != "" {
		if len(path) > MaxArchivePathLen {
			return fmt.Errorf("the archive path cannot be longer than %d characters", MaxArchivePathLen)
		}
		if strings.ContainsAny(path, "\x00\r\n") {
			return errors.New("the archive path cannot contain control characters")
		}
		if !filepath.IsAbs(path) {
			return errors.New("the archive path must be absolute, like /mnt/usb/tickers-archive")
		}
		path = filepath.Clean(path)
		if path == "/" {
			return errors.New("the archive cannot be the root of the filesystem")
		}
	}
	return s.SetSettings(map[string]string{SettingArchivePath: path})
}
