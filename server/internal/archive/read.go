package archive

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/chinmay28/tickers/server/internal/quotes"
)

// Query is one read of a symbol's bars.
type Query struct {
	Symbol   string
	Interval quotes.Interval
	From, To time.Time
	// Extended includes the bars before the open and after the close, each
	// tagged with its Session. Off — every reader in the app — returns the
	// regular session alone, whatever the archive has collected.
	Extended bool
}

// Candles returns a symbol's stored regular-session bars at exactly this
// interval in [from, to), oldest first.
func (a *Archive) Candles(symbol string, interval quotes.Interval, from, to time.Time) ([]quotes.Candle, error) {
	return a.Stored(Query{Symbol: symbol, Interval: interval, From: from, To: to})
}

// Stored returns the bars stored at exactly the query's interval.
//
// A symbol that was renamed reads as one series: bars kept under a former
// symbol before the rename are included, and on a moment both hold, the
// current symbol's bar wins.
func (a *Archive) Stored(q Query) ([]quotes.Candle, error) {
	s, err := a.Lookup(q.Symbol)
	if err == ErrUnknownSymbol {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return a.stored(s.ID, q)
}

func (a *Archive) stored(id int64, q Query) ([]quotes.Candle, error) {
	out, err := a.candles(id, q.Interval, q.From, q.To, q.Extended)
	if err != nil {
		return nil, err
	}
	aliases, err := a.aliasIDs(id)
	if err != nil || len(aliases) == 0 {
		return out, err
	}
	have := make(map[int64]bool, len(out))
	for _, c := range out {
		have[c.Time.Unix()] = true
	}
	for _, al := range aliases {
		to := q.To
		if al.until.Before(to) {
			to = al.until
		}
		if !q.From.Before(to) {
			continue
		}
		former, err := a.candles(al.id, q.Interval, q.From, to, q.Extended)
		if err != nil {
			return nil, err
		}
		for _, c := range former {
			if !have[c.Time.Unix()] {
				out = append(out, c)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}

func (a *Archive) candles(id int64, interval quotes.Interval, from, to time.Time, extended bool) ([]quotes.Candle, error) {
	if interval.Step() == 0 {
		return nil, fmt.Errorf("unknown interval %q", interval)
	}
	session := ` AND session = 0`
	if extended {
		session = ``
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
		rows, err := db.Query(`SELECT ts, open, high, low, close, volume, coalesce(vwap, 0), coalesce(trades, 0), session FROM bars
			WHERE symbol_id = ? AND ts >= ? AND ts < ?`+session+` ORDER BY ts`, id, from.Unix(), to.Unix())
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var c quotes.Candle
			var ts int64
			if err := rows.Scan(&ts, &c.Open, &c.High, &c.Low, &c.Close, &c.Volume, &c.VWAP, &c.Trades, &c.Session); err != nil {
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

// Bars returns the best regular-session series the archive can give at an
// interval; see Best.
func (a *Archive) Bars(symbol string, interval quotes.Interval, from, to time.Time) ([]quotes.Candle, error) {
	return a.Best(Query{Symbol: symbol, Interval: interval, From: from, To: to})
}

// Best returns the best series the archive can give at an interval: its own
// bars where it has them, and bars built from a finer interval where it
// doesn't. Five-minute bars for a day only the one-minute series reached are
// the one-minute bars, resampled. Daily bars are never built from intraday
// ones — a daily bar is the exchange's official session, which intraday bars
// only approximate.
func (a *Archive) Best(q Query) ([]quotes.Candle, error) {
	s, err := a.Lookup(q.Symbol)
	if err == ErrUnknownSymbol {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out, err := a.stored(s.ID, q)
	if err != nil || !q.Interval.Intraday() {
		return out, err
	}
	have := map[int64]bool{}
	for _, c := range out {
		have[dayOf(c.Time)] = true
	}
	for _, finer := range quotes.Intervals {
		if !finer.Intraday() || finer.Step() >= q.Interval.Step() {
			continue
		}
		fq := q
		fq.Interval = finer
		fine, err := a.stored(s.ID, fq)
		if err != nil {
			return nil, err
		}
		var missing []quotes.Candle
		for _, c := range fine {
			if !have[dayOf(c.Time)] {
				missing = append(missing, c)
			}
		}
		built := Resample(missing, q.Interval)
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
// Buckets are anchored to the first bar of each day's session rather than to
// the clock. A US session opens at 9:30, so an hourly bar runs 9:30–10:30 —
// which is how every provider prints one — and anchoring to the clock would
// make it 9:00–10:00 with half an hour missing. Anchoring per session keeps a
// pre-market hour from swallowing the open. It also follows the session
// through daylight saving without a timezone database, and gives a crypto
// series, whose day starts at midnight, clock-aligned buckets.
//
// A built bar's VWAP is its parts' VWAPs weighted by their volume, and only
// when every part had one: a VWAP over half the bucket's trades would be a
// number that looked right and wasn't.
func Resample(candles []quotes.Candle, to quotes.Interval) []quotes.Candle {
	step := int64(to.Step() / time.Second)
	if step == 0 || len(candles) == 0 {
		return nil
	}
	sorted := append([]quotes.Candle(nil), candles...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Time.Before(sorted[j].Time) })

	var out []quotes.Candle
	var notional []float64 // Σ vwap×volume per built bar; NaN once a part lacks one
	var day, anchor, bucket int64 = -1, 0, -1
	session := quotes.Session(255)
	for _, c := range sorted {
		ts := c.Time.Unix()
		if d := dayOf(c.Time); d != day || c.Session != session {
			day, anchor, session = d, ts, c.Session
		}
		b := anchor + (ts-anchor)/step*step
		part := c.VWAP * float64(c.Volume)
		if c.VWAP == 0 {
			part = math.NaN()
		}
		if b != bucket || len(out) == 0 {
			bucket = b
			out = append(out, quotes.Candle{Time: time.Unix(b, 0).UTC(), Open: c.Open, High: c.High, Low: c.Low, Close: c.Close,
				Volume: c.Volume, Trades: c.Trades, Session: c.Session})
			notional = append(notional, part)
			continue
		}
		cur := &out[len(out)-1]
		cur.High = max(cur.High, c.High)
		cur.Low = min(cur.Low, c.Low)
		cur.Close = c.Close
		cur.Volume += c.Volume
		cur.Trades += c.Trades
		notional[len(notional)-1] += part
	}
	for i := range out {
		if n := notional[i]; !math.IsNaN(n) && out[i].Volume > 0 {
			out[i].VWAP = n / float64(out[i].Volume)
		}
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
	Aliases   []Alias                    `json:"aliases"`
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
	if cov.Aliases, err = a.Aliases(s.ID); err != nil {
		return cov, err
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
