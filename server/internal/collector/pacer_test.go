package collector

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/chinmay28/tickers/server/internal/quotes"
)

func TestPacerSpacesRequestsEvenly(t *testing.T) {
	p := pacer{spacing: 2 * time.Second}
	if d := p.delay(now); d != 0 {
		t.Errorf("first request waits %s, want none", d)
	}
	p.observe(nil, now)
	if d := p.delay(now.Add(500 * time.Millisecond)); d != 1500*time.Millisecond {
		t.Errorf("half a second later the wait is %s, want 1.5s", d)
	}
	if d := p.delay(now.Add(3 * time.Second)); d != 0 {
		t.Errorf("after the spacing the wait is %s, want none", d)
	}
}

func TestPacerBacksOffOnRateLimitingAndRecovers(t *testing.T) {
	p := pacer{spacing: time.Second}
	limited := fmt.Errorf("AAPL: %w", quotes.ErrRateLimited)
	var pauses []time.Duration
	at := now
	for i := 0; i < 8; i++ {
		p.observe(limited, at)
		pauses = append(pauses, p.delay(at))
		at = at.Add(p.delay(at))
	}
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour}
	for i := range want {
		if pauses[i] != want[i] {
			t.Fatalf("pauses = %v, want %v", pauses, want)
		}
	}
	if !p.paused(at.Add(-time.Second)) {
		t.Error("the pacer does not report the pause")
	}
	p.observe(nil, at)
	p.observe(limited, at)
	if d := p.delay(at); d != time.Minute {
		t.Errorf("after a success the next 429 waits %s, want the penalty reset to a minute", d)
	}
}

func TestPacerPausesOnARunOfFailuresButNotOnMissingSymbols(t *testing.T) {
	p := pacer{spacing: time.Second}
	for i := 0; i < 50; i++ {
		p.observe(fmt.Errorf("ZZZ%d: %w", i, quotes.ErrNotFound), now)
	}
	if p.paused(now) {
		t.Error("fifty unknown symbols paused the pacer — they say nothing about the connection")
	}
	for i := 0; i < streakLimit-1; i++ {
		p.observe(errors.New("connection refused"), now)
	}
	if p.paused(now) {
		t.Fatal("paused before the streak limit")
	}
	p.observe(errors.New("connection refused"), now)
	if d := p.delay(now); d != streakPause {
		t.Errorf("after %d failures in a row the wait is %s, want %s", streakLimit, d, streakPause)
	}
}
