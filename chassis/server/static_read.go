package server

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/filecas"
)

// boundedReads fronts the file store for txco://static. Each Get has its
// own budget (--static-read-timeout) inside the op's deadline, and a read
// that fails for any reason but "not found" is tried once more, on a fresh
// request, while the op still has time. A hung object read (R2 under load)
// then costs a page one slow asset instead of a broken one; past the second
// attempt the caller answers 503, never 404 (staticResultBody).
//
// Only static reads go through it: read-file and the dataset cache keep the
// store as it is.
type boundedReads struct {
	filecas.Store
	budget time.Duration // one attempt; 0 = the op's own deadline
	log    *zap.Logger
}

// staticReadAttempts is how many times one Get asks the store.
const staticReadAttempts = 2

// staticReadError is a read that did not come back: how many attempts were
// made and whether the last one ran out of time. It wraps the store's own
// error, which is never shown to the client.
type staticReadError struct {
	Attempts int
	Timeout  bool
	Err      error
}

func (e *staticReadError) Error() string { return "static read: " + e.Err.Error() }
func (e *staticReadError) Unwrap() error { return e.Err }

func (b boundedReads) Get(ctx context.Context, hash string) ([]byte, error) {
	start := time.Now()
	var last error
	timedOut := false
	attempts := 0
	for attempts < staticReadAttempts && ctx.Err() == nil {
		attempts++
		actx, cancel := ctx, context.CancelFunc(func() {})
		if b.budget > 0 {
			actx, cancel = context.WithTimeout(ctx, b.budget)
		}
		body, err := b.Store.Get(actx, hash)
		timedOut = err != nil && (actx.Err() != nil || errors.Is(err, context.DeadlineExceeded))
		cancel()
		if err == nil {
			if attempts > 1 && b.log != nil {
				b.log.Info("static read served on retry",
					zap.String("hash", shortHash(hash)), zap.Int("attempts", attempts),
					zap.Duration("took", time.Since(start)))
			}
			return body, nil
		}
		if errors.Is(err, filecas.ErrNotFound) {
			return nil, err
		}
		last = err
	}
	if last == nil {
		// The op's deadline had passed before the first attempt.
		last, timedOut = ctx.Err(), true
	}
	if b.log != nil {
		b.log.Warn("static read failed",
			zap.String("hash", shortHash(hash)), zap.Int("attempts", attempts),
			zap.Bool("timeout", timedOut), zap.Duration("took", time.Since(start)), zap.Error(last))
	}
	return nil, &staticReadError{Attempts: attempts, Timeout: timedOut, Err: last}
}

// shortHash is enough of a content hash to find the object, for logs and
// trace events.
func shortHash(h string) string {
	if len(h) > 16 {
		return h[:16]
	}
	return h
}
