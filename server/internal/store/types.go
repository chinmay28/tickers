package store

import (
	"strings"
	"time"
)

// Origin values for a Ticker. It records where a row came from — the shipped
// watchlist or a person — and nothing reads it at runtime; it is provenance
// for whoever opens the database, and what migration 002 used to work out
// which symbols to pin on an existing install.
const (
	OriginSeed = "seed"
	OriginUser = "user"
)

// Quote status values.
const (
	StatusOK    = "ok"
	StatusError = "error"
)

// Ticker is one symbol on the watchlist.
type Ticker struct {
	ID     string `json:"id"`
	Symbol string `json:"symbol"`
	// Expression is the formula behind a composite row — "VTI/GLD", "P/VTI" —
	// and empty for an ordinary symbol, which is the only thing that
	// distinguishes the two. A composite's Symbol is derived from it (the same
	// formula with the spaces taken out), so a composite still has one stable,
	// unique, publishable key like every other row.
	Expression string `json:"expression"`
	// PortfolioID marks a row that is a saved portfolio's live value rather
	// than a symbol or a formula. It is the third kind of row, and it is a
	// column rather than an expression for one reason: the row's symbol has to
	// be the portfolio's *name*, because that is what a downstream dashboard
	// reads the value under, and a composite's symbol is its formula.
	PortfolioID string    `json:"portfolioId"`
	Label       string    `json:"label"`
	Position    int       `json:"position"`
	Enabled     bool      `json:"enabled"`
	Origin      string    `json:"origin"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`

	// Pinned is derived, not stored: it says whether this row's symbol is on
	// the pinned list in Settings. Every query that returns a Ticker stamps it,
	// so the client never has to join the watchlist against the settings to
	// know which rows carry the chip.
	Pinned bool `json:"pinned"`
}

// IsComposite reports whether this row is priced from a formula rather than
// fetched from the provider.
func (t Ticker) IsComposite() bool { return t.Expression != "" }

// IsPortfolio reports whether this row is a saved allocation's live value.
func (t Ticker) IsPortfolio() bool { return t.PortfolioID != "" }

// OriginPortfolio marks a ticker the app maintains on a portfolio's behalf.
// Nothing reads it at runtime — IsPortfolio does that — but it keeps the
// provenance column honest for whoever opens the database.
const OriginPortfolio = "portfolio"

// PortfolioSymbol turns a portfolio's name into the key its row is published
// under.
//
// Uppercase like every other symbol, with runs of anything else collapsed to a
// hyphen: "Four fund" publishes as "FOUR-FUND". A space would work in SQLite
// and in JSON and then quietly break the first consumer that splits on one.
func PortfolioSymbol(name string) string {
	var b strings.Builder
	hyphen := false
	for _, r := range strings.ToUpper(strings.TrimSpace(name)) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
			hyphen = false
		case b.Len() > 0 && !hyphen:
			b.WriteRune('-')
			hyphen = true
		}
	}
	symbol := strings.Trim(b.String(), "-")
	if len(symbol) > 40 {
		symbol = strings.Trim(symbol[:40], "-")
	}
	return symbol
}

// Quote is the most recent reading for a ticker. A failed fetch is still a
// quote — status "error" with the reason — because "we tried and it didn't
// work" is information the UI needs to show, and silently dropping the row
// would make a broken symbol look like a symbol that was never asked about.
type Quote struct {
	TickerID      string    `json:"tickerId"`
	Symbol        string    `json:"symbol"`
	Price         *float64  `json:"price"`
	PreviousClose *float64  `json:"previousClose"`
	Currency      string    `json:"currency"`
	ShortName     string    `json:"shortName"`
	MarketState   string    `json:"marketState"`
	Status        string    `json:"status"`
	Error         string    `json:"error"`
	FetchedAt     time.Time `json:"fetchedAt"`

	// Composite says this reading was computed from a formula rather than
	// fetched. It is derived from the ticker, not stored on the quote — the
	// join that produces a snapshot stamps it — and it exists so the published
	// payload can give a ratio enough decimal places to mean something. A
	// P/VTI of 0.0335 published as the legacy "0.03" is not a number anyone
	// downstream can use.
	Composite bool `json:"composite"`
}

// Change is the absolute move from the previous close, and ok reports whether
// there was enough data to compute one.
func (q Quote) Change() (float64, bool) {
	if q.Price == nil || q.PreviousClose == nil || *q.PreviousClose == 0 {
		return 0, false
	}
	return *q.Price - *q.PreviousClose, true
}

// ChangePercent is Change as a percentage of the previous close.
func (q Quote) ChangePercent() (float64, bool) {
	change, ok := q.Change()
	if !ok {
		return 0, false
	}
	return change / *q.PreviousClose * 100, true
}

// HistoryPoint is one price observation, for the sparklines.
type HistoryPoint struct {
	Symbol string    `json:"symbol"`
	Price  float64   `json:"price"`
	At     time.Time `json:"at"`
}

// Publish formats. A sink declares which shape it wants the snapshot in.
const (
	// FormatMinion is the payload the original update_minion_quotes.py script
	// published, and the reason this constant exists: a flat map of symbol to
	// a 2-decimal string, plus a "timestamp" key in MM/DD HH:MM:SS. Anything
	// already consuming that feed keeps working unchanged.
	FormatMinion = "minion"
	// FormatDetailed is the richer shape for new consumers: per-symbol objects
	// with the numeric price, previous close, change, currency and status.
	FormatDetailed = "detailed"
)

