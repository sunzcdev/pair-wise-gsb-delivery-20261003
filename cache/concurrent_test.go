package cache

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestCoalescedRevalidation proves that many concurrent callers for one
// stale representation share a single upstream conditional request.
func TestCoalescedRevalidation(t *testing.T) {
	clock := NewManualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	c := New(Options{Clock: clock})

	prime := func(ctx context.Context, r *http.Request) (*http.Response, error) {
		resp := okResponse("max-age=60", "shared", func(h http.Header) {
			h.Set("ETag", `"v1"`)
		})
		return resp, nil
	}

	req := newTestRequest("http://example.com/co")
	readBody(t, mustFetch(t, c, req, prime))
	clock.Advance(61 * time.Second)

	var revHits int32
	release := make(chan struct{})
	revalidate := func(ctx context.Context, r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&revHits, 1)
		if got := r.Header.Get("If-None-Match"); got != `"v1"` {
			t.Errorf("If-None-Match=%q", got)
		}
		<-release
		return &http.Response{
			StatusCode: http.StatusNotModified,
			Header:     http.Header{},
			Body:       io.NopCloser(emptyReader{}),
		}, nil
	}

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	bodies := make([]string, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			resp, err := c.RoundTrip(newTestRequest("http://example.com/co"), revalidate)
			if err != nil {
				errs[i] = err
				return
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			bodies[i] = string(b)
		}(i)
	}

	// Give goroutines time to park on the single flight.
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&revHits); got != 1 {
		t.Fatalf("revalidation requests = %d, want 1", got)
	}

	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d err: %v", i, err)
		}
		if bodies[i] != "shared" {
			t.Fatalf("caller %d body=%q", i, bodies[i])
		}
	}
}

// TestCancellingOneWaiterDoesNotCancelOthers proves cancellation isolation:
// the shared upstream revalidation uses a detached context, so one caller
// giving up neither aborts the fetch nor prevents the others being served.
func TestCancellingOneWaiterDoesNotCancelOthers(t *testing.T) {
	clock := NewManualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	c := New(Options{Clock: clock})

	prime := func(ctx context.Context, r *http.Request) (*http.Response, error) {
		return okResponse("max-age=60", "shared", func(h http.Header) {
			h.Set("ETag", `"v1"`)
		}), nil
	}
	readBody(t, mustFetch(t, c, newTestRequest("http://example.com/iso"), prime))
	clock.Advance(61 * time.Second)

	var upstreamAlive int32
	release := make(chan struct{})
	revalidate := func(ctx context.Context, r *http.Request) (*http.Response, error) {
		atomic.StoreInt32(&upstreamAlive, 1)
		defer atomic.StoreInt32(&upstreamAlive, 0)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &http.Response{
			StatusCode: http.StatusNotModified,
			Header:     http.Header{},
			Body:       io.NopCloser(emptyReader{}),
		}, nil
	}

	cancelCtx, cancel := context.WithCancel(context.Background())

	quitErr := make(chan error, 1)
	go func() {
		req := newTestRequest("http://example.com/iso").WithContext(cancelCtx)
		_, err := c.RoundTrip(req, revalidate)
		quitErr <- err
	}()

	otherDone := make(chan string, 1)
	go func() {
		resp, err := c.RoundTrip(newTestRequest("http://example.com/iso"), revalidate)
		if err != nil {
			otherDone <- "ERR:" + err.Error()
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		otherDone <- string(b)
	}()

	time.Sleep(50 * time.Millisecond)

	// First caller cancels its own request.
	cancel()

	select {
	case err := <-quitErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled waiter err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled waiter did not return")
	}

	// The upstream revalidation must still be running.
	if atomic.LoadInt32(&upstreamAlive) != 1 {
		t.Fatal("shared revalidation was cancelled by one waiter")
	}

	close(release)

	select {
	case got := <-otherDone:
		if got != "shared" {
			t.Fatalf("other waiter got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("other waiter was not served")
	}
}
