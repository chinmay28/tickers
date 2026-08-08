package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
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
	settled, err := st.SettledLogos(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("settled set: %v", err)
	}
	if !settled["GLD"] {
		t.Error("a symbol answered with 'no logo' is not settled, so every cycle will ask again")
	}
	versions, err := st.LogoVersions()
	if err != nil {
		t.Fatalf("logo versions: %v", err)
	}
	if len(versions) != 0 {
		t.Errorf("LogoVersions returned %v, want nothing — the client would draw a broken image for it", versions)
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

	asked, err := st.SettledLogos(time.Now().Add(-time.Hour))
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

func TestLogoURLTemplateIsChecked(t *testing.T) {
	st := newTestStore(t)

	bad := map[string]string{
		"no placeholder": "https://logos.test/aapl.png",
		"not a URL":      "logos.test/{symbol}.png",
		"not http":       "file:///etc/{symbol}",
		"a line break":   "https://logos.test/{symbol}\nX-Evil: 1",
	}
	for name, template := range bad {
		v := template
		if _, err := st.UpdateConfig(ConfigPatch{LogoURLTemplate: &v}); err == nil {
			t.Errorf("%s was accepted as a logo URL", name)
		}
	}

	good := "https://logos.test/{symbol_lower}.png"
	cfg, err := st.UpdateConfig(ConfigPatch{LogoURLTemplate: &good})
	if err != nil {
		t.Fatalf("a valid template was rejected: %v", err)
	}
	if cfg.LogoURLTemplate != good {
		t.Errorf("template stored as %q, want %q", cfg.LogoURLTemplate, good)
	}

	// Blank is how you go back to the provider's own idea of a logo.
	blank := ""
	if cfg, err = st.UpdateConfig(ConfigPatch{LogoURLTemplate: &blank}); err != nil {
		t.Fatalf("clearing the template failed: %v", err)
	}
	if cfg.LogoURLTemplate != "" {
		t.Error("the template could not be cleared")
	}
}

func TestChangingTheLogoURLEmptiesTheCache(t *testing.T) {
	st := newTestStore(t)

	if err := st.SaveLogo(Logo{
		Symbol: "AAPL", Status: LogoOK, ContentType: "image/png", Bytes: []byte{1, 2},
	}); err != nil {
		t.Fatalf("save logo: %v", err)
	}
	if err := st.SaveLogo(Logo{Symbol: "GLD", Status: LogoNone, Reason: "no logoUrl"}); err != nil {
		t.Fatalf("save tombstone: %v", err)
	}

	next := "https://logos.test/{symbol}.png"
	if _, err := st.UpdateConfig(ConfigPatch{LogoURLTemplate: &next}); err != nil {
		t.Fatalf("set template: %v", err)
	}

	asked, err := st.SettledLogos(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("asked set: %v", err)
	}
	if len(asked) != 0 {
		t.Errorf("%d answers from the old source survived a source change; a working "+
			"new URL would look broken until they expired", len(asked))
	}
}

func TestLogoStatsExplainTheNoes(t *testing.T) {
	st := newTestStore(t)

	if err := st.SaveLogo(Logo{
		Symbol: "AAPL", Status: LogoOK, ContentType: "image/png", Bytes: []byte{1},
	}); err != nil {
		t.Fatalf("save logo: %v", err)
	}
	for _, symbol := range []string{"GLD", "VTI", "IBIT"} {
		if err := st.SaveLogo(Logo{
			Symbol: symbol, Status: LogoNone, Reason: "the search result carries no logo",
		}); err != nil {
			t.Fatalf("save tombstone: %v", err)
		}
	}

	stats, err := st.LogoStats()
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.OK != 1 || stats.None != 3 {
		t.Errorf("stats = %d ok / %d none, want 1 and 3", stats.OK, stats.None)
	}
	if stats.Reason != "the search result carries no logo" {
		t.Errorf("reason = %q, want the commonest explanation — without it the Settings "+
			"page cannot tell a symbol with no logo from a misconfigured source", stats.Reason)
	}
}

func TestLogoKeyIsStoredButNeverServed(t *testing.T) {
	st := newTestStore(t)

	key := "sk_live_secret"
	cfg, err := st.UpdateConfig(ConfigPatch{LogoKey: &key})
	if err != nil {
		t.Fatalf("set key: %v", err)
	}
	if cfg.LogoKey != key {
		t.Errorf("key not held in the config the engine reads: %q", cfg.LogoKey)
	}
	if !cfg.LogoKeySet {
		t.Error("logoKeySet is false with a key stored; the field cannot show it is configured")
	}

	// The whole config is what /api/state hands to every browser on the LAN.
	blob, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if strings.Contains(string(blob), key) {
		t.Errorf("the key is in the serialised settings: %s", blob)
	}
	if !strings.Contains(string(blob), `"logoKeySet":true`) {
		t.Errorf("logoKeySet missing from the serialised settings: %s", blob)
	}
}

func TestOmittingTheLogoKeyLeavesItAlone(t *testing.T) {
	st := newTestStore(t)

	key := "sk_keep_me"
	if _, err := st.UpdateConfig(ConfigPatch{LogoKey: &key}); err != nil {
		t.Fatalf("set key: %v", err)
	}

	// A save from a settings form that could not show the key must not wipe it.
	hours := 48
	cfg, err := st.UpdateConfig(ConfigPatch{HistoryHours: &hours})
	if err != nil {
		t.Fatalf("unrelated patch: %v", err)
	}
	if cfg.LogoKey != key {
		t.Errorf("the stored key was lost saving an unrelated setting: %q", cfg.LogoKey)
	}

	// Deleting it is explicit.
	blank := ""
	if cfg, err = st.UpdateConfig(ConfigPatch{LogoKey: &blank}); err != nil {
		t.Fatalf("clear key: %v", err)
	}
	if cfg.LogoKey != "" || cfg.LogoKeySet {
		t.Error("the key could not be deleted")
	}
}

func TestChangingTheLogoKeyEmptiesTheCache(t *testing.T) {
	st := newTestStore(t)

	if err := st.SaveLogo(Logo{
		Symbol: "AAPL", Status: LogoNone, Reason: "rejected the request (HTTP 401)",
	}); err != nil {
		t.Fatalf("save tombstone: %v", err)
	}

	key := "sk_now_correct"
	if _, err := st.UpdateConfig(ConfigPatch{LogoKey: &key}); err != nil {
		t.Fatalf("set key: %v", err)
	}

	asked, err := st.SettledLogos(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("asked set: %v", err)
	}
	if len(asked) != 0 {
		t.Error("answers from before the key was fixed survived it; every symbol a wrong " +
			"key rejected would stay rejected forever")
	}
}

func TestLogoKeyIsChecked(t *testing.T) {
	st := newTestStore(t)

	long := strings.Repeat("k", MaxLogoKeyLen+1)
	if _, err := st.UpdateConfig(ConfigPatch{LogoKey: &long}); err == nil {
		t.Error("an oversized key was accepted")
	}
	// It is sent as a request header, so a newline would append headers of its own.
	sneaky := "abc\nX-Evil: 1"
	if _, err := st.UpdateConfig(ConfigPatch{LogoKey: &sneaky}); err == nil {
		t.Error("a key with a line break was accepted")
	}
}

func TestUploadedLogosOutliveTheFetchedOnes(t *testing.T) {
	st := newTestStore(t)

	if err := st.SaveLogo(Logo{
		Symbol: "AAPL", Status: LogoOK, Origin: LogoFetched,
		ContentType: "image/png", Bytes: []byte{1},
	}); err != nil {
		t.Fatalf("save fetched: %v", err)
	}
	if err := st.SaveLogo(Logo{
		Symbol: "GLINTY20", Status: LogoOK, Origin: LogoCustom,
		ContentType: "image/png", Bytes: []byte{2},
	}); err != nil {
		t.Fatalf("save upload: %v", err)
	}

	if _, err := st.ForgetLogos(); err != nil {
		t.Fatalf("forget: %v", err)
	}

	if _, err := st.Logo("AAPL"); !errors.Is(err, ErrNotFound) {
		t.Error("a fetched logo survived the cache being emptied")
	}
	mine, err := st.Logo("GLINTY20")
	if err != nil {
		t.Fatalf("an uploaded logo was deleted by emptying the *fetch* cache: %v", err)
	}
	if mine.Origin != LogoCustom {
		t.Errorf("origin came back as %q, want custom", mine.Origin)
	}
}

func TestUploadsAreNeverStale(t *testing.T) {
	st := newTestStore(t)

	// Both stored a week ago; only the fetched one is worth asking about again.
	old := time.Now().Add(-7 * 24 * time.Hour)
	if err := st.SaveLogo(Logo{
		Symbol: "AAPL", Status: LogoOK, Origin: LogoFetched,
		ContentType: "image/png", Bytes: []byte{1}, FetchedAt: old,
	}); err != nil {
		t.Fatalf("save fetched: %v", err)
	}
	if err := st.SaveLogo(Logo{
		Symbol: "GLINTY20", Status: LogoOK, Origin: LogoCustom,
		ContentType: "image/png", Bytes: []byte{2}, FetchedAt: old,
	}); err != nil {
		t.Fatalf("save upload: %v", err)
	}

	settled, err := st.SettledLogos(time.Now().Add(-24 * time.Hour))
	if err != nil {
		t.Fatalf("settled: %v", err)
	}
	if settled["AAPL"] {
		t.Error("a week-old fetched logo counts as settled; nothing would ever be refreshed")
	}
	if !settled["GLINTY20"] {
		t.Error("an uploaded logo went stale — the next cycle would fetch over somebody's own image")
	}
}

func TestUploadedTombstonesAreRefused(t *testing.T) {
	st := newTestStore(t)

	err := st.SaveLogo(Logo{Symbol: "VTI", Status: LogoNone, Origin: LogoCustom})
	if err == nil {
		t.Error("a custom row with no image was accepted; it is never re-asked, so the " +
			"symbol would be marked as having no logo forever with no way back")
	}
}

func TestDeleteLogo(t *testing.T) {
	st := newTestStore(t)

	if err := st.SaveLogo(Logo{
		Symbol: "VTI/GLD", Status: LogoOK, Origin: LogoCustom,
		ContentType: "image/png", Bytes: []byte{1},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := st.DeleteLogo("vti/gld"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.Logo("VTI/GLD"); !errors.Is(err, ErrNotFound) {
		t.Error("the logo survived being deleted")
	}
	if err := st.DeleteLogo("VTI/GLD"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting a logo that isn't there gave %v, want ErrNotFound", err)
	}
}

func TestTouchingALogoAgesItWithoutChangingIt(t *testing.T) {
	st := newTestStore(t)

	if err := st.SaveLogo(Logo{
		Symbol: "AAPL", Status: LogoOK, ContentType: "image/png", Bytes: []byte{1, 2, 3},
		ETag: `"v1"`, FetchedAt: time.Now().Add(-48 * time.Hour),
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	before, err := st.Logo("AAPL")
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if err := st.TouchLogo("AAPL", time.Now()); err != nil {
		t.Fatalf("touch: %v", err)
	}
	after, err := st.Logo("AAPL")
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if !after.FetchedAt.After(before.FetchedAt) {
		t.Error("the check time did not move, so the symbol is due a check again immediately")
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Error("the image's version moved without the image changing; every browser would " +
			"re-download it")
	}
	if string(after.Bytes) != string(before.Bytes) || after.ETag != before.ETag {
		t.Error("touching a row disturbed the image or its validator")
	}

	// An upload is never re-checked, so there is nothing to touch.
	if err := st.SaveLogo(Logo{
		Symbol: "MINE", Status: LogoOK, Origin: LogoCustom,
		ContentType: "image/png", Bytes: []byte{9},
	}); err != nil {
		t.Fatalf("save upload: %v", err)
	}
	if err := st.TouchLogo("MINE", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("touching an uploaded logo gave %v, want ErrNotFound", err)
	}
}

func TestLogoChecksCarryWhatTheNextRequestNeeds(t *testing.T) {
	st := newTestStore(t)

	if err := st.SaveLogo(Logo{
		Symbol: "AAPL", Status: LogoOK, ContentType: "image/png", Bytes: []byte{1, 2},
		ETag: `"v1"`, LastModified: "Wed, 21 Oct 2026 07:28:00 GMT",
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := st.SaveLogo(Logo{
		Symbol: "MINE", Status: LogoOK, Origin: LogoCustom,
		ContentType: "image/png", Bytes: []byte{9},
	}); err != nil {
		t.Fatalf("save upload: %v", err)
	}

	checks, err := st.LogoChecks()
	if err != nil {
		t.Fatalf("checks: %v", err)
	}
	if checks["AAPL"].ETag != `"v1"` || checks["AAPL"].LastModified == "" {
		t.Errorf("AAPL's validators came back as %+v", checks["AAPL"])
	}
	if len(checks["AAPL"].Bytes) == 0 {
		t.Error("the stored bytes are missing; a source with no validators could not be " +
			"compared against what we already have")
	}
	if _, listed := checks["MINE"]; listed {
		t.Error("an uploaded logo is listed for re-checking; it is never asked about")
	}
}
