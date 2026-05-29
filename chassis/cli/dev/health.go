package dev

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// WaitHealthy polls url with HTTP GET until it returns a 2xx response,
// or until timeout elapses, or until ctx is canceled. interval governs
// the gap between attempts.
//
// Returns nil when the URL returns 2xx; an error otherwise. Uses a
// short per-request timeout so a slow-starting service doesn't block
// the polling loop on a single attempt.
func WaitHealthy(ctx context.Context, url string, timeout, interval time.Duration) error {
	if interval <= 0 {
		interval = time.Second
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := &http.Client{Timeout: 2 * time.Second}

	var lastErr error
	for {
		select {
		case <-pollCtx.Done():
			if lastErr != nil {
				return fmt.Errorf("health check %s: timeout after %s (last error: %w)", url, timeout, lastErr)
			}
			return fmt.Errorf("health check %s: timeout after %s", url, timeout)
		default:
		}

		req, err := http.NewRequestWithContext(pollCtx, http.MethodGet, url, nil)
		if err != nil {
			return fmt.Errorf("health check %s: build request: %w", url, err)
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}

		select {
		case <-pollCtx.Done():
			if lastErr != nil {
				return fmt.Errorf("health check %s: timeout after %s (last error: %w)", url, timeout, lastErr)
			}
			return fmt.Errorf("health check %s: timeout after %s", url, timeout)
		case <-time.After(interval):
		}
	}
}
