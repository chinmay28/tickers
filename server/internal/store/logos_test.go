package store

import (
	"errors"
	"testing"
)

func TestLogoRoundTrips(t *testing.T) {
	st := newTestStore(t)

	png := []byte{0x89, 'P', 'N', 'G', 1, 2, 3}
	if err := st.SaveLogo(Logo{
		Symbol: "aapl", Status: LogoOK, ContentType: "image/png",
		Bytes: png, Source: "https://example.test/aapl.png",
	}); err != nil {
		t.Fatalf("save logo: %v", err)
	}

	got, err := st.Logo("AAPL")
	if err != nil {
		t.Fatalf("read logo: %v", err)
	}
	if string(got.Bytes) != string(png) {
		t.Errorf("stored image came back as %v, want the bytes that went in", got.Bytes)
	}
	if got.ContentType != "image/png" {
		t.Errorf("content type = %q, want image/png — the API serves this verbatim", got.ContentType)
	}
	// Symbols are normalised on the way in, or "aapl" and "AAPL" become two
	// cache entries and the second one is never found by the watchlist.
	if got.Symbol != "AAPL" {
		t.Errorf("symbol stored as %q, want it upper-cased", got.Symbol)
	}
}

func TestLogoNoneIsNotServed(t *testing.T) {
	st := newTestStore(t)

	if err := st.SaveLogo(Logo{Symbol: "GLD", Status: LogoNone}); err != nil {
		t.Fatalf("save tombstone: %v", err)
	}

	if _, err := st.Logo("GLD"); !errors.Is(err, ErrNotFound) {
		t.Errorf("reading a symbol with no logo gave %v, want ErrNotFound — "+
			"a tombstone is an answer to the cycle, not an image to serve", err)
	}

	// It still counts as asked, which is the entire point of storing it.
	asked, err := st.AskedAboutLogos()
	if err != nil {
		t.Fatalf("asked set: %v", err)
	}
	if !asked["GLD"] {
		t.Error("a symbol answered with 'no logo' is not marked as asked, so every cycle will ask again")
	}
	symbols, err := st.LogoSymbols()
	if err != nil {
		t.Fatalf("logo symbols: %v", err)
	}
	if len(symbols) != 0 {
		t.Errorf("LogoSymbols returned %v, want nothing — the client would draw a broken image for it", symbols)
	}
}

func TestSaveLogoRejectsNonsense(t *testing.T) {
	st := newTestStore(t)

	cases := map[string]Logo{
		"no symbol":       {Status: LogoOK, ContentType: "image/png", Bytes: []byte{1}},
		"unknown status":  {Symbol: "VTI", Status: "maybe"},
		"ok with no data": {Symbol: "VTI", Status: LogoOK, ContentType: "image/png"},
		"ok with no type": {Symbol: "VTI", Status: LogoOK, Bytes: []byte{1}},
		"oversized": {
			Symbol: "VTI", Status: LogoOK, ContentType: "image/png",
			Bytes: make([]byte, MaxLogoBytes+1),
		},
	}
	for name, logo := range cases {
		if err := st.SaveLogo(logo); err == nil {
			t.Errorf("%s was accepted; the cache is a Pi's SD card and takes only complete, bounded images", name)
		}
	}
}

func TestTurningLogosOffEmptiesTheCache(t *testing.T) {
	st := newTestStore(t)

	on := true
	if _, err := st.UpdateConfig(ConfigPatch{Logos: &on}); err != nil {
		t.Fatalf("enable logos: %v", err)
	}
	if err := st.SaveLogo(Logo{
		Symbol: "AAPL", Status: LogoOK, ContentType: "image/png", Bytes: []byte{1, 2},
	}); err != nil {
		t.Fatalf("save logo: %v", err)
	}

	off := false
	cfg, err := st.UpdateConfig(ConfigPatch{Logos: &off})
	if err != nil {
		t.Fatalf("disable logos: %v", err)
	}
	if cfg.Logos {
		t.Fatal("config still reports logos as on after being turned off")
	}

	asked, err := st.AskedAboutLogos()
	if err != nil {
		t.Fatalf("asked set: %v", err)
	}
	if len(asked) != 0 {
		t.Errorf("%d cached logos survived the setting being turned off; "+
			"third-party images should not outlive the feature that fetched them", len(asked))
	}
}

func TestLogosDefaultOff(t *testing.T) {
	st := newTestStore(t)

	cfg, err := st.Config()
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if cfg.Logos {
		t.Error("a fresh install has logo fetching on; nothing should talk to a third party until asked to")
	}
}
