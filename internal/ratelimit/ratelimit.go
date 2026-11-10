// Package ratelimit implements a small token-bucket limiter used to simulate
// provider RPM/TPM throttling.
package ratelimit

import (
	"sync"
	"time"

	"github.com/jahrulnr/fakeagents/internal/clock"
)

// Config configures the limiter.
type Config struct {
	Enabled           bool
	RequestsPerMinute int
	TokensPerMinute   int
	RetryAfter        time.Duration
}

// Decision is the outcome of one admission check.
type Decision struct {
	Allowed    bool
	Limit      int
	Remaining  int
	ResetAt    time.Time
	RetryAfter time.Duration
}

// Limiter is a concurrency-safe token bucket.
type Limiter struct {
	mu      sync.Mutex
	cfg     Config
	clock   clock.Clock
	buckets map[string]*bucket
}

type bucket struct {
	requests float64
	tokens   float64
	last     time.Time
	lastSeen time.Time
}

// New builds a limiter.
func New(cfg Config, clk clock.Clock) *Limiter {
	if clk == nil {
		clk = clock.Real{}
	}
	return &Limiter{cfg: cfg, clock: clk, buckets: make(map[string]*bucket)}
}

// Enabled reports whether throttling is active.
func (l *Limiter) Enabled() bool { return l != nil && l.cfg.Enabled }

// AdmitRequest consumes one request credit.
func (l *Limiter) AdmitRequest(key string) Decision {
	if l == nil || !l.cfg.Enabled || l.cfg.RequestsPerMinute <= 0 {
		return Decision{Allowed: true}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()
	b := l.bucket(key, now)
	rate := float64(l.cfg.RequestsPerMinute) / 60.0
	refill(&b.requests, rate, l.cfg.RequestsPerMinute, b.last, now)
	b.last = now
	limit := l.cfg.RequestsPerMinute
	if b.requests < 1 {
		retry := l.retryAfter((1 - b.requests) / rate)
		return Decision{
			Allowed:    false,
			Limit:      limit,
			Remaining:  0,
			ResetAt:    now.Add(l.retryAfter((float64(limit) - b.requests) / rate)),
			RetryAfter: retry,
		}
	}
	b.requests--
	return Decision{
		Allowed:   true,
		Limit:     limit,
		Remaining: int(b.requests),
		ResetAt:   now.Add(l.retryAfter((float64(limit) - b.requests) / rate)),
	}
}

// AdmitTokens consumes token credits. n <= 0 always succeeds.
func (l *Limiter) AdmitTokens(key string, n int) Decision {
	if l == nil || !l.cfg.Enabled || l.cfg.TokensPerMinute <= 0 || n <= 0 {
		return Decision{Allowed: true}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()
	b := l.bucket(key, now)
	rate := float64(l.cfg.TokensPerMinute) / 60.0
	refill(&b.tokens, rate, l.cfg.TokensPerMinute, b.last, now)
	b.last = now
	limit := l.cfg.TokensPerMinute
	if b.tokens < float64(n) {
		retry := l.retryAfter((float64(n) - b.tokens) / rate)
		return Decision{
			Allowed:    false,
			Limit:      limit,
			Remaining:  int(b.tokens),
			ResetAt:    now.Add(retry),
			RetryAfter: retry,
		}
	}
	b.tokens -= float64(n)
	return Decision{
		Allowed:   true,
		Limit:     limit,
		Remaining: int(b.tokens),
		ResetAt:   now.Add(l.retryAfter((float64(limit) - b.tokens) / rate)),
	}
}

// Snapshot reports current request and token capacity without consuming.
func (l *Limiter) Snapshot(key string) (Decision, Decision) {
	if l == nil || !l.cfg.Enabled {
		return Decision{Allowed: true}, Decision{Allowed: true}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()
	b := l.bucket(key, now)
	reqRate := float64(l.cfg.RequestsPerMinute) / 60.0
	tokRate := float64(l.cfg.TokensPerMinute) / 60.0
	if reqRate > 0 {
		refill(&b.requests, reqRate, l.cfg.RequestsPerMinute, b.last, now)
	}
	if tokRate > 0 {
		refill(&b.tokens, tokRate, l.cfg.TokensPerMinute, b.last, now)
	}
	b.last = now
	request := Decision{Limit: l.cfg.RequestsPerMinute, Remaining: int(b.requests)}
	if reqRate > 0 {
		request.ResetAt = now.Add(l.retryAfter((float64(l.cfg.RequestsPerMinute) - b.requests) / reqRate))
	}
	tokens := Decision{Limit: l.cfg.TokensPerMinute, Remaining: int(b.tokens)}
	if tokRate > 0 {
		tokens.ResetAt = now.Add(l.retryAfter((float64(l.cfg.TokensPerMinute) - b.tokens) / tokRate))
	}
	return request, tokens
}

// Reset clears all buckets.
func (l *Limiter) Reset() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buckets = make(map[string]*bucket)
}

// Prune removes buckets that have not been used recently.
func (l *Limiter) Prune(olderThan time.Duration) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()
	for key, b := range l.buckets {
		if now.Sub(b.lastSeen) > olderThan {
			delete(l.buckets, key)
		}
	}
}

func (l *Limiter) bucket(key string, now time.Time) *bucket {
	if key == "" {
		key = "global"
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{
			requests: float64(l.cfg.RequestsPerMinute),
			tokens:   float64(l.cfg.TokensPerMinute),
			last:     now,
		}
		l.buckets[key] = b
	}
	b.lastSeen = now
	return b
}

func (l *Limiter) retryAfter(seconds float64) time.Duration {
	if l.cfg.RetryAfter > 0 {
		return l.cfg.RetryAfter
	}
	if seconds <= 0 {
		return time.Second
	}
	d := max(time.Duration(seconds*float64(time.Second)), time.Millisecond)
	return d
}

func refill(value *float64, ratePerSecond float64, capacity int, last, now time.Time) {
	if ratePerSecond <= 0 {
		return
	}
	elapsed := now.Sub(last).Seconds()
	if elapsed <= 0 {
		return
	}
	*value += elapsed * ratePerSecond
	if *value > float64(capacity) {
		*value = float64(capacity)
	}
}
