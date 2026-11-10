package ratelimit_test

import (
	"testing"
	"time"

	"github.com/jahrulnr/fakeagents/internal/clock"
	"github.com/jahrulnr/fakeagents/internal/ratelimit"
)

func TestRequestBucket(t *testing.T) {
	limiter := ratelimit.New(ratelimit.Config{
		Enabled:           true,
		RequestsPerMinute: 2,
	}, clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))

	first := limiter.AdmitRequest("a")
	second := limiter.AdmitRequest("a")
	third := limiter.AdmitRequest("a")
	if !first.Allowed || !second.Allowed {
		t.Fatal("initial burst should be allowed")
	}
	if third.Allowed {
		t.Fatal("third request should be denied")
	}
	if third.RetryAfter <= 0 {
		t.Fatalf("retry after = %s", third.RetryAfter)
	}
}

func TestRefill(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	limiter := ratelimit.New(ratelimit.Config{
		Enabled:           true,
		RequestsPerMinute: 2,
	}, clk)
	limiter.AdmitRequest("a")
	limiter.AdmitRequest("a")
	if limiter.AdmitRequest("a").Allowed {
		t.Fatal("bucket should be empty")
	}
	clk.Advance(30 * time.Second)
	if !limiter.AdmitRequest("a").Allowed {
		t.Fatal("one token should have refilled")
	}
}

func TestTokenBucket(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	limiter := ratelimit.New(ratelimit.Config{
		Enabled:         true,
		TokensPerMinute: 100,
	}, clk)
	if !limiter.AdmitTokens("a", 60).Allowed {
		t.Fatal("first 60 tokens should pass")
	}
	denied := limiter.AdmitTokens("a", 50)
	if denied.Allowed {
		t.Fatal("second request should be denied")
	}
	clk.Advance(30 * time.Second)
	if !limiter.AdmitTokens("a", 50).Allowed {
		t.Fatal("tokens should refill")
	}
}

func TestDisabledAndReset(t *testing.T) {
	limiter := ratelimit.New(ratelimit.Config{Enabled: false}, clock.NewFake(time.Now()))
	if !limiter.AdmitRequest("a").Allowed || !limiter.AdmitTokens("a", 1000).Allowed {
		t.Fatal("disabled limiter must allow everything")
	}
	limiter.Reset()
}
