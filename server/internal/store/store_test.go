package store

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	// A file in the test's temp dir rather than :memory: — the pragmas, the WAL
	// and the migration runner all behave slightly differently against a real
	// file, and that is what production runs on.
	st, err := Open(t.TempDir() + "/test.sqlite")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestOpenAppliesMigrationsAndSeeds(t *testing.T) {
	st := newTestStore(t)

	applied, err := st.AppliedMigrations()
	if err != nil {
		t.Fatalf("applied migrations: %v", err)
	}
	if len(applied) != len(migrations) {
		t.Fatalf("applied %d migrations, want %d", len(applied), len(migrations))
	}

	tickers, err := st.Tickers()
	if err != nil {
		t.Fatalf("tickers: %v", err)
	}
	if len(tickers) != len(SeedSymbols) {
		t.Fatalf("seeded %d tickers, want %d", len(tickers), len(SeedSymbols))
	}
	for i, ticker := range tickers {
		if ticker.Symbol != SeedSymbols[i] {
			t.Errorf("ticker %d is %s, want %s (seed order is the payload order)", i, ticker.Symbol, SeedSymbols[i])
		}
		if !ticker.Pinned {
			t.Errorf("seeded %s should be pinned", ticker.Symbol)
		}
	}

	cfg, err := st.Config()
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if !slices.Equal(cfg.PinnedSymbols, SeedSymbols) {
		t.Errorf("pinned list is %v, want the seeded symbols %v", cfg.PinnedSymbols, SeedSymbols)
	}
}

func TestPinnedTickersSortToTheTop(t *testing.T) {
	st := newTestStore(t)

	// Nothing pinned: the list is exactly `position` order.
	if _, err := st.UpdateConfig(ConfigPatch{PinnedSymbols: &[]string{}}); err != nil {
		t.Fatalf("unpin everything: %v", err)
	}
	unpinned, _ := st.Tickers()
	for i, ticker := range unpinned {
		if ticker.Symbol != SeedSymbols[i] {
			t.Fatalf("with nothing pinned, ticker %d is %s, want %s", i, ticker.Symbol, SeedSymbols[i])
		}
		if ticker.Pinned {
			t.Errorf("%s is flagged pinned after the list was cleared", ticker.Symbol)
		}
	}

	// Pinning lifts rows to the front. The pinned list is a set, so the
	// watchlist's own order still decides the sequence within each group —
	// BTC-USD is last on the watchlist and stays behind P once both are up top.
	pins := []string{"BTC-USD", "P"}
	if _, err := st.UpdateConfig(ConfigPatch{PinnedSymbols: &pins}); err != nil {
		t.Fatalf("pin: %v", err)
	}
	got, err := st.Tickers()
	if err != nil {
		t.Fatalf("tickers: %v", err)
	}
	if got[0].Symbol != "P" || got[1].Symbol != "BTC-USD" {
		t.Fatalf("pinned rows are %s,%s; want P,BTC-USD", got[0].Symbol, got[1].Symbol)
	}
	if !got[0].Pinned || !got[1].Pinned || got[2].Pinned {
		t.Error("Pinned was not stamped to match the configured list")
	}
	var rest []string
	for _, ticker := range got[2:] {
		rest = append(rest, ticker.Symbol)
	}
	if want := []string{"VTI", "GLD", "ORCL", "STRC", "IBIT"}; !slices.Equal(rest, want) {
		t.Errorf("unpinned tail is %v, want %v — relative order was not preserved", rest, want)
	}

	// The refresh loop and the payload see the same order.
	enabled, err := st.EnabledTickers()
	if err != nil {
		t.Fatalf("enabled tickers: %v", err)
	}
	if enabled[0].Symbol != "P" {
		t.Errorf("enabled list starts with %s, want the pinned P", enabled[0].Symbol)
	}

	// A single lookup carries the same flag as the list does.
	if one, err := st.Ticker(got[0].ID); err != nil || !one.Pinned {
		t.Errorf("Ticker(%s).Pinned = %v (err %v), want true", got[0].Symbol, one.Pinned, err)
	}
}

