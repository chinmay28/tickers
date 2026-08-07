package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
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
	SettingSeeded           = "seeded"
)

// MinRefreshSeconds is the floor on the poll interval. Yahoo's endpoint is
// free and unauthenticated; hammering it every few seconds is how a self-
// hosted watchlist gets its host rate-limited, so the UI can't set less.
const MinRefreshSeconds = 30

// Config is the tunable behaviour of the refresh loop, as one value.
type Config struct {
	RefreshSeconds   int  `json:"refreshSeconds"`
	PublishOnRefresh bool `json:"publishOnRefresh"`
	HistoryHours     int  `json:"historyHours"`
}

// DefaultConfig is what a fresh install runs with: a five-minute poll (the
// cadence the original script's cron entry used), publishing every cycle, and
// three days of history behind the sparklines.
func DefaultConfig() Config {
	return Config{RefreshSeconds: 300, PublishOnRefresh: true, HistoryHours: 72}
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
	return cfg, nil
}

// ConfigPatch is a partial configuration update.
type ConfigPatch struct {
	RefreshSeconds   *int  `json:"refreshSeconds"`
	PublishOnRefresh *bool `json:"publishOnRefresh"`
	HistoryHours     *int  `json:"historyHours"`
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

	if err := s.SetSettings(map[string]string{
		SettingRefreshSeconds:   strconv.Itoa(cfg.RefreshSeconds),
		SettingPublishOnRefresh: strconv.FormatBool(cfg.PublishOnRefresh),
		SettingHistoryHours:     strconv.Itoa(cfg.HistoryHours),
	}); err != nil {
		return cfg, err
	}
	return cfg, nil
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
