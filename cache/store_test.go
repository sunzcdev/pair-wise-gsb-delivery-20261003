package cache

import (
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestDistinctVaryRevalidateSeparately ensures two stale variants of the same
// URL never share one revalidation.
func TestDistinctVaryRevalidateSeparately(t *testing.T) {
	clock := NewManualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	c := New(Options{Clock: clock})

	prime := func(ctx context.Context, r *http.Request) (*http.Response, error) {
		lang := r.Header.Get("Accept-Language")
		h := http.Header{}
		h.Set("Cache-Control", "max-age=60")
		h.Set("Vary", "Accept-Language")
		h.Set("ETag", `"`+lang+`"`)
		return &http.Response{StatusCode: http.StatusOK, Header: h, Body: io.NopCloser(stringReader("b-" + lang))}, nil
	}
	en := newTestRequest("http://example.com/v")
	en.Header.Set("Accept-Language", "en")
	fr := newTestRequest("http://example.com/v")
	fr.Header.Set("Accept-Language", "fr")
	readBody(t, mustFetch(t, c, en, prime))
	readBody(t, mustFetch(t, c, fr, prime))

	clock.Advance(61 * time.Second)

	var enHits, frHits int32
	release := make(chan struct{})
	rev := func(ctx context.Context, r *http.Request) (*http.Response, error) {
		switch r.Header.Get("Accept-Language") {
		case "en":
			atomic.AddInt32(&enHits, 1)
		case "fr":
			atomic.AddInt32(&frHits, 1)
		}
		<-release
		return &http.Response{
			StatusCode: http.StatusNotModified,
			Header:     http.Header{},
			Body:       io.NopCloser(emptyReader{}),
		}, nil
	}

	var wg sync.WaitGroup
	wg.Add(2)
	for _, r := range []*http.Request{en, fr} {
		r := r
		go func() {
			defer wg.Done()
			resp, err := c.RoundTrip(r, rev)
			if err != nil {
				t.Errorf("revalidate: %v", err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if enHits != 1 || frHits != 1 {
		t.Fatalf("revalidations en=%d fr=%d, want 1 each", enHits, frHits)
	}
}

// fakeStore is a minimal custom Store proving the backend is replaceable.
type fakeStore struct {
	mu      sync.Mutex
	entries map[string]*Entry
	bodies  map[string][]byte
}

func newFakeStore() *fakeStore {
	return &fakeStore{entries: map[string]*Entry{}, bodies: map[string][]byte{}}
}

func (s *fakeStore) GetEntry(key string) (*Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	return e, ok
}
func (s *fakeStore) PutEntry(e *Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[e.PrimaryKey] = e
}
func (s *fakeStore) DeleteEntry(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, key)
}
func (s *fakeStore) PutBody(id string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bodies[id] = body
}
func (s *fakeStore) GetBody(id string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.bodies[id]
	return b, ok
}
func (s *fakeStore) DeleteBody(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.bodies, id)
}

func TestCustomStore(t *testing.T) {
	fs := newFakeStore()
	c := New(Options{Store: fs})
	fetch := func(ctx context.Context, r *http.Request) (*http.Response, error) {
		return okResponse("max-age=60", "custom", nil), nil
	}
	readBody(t, mustFetch(t, c, newTestRequest("http://example.com/cs"), fetch))

	fs.mu.Lock()
	ne, nb := len(fs.entries), len(fs.bodies)
	fs.mu.Unlock()
	if ne != 1 {
		t.Fatalf("custom store entries=%d want 1", ne)
	}
	if nb != 1 {
		t.Fatalf("custom store bodies=%d want 1", nb)
	}

	// Second read is served from the custom store, not origin.
	readBody(t, mustGet(t, c, newTestRequest("http://example.com/cs")))
}
