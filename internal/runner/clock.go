package runner

import (
	"context"
	"time"
)

// Clock abstracts time so the dispatcher can be tested in virtual time.
type Clock interface {
	Now() time.Time
	// SleepUntil blocks until t (returning at once if t has passed) or until
	// ctx is done, in which case it returns ctx.Err().
	SleepUntil(ctx context.Context, t time.Time) error
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) SleepUntil(ctx context.Context, t time.Time) error {
	d := time.Until(t)
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
