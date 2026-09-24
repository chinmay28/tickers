package store

import (
	"strings"
	"testing"
)

func TestArchiveConfigDefaultsAndRoundTrips(t *testing.T) {
	s := newTestStore(t)
	cfg, err := s.ArchiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled || cfg.Path != "" || !cfg.Listed || len(cfg.Intervals) != 4 || cfg.SpacingMS != 2000 || cfg.PolygonKeySet {
		t.Fatalf("defaults = %+v", cfg)
	}

	intervals := []string{"1m", "1d", "1m"}
	extras := []string{"btc-usd, gld", ""}
	key := " pk_123 "
	cfg, err = s.UpdateArchiveConfig(ArchivePatch{Intervals: &intervals, Extras: &extras, PolygonKey: &key})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	again, _ := s.ArchiveConfig()
	if strings.Join(again.Intervals, ",") != "1d,1m" {
		t.Errorf("intervals = %v, want canonical order, deduplicated", again.Intervals)
	}
	if strings.Join(again.Extras, ",") != "BTC-USD,GLD" {
		t.Errorf("extras = %v", again.Extras)
	}
	if again.PolygonKey != "pk_123" || !again.PolygonKeySet {
		t.Errorf("key = %q set=%v", again.PolygonKey, again.PolygonKeySet)
	}

	// Emptying the extras sticks, rather than reading back as the defaults.
	none := []string{}
	s.UpdateArchiveConfig(ArchivePatch{Extras: &none})
	if again, _ := s.ArchiveConfig(); len(again.Extras) != 0 {
		t.Errorf("emptied extras read back as %v", again.Extras)
	}
}

func TestArchiveConfigRefusesBadValues(t *testing.T) {
	s := newTestStore(t)
	bad := func(name string, p ArchivePatch) {
		t.Helper()
		if _, err := s.UpdateArchiveConfig(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	unknown := []string{"2m"}
	empty := []string{}
	fast, years, url := 10, 99, "file:///etc"
	crlf := "a\r\nb"
	bad("unknown interval", ArchivePatch{Intervals: &unknown})
	bad("no intervals", ArchivePatch{Intervals: &empty})
	bad("spacing below the floor", ArchivePatch{SpacingMS: &fast})
	bad("too many years", ArchivePatch{PolygonYears: &years})
	bad("non-http URL", ArchivePatch{PolygonBaseURL: &url})
	bad("header injection", ArchivePatch{PolygonKey: &crlf})

	for _, p := range []string{"relative/path", "/", "/tmp/a\nb"} {
		if err := s.SetArchivePath(p); err == nil {
			t.Errorf("path %q accepted", p)
		}
	}
	if err := s.SetArchivePath("/mnt/usb/archive/"); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := s.ArchiveConfig(); cfg.Path != "/mnt/usb/archive" {
		t.Errorf("path = %q, want it cleaned", cfg.Path)
	}
}

func TestTheArchiveSwitchDefaultsOnAndTurnsOff(t *testing.T) {
	s := newTestStore(t)
	off := false
	if _, err := s.UpdateArchiveConfig(ArchivePatch{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := s.ArchiveConfig(); cfg.Enabled {
		t.Error("turning the archive off did not stick — an unset value must not read back as the default once set")
	}
}
