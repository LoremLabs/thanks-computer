package registry

import (
	"context"
	"time"
)

// schemaRetryStep is the pause before the first retry; the second waits twice
// as long. A variable so a test does not sleep.
var schemaRetryStep = time.Second

// RetrySchema runs a store's EnsureSchema body, and runs it again if it fails.
//
// On a fleet every node creates its protocol stores' tables in the shared
// Postgres at boot, with no lock. Two nodes booting together can race a
// release that adds a table or a column: both probe, both find it missing,
// and the loser's CREATE or ALTER fails (a duplicate — IF NOT EXISTS does
// not make concurrent DDL safe on Postgres). By then the winner has
// committed, so the retry's probe finds the column and its CREATE ... IF NOT
// EXISTS is a no-op. It is the migration runner's answer to the same race
// (app.applyMigrationsRetrying), and like it holds no lock and no
// connection between attempts.
//
// fn must be safe to re-run: every statement idempotent, every additive
// column probed first. A schema that is genuinely broken fails all three
// attempts, and the error comes back a few seconds later than it would have.
func RetrySchema(ctx context.Context, fn func(context.Context) error) error {
	const attempts = 3
	for i := 1; ; i++ {
		err := fn(ctx)
		if err == nil || i == attempts {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Duration(i) * schemaRetryStep):
		}
	}
}
