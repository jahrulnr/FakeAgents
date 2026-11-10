// Package clock abstracts time so simulated latency can be tested without
// sleeping in unit tests.
package clock

import (
	"context"
	"time"
)

// Clock is the minimal time surface used by FakeAgents.
type Clock interface {
	Now() time.Time
	Since(time.Time) time.Duration
	Sleep(context.Context, time.Duration) error
}

// Real is the production Clock backed by the standard library.
type Real struct{}

// Now implements Clock.
func (Real) Now() time.Time { return time.Now() }

// Since implements Clock.
func (Real) Since(t time.Time) time.Duration { return time.Since(t) }

// Sleep implements Clock and respects context cancellation.
func (Real) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Fake is a manually controlled Clock for tests.
type Fake struct {
	Current time.Time
}

// NewFake returns a Fake clock pinned at the given time.
func NewFake(now time.Time) *Fake { return &Fake{Current: now} }

// Now implements Clock.
func (f *Fake) Now() time.Time { return f.Current }

// Since implements Clock.
func (f *Fake) Since(t time.Time) time.Duration { return f.Current.Sub(t) }

// Sleep implements Clock by advancing the fake clock immediately.
func (f *Fake) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.Current = f.Current.Add(d)
	return nil
}

// Advance moves the fake clock forward.
func (f *Fake) Advance(d time.Duration) { f.Current = f.Current.Add(d) }