func TestPinnedSymbolsNormalizeAndAreBounded(t *testing.T) {
	st := newTestStore(t)

	messy := []string{" vti ", "", "gld,ibit", "VTI"}
	cfg, err := st.UpdateConfig(ConfigPatch{PinnedSymbols: &messy})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	want := []string{"VTI", "GLD", "IBIT"}
	if !slices.Equal(cfg.PinnedSymbols, want) {
		t.Fatalf("pinned = %v, want %v (upper-cased, comma-split, deduped, blanks dropped)", cfg.PinnedSymbols, want)
	}
	if again, _ := st.Config(); !slices.Equal(again.PinnedSymbols, want) {
		t.Errorf("pinned list did not persist: %v", again.PinnedSymbols)
	}

	// Clearing must stay cleared rather than reading back as the seeded default.
	if _, err := st.UpdateConfig(ConfigPatch{PinnedSymbols: &[]string{}}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if again, _ := st.Config(); len(again.PinnedSymbols) != 0 {
		t.Errorf("cleared pinned list came back as %v", again.PinnedSymbols)
	}

	tooMany := make([]string, MaxPinnedSymbols+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("SYM%d", i)
	}
	if _, err := st.UpdateConfig(ConfigPatch{PinnedSymbols: &tooMany}); err == nil {
		t.Error("a pinned list over the cap was accepted")
	}
	if again, _ := st.Config(); len(again.PinnedSymbols) != 0 {
		t.Errorf("a rejected pin list still wrote %v", again.PinnedSymbols)
	}
}

func TestMigrationPinsAnExistingInstallsSeededSymbols(t *testing.T) {
	path := t.TempDir() + "/legacy.sqlite"

	// Stand up a pre-pinning database: schema at 001, seeded, no pinned key.
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := st.db.Exec(`DELETE FROM settings WHERE key = ?`, SettingPinnedSymbols); err != nil {
		t.Fatalf("clear pinned key: %v", err)
	}
	if _, err := st.db.Exec(`DELETE FROM schema_migrations WHERE id = '002_pin_seeded_symbols'`); err != nil {
		t.Fatalf("rewind migration: %v", err)
	}
	// One symbol was replaced by the user, so it must not come back pinned.
	replaced := mustTicker(t, st, "STRC")
	symbol := "AAPL"
	if _, err := st.UpdateTicker(replaced.ID, TickerPatch{Symbol: &symbol}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	st.Close()

	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer upgraded.Close()

	cfg, err := upgraded.Config()
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	want := []string{"VTI", "GLD", "P", "ORCL", "IBIT", "BTC-USD"}
	if !slices.Equal(cfg.PinnedSymbols, want) {
		t.Fatalf("migrated pinned list is %v, want %v", cfg.PinnedSymbols, want)
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	path := t.TempDir() + "/reopen.sqlite"

	first, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := first.DeleteTicker(mustTicker(t, first, "VTI").ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	first.Close()

	// Reopening must neither re-run a migration nor resurrect the seed data —
	// this is the "re-run quickstart.sh to upgrade" path, and it has to leave
	// the user's watchlist alone.
	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()

	tickers, err := second.Tickers()
	if err != nil {
		t.Fatalf("tickers: %v", err)
	}
	if len(tickers) != len(SeedSymbols)-1 {
		t.Fatalf("reopen left %d tickers, want %d — seeding re-ran", len(tickers), len(SeedSymbols)-1)
	}
	for _, ticker := range tickers {
		if ticker.Symbol == "VTI" {
			t.Fatal("deleted ticker came back after reopening")
		}
	}
}

func TestCreateTickerNormalizesAndRejectsDuplicates(t *testing.T) {
	st := newTestStore(t)

	created, err := st.CreateTicker(NewTicker{Symbol: "  aapl  ", Label: " Fruit "})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Symbol != "AAPL" {
		t.Errorf("symbol %q, want AAPL", created.Symbol)
	}
	if created.Label != "Fruit" {
		t.Errorf("label %q, want Fruit", created.Label)
	}
	if created.Origin != OriginUser {
		t.Errorf("origin %q, want %q", created.Origin, OriginUser)
	}

	if _, err := st.CreateTicker(NewTicker{Symbol: "aapl"}); !errors.Is(err, ErrDuplicateSymbol) {
		t.Fatalf("duplicate create returned %v, want ErrDuplicateSymbol", err)
	}
	if _, err := st.CreateTicker(NewTicker{Symbol: "   "}); err == nil {
		t.Fatal("empty symbol was accepted")
	}
}

func TestUpdateTickerPromotesOriginAndDropsStaleQuote(t *testing.T) {
	st := newTestStore(t)
	seeded := mustTicker(t, st, "P")

	price := 42.5
	if err := st.SaveQuote(Quote{
		TickerID: seeded.ID, Symbol: "P", Price: &price,
		Status: StatusOK, FetchedAt: time.Now(),
	}); err != nil {
		t.Fatalf("save quote: %v", err)
	}

	symbol := "MSFT"
	updated, err := st.UpdateTicker(seeded.ID, TickerPatch{Symbol: &symbol})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Symbol != "MSFT" {
		t.Errorf("symbol %q, want MSFT", updated.Symbol)
	}
	if updated.Origin != OriginUser {
		t.Errorf("origin %q after a replacement, want %q", updated.Origin, OriginUser)
	}
	// Pins are keyed by symbol, so retyping a pinned row's symbol unpins it.
	if updated.Pinned {
		t.Error("the new symbol isn't on the pinned list, so the row must not be pinned")
	}

	quotes, err := st.Quotes()
	if err != nil {
		t.Fatalf("quotes: %v", err)
	}
	if _, ok := quotes[seeded.ID]; ok {
		t.Error("the old symbol's quote survived the replacement")
	}
}

func TestUpdateTickerLabelOnlyKeepsQuoteAndOrigin(t *testing.T) {
	st := newTestStore(t)
	seeded := mustTicker(t, st, "GLD")

	price := 200.0
	if err := st.SaveQuote(Quote{
		TickerID: seeded.ID, Symbol: "GLD", Price: &price,
		Status: StatusOK, FetchedAt: time.Now(),
	}); err != nil {
		t.Fatalf("save quote: %v", err)
	}

	label := "Shiny"
	updated, err := st.UpdateTicker(seeded.ID, TickerPatch{Label: &label})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Origin != OriginSeed {
		t.Error("relabelling shouldn't promote the origin — only replacing the symbol does")
	}
	if !updated.Pinned {
		t.Error("relabelling dropped the pin; only a symbol change should")
	}

	quotes, _ := st.Quotes()
	if _, ok := quotes[seeded.ID]; !ok {
		t.Error("relabelling dropped the quote; only a symbol change should")
	}
}

func TestReorderTickersPlacesUnlistedIDsAfter(t *testing.T) {
	st := newTestStore(t)
	tickers, _ := st.Tickers()

	// Reorder only the last two; everything unnamed should follow, in its old
	// relative order.
	last := tickers[len(tickers)-1]
	secondLast := tickers[len(tickers)-2]
	if err := st.ReorderTickers([]string{last.ID, secondLast.ID}); err != nil {
		t.Fatalf("reorder: %v", err)
	}

	got, _ := st.Tickers()
	if got[0].ID != last.ID || got[1].ID != secondLast.ID {
		t.Fatalf("reorder put %s,%s first, want %s,%s",
			got[0].Symbol, got[1].Symbol, last.Symbol, secondLast.Symbol)
	}
	for i, ticker := range got[2:] {
		if ticker.ID != tickers[i].ID {
			t.Fatalf("unlisted ticker %d is %s, want %s — relative order was not preserved",
				i, ticker.Symbol, tickers[i].Symbol)
		}
	}

	if err := st.ReorderTickers([]string{"nonexistent"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reorder with unknown id returned %v, want ErrNotFound", err)
	}
}

func TestSaveQuoteRecordsHistoryOnlyForGoodReads(t *testing.T) {
	st := newTestStore(t)
	ticker := mustTicker(t, st, "VTI")

	price := 100.0
	for i := range 3 {
		p := price + float64(i)
		if err := st.SaveQuote(Quote{
			TickerID: ticker.ID, Symbol: "VTI", Price: &p,
			Status: StatusOK, FetchedAt: time.Now().Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	if err := st.SaveQuote(Quote{
		TickerID: ticker.ID, Symbol: "VTI", Status: StatusError,
		Error: "boom", FetchedAt: time.Now().Add(4 * time.Minute),
	}); err != nil {
		t.Fatalf("save error quote: %v", err)
	}

	points, err := st.History("vti", 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(points) != 3 {
		t.Fatalf("history has %d points, want 3 (failures must not be plotted)", len(points))
	}
	if points[0].Price != 100 || points[2].Price != 102 {
		t.Errorf("history is not oldest-first: %v", points)
	}

	// The latest reading is the failure, and it replaced the good one.
	quotes, _ := st.Quotes()
	if got := quotes[ticker.ID]; got.Status != StatusError || got.Error != "boom" {
		t.Errorf("latest quote is %+v, want the recorded failure", got)
	}
}

func TestPruneHistory(t *testing.T) {
	st := newTestStore(t)
	ticker := mustTicker(t, st, "VTI")

	old := 1.0
	recent := 2.0
	_ = st.SaveQuote(Quote{TickerID: ticker.ID, Symbol: "VTI", Price: &old,
		Status: StatusOK, FetchedAt: time.Now().Add(-48 * time.Hour)})
	_ = st.SaveQuote(Quote{TickerID: ticker.ID, Symbol: "VTI", Price: &recent,
		Status: StatusOK, FetchedAt: time.Now()})

	removed, err := st.PruneHistory(24 * time.Hour)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if removed != 1 {
		t.Fatalf("pruned %d rows, want 1", removed)
	}

	// A zero window means "keep everything", not "delete everything".
	if removed, err := st.PruneHistory(0); err != nil || removed != 0 {
		t.Fatalf("prune(0) removed %d (err %v), want 0 — zero must disable pruning", removed, err)
	}
}

func TestSinkValidation(t *testing.T) {
	st := newTestStore(t)

	for name, in := range map[string]NewSink{
		"no url":       {Key: "k"},
		"no key":       {BaseURL: "http://host/api"},
		"bad scheme":   {BaseURL: "file:///etc/passwd", Key: "k"},
		"no host":      {BaseURL: "http://", Key: "k"},
		"slash in key": {BaseURL: "http://host/api", Key: "a/b"},
		"bad format":   {BaseURL: "http://host/api", Key: "k", Format: "yaml"},
	} {
		if _, err := st.CreateSink(in); err == nil {
			t.Errorf("%s: sink was accepted, want a validation error", name)
		}
	}

	sink, err := st.CreateSink(NewSink{
		BaseURL: "http://host:9999/api/entries/", Key: "minion-quotes", Category: "minion",
	})
	if err != nil {
		t.Fatalf("create valid sink: %v", err)
	}
	if sink.BaseURL != "http://host:9999/api/entries" {
		t.Errorf("trailing slash not trimmed: %q", sink.BaseURL)
	}
	if sink.Name != "minion-quotes" {
		t.Errorf("name defaulted to %q, want the key", sink.Name)
	}
	if sink.Format != FormatMinion {
		t.Errorf("format defaulted to %q, want %q", sink.Format, FormatMinion)
	}
	if sink.TimeoutMS != 10000 {
		t.Errorf("timeout defaulted to %d, want 10000", sink.TimeoutMS)
	}
}

func TestConfigRoundTripAndFloor(t *testing.T) {
	st := newTestStore(t)

	// A fresh store has the seeded symbols pinned; everything else is default.
	fresh := DefaultConfig()
	fresh.PinnedSymbols = SeedSymbols
	if cfg, _ := st.Config(); !reflect.DeepEqual(cfg, fresh) {
		t.Fatalf("fresh config is %+v, want %+v", cfg, fresh)
	}

	tooFast := 5
	if _, err := st.UpdateConfig(ConfigPatch{RefreshSeconds: &tooFast}); err == nil {
		t.Fatal("an interval below the floor was accepted")
	}

	seconds := 90
	publish := false
	cfg, err := st.UpdateConfig(ConfigPatch{RefreshSeconds: &seconds, PublishOnRefresh: &publish})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if cfg.RefreshSeconds != 90 || cfg.PublishOnRefresh {
		t.Fatalf("config is %+v after update", cfg)
	}
	if cfg.HistoryHours != DefaultConfig().HistoryHours {
		t.Error("an untouched field changed")
	}
	if again, _ := st.Config(); !reflect.DeepEqual(again, cfg) {
		t.Fatalf("config did not persist: %+v vs %+v", again, cfg)
	}
}

func TestAppendRunPrunesToRunKeep(t *testing.T) {
	st := newTestStore(t)

	for i := range RunKeep + 5 {
		if _, err := st.AppendRun(Run{
			StartedAt: time.Now(), FinishedAt: time.Now(),
			Trigger: TriggerSchedule, OKCount: i,
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	runs, err := st.Runs(RunKeep)
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	if len(runs) != RunKeep {
		t.Fatalf("kept %d runs, want %d", len(runs), RunKeep)
	}
	if runs[0].OKCount != RunKeep+4 {
		t.Errorf("newest run has okCount %d, want %d — ordering is wrong", runs[0].OKCount, RunKeep+4)
	}
}

func TestRunPublishResultsSurviveRoundTrip(t *testing.T) {
	st := newTestStore(t)

	saved, err := st.AppendRun(Run{
		StartedAt: time.Now(), FinishedAt: time.Now(), Trigger: TriggerManual,
		Publishes: []PublishResult{{SinkName: "Home", Method: "POST", StatusCode: 201, OK: true}},
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}

	last, ok, err := st.LastRun()
	if err != nil || !ok {
		t.Fatalf("last run: ok=%v err=%v", ok, err)
	}
	if last.ID != saved.ID || len(last.Publishes) != 1 || last.Publishes[0].Method != "POST" {
		t.Fatalf("round trip lost detail: %+v", last)
	}
}

func TestQuoteChange(t *testing.T) {
	price, prev := 110.0, 100.0
	q := Quote{Price: &price, PreviousClose: &prev}

	if change, ok := q.Change(); !ok || change != 10 {
		t.Errorf("Change() = %v, %v; want 10, true", change, ok)
	}
	if pct, ok := q.ChangePercent(); !ok || pct != 10 {
		t.Errorf("ChangePercent() = %v, %v; want 10, true", pct, ok)
	}

	// A missing or zero previous close means there is no change to report —
	// not a change of zero, and certainly not a division by zero.
	zero := 0.0
	for name, q := range map[string]Quote{
		"no price": {PreviousClose: &prev},
		"no close": {Price: &price},
		"zero":     {Price: &price, PreviousClose: &zero},
	} {
		if _, ok := q.Change(); ok {
			t.Errorf("%s: Change() reported a value it can't know", name)
		}
	}
}

func mustTicker(t *testing.T, st *Store, symbol string) Ticker {
	t.Helper()
	tickers, err := st.Tickers()
	if err != nil {
		t.Fatalf("tickers: %v", err)
	}
	for _, ticker := range tickers {
		if ticker.Symbol == symbol {
			return ticker
		}
	}
	t.Fatalf("no ticker %s on the watchlist", symbol)
	return Ticker{}
}

func TestQuoteSourceSettingsRoundTripAndValidate(t *testing.T) {
	st := newTestStore(t)

	// Defaults are all "unset", meaning "use whatever the provider decides".
	cfg, _ := st.Config()
	if cfg.QuoteBaseURL != "" || cfg.QuoteUserAgent != "" || cfg.QuoteTimeoutSeconds != 0 {
		t.Fatalf("fresh quote settings are not empty: %+v", cfg)
	}

	url := "https://quotes.example.com/api/"
	ua := "Mozilla/5.0 (custom)"
	timeout := 30
	cfg, err := st.UpdateConfig(ConfigPatch{
		QuoteBaseURL: &url, QuoteUserAgent: &ua, QuoteTimeoutSeconds: &timeout,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if cfg.QuoteBaseURL != "https://quotes.example.com/api" {
		t.Errorf("base URL = %q; the trailing slash should be trimmed", cfg.QuoteBaseURL)
	}
	if cfg.QuoteUserAgent != ua || cfg.QuoteTimeoutSeconds != 30 {
		t.Errorf("quote settings = %+v", cfg)
	}
	if cfg.QuoteTimeout() != 30*time.Second {
		t.Errorf("QuoteTimeout() = %v, want 30s", cfg.QuoteTimeout())
	}
	if again, _ := st.Config(); !reflect.DeepEqual(again, cfg) {
		t.Fatalf("quote settings did not persist: %+v vs %+v", again, cfg)
	}

	// Clearing is how you ask for the default back, so "" must be accepted and
	// must survive the round trip as "" rather than being ignored.
	empty := ""
	zero := 0
	cfg, err = st.UpdateConfig(ConfigPatch{
		QuoteBaseURL: &empty, QuoteUserAgent: &empty, QuoteTimeoutSeconds: &zero,
	})
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if cfg.QuoteBaseURL != "" || cfg.QuoteUserAgent != "" || cfg.QuoteTimeoutSeconds != 0 {
		t.Errorf("clearing left values behind: %+v", cfg)
	}
	if again, _ := st.Config(); again.QuoteBaseURL != "" {
		t.Errorf("cleared base URL came back as %q", again.QuoteBaseURL)
	}
}

func TestQuoteSourceSettingsRejectBadInput(t *testing.T) {
	st := newTestStore(t)

	bad := "file:///etc/passwd"
	if _, err := st.UpdateConfig(ConfigPatch{QuoteBaseURL: &bad}); err == nil {
		t.Error("a file:// quote URL was accepted — that is an arbitrary-read primitive")
	}
	noHost := "http://"
	if _, err := st.UpdateConfig(ConfigPatch{QuoteBaseURL: &noHost}); err == nil {
		t.Error("a hostless quote URL was accepted")
	}

	tooShort := MinQuoteTimeout - 1
	if _, err := st.UpdateConfig(ConfigPatch{QuoteTimeoutSeconds: &tooShort}); err == nil {
		t.Error("a timeout below the floor was accepted")
	}
	tooLong := MaxQuoteTimeout + 1
	if _, err := st.UpdateConfig(ConfigPatch{QuoteTimeoutSeconds: &tooLong}); err == nil {
		t.Error("a timeout above the ceiling was accepted")
	}

	// A newline in the user agent would let the value inject request headers.
	injected := "curl/8\r\nX-Evil: yes"
	if _, err := st.UpdateConfig(ConfigPatch{QuoteUserAgent: &injected}); err == nil {
		t.Error("a user agent containing CRLF was accepted")
	}
	long := strings.Repeat("a", MaxUserAgentLen+1)
	if _, err := st.UpdateConfig(ConfigPatch{QuoteUserAgent: &long}); err == nil {
		t.Error("an over-long user agent was accepted")
	}

	// None of the rejections may have partially written.
	if cfg, _ := st.Config(); cfg.QuoteBaseURL != "" || cfg.QuoteUserAgent != "" || cfg.QuoteTimeoutSeconds != 0 {
		t.Errorf("a rejected patch still changed stored settings: %+v", cfg)
	}
}
