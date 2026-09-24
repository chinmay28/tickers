// Package archive owns the market-data archive: OHLCV bars for every symbol
// the collector tracks, where each bar came from, the corporate actions that
// explain them, and a ledger of what has been fetched.
//
// The archive is a *folder*, not a file, and not tables in the main database.
// It grows by tens of gigabytes a year where the main database stays
// kilobytes, so it has to be able to live somewhere else — an external drive
// on the Pi — and the upgrade rollback, which snapshots and health-checks the
// main database, must never wait on it or fail because of it.
//
// Inside the folder:
//
//	tickers-archive.json      the marker: this folder is an archive
//	catalog.sqlite            symbols, cursors, the day ledger, splits, dividends
//	bars/1d/all.sqlite        every daily bar
//	bars/1m/2026.sqlite       one file per intraday interval per year
//
// Intraday bars are split by year so a finished year is a file that never
// changes again: it can be backed up once, copied to another disk, or deleted
// without touching the rest. Daily bars are few enough to stay in one file.
//
// This package is the only thing that opens any of these files. It validates
// and persists; the collector decides what to fetch.
package archive

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chinmay28/tickers/server/internal/quotes"
	_ "modernc.org/sqlite"
)

// MarkerFile names the file that makes a folder an archive.
const MarkerFile = "tickers-archive.json"

// ErrUnavailable means there is no archive at the path: the folder is missing,
// or it has no marker. It is what an unplugged drive looks like — the mount
// point is still there, empty — and it is why Open never creates anything.
// Writing into an empty mount point would fill the SD card underneath it.
var ErrUnavailable = errors.New("archive unavailable")

// marker is the marker file's content. Nothing reads the fields at runtime;
// they are there so a person looking at a drive knows what the folder is.
type marker struct {
	Format  int    `json:"format"`
	Created string `json:"created"`
	Note    string `json:"note"`
}

// Init makes an existing folder an archive by writing the marker. The folder
// must already exist: creating it would make an unmounted drive's mount point
// look like a place to put things. Initialising a folder that already is an
// archive is a no-op.
func Init(root string) error {
	root = filepath.Clean(root)
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("%s does not exist; create the folder (or mount the drive) first", root)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a folder", root)
	}
	path := filepath.Join(root, MarkerFile)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	body, _ := json.MarshalIndent(marker{
		Format:  1,
		Created: time.Now().UTC().Format(time.RFC3339),
		Note:    "Market-data archive written by tickers. Do not edit the files in this folder by hand.",
	}, "", "  ")
	if err := os.WriteFile(path, append(body, '\n'), 0o640); err != nil {
		return fmt.Errorf("cannot write to %s: %w", root, err)
	}
	return nil
}

// IsArchive reports whether root holds an archive.
func IsArchive(root string) bool {
	_, err := os.Stat(filepath.Join(root, MarkerFile))
	return err == nil
}

// Archive is a handle on an open archive. It is safe for concurrent use; the
// collector is its one writer and the API reads alongside it.
type Archive struct {
	root    string
	catalog *sql.DB

	mu         sync.Mutex
	partitions map[string]*sql.DB
	sources    map[string]int64
}

// Open opens the archive at root, applying any pending migrations and
// finishing any split rescale a crash interrupted.
func Open(root string) (*Archive, error) {
	root = filepath.Clean(root)
	if !IsArchive(root) {
		return nil, fmt.Errorf("%w: %s is not an archive folder (is the drive mounted?)", ErrUnavailable, root)
	}
	if err := os.MkdirAll(filepath.Join(root, "bars"), 0o750); err != nil {
		return nil, fmt.Errorf("archive: %w", err)
	}
	db, err := openDB(filepath.Join(root, "catalog.sqlite"), catalogMigrations)
	if err != nil {
		return nil, err
	}
	a := &Archive{root: root, catalog: db, partitions: map[string]*sql.DB{}, sources: map[string]int64{}}
	if err := a.resumeSplits(); err != nil {
		a.Close()
		return nil, err
	}
	return a, nil
}

// Root is the folder the archive lives in.
func (a *Archive) Root() string { return a.root }

// Close releases every file handle.
func (a *Archive) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	var first error
	for key, db := range a.partitions {
		if err := db.Close(); err != nil && first == nil {
			first = err
		}
		delete(a.partitions, key)
	}
	if err := a.catalog.Close(); err != nil && first == nil {
		first = err
	}
	return first
}

