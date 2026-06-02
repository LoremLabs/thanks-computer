package cron

import (
	"context"
	"strings"
	"testing"
)

func TestOpenUnknownQueue(t *testing.T) {
	_, err := Open("does-not-exist", Config{})
	if err == nil {
		t.Fatal("Open of unknown backend returned nil error")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error %q does not name the unknown backend", err)
	}
}

func TestRegisterAndOpen(t *testing.T) {
	Register("fake-test-backend", func(cfg Config) (Queue, error) {
		return fakeQueue{period: cfg.Period}, nil
	})
	q, err := Open("fake-test-backend", Config{Period: 42})
	if err != nil {
		t.Fatalf("Open registered backend: %v", err)
	}
	if q.Name() != "fake" {
		t.Errorf("Name = %q, want fake", q.Name())
	}
}

type fakeQueue struct{ period int }

func (fakeQueue) Name() string                       { return "fake" }
func (fakeQueue) Enqueue(context.Context, Job) error { return nil }
func (fakeQueue) Work(ctx context.Context, _ func(context.Context, Job) error) error {
	<-ctx.Done()
	return ctx.Err()
}
