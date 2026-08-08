package store

import "time"

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
	Expression string    `json:"expression"`
	Label      string    `json:"label"`
	Position   int       `json:"position"`
	Enabled    bool      `json:"enabled"`
	Origin     string    `json:"origin"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`

	// Pinned is derived, not stored: it says whether this row's symbol is on
	// the pinned list in Settings. Every query that returns a Ticker stamps it,
	// so the client never has to join the watchlist against the settings to
	// know which rows carry the chip.
	Pinned bool `json:"pinned"`
}

// IsComposite reports whether this row is priced from a formula rather than
// fetched from the provider.
func (t Ticker) IsComposite() bool { return t.Expression != "" }

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