// Sink is a downstream key-value endpoint the snapshot is published to.
type Sink struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	BaseURL   string    `json:"baseUrl"`
	Key       string    `json:"key"`
	Category  string    `json:"category"`
	Format    string    `json:"format"`
	Enabled   bool      `json:"enabled"`
	TimeoutMS int       `json:"timeoutMs"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Rebalance cadences. A portfolio's weights drift as its holdings move;
// rebalancing sells what grew and buys what didn't, back to the target.
const (
	// RebalanceNone lets the weights drift for the whole run — "buy once and
	// never touch it", which over decades is a materially different portfolio
	// from the one it started as.
	RebalanceNone      = "none"
	RebalanceAnnually  = "annually"
	RebalanceQuarterly = "quarterly"
	RebalanceMonthly   = "monthly"
)

// Cadences is every rebalancing and contribution frequency, in the order a
// picker should offer them. The two share a vocabulary because they share a
// meaning — "at the end of each of these periods" — and the simulation applies
// both on the same calendar boundaries.
var Cadences = []string{RebalanceAnnually, RebalanceQuarterly, RebalanceMonthly, RebalanceNone}

// ValidCadence reports whether s is one of them.
func ValidCadence(s string) bool {
	for _, c := range Cadences {
		if c == s {
			return true
		}
	}
	return false
}

// MaxHoldings bounds one portfolio. Each holding is an upstream request the
// first time it is priced, and a Raspberry Pi asking for forty full-history
// series at once is how you collect timeouts rather than a backtest.
const MaxHoldings = 20

// weightTolerance is how far a portfolio's weights may sum from 100 and still
// be accepted. Three holdings of a third each is the case that matters: 33.33
// three times is 99.99, and rejecting that would be arithmetic pedantry aimed
// at the one person who typed the honest thing.
const weightTolerance = 0.05

// Holding is one line of an allocation: a symbol and its target weight in
// percent. It is not a Ticker — a portfolio can hold something that was never
// on the watchlist, and adding one doesn't add a row to be polled every cycle.
type Holding struct {
	Symbol string  `json:"symbol"`
	Weight float64 `json:"weight"`
	// Units is how many shares the watchlist row holds — the weight's share of
	// the initial amount, divided by the price when the portfolio was last
	// saved. It is stored rather than derived because deriving it would mean
	// re-reading a historical price on every refresh cycle, and because fixing
	// it is what makes the row a *holding* whose weights drift rather than a
	// number that silently rebalances itself every thirty seconds.
	//
	// Zero means the row has never been priced — a portfolio saved while the
	// quote source was unreachable.
	Units float64 `json:"units"`
	// Replacement is a stand-in for the months before this symbol has any
	// history of its own — a broad fund in place of something that listed
	// recently, so a five-year-old holding doesn't truncate a thirty-year
	// backtest to five. Empty means the run simply starts where the symbol does.
	//
	// It is a substitution, not a blend: the stand-in's returns are used up to
	// the day the real series begins, and the real one from there. The result
	// always says which months were which, because a proxy nobody was told
	// about is a fabrication.
	Replacement string `json:"replacement"`
}

// Portfolio is a saved allocation to backtest.
//
// StartYear and EndYear are years rather than dates because that is the
// granularity the answer has: the simulation steps a month at a time, so a
// start of "1996" and one of "3 May 1996" produce the same run. Zero means
// "as far as the data goes", in both directions.
type Portfolio struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Holdings      []Holding `json:"holdings"`
	InitialAmount float64   `json:"initialAmount"`
	StartYear     int       `json:"startYear"`
	EndYear       int       `json:"endYear"`
	Rebalance     string    `json:"rebalance"`
	// Contribution is paid in at every ContributionFrequency boundary, split by
	// the target weights. Zero — with a frequency of "none" — is a lump sum
	// left alone, which is what every portfolio was before this existed.
	Contribution          float64 `json:"contribution"`
	ContributionFrequency string  `json:"contributionFrequency"`
	// Benchmark is a single symbol to run alongside at 100%, or empty for none.
	Benchmark string    `json:"benchmark"`
	Position  int       `json:"position"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Run triggers.
const (
	TriggerSchedule = "schedule"
	TriggerManual   = "manual"
	TriggerStartup  = "startup"
)

// PublishResult records what one sink did with one snapshot.
type PublishResult struct {
	SinkID   string `json:"sinkId"`
	SinkName string `json:"sinkName"`
	// Method is what actually succeeded — "PUT", "POST" (the fallback), or ""
	// when neither did.
	Method     string `json:"method"`
	StatusCode int    `json:"statusCode"`
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"durationMs"`
}

// Run is one refresh (+ publish) cycle, appended to an audit log the Activity
// page reads. It is never updated after insert.
type Run struct {
	ID         int64           `json:"id"`
	StartedAt  time.Time       `json:"startedAt"`
	FinishedAt time.Time       `json:"finishedAt"`
	Trigger    string          `json:"trigger"`
	OKCount    int             `json:"okCount"`
	ErrorCount int             `json:"errorCount"`
	Publishes  []PublishResult `json:"publishes"`
	Error      string          `json:"error,omitempty"`
}