// Ping checks the catalog is still readable — what an unplugged drive fails.
func (a *Archive) Ping() error {
	if !IsArchive(a.root) {
		return fmt.Errorf("%w: the marker file at %s has gone (was the drive unplugged?)", ErrUnavailable, a.root)
	}
	return a.catalog.Ping()
}

// openDB opens one SQLite file with the pragmas every file here uses: WAL so a
// reader never blocks the collector, busy_timeout so a second process waits
// instead of failing, one connection so writes queue in Go.
func openDB(path string, migrations []migration) (*sql.DB, error) {
	dsn := path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("archive: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("archive: open %s: %w", path, err)
	}
	if err := migrate(db, migrations); err != nil {
		db.Close()
		return nil, fmt.Errorf("archive: %s: %w", filepath.Base(path), err)
	}
	return db, nil
}

// ---------------------------------------------------------------------------
// Partitions
// ---------------------------------------------------------------------------

// partitionKey is where a bar lives: "1d/all" for daily bars, "1m/2026" for
// intraday ones.
func partitionKey(i quotes.Interval, t time.Time) string {
	if !i.Intraday() {
		return string(i) + "/all"
	}
	return fmt.Sprintf("%s/%04d", i, t.UTC().Year())
}

// partitionKeys lists every key an interval's bars in [from, to) could be in.
func partitionKeys(i quotes.Interval, from, to time.Time) []string {
	if !i.Intraday() {
		return []string{partitionKey(i, from)}
	}
	var keys []string
	for y := from.UTC().Year(); y <= to.UTC().Year(); y++ {
		keys = append(keys, fmt.Sprintf("%s/%04d", i, y))
	}
	return keys
}

func (a *Archive) partitionPath(key string) string {
	return filepath.Join(a.root, "bars", filepath.FromSlash(key)+".sqlite")
}

// partition returns a handle on one partition. With create false, a partition
// that doesn't exist yet comes back nil rather than as an empty file — a read
// of a year nothing was collected in must not litter the drive.
func (a *Archive) partition(key string, create bool) (*sql.DB, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if db, ok := a.partitions[key]; ok {
		return db, nil
	}
	path := a.partitionPath(key)
	if _, err := os.Stat(path); err != nil {
		if !create {
			return nil, nil
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, fmt.Errorf("archive: %w", err)
		}
	}
	db, err := openDB(path, partitionMigrations)
	if err != nil {
		return nil, err
	}
	a.partitions[key] = db
	return db, nil
}

// existingPartitions lists the keys of every partition on disk, sorted.
func (a *Archive) existingPartitions() ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(a.root, "bars", "*", "*.sqlite"))
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(matches))
	for _, m := range matches {
		rel, err := filepath.Rel(filepath.Join(a.root, "bars"), m)
		if err != nil {
			continue
		}
		keys = append(keys, strings.TrimSuffix(filepath.ToSlash(rel), ".sqlite"))
	}
	sort.Strings(keys)
	return keys, nil
}

// ---------------------------------------------------------------------------
// Sources
// ---------------------------------------------------------------------------

// sourceID maps a source's name to the small integer every bar carries. A
// name is five-plus bytes on a billion rows; an ID under 128 is one.
func (a *Archive) sourceID(name string) (int64, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return 0, errors.New("a bar needs a source")
	}
	a.mu.Lock()
	id, ok := a.sources[name]
	a.mu.Unlock()
	if ok {
		return id, nil
	}
	if _, err := a.catalog.Exec(`INSERT OR IGNORE INTO sources (name) VALUES (?)`, name); err != nil {
		return 0, err
	}
	if err := a.catalog.QueryRow(`SELECT id FROM sources WHERE name = ?`, name).Scan(&id); err != nil {
		return 0, err
	}
	a.mu.Lock()
	a.sources[name] = id
	a.mu.Unlock()
	return id, nil
}

// sourceNames maps IDs back to names.
func (a *Archive) sourceNames() (map[int64]string, error) {
	rows, err := a.catalog.Query(`SELECT id, name FROM sources`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Meta
// ---------------------------------------------------------------------------

// SetMeta and Meta keep small facts about the archive itself — when the
// exchange lists were last read, say.
func (a *Archive) SetMeta(key, value string) error {
	_, err := a.catalog.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (a *Archive) Meta(key string) (string, error) {
	var v string
	err := a.catalog.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func fromNull(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.Unix(v.Int64, 0).UTC()
}

func toNull(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Unix()
}
