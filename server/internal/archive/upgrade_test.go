package archive

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"

	"github.com/chinmay28/tickers/server/internal/quotes"
)

// These pin what an upgrade relies on. The quick start stops the old server,
// starts the new one and rolls back to the old binary if the new one fails its
// health check — and the archive, tens of gigabytes, is never snapshotted
// along the way. So it has to survive a writer killed mid-write, a second
// process arriving before the first has gone, and being opened by an older
// binary after a newer one has migrated it.

func TestASecondWriterIsRefusedUntilTheFirstCloses(t *testing.T) {
	a := newTestArchive(t)
	track(t, a, User, "AAPL")
	record(t, a, Batch{SymbolID: idOf(t, a, "AAPL"), Interval: quotes.OneMinute, Source: "yahoo",
		Series: quotes.CandleSeries{Candles: minutes(session(1), 3, 100)}})

	if b, err := Open(a.Root()); !errors.Is(err, ErrLocked) {
		if b != nil {
			b.Close()
		}
		t.Fatalf("a second writer opened an archive the first still had open (err %v); two collectors would interleave splits and ledgers", err)
	}

	// A reader needs no lock, and reads what the writer has committed.
	r, err := OpenReadOnly(a.Root())
	if err != nil {
		t.Fatalf("read-only open beside a writer: %v", err)
	}
	got, err := r.Candles("AAPL", quotes.OneMinute, day(0), day(3))
	if err != nil || len(got) != 3 {
		t.Errorf("read-only handle read %d bars (%v), want the writer's 3", len(got), err)
	}
	if err := r.Add(User, Entry{Symbol: "MSFT"}, t0); err == nil {
		t.Error("a read-only handle wrote to the catalog")
	}
	if _, err := r.partition("1m/2031", true); err == nil {
		t.Error("a read-only handle created a partition")
	}
	r.Close()

	if err := a.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	b, err := Open(a.Root())
	if err != nil {
		t.Fatalf("the lock outlived the writer that held it: %v", err)
	}
	b.Close()
}

// A read-only open must not do the writer's startup work: resuming a pending
// split beside a live collector is exactly the double rescale the lock exists
// to prevent.
func TestReadOnlyOpenLeavesAPendingSplitToTheWriter(t *testing.T) {
	a := newTestArchive(t)
	track(t, a, User, "AAPL")
	id := idOf(t, a, "AAPL")
	record(t, a, Batch{SymbolID: id, Interval: quotes.OneMinute, Source: "yahoo",
		Series: quotes.CandleSeries{Candles: minutes(session(1), 1, 400)}})
	if _, err := a.catalog.write.Exec(`INSERT INTO splits (symbol_id, ts, numerator, denominator, state) VALUES (?, ?, 2, 1, 'pending')`,
		id, day(2).Unix()); err != nil {
		t.Fatal(err)
	}

	r, err := OpenReadOnly(a.Root())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, _ := r.Candles("AAPL", quotes.OneMinute, day(0), day(3))
	if len(got) != 1 || got[0].Close != 400 {
		t.Errorf("read-only open rescaled a pending split: %+v", got)
	}
}

// Record writes bars, then the ledger and cursor, in two files. A kill between
// the two commits leaves bars no cursor claims; the collector asks for the
// same window again and the retry must land the archive exactly where an
// uninterrupted write would have.
func TestAWriteInterruptedBetweenFilesIsRepairedByTheRetry(t *testing.T) {
	a := newTestArchive(t)
	track(t, a, User, "AAPL")
	id := idOf(t, a, "AAPL")
	batch := Batch{SymbolID: id, Interval: quotes.OneMinute, Source: "yahoo",
		Series: quotes.CandleSeries{Candles: minutes(session(1), 5, 100)},
		Cursor: &Cursor{Oldest: day(1), Newest: day(2)}}

	// The bars commit; the process dies before the catalog does.
	sid, err := a.sourceID("yahoo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.writeBars(batch, sid); err != nil {
		t.Fatal(err)
	}
	root := a.Root()
	a.Close()

	b, err := Open(root)
	if err != nil {
		t.Fatalf("reopen after the interrupted write: %v", err)
	}
	defer b.Close()
	if cursors, _ := b.SymbolCursors(id); len(cursors) != 0 {
		t.Fatalf("a cursor claims a window whose write never finished: %+v", cursors)
	}
	record(t, b, batch)

	got, _ := b.Candles("AAPL", quotes.OneMinute, day(0), day(3))
	if len(got) != 5 {
		t.Errorf("after the retry the archive holds %d bars, want the 5 fetched, once each", len(got))
	}
	held, _ := b.HeldDays(id, quotes.OneMinute, day(0), day(3))
	if !held[dayOf(session(1))] {
		t.Error("the retry left the day out of the ledger")
	}
	var bars int64
	b.catalog.read.QueryRow(`SELECT bars FROM days WHERE symbol_id = ? AND interval = '1m'`, id).Scan(&bars)
	if bars != 5 {
		t.Errorf("the ledger counts %d bars for the day, want 5", bars)
	}
	if cursors, _ := b.SymbolCursors(id); len(cursors) != 1 || !cursors[0].Newest.Equal(day(2)) {
		t.Errorf("cursor after the retry = %+v, want the fetched window", cursors)
	}
}

