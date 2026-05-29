package dev

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitHealthyReturnsWhen2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	if err := WaitHealthy(context.Background(), srv.URL, 2*time.Second, 100*time.Millisecond); err != nil {
		t.Errorf("WaitHealthy: %v", err)
	}
}

// TestWaitHealthyWaitsForRecovery: server returns 503 for the first two
// hits, then 200. The poller should keep trying until the recovery and
// return nil before the timeout.
func TestWaitHealthyWaitsForRecovery(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	start := time.Now()
	err := WaitHealthy(context.Background(), srv.URL, 5*time.Second, 100*time.Millisecond)
	if err != nil {
		t.Errorf("WaitHealthy: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Errorf("WaitHealthy took %v; expected to recover quickly", elapsed)
	}
}

func TestWaitHealthyTimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	err := WaitHealthy(context.Background(), srv.URL, 300*time.Millisecond, 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("expected timeout in err, got %v", err)
	}
}

func TestWaitHealthyConnRefused(t *testing.T) {
	// Pick a port unlikely to have anything on it.
	err := WaitHealthy(context.Background(), "http://127.0.0.1:1/", 300*time.Millisecond, 100*time.Millisecond)
	if err == nil {
		t.Error("expected error for unreachable URL")
	}
}
