package carddav

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/emersion/go-vcard"
	"github.com/emersion/go-webdav"
)

type putCapturingBackend struct {
	testBackend
	got *PutAddressObjectOptions
}

func (b *putCapturingBackend) PutAddressObject(ctx context.Context, path string, card vcard.Card, opts *PutAddressObjectOptions) (*AddressObject, error) {
	b.got = opts
	return &AddressObject{Path: path, ETag: `"new"`}, nil
}

func newCard() vcard.Card {
	card := vcard.Card{}
	card.AddValue(vcard.FieldVersion, "4.0")
	card.AddValue(vcard.FieldUID, "uid@example.com")
	card.AddValue(vcard.FieldFormattedName, "Alice")
	return card
}

func TestClientPutConditionalHeaders(t *testing.T) {
	var seen http.Header
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Header().Set("ETag", `"v2"`)
		w.WriteHeader(http.StatusCreated)
	}))
	defer ts.Close()

	c, err := NewClient(nil, ts.URL)
	if err != nil {
		t.Fatal(err)
	}

	opts := &PutAddressObjectOptions{}
	opts.IfMatch, err = webdav.ParseConditionalMatch(`"v1", W/"other"`)
	if err != nil {
		t.Fatal(err)
	}
	opts.IfNoneMatch, err = webdav.ParseConditionalMatch("*")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.PutAddressObject(context.Background(), "/ab/a.vcf", newCard(), opts); err != nil {
		t.Fatal(err)
	}

	if got := seen.Get("If-Match"); got != `"v1", W/"other"` {
		t.Errorf("If-Match = %q", got)
	}
	if got := seen.Get("If-None-Match"); got != "*" {
		t.Errorf("If-None-Match = %q", got)
	}
}

func TestServerPutMalformedHeaderRejectsEarly(t *testing.T) {
	b := &putCapturingBackend{}
	h := &Handler{Backend: b}
	req := httptest.NewRequest(http.MethodPut, "/ab/a.vcf", strings.NewReader("not a vcard"))
	req.Header.Set("Content-Type", "text/vcard")
	req.Header.Set("If-None-Match", `*, "a"`)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if b.got != nil {
		t.Fatalf("backend must not be invoked when the header fails to parse")
	}
}
