package registry

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetrySchema(t *testing.T) {
	old := schemaRetryStep
	schemaRetryStep = time.Millisecond
	t.Cleanup(func() { schemaRetryStep = old })

	boom := errors.New("duplicate column")

	t.Run("first attempt succeeds", func(t *testing.T) {
		calls := 0
		err := RetrySchema(context.Background(), func(context.Context) error { calls++; return nil })
		if err != nil || calls != 1 {
			t.Fatalf("err=%v calls=%d, want nil and 1", err, calls)
		}
	})

	t.Run("a lost race is retried", func(t *testing.T) {
		calls := 0
		err := RetrySchema(context.Background(), func(context.Context) error {
			calls++
			if calls == 1 {
				return boom
			}
			return nil
		})
		if err != nil || calls != 2 {
			t.Fatalf("err=%v calls=%d, want nil and 2", err, calls)
		}
	})

	t.Run("a broken schema fails after three attempts", func(t *testing.T) {
		calls := 0
		err := RetrySchema(context.Background(), func(context.Context) error { calls++; return boom })
		if !errors.Is(err, boom) || calls != 3 {
			t.Fatalf("err=%v calls=%d, want boom and 3", err, calls)
		}
	})

	t.Run("a cancelled context stops the wait", func(t *testing.T) {
		schemaRetryStep = time.Hour
		defer func() { schemaRetryStep = time.Millisecond }()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		err := RetrySchema(ctx, func(context.Context) error { calls++; return boom })
		if !errors.Is(err, boom) || calls != 1 {
			t.Fatalf("err=%v calls=%d, want boom and 1", err, calls)
		}
	})
}
