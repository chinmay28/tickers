package archiver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chinmay28/tickers/server/internal/archive"
	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/store"
)

type emptySource struct{}

func (emptySource) Candles(context.Context, string, quotes.Interval, time.Time, time.Time) (quotes.CandleSeries, error) {
	return quotes.CandleSeries{}, nil
}

func newManager(t *testing.T, fallback string) (*Manager, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "tickers.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	// Paused and off the exchange lists: these tests are about the folder,
	// not about fetching.
	paused, listed := true, false
	if _, err := st.UpdateArchiveConfig(store.ArchivePatch{Paused: &paused, Listed: &listed}); err != nil {
		t.Fatal(err)
	}
	m := New(Options{Store: st, Yahoo: emptySource{}, FallbackPath: fallback,
		Symbols: func() ([]string, error) { return []string{"VTI"}, nil }})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return m, st
}

func waitFor(t *testing.T, m *Manager, what string, ok func(Status) bool) Status {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := m.Status(true)
		if ok(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; status %+v", what, st)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func isState(s string) func(Status) bool { return func(st Status) bool { return st.State == s } }

func TestNoFolderMeansOffAndReadsFallBack(t *testing.T) {
	m, _ := newManager(t, "")
	waitFor(t, m, "off", isState(StateOff))
	if err := m.Read(func(*archive.Archive) error { return nil }); !errors.Is(err, ErrNotOpen) {
		t.Errorf("a read with no archive gave %v, want ErrNotOpen", err)
	}
}

func TestUsingAFolderStartsAnArchiveAndCollectsTheApp(t *testing.T) {
	m, st := newManager(t, "")
	dir := t.TempDir()
	if err := m.Use(dir); err != nil {
		t.Fatalf("use: %v", err)
	}
	status := waitFor(t, m, "open with the app's symbols tracked", func(s Status) bool {
		return s.State == StateOpen && s.Stats != nil && s.Stats.Lists[archive.Watchlist] == 1 &&
			s.Collector != nil && s.Collector.State == "paused"
	})
	if status.Path != dir || !archive.IsArchive(dir) {
		t.Errorf("status %+v, want the new folder initialised and open", status)
	}
	if cfg, _ := st.ArchiveConfig(); cfg.Path != dir {
		t.Errorf("stored path = %q", cfg.Path)
	}
	if status.Collector == nil || status.Collector.State != "paused" {
		t.Errorf("collector = %+v, want running and paused", status.Collector)
	}
}

func TestAMissingDriveIsUnavailableUntilItComesBack(t *testing.T) {
	dir := t.TempDir()
	m, _ := newManager(t, dir) // the flag names a folder that is not an archive
	st := waitFor(t, m, "unavailable", isState(StateUnavailable))
	if !st.FromFlag || st.Error == "" {
		t.Errorf("status = %+v, want the flag's folder reported with the reason", st)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("an unavailable folder was written to: %d entries — an empty mount point must stay empty", len(entries))
	}
	// The drive comes back.
	if err := archive.Init(dir); err != nil {
		t.Fatal(err)
	}
	m.Nudge()
	waitFor(t, m, "open", isState(StateOpen))
}

func TestMovingCopiesEverythingAndSwitches(t *testing.T) {
	m, st := newManager(t, "")
	from := t.TempDir()
	if err := m.Use(from); err != nil {
		t.Fatal(err)
	}
	// Open is not the same as having tracked the watchlist: that is the
	// collector's first step, and reads no longer queue behind it.
	var vti archive.Symbol
	waitFor(t, m, "open with the watchlist tracked", func(s Status) bool {
		return s.State == StateOpen && m.Read(func(a *archive.Archive) (err error) {
			vti, err = a.Lookup("VTI")
			return err
		}) == nil
	})
	// Put some bars in so there is something to copy.
	if err := m.Read(func(a *archive.Archive) error {
		return a.Record(archive.Batch{SymbolID: vti.ID, Interval: quotes.OneMinute, Source: "yahoo",
			Series: quotes.CandleSeries{Candles: []quotes.Candle{{Time: time.Date(2026, 1, 5, 14, 30, 0, 0, time.UTC), Open: 1, High: 1, Low: 1, Close: 1}}}})
	}); err != nil {
		t.Fatalf("record a bar to move: %v", err)
	}

	full := t.TempDir()
	os.WriteFile(filepath.Join(full, "photo.jpg"), []byte("x"), 0o600)
	if err := m.MoveTo(full); err == nil {
		t.Error("moved into a folder with other files in it")
	}
	if err := m.MoveTo(filepath.Join(from, "bars")); err == nil {
		t.Error("moved into a folder inside the archive")
	}

	to := t.TempDir()
	if err := m.MoveTo(to); err != nil {
		t.Fatalf("move: %v", err)
	}
	status := waitFor(t, m, "moved and reopened", func(s Status) bool {
		return s.Move != nil && s.Move.State == MoveDone && s.State == StateOpen && s.Path == to
	})
	if status.Move.Copied != status.Move.Total || status.Move.Total == 0 {
		t.Errorf("copied %d of %d bytes", status.Move.Copied, status.Move.Total)
	}
	if cfg, _ := st.ArchiveConfig(); cfg.Path != to {
		t.Errorf("stored path = %q after the move", cfg.Path)
	}
	if !archive.IsArchive(from) {
		t.Error("the old folder was changed; a move leaves it for the user to delete")
	}
	var bars int
	m.Read(func(a *archive.Archive) error {
		got, err := a.Candles("VTI", quotes.OneMinute, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
		bars = len(got)
		return err
	})
	if bars != 1 {
		t.Errorf("the moved archive has %d bars, want the one written before the move", bars)
	}
}

func TestInspectExplainsWhatIsWrong(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	os.WriteFile(file, nil, 0o600)
	cases := map[string]string{
		"relative/path":               "absolute",
		filepath.Join(dir, "missing"): "does not exist",
		file:                          "not a folder",
	}
	for path, want := range cases {
		if f := Inspect(path); f.Problem == "" || !contains(f.Problem, want) {
			t.Errorf("Inspect(%q).Problem = %q, want it to mention %q", path, f.Problem, want)
		}
	}
	f := Inspect(dir)
	if f.Problem != "" || !f.Writable || !f.Exists || f.IsArchive {
		t.Errorf("Inspect(temp dir) = %+v", f)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func TestUnpluggingAnOpenArchiveIsNoticedWhileIdle(t *testing.T) {
	m, _ := newManager(t, "")
	dir := t.TempDir()
	if err := m.Use(dir); err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, "open", isState(StateOpen))
	// The drive goes, and its marker with it. The collector is paused, so
	// nothing it writes would fail.
	os.Remove(filepath.Join(dir, archive.MarkerFile))
	m.Nudge()
	st := waitFor(t, m, "unavailable", isState(StateUnavailable))
	if !strings.Contains(st.Error, "unplugged") && !strings.Contains(st.Error, "not an archive") {
		t.Errorf("error = %q, want it to say the drive went", st.Error)
	}
	if err := m.Read(func(*archive.Archive) error { return nil }); !errors.Is(err, ErrNotOpen) {
		t.Errorf("a read after unplugging gave %v, want ErrNotOpen so callers fall back", err)
	}
	archive.Init(dir)
	m.Nudge()
	waitFor(t, m, "open again", isState(StateOpen))
}

func TestSwitchingTheArchiveOffClosesItAndOnReopensIt(t *testing.T) {
	m, st := newManager(t, "")
	dir := t.TempDir()
	if err := m.Use(dir); err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, "open", isState(StateOpen))
	off := false
	if _, err := st.UpdateArchiveConfig(store.ArchivePatch{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	m.Nudge()
	status := waitFor(t, m, "disabled", isState(StateDisabled))
	if status.Enabled || status.Path != dir {
		t.Errorf("status = %+v, want off, still showing its folder", status)
	}
	if err := m.Read(func(*archive.Archive) error { return nil }); !errors.Is(err, ErrNotOpen) {
		t.Errorf("a read while off gave %v, want ErrNotOpen", err)
	}
	if !archive.IsArchive(dir) {
		t.Error("switching off touched the folder")
	}
	on := true
	st.UpdateArchiveConfig(store.ArchivePatch{Enabled: &on})
	m.Nudge()
	waitFor(t, m, "open again", isState(StateOpen))
}

// An upgrade starts the new server as the old one lets go. If the old one is
// still finishing its last write, the new one must wait its turn — reported as
// such, not as an unplugged drive — and take over the moment the lock is free,
// without the half minute a missing drive is given.
func TestAnArchiveAnotherProcessHoldsIsWaitedFor(t *testing.T) {
	dir := t.TempDir()
	if err := archive.Init(dir); err != nil {
		t.Fatal(err)
	}
	old, err := archive.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := newManager(t, dir)
	st := waitFor(t, m, "the lock noticed", func(s Status) bool { return s.State == StateUnavailable })
	if !st.Locked || !strings.Contains(st.Error, "another tickers process") {
		t.Errorf("status = %+v, want it reported as locked by another process", st)
	}

	old.Close()
	deadline := time.Now().Add(lockedRetry + 3*time.Second)
	for st = m.Status(false); st.State != StateOpen; st = m.Status(false) {
		if time.Now().After(deadline) {
			t.Fatalf("did not take over a released archive within %s of the retry; status %+v", lockedRetry, st)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if st.Locked {
		t.Error("still reported locked after opening")
	}
}

// Closing the archive — for shutdown or a move — must not wait out a stats
// count: the next process is waiting on the lock this one holds.
func TestClosingAbandonsAStatsCount(t *testing.T) {
	m, st := newManager(t, "")
	dir := t.TempDir()
	if err := m.Use(dir); err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, "open", isState(StateOpen))
	m.Status(true) // starts a count
	off := false
	if _, err := st.UpdateArchiveConfig(store.ArchivePatch{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	m.Nudge()
	waitFor(t, m, "closed", isState(StateDisabled))
	b, err := archive.Open(dir)
	if err != nil {
		t.Fatalf("the lock was not released on close: %v", err)
	}
	b.Close()
}
