package archive

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/chinmay28/tickers/server/internal/quotes"
)

// Candles returns a symbol's stored bars at exactly this interval in
// [from, to), oldest first.
func (a *Archive) Candles(symbol string, interval quotes.Interval, from, to time.Time) ([]quotes.Candle, error) {
	s, err := a.Lookup(symbol)
	if err == ErrUnknownSymbol {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return a.candles(s.ID, interval, from, to)
}

func (a *Archive) candles(id int64, interval quotes.Interval, from, to time.Time) ([]quotes.Candle, error) {
	if interval.Step() == 0 {
		return nil, fmt.Errorf("unknown interval %q", interval)
	}
	var out []quotes.Candle
	for _, key := range partitionKeys(interval, from, to) {
		db, err := a.partition(key, false)
		if err != nil {
			return nil, err
		}
		if db == nil {
			continue
		}
		rows, err := db.Query(`SELECT ts, open, high, low, close, volume FROM bars
			WHERE symbol_id = ? AND ts >= ? AND ts < ? ORDER BY ts`, id, from.Unix(), to.Unix())
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var c quotes.Candle
			var ts int64
			if err := rows.Scan(&ts, &c.Open, &c.High, &c.Low, &c.Close, &c.Volume); err != nil {
				rows.Close()
				return nil, err
			}
			c.Time = time.Unix(ts, 0).UTC()
			out = append(out, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Bars returns the best series the archive can give at an interval: its own
// bars where it has them, and bars built from a finer interval where it
// doesn't. Five-minute bars for a day only the one-minute series reached are
// the one-minute bars, resampled. Daily bars are never built from intraday
// ones — a daily bar is the exchange's official session, which intraday bars
// only approximate.
func (a *Archive) Bars(symbol string, interval quotes.Interval, from, to time.Time) ([]quotes.Candle, error) {
	s, err := a.Lookup(symbol)
	if err == ErrUnknownSymbol {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out, err := a.candles(s.ID, interval, from, to)
	if err != nil || !interval.Intraday() {
		return out, err
	}
	have := map[int64]bool{}
	for _, c := range out {
		have[dayOf(c.Time)] = true
	}
	for _, finer := range quotes.Intervals {
		if !finer.Intraday() || finer.Step() >= interval.Step() {
			continue
		}
		fine, err := a.candles(s.ID, finer, from, to)
		if err != nil {
			return nil, err
		}
		var missing []quotes.Candle
		for _, c := range fine {
			if !have[dayOf(c.Time)] {
				missing = append(missing, c)
			}
		}
		built := Resample(missing, interval)
		for _, c := range built {
			have[dayOf(c.Time)] = true
		}
		out = append(out, built...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}

// Resample builds coarser intraday bars from finer ones.
//
// Buckets are anchored to each day's first bar rather than to the clock. A
// US session opens at 9:30, so an hourly bar runs 9:30–10:30 — which is how
// every provider prints one — and anchoring to the clock would make it
// 9:00–10:00 with half an hour missing. Anchoring to the open also follows
// the session through daylight saving without a timezone database, and gives
// a crypto series, whose day starts at midnight, clock-aligned buckets.
func Resample(candles []quotes.Candle, to quotes.Interval) []quotes.Candle {
	step := int64(to.Step() / time.Second)
	if step == 0 || len(candles) == 0 {
		return nil
	}
	sorted := append([]quotes.Candle(nil), candles...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Time.Before(sorted[j].Time) })

	var out []quotes.Candle
	var day, anchor, bucket int64 = -1, 0, -1
	for _, c := range sorted {
		ts := c.Time.Unix()
		if d := dayOf(c.Time); d != day {
			day, anchor = d, ts
		}
		b := anchor + (ts-anchor)/step*step
		if b != bucket || len(out) == 0 {
			bucket = b
			out = append(out, quotes.Candle{Time: time.Unix(b, 0).UTC(), Open: c.Open, High: c.High, Low: c.Low, Close: c.Close, Volume: c.Volume})
			continue
		}
		cur := &out[len(out)-1]
		cur.High = max(cur.High, c.High)
		cur.Low = min(cur.Low, c.Low)
		cur.Close = c.Close
		cur.Volume += c.Volume
	}
	return out
}

// Dividends returns a symbol's recorded dividends in [from, to).
func (a *Archive) Dividends(symbol string, from, to time.Time) ([]quotes.Dividend, error) {
	rows, err := a.catalog.Query(`SELECT d.ts, d.amount FROM dividends d JOIN symbols s ON s.id = d.symbol_id
		WHERE s.symbol = ? AND d.ts >= ? AND d.ts < ? ORDER BY d.ts`, NormalizeSymbol(symbol), from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []quotes.Dividend
	for rows.Next() {
		var d quotes.Dividend
		var ts int64
		if err := rows.Scan(&ts, &d.Amount); err != nil {
			return nil, err
		}
		d.Time = time.Unix(ts, 0).UTC()
		out = append(out, d)
	}
	return out, rows.Err()
}

// Splits returns a symbol's recorded splits.
func (a *Archive) Splits(symbol string) ([]quotes.Split, error) {
	rows, err := a.catalog.Query(`SELECT sp.ts, sp.numerator, sp.denominator FROM splits sp JOIN symbols s ON s.id = sp.symbol_id
		WHERE s.symbol = ? ORDER BY sp.ts`, NormalizeSymbol(symbol))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []quotes.Split
	for rows.Next() {
		var s quotes.Split
		var ts int64
		if err := rows.Scan(&ts, &s.Numerator, &s.Denominator); err != nil {
			return nil, err
		}
		s.Time = time.Unix(ts, 0).UTC()
		out = append(out, s)
	}
	return out, rows.Err()
}

// HeldDays reports which UTC days in [from, to) already have intraday bars
// for a symbol at an interval — what a gap-filling source skips.
func (a *Archive) HeldDays(id int64, interval quotes.Interval, from, to time.Time) (map[int64]bool, error) {
	rows, err := a.catalog.Query(`SELECT day FROM days WHERE symbol_id = ? AND interval = ? AND day >= ? AND day < ? AND bars > 0`,
		id, string(interval), dayOf(from), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var d int64
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out[d] = true
	}
	return out, rows.Err()
}

// TradingDays lists the UTC days in [from, to) the symbol has a daily bar
// for: its own trading calendar, which is what says whether a day with no
// intraday bars is a gap or a holiday.
func (a *Archive) TradingDays(id int64, from, to time.Time) ([]int64, error) {
	db, err := a.partition(partitionKey(quotes.Daily, from), false)
	if err != nil || db == nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT ts FROM bars WHERE symbol_id = ? AND ts >= ? AND ts < ? ORDER BY ts`, id, dayOf(from), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var ts int64
		if err := rows.Scan(&ts); err != nil {
			return nil, err
		}
		out = append(out, dayOf(time.Unix(ts, 0)))
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Summaries
// ---------------------------------------------------------------------------

// IntervalStats summarises one interval across every active symbol.
type IntervalStats struct {
	Interval quotes.Interval `json:"interval"`
	// Started counts symbols with anything fetched from any source,
	// Complete those some source has walked back as far as it goes, and
	// Failing those whose last attempt at some source failed — including
	// ones that have never once succeeded, which are the ones worth seeing.
	Started  int `json:"started"`
	Complete int `json:"complete"`
	Failing  int `json:"failing"`
	// Oldest is the deepest any series reaches, Newest the freshest, and
	// Stalest the least fresh series' newest point — how far behind the
	// forward pass is.
	Oldest  time.Time `json:"oldest"`
	Newest  time.Time `json:"newest"`
	Stalest time.Time `json:"stalest"`
	// Bars is how many bars are stored. For intraday intervals it is the
	// ledger's exact count; for daily bars it is counted from the file.
	Bars int64 `json:"bars"`
}

// Stats is the archive at a glance.
type Stats struct {
	Active    int              `json:"active"`
	Priority  int              `json:"priority"`
	Retired   int              `json:"retired"`
	Excluded  int              `json:"excluded"`
	Lists     map[List]int     `json:"lists"`
	Intervals []IntervalStats  `json:"intervals"`
	Sources   map[string]int64 `json:"sources"`
}

// Stats summarises the archive. It reads the catalog and counts the daily
// file; it never scans an intraday partition. Callers on a timer should cache
// it — on a full archive it is a second or two of work.
func (a *Archive) Stats() (Stats, error) {
	st := Stats{Lists: map[List]int{}, Sources: map[string]int64{}}
	if err := a.catalog.QueryRow(`SELECT
		  coalesce(sum(`+activeSQL+`), 0),
		  coalesce(sum(`+activeSQL+` AND `+priorityExpr+`), 0),
		  coalesce(sum(excluded = 0 AND (listed + extra + watchlist + user) = 0), 0),
		  coalesce(sum(excluded), 0)
		FROM symbols`).Scan(&st.Active, &st.Priority, &st.Retired, &st.Excluded); err != nil {
		return st, err
	}
	var listed, extra, watchlist, user int
	if err := a.catalog.QueryRow(`SELECT coalesce(sum(listed), 0), coalesce(sum(extra), 0),
		  coalesce(sum(watchlist), 0), coalesce(sum(user), 0) FROM symbols WHERE excluded = 0`).
		Scan(&listed, &extra, &watchlist, &user); err != nil {
		return st, err
	}
	st.Lists[Listed], st.Lists[Extra], st.Lists[Watchlist], st.Lists[User] = listed, extra, watchlist, user

	// Per interval, a symbol's coverage is the union of its sources'.
	rows, err := a.catalog.Query(`SELECT interval, count(newest), coalesce(sum(complete), 0), coalesce(sum(failing), 0),
		  min(oldest), max(newest), min(newest)
		FROM (SELECT c.symbol_id, c.interval, min(c.oldest) AS oldest, max(c.newest) AS newest,
		        max(c.complete) AS complete, max(c.failures > 0) AS failing
		      FROM cursors c JOIN symbols s ON s.id = c.symbol_id
		      WHERE ` + activeSQL + `
		      GROUP BY c.symbol_id, c.interval)
		GROUP BY interval`)
	if err != nil {
		return st, err
	}
	byInterval := map[quotes.Interval]IntervalStats{}
	for rows.Next() {
		var s IntervalStats
		var interval string
		var oldest, newest, stalest nullInt
		if err := rows.Scan(&interval, &s.Started, &s.Complete, &s.Failing, &oldest, &newest, &stalest); err != nil {
			rows.Close()
			return st, err
		}
		s.Interval = quotes.Interval(interval)
		s.Oldest, s.Newest, s.Stalest = oldest.time(), newest.time(), stalest.time()
		byInterval[s.Interval] = s
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return st, err
	}

	counts, err := a.catalog.Query(`SELECT interval, sum(bars) FROM days GROUP BY interval`)
	if err != nil {
		return st, err
	}
	for counts.Next() {
		var interval string
		var n int64
		if err := counts.Scan(&interval, &n); err != nil {
			counts.Close()
			return st, err
		}
		s := byInterval[quotes.Interval(interval)]
		s.Interval, s.Bars = quotes.Interval(interval), n
		byInterval[s.Interval] = s
	}
	counts.Close()
	if db, err := a.partition(partitionKey(quotes.Daily, time.Time{}), false); err != nil {
		return st, err
	} else if db != nil {
		s := byInterval[quotes.Daily]
		s.Interval = quotes.Daily
		if err := db.QueryRow(`SELECT count(*) FROM bars`).Scan(&s.Bars); err != nil {
			return st, err
		}
		byInterval[quotes.Daily] = s
	}
	for _, i := range quotes.Intervals {
		if s, ok := byInterval[i]; ok {
			st.Intervals = append(st.Intervals, s)
		}
	}

	names, err := a.sourceNames()
	if err != nil {
		return st, err
	}
	srcRows, err := a.catalog.Query(`SELECT source_id, count(*) FROM cursors WHERE newest IS NOT NULL GROUP BY source_id`)
	if err != nil {
		return st, err
	}
	defer srcRows.Close()
	for srcRows.Next() {
		var id, n int64
		if err := srcRows.Scan(&id, &n); err != nil {
			return st, err
		}
		st.Sources[names[id]] = n
	}
	return st, srcRows.Err()
}

// Span is a stretch of a coverage timeline: one month of one interval, how
// many bars it holds and which sources they came from.
type Span struct {
	Month   string   `json:"month"` // YYYY-MM
	Bars    int64    `json:"bars"`
	Days    int      `json:"days"`
	Sources []string `json:"sources"`
}

// Coverage is one symbol's detail: its cursors and a month-by-month timeline
// per interval.
type Coverage struct {
	Symbol    Symbol                     `json:"symbol"`
	Cursors   []Cursor                   `json:"cursors"`
	Timelines map[quotes.Interval][]Span `json:"timelines"`
	Splits    int                        `json:"splits"`
	Dividends int                        `json:"dividends"`
}

// SymbolCoverage reports one symbol in detail.
func (a *Archive) SymbolCoverage(symbol string) (Coverage, error) {
	s, err := a.Lookup(symbol)
	if err != nil {
		return Coverage{}, err
	}
	cov := Coverage{Symbol: s, Timelines: map[quotes.Interval][]Span{}}
	if cov.Cursors, err = a.SymbolCursors(s.ID); err != nil {
		return cov, err
	}
	if cov.Cursors == nil {
		cov.Cursors = []Cursor{}
	}
	names, err := a.sourceNames()
	if err != nil {
		return cov, err
	}

	// SQLite has no bitwise aggregate, so the months are folded here. A
	// symbol's ledger is a few hundred rows a year.
	rows, err := a.catalog.Query(`SELECT interval, day, bars, sources FROM days WHERE symbol_id = ? ORDER BY interval, day`, s.ID)
	if err != nil {
		return cov, err
	}
	masks := map[quotes.Interval]map[string]int64{}
	for rows.Next() {
		var interval string
		var day, bars, mask int64
		if err := rows.Scan(&interval, &day, &bars, &mask); err != nil {
			rows.Close()
			return cov, err
		}
		i := quotes.Interval(interval)
		month := time.Unix(day, 0).UTC().Format("2006-01")
		spans := cov.Timelines[i]
		if len(spans) == 0 || spans[len(spans)-1].Month != month {
			spans = append(spans, Span{Month: month})
		}
		spans[len(spans)-1].Bars += bars
		spans[len(spans)-1].Days++
		cov.Timelines[i] = spans
		if masks[i] == nil {
			masks[i] = map[string]int64{}
		}
		masks[i][month] |= mask
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return cov, err
	}
	for i, spans := range cov.Timelines {
		for k := range spans {
			spans[k].Sources = maskNames(masks[i][spans[k].Month], names)
		}
	}

	// Daily bars have no ledger — one row per day would be the file again —
	// so their timeline is counted straight from the daily file.
	if db, err := a.partition(partitionKey(quotes.Daily, time.Time{}), false); err != nil {
		return cov, err
	} else if db != nil {
		drows, err := db.Query(`SELECT strftime('%Y-%m', ts, 'unixepoch') AS month, count(*), group_concat(DISTINCT source_id)
			FROM bars WHERE symbol_id = ? GROUP BY month ORDER BY month`, s.ID)
		if err != nil {
			return cov, err
		}
		for drows.Next() {
			var sp Span
			var ids string
			if err := drows.Scan(&sp.Month, &sp.Bars, &ids); err != nil {
				drows.Close()
				return cov, err
			}
			sp.Days = int(sp.Bars)
			var mask int64
			for _, part := range splitComma(ids) {
				var id int64
				if _, err := fmt.Sscan(part, &id); err == nil {
					mask |= 1 << id
				}
			}
			sp.Sources = maskNames(mask, names)
			cov.Timelines[quotes.Daily] = append(cov.Timelines[quotes.Daily], sp)
		}
		drows.Close()
	}

	if err := a.catalog.QueryRow(`SELECT count(*) FROM splits WHERE symbol_id = ?`, s.ID).Scan(&cov.Splits); err != nil {
		return cov, err
	}
	if err := a.catalog.QueryRow(`SELECT count(*) FROM dividends WHERE symbol_id = ?`, s.ID).Scan(&cov.Dividends); err != nil {
		return cov, err
	}
	return cov, nil
}

func maskNames(mask int64, names map[int64]string) []string {
	out := []string{}
	for id := int64(1); id < 63; id++ {
		if mask&(1<<id) != 0 {
			if n, ok := names[id]; ok {
				out = append(out, n)
			}
		}
	}
	sort.Strings(out)
	return out
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}

// Size is how many bytes the archive's files take.
func (a *Archive) Size() (int64, error) {
	var total int64
	err := filepath.WalkDir(a.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// nullInt reads a nullable integer column as a time.
type nullInt struct {
	v     int64
	valid bool
}

func (n *nullInt) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		n.valid = false
	case int64:
		n.v, n.valid = v, true
	default:
		return fmt.Errorf("unexpected %T", src)
	}
	return nil
}

func (n nullInt) time() time.Time {
	if !n.valid {
		return time.Time{}
	}
	return time.Unix(n.v, 0).UTC()
}
