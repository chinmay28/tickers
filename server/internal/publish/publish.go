// Package publish pushes a quote snapshot to downstream key-value endpoints.
//
// This is the part of the original update_minion_quotes.py that mattered most:
// the script existed to write one entry into a home-automation key-value
// store, and anything already reading that entry must keep working. So the
// wire format below is not a redesign — it is the script's payload, preserved
// byte for byte, including the PUT-then-POST fallback and the "MM/DD HH:MM:SS"
// timestamp.
package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/chinmay28/tickers/server/internal/store"
)

// TimestampLayout is the timestamp format the original script wrote into the
// payload ("%m/%d %H:%M:%S"). Consumers parse it, so it is a contract.
const TimestampLayout = "01/02 15:04:05"

// Snapshot is what gets published: the enabled watchlist with its latest
// readings, taken at one instant.
type Snapshot struct {
	Quotes []store.Quote
	// At is the snapshot time, rendered into the payload's "timestamp" key.
	At time.Time
}

// Payload renders the snapshot in the shape the given format asks for.
//
// FormatMinion is the legacy shape and the default:
//
//	{"VTI": "295.50", "BTC-USD": "68120.11", "timestamp": "08/07 14:03:22"}
//
// Prices are 2-decimal *strings* and an unavailable one is the literal "N/A",
// both exactly as the script produced them — a consumer that does
// `float(value)` on a good day and shows the raw string on a bad one keeps
// behaving the same way.
//
// FormatDetailed is for new consumers that would rather not parse strings.
func Payload(snap Snapshot, format string) map[string]any {
	out := map[string]any{}

	switch format {
	case store.FormatDetailed:
		for _, q := range snap.Quotes {
			entry := map[string]any{
				"status":   q.Status,
				"currency": q.Currency,
			}
			if q.ShortName != "" {
				entry["name"] = q.ShortName
			}
			if q.Price != nil {
				entry["price"] = round2(*q.Price)
			}
			if q.PreviousClose != nil {
				entry["previousClose"] = round2(*q.PreviousClose)
			}
			if change, ok := q.Change(); ok {
				entry["change"] = round2(change)
			}
			if pct, ok := q.ChangePercent(); ok {
				entry["changePercent"] = round2(pct)
			}
			if q.Error != "" {
				entry["error"] = q.Error
			}
			out[q.Symbol] = entry
		}
		out["timestamp"] = snap.At.Format(TimestampLayout)
		out["timestampISO"] = snap.At.UTC().Format(time.RFC3339)

	default: // store.FormatMinion
		for _, q := range snap.Quotes {
			if q.Status == store.StatusOK && q.Price != nil {
				out[q.Symbol] = fmt.Sprintf("%.2f", *q.Price)
			} else {
				out[q.Symbol] = "N/A"
			}
		}
		out["timestamp"] = snap.At.Format(TimestampLayout)
	}
	return out
}

// Publisher writes snapshots to sinks over HTTP.
type Publisher struct {
	// Client is the HTTP client; leave nil for a per-sink timeout-bounded one.
	Client *http.Client
}

// New returns a Publisher with default behaviour.
func New() *Publisher { return &Publisher{} }

// Publish writes one snapshot to one sink.
//
// The two-step dance is the script's, kept deliberately: PUT the entry at
// {baseURL}/{key} first, because that is the idempotent update most key-value
// stores expose; if the store answers anything but 2xx — typically a 404
// because the entry doesn't exist yet — POST to {baseURL} to create it. The
// result records which verb actually landed, so the Activity page can show
// "created" versus "updated" without guessing.
func (p *Publisher) Publish(ctx context.Context, sink store.Sink, snap Snapshot) store.PublishResult {
	start := time.Now()
	result := store.PublishResult{SinkID: sink.ID, SinkName: sink.Name}
	defer func() { result.DurationMS = time.Since(start).Milliseconds() }()

	value := Payload(snap, sink.Format)

	timeout := time.Duration(sink.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	base := strings.TrimRight(sink.BaseURL, "/")

	putBody := map[string]any{"value": value}
	if sink.Category != "" {
		putBody["category"] = sink.Category
	}
	status, putErr := p.send(ctx, http.MethodPut, base+"/"+sink.Key, putBody)
	if putErr == nil {
		result.Method = http.MethodPut
		result.StatusCode = status
		result.OK = true
		return result
	}

	postBody := map[string]any{"key": sink.Key, "value": value}
	if sink.Category != "" {
		postBody["category"] = sink.Category
	}
	status, postErr := p.send(ctx, http.MethodPost, base, postBody)
	if postErr == nil {
		result.Method = http.MethodPost
		result.StatusCode = status
		result.OK = true
		return result
	}

	// Report both attempts. Being told only that the POST failed sends people
	// looking at the wrong endpoint — the PUT's status is usually the one that
	// explains what the store actually wants.
	result.StatusCode = status
	result.Error = fmt.Sprintf("PUT %s/%s failed (%v); POST %s also failed (%v)",
		base, sink.Key, putErr, base, postErr)
	return result
}

// PublishAll writes the snapshot to every sink given, in order.
func (p *Publisher) PublishAll(ctx context.Context, sinks []store.Sink, snap Snapshot) []store.PublishResult {
	results := make([]store.PublishResult, 0, len(sinks))
	for _, sink := range sinks {
		results = append(results, p.Publish(ctx, sink, snap))
	}
	return results
}

func (p *Publisher) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return http.DefaultClient
}

// send performs one request and returns its status code. A non-2xx status is
// an error carrying a trimmed body, because the body is where a key-value
// store explains itself.
func (p *Publisher) send(ctx context.Context, method, endpoint string, body map[string]any) (int, error) {
	blob, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(blob))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := p.client().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	// Drain (bounded) so the connection can be reused, and so the error below
	// has something to say.
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("HTTP %d: %s", resp.StatusCode, trim(respBody))
	}
	return resp.StatusCode, nil
}

func trim(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	if s == "" {
		s = "(empty response)"
	}
	return s
}

// round2 keeps published numbers at the two decimals the legacy format used,
// so a consumer never sees the detailed format disagree with the minion
// format about a price. Half-way cases follow float64's own representation —
// 1.005 is really 1.00499…, so it rounds down; that is the same answer
// Python's f"{x:.2f}" gave in the original script.
func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