// A rolled-back binary opens the archive a newer one migrated: the rollback
// restores the main database's snapshot, but there is no snapshot of this.
// Additive migrations are what make that work, and this is what additive
// has to mean — a column with a default or NULL, a table the old code never
// names — checked against the code as it stands.
func TestAnArchiveMigratedByANewerVersionStillOpensAndWrites(t *testing.T) {
	a := newTestArchive(t)
	track(t, a, User, "AAPL")
	id := idOf(t, a, "AAPL")
	record(t, a, Batch{SymbolID: id, Interval: quotes.OneMinute, Source: "yahoo",
		Series: quotes.CandleSeries{Candles: minutes(session(1), 2, 100)}})
	part, _ := a.partition("1m/2026", false)
	for _, stmt := range []string{
		`ALTER TABLE symbols ADD COLUMN future TEXT`,
		`CREATE TABLE future_things (id INTEGER PRIMARY KEY)`,
		`INSERT INTO schema_migrations (id, applied_at) VALUES ('999_future', 'later')`,
	} {
		if _, err := a.catalog.write.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	for _, stmt := range []string{
		`ALTER TABLE bars ADD COLUMN future REAL NOT NULL DEFAULT 0`,
		`INSERT INTO schema_migrations (id, applied_at) VALUES ('999_future', 'later')`,
	} {
		if _, err := part.write.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	root := a.Root()
	a.Close()

	b, err := Open(root)
	if err != nil {
		t.Fatalf("this version cannot open an archive a newer one migrated: %v", err)
	}
	defer b.Close()
	track(t, b, User, "MSFT")
	record(t, b, Batch{SymbolID: id, Interval: quotes.OneMinute, Source: "yahoo",
		Series: quotes.CandleSeries{Candles: minutes(session(2), 2, 101)}})
	got, err := b.Candles("AAPL", quotes.OneMinute, day(0), day(4))
	if err != nil || len(got) != 4 {
		t.Errorf("read %d bars (%v) from a newer archive, want 4", len(got), err)
	}
	if _, err := b.Stats(); err != nil {
		t.Errorf("stats over a newer archive: %v", err)
	}
}

// Shipped migrations are append-only, as the main database's are: a rolled-
// back binary and a newer one must agree on what every applied ID did. A
// changed hash here means a shipped migration was edited — add a new one.
func TestShippedArchiveMigrationsAreNeverEdited(t *testing.T) {
	shipped := map[string]string{
		"catalog/001_initial":               "82e1d690d4d84a63c82a3d151ad3e506491ee150e42547ebaf3db0a994e4de76",
		"catalog/002_aliases":               "b8072900ab9b983117ec997f77646d9c5a298ee3fcbb448358472fce3ae7c09d",
		"partition/001_initial":             "cf6ae3100afa70a4cc4195f1e917058ac266fc45ce1a89263771c113207dad12",
		"partition/002_vwap_trades_session": "6b8069b2ed3b480f7c4715f7e0fe260533af59e5e35dc5e4b69ad712764cce0c",
	}
	seen := map[string]bool{}
	check := func(kind string, list []migration) {
		for _, m := range list {
			key := kind + "/" + m.ID
			seen[key] = true
			want, ok := shipped[key]
			if !ok {
				continue // new, and pinned here once it ships
			}
			if got := fmt.Sprintf("%x", sha256.Sum256([]byte(m.SQL))); got != want {
				t.Errorf("shipped migration %s was edited (sha256 %s, was %s); add a new migration instead", key, got, want)
			}
		}
	}
	check("catalog", catalogMigrations)
	check("partition", partitionMigrations)
	for key := range shipped {
		if !seen[key] {
			t.Errorf("shipped migration %s was removed or renamed; an archive that applied it would be read by code that doesn't know it", key)
		}
	}
}
