package cli

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// shrink retryBackoffBase so the sleeping paths don't cost real seconds.
func fastBackoff(t *testing.T) {
	t.Helper()
	prev := retryBackoffBase
	retryBackoffBase = time.Millisecond
	t.Cleanup(func() { retryBackoffBase = prev })
}

func transient() error { return &client.HTTPError{StatusCode: 502} }
func fatal() error     { return &client.HTTPError{StatusCode: 400} }

func TestRetryStepSucceedsAfterTransient(t *testing.T) {
	fastBackoff(t)
	calls := 0
	err := retryStep(context.Background(), io.Discard, "x", 3, nil, func() error {
		calls++
		if calls < 3 {
			return transient()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("want success after retries, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("want 3 calls (2 transient + 1 ok), got %d", calls)
	}
}

func TestRetryStepStopsOnFatal(t *testing.T) {
	fastBackoff(t)
	calls := 0
	err := retryStep(context.Background(), io.Discard, "x", 5, nil, func() error {
		calls++
		return fatal()
	})
	if err == nil {
		t.Fatal("want the fatal error, got nil")
	}
	if calls != 1 {
		t.Fatalf("fatal must not retry: want 1 call, got %d", calls)
	}
}

func TestRetryStepExhaustsRetries(t *testing.T) {
	fastBackoff(t)
	calls := 0
	err := retryStep(context.Background(), io.Discard, "x", 2, nil, func() error {
		calls++
		return transient()
	})
	if err == nil {
		t.Fatal("want the last error after exhausting retries")
	}
	if calls != 3 { // 1 initial + 2 retries
		t.Fatalf("want 3 calls (1+2 retries), got %d", calls)
	}
}

func TestRetryStepVerifyShortCircuitsBeforeFn(t *testing.T) {
	calls := 0
	// verify true on the very first check → fn must never run (the idempotent
	// "already active" path that avoids re-running an expensive activate).
	err := retryStep(context.Background(), io.Discard, "x", 3,
		func() bool { return true },
		func() error { calls++; return nil })
	if err != nil {
		t.Fatalf("want nil when already applied, got %v", err)
	}
	if calls != 0 {
		t.Fatalf("fn must not run when verify is true up front, ran %d times", calls)
	}
}

func TestRetryStepVerifyRescuesFalse502(t *testing.T) {
	fastBackoff(t)
	calls := 0
	landed := false
	// fn always 502s (edge timeout), but the work landed server-side after the
	// first attempt — the post-loop verify must rescue it as success.
	err := retryStep(context.Background(), io.Discard, "activate", 2,
		func() bool { return landed },
		func() error { calls++; landed = true; return transient() })
	if err != nil {
		t.Fatalf("want nil (verified server-side), got %v", err)
	}
	// attempt 0: verify false → fn runs (sets landed). attempt 1: verify true → returns nil.
	if calls != 1 {
		t.Fatalf("want fn to run once before verify rescues it, ran %d", calls)
	}
}

func TestRetryStepHonorsContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := retryStep(ctx, io.Discard, "x", 3, nil, func() error { return transient() })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}
