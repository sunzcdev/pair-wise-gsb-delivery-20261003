package carddav

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/emersion/go-vcard"
	"github.com/emersion/go-webdav"
)

// atomicAddressBackend is a CardDAV backend evaluating conditions inside a
// mutex-guarded commit, i.e. atomically.
type atomicAddressBackend struct {
	*testBackend

	mu     sync.Mutex
	store  map[string]storedAddress
	nextID int
}

type storedAddress struct {
	card vcard.Card
	etag string
}

func newAtomicAddressBackend() *atomicAddressBackend {
	return &atomicAddressBackend{
		testBackend: &testBackend{
			addressBooks: []AddressBook{{Path: "/user/abooks/default"}},
		},
		store: map[string]storedAddress{},
	}
}

func (b *atomicAddressBackend) GetAddressObject(ctx context.Context, path string, req *AddressDataRequest) (*AddressObject, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	obj, ok := b.store[path]
	if !ok {
		return nil, webdav.NewHTTPError(http.StatusNotFound, nil)
	}
	return &AddressObject{Path: path, Card: obj.card, ETag: obj.etag}, nil
}

func (b *atomicAddressBackend) PutAddressObject(ctx context.Context, path string, card vcard.Card, opts *PutAddressObjectOptions) (*AddressObject, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	var conds webdav.Conditions
	if opts != nil {
		conds = opts.Conditions
	}

	current, exists := b.store[path]
	var currentETag webdav.ETag
	if exists {
		tag, err := webdav.ParseETag(current.etag)
		if err != nil {
			return nil, err
		}
		currentETag = tag
	}

	if err := webdav.CheckConditionsAtomic(conds, exists, currentETag); err != nil {
		return nil, err
	}

	b.nextID++
	etag := "etag-" + string(rune('a'+b.nextID))
	b.store[path] = storedAddress{card: card, etag: etag}
	return &AddressObject{Path: path, Card: card, ETag: etag}, nil
}

func validCard() vcard.Card {
	card := vcard.Card{}
	card.SetValue(vcard.FieldVersion, "4.0")
	card.SetValue(vcard.FieldFormattedName, "Alice Gopher")
	card.SetValue(vcard.FieldUID, "uid-1@example.com")
	return card
}

// Two clients concurrently PUT using the same current ETag: exactly one
// succeeds.
func TestAddressObjectConcurrentIfMatchRace(t *testing.T) {
	srv := httptest.NewServer(&Handler{Backend: newAtomicAddressBackend()})
	defer srv.Close()

	c, err := NewClient(http.DefaultClient, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	const path = "/user/abooks/default/alice.vcf"
	if _, err := c.PutAddressObject(context.Background(), path, validCard(), nil); err != nil {
		t.Fatal(err)
	}
	obj, err := c.GetAddressObject(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	etag := obj.ETag

	put := func() int {
		tag, err := webdav.ParseETag(etag)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.PutAddressObject(context.Background(), path, validCard(), &PutAddressObjectClientOptions{
			Conditions: webdav.Conditions{IfMatch: webdav.NewConditionalMatch(tag)},
		})
		return addressStatus(t, err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- put()
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var ok, failed int
	for code := range results {
		switch code {
		case http.StatusCreated, http.StatusNoContent:
			ok++
		case http.StatusPreconditionFailed:
			failed++
		default:
			t.Fatalf("unexpected status %d", code)
		}
	}
	if ok != 1 || failed != 1 {
		t.Fatalf("expected one success and one 412, got %d ok / %d fail", ok, failed)
	}
}

// If-None-Match "*" allows the first create and rejects the second without
// replacing the stored object.
func TestAddressObjectIfNoneMatchWildcard(t *testing.T) {
	srv := httptest.NewServer(&Handler{Backend: newAtomicAddressBackend()})
	defer srv.Close()

	c, err := NewClient(http.DefaultClient, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	const path = "/user/abooks/default/new.vcf"

	opts := &PutAddressObjectClientOptions{Conditions: webdav.Conditions{IfNoneMatch: webdav.NewWildcardMatch()}}
	if _, err := c.PutAddressObject(context.Background(), path, validCard(), opts); err != nil {
		t.Fatalf("first create: %v", err)
	}
	first, err := c.GetAddressObject(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.PutAddressObject(context.Background(), path, validCard(), opts); err == nil {
		t.Fatal("second create with If-None-Match * should fail")
	} else if addressStatus(t, err) != http.StatusPreconditionFailed {
		t.Fatalf("expected 412, got %v", err)
	}

	second, err := c.GetAddressObject(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if second.ETag != first.ETag {
		t.Fatalf("failed If-None-Match replaced object: %q -> %q", first.ETag, second.ETag)
	}
}

// A malformed conditional header is rejected with 400 before the vCard body
// is parsed.
func TestAddressObjectMalformedHeader(t *testing.T) {
	h := &Handler{Backend: newAtomicAddressBackend()}

	req := httptest.NewRequest(http.MethodPut, "/user/abooks/default/alice.vcf",
		strings.NewReader("BEGIN:VCARD\r\nVERSION:4.0\r\nEND:VCARD\r\n"))
	req.Header.Set("Content-Type", vcard.MIMEType)
	req.Header.Set("If-None-Match", "W/unquoted")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func addressStatus(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return http.StatusCreated
	}
	if httpErr, ok := err.(*webdav.HTTPError); ok {
		return httpErr.Code
	}
	t.Fatalf("unexpected non-HTTP error: %v", err)
	return 0
}
