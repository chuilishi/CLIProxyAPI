// Package executor provides first byte timeout detection for streaming requests.
// This file implements a wrapper that detects when the first response byte
// takes too long, enabling fast failover to alternative accounts/providers.
package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// ErrFirstByteTimeout is returned when the first byte is not received within the timeout
var ErrFirstByteTimeout = errors.New("first byte timeout: no response received within timeout period")

// FirstByteTimeoutConfig configures the first byte timeout behavior
type FirstByteTimeoutConfig struct {
	Timeout time.Duration // Time to wait for first byte
	OnTimeout func()      // Callback when timeout occurs
	OnFirstByte func(latencyMs int64) // Callback when first byte received
}

// FirstByteReader wraps an io.ReadCloser with first byte timeout detection
type FirstByteReader struct {
	reader       io.ReadCloser
	timeout      time.Duration
	onTimeout    func()
	onFirstByte  func(latencyMs int64)
	startTime    time.Time
	firstByteAt  time.Time
	mu           sync.Mutex
	gotFirstByte bool
	timedOut     bool
	cancelFunc   context.CancelFunc
}

// NewFirstByteReader creates a reader that monitors for first byte timeout
func NewFirstByteReader(reader io.ReadCloser, config FirstByteTimeoutConfig) *FirstByteReader {
	return &FirstByteReader{
		reader:      reader,
		timeout:     config.Timeout,
		onTimeout:   config.OnTimeout,
		onFirstByte: config.OnFirstByte,
		startTime:   time.Now(),
	}
}

// Read reads from the underlying reader with first byte timeout monitoring
func (r *FirstByteReader) Read(p []byte) (n int, err error) {
	r.mu.Lock()
	if r.timedOut {
		r.mu.Unlock()
		return 0, ErrFirstByteTimeout
	}
	gotFirst := r.gotFirstByte
	r.mu.Unlock()

	if gotFirst {
		// Already got first byte, just read normally
		return r.reader.Read(p)
	}

	// First read - set up timeout
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()

	r.mu.Lock()
	r.cancelFunc = cancel
	r.mu.Unlock()

	done := make(chan struct{})
	var readN int
	var readErr error

	go func() {
		readN, readErr = r.reader.Read(p)
		close(done)
	}()

	select {
	case <-done:
		r.mu.Lock()
		if !r.gotFirstByte && readN > 0 {
			r.gotFirstByte = true
			r.firstByteAt = time.Now()
			latencyMs := r.firstByteAt.Sub(r.startTime).Milliseconds()
			r.mu.Unlock()

			if r.onFirstByte != nil {
				r.onFirstByte(latencyMs)
			}
			log.Debugf("first byte timeout: received first byte after %dms", latencyMs)
		} else {
			r.mu.Unlock()
		}
		return readN, readErr

	case <-ctx.Done():
		r.mu.Lock()
		r.timedOut = true
		r.mu.Unlock()

		if r.onTimeout != nil {
			r.onTimeout()
		}
		log.Warnf("first byte timeout: no response after %v", r.timeout)
		return 0, ErrFirstByteTimeout
	}
}

// Close closes the underlying reader
func (r *FirstByteReader) Close() error {
	r.mu.Lock()
	if r.cancelFunc != nil {
		r.cancelFunc()
	}
	r.mu.Unlock()
	return r.reader.Close()
}

// GetLatency returns the first byte latency in milliseconds, or -1 if not received
func (r *FirstByteReader) GetLatency() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gotFirstByte {
		return r.firstByteAt.Sub(r.startTime).Milliseconds()
	}
	return -1
}

// IsTimedOut returns true if the first byte timeout was triggered
func (r *FirstByteReader) IsTimedOut() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.timedOut
}

// ================================================================
// HTTP Response wrapper with first byte timeout
// ================================================================

// FirstByteTimeoutTransport wraps an http.RoundTripper with first byte timeout
type FirstByteTimeoutTransport struct {
	Transport   http.RoundTripper
	Timeout     time.Duration
	OnTimeout   func(req *http.Request)
	OnFirstByte func(req *http.Request, latencyMs int64)
}

// RoundTrip implements http.RoundTripper with first byte timeout detection
func (t *FirstByteTimeoutTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	startTime := time.Now()

	// Create a context with timeout for the entire request initiation
	ctx, cancel := context.WithTimeout(req.Context(), t.Timeout)
	defer cancel()

	// Create request with timeout context
	reqWithTimeout := req.WithContext(ctx)

	resp, err := t.Transport.RoundTrip(reqWithTimeout)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			if t.OnTimeout != nil {
				t.OnTimeout(req)
			}
			return nil, ErrFirstByteTimeout
		}
		return nil, err
	}

	// Got response headers - this is our "first byte"
	latencyMs := time.Since(startTime).Milliseconds()
	if t.OnFirstByte != nil {
		t.OnFirstByte(req, latencyMs)
	}

	return resp, nil
}

// ================================================================
// Helper functions for stream execution with first byte timeout
// ================================================================

// StreamWithFirstByteTimeout wraps a streaming HTTP request with first byte timeout
// Returns the response and whether timeout occurred
func StreamWithFirstByteTimeout(
	ctx context.Context,
	client *http.Client,
	req *http.Request,
	timeout time.Duration,
	onTimeout func(),
	onFirstByte func(latencyMs int64),
) (*http.Response, error) {
	startTime := time.Now()

	// Create a separate context for first byte timeout
	firstByteCtx, cancel := context.WithTimeout(ctx, timeout)

	// Channel to receive the response
	type result struct {
		resp *http.Response
		err  error
	}
	resultCh := make(chan result, 1)

	go func() {
		resp, err := client.Do(req)
		resultCh <- result{resp: resp, err: err}
	}()

	select {
	case res := <-resultCh:
		cancel() // Cancel the timeout context

		if res.err != nil {
			return nil, res.err
		}

		// Got response headers
		latencyMs := time.Since(startTime).Milliseconds()
		if onFirstByte != nil {
			onFirstByte(latencyMs)
		}
		log.Debugf("first byte timeout: response headers received after %dms", latencyMs)

		return res.resp, nil

	case <-firstByteCtx.Done():
		cancel()

		if onTimeout != nil {
			onTimeout()
		}
		log.Warnf("first byte timeout: no response headers after %v", timeout)

		// Try to get the response to close it properly
		go func() {
			res := <-resultCh
			if res.resp != nil {
				res.resp.Body.Close()
			}
		}()

		return nil, ErrFirstByteTimeout

	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	}
}
