package carddav

import (
	"context"
	"encoding/xml"
	"io/ioutil"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/emersion/go-vcard"
	"github.com/emersion/go-webdav/internal"
)

const abPath = "/user/contacts/ab/"

func testCard(uid, fn string) vcard.Card {
	card := vcard.Card{}
	card.AddValue(vcard.FieldVersion, "4.0")
	card.AddValue(vcard.FieldUID, uid)
	card.AddValue(vcard.FieldFormattedName, fn)
	return card
}

func doCardSyncReport(t *testing.T, h *Handler, path, body string) (int, *internal.MultiStatus) {
	t.Helper()
	req := httptest.NewRequest("REPORT", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/xml")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	res := w.Result()
	defer res.Body.Close()
	data, _ := ioutil.ReadAll(res.Body)
	if res.StatusCode != 207 {
		return res.StatusCode, nil
	}
	var ms internal.MultiStatus
	if err := xml.Unmarshal(data, &ms); err != nil {
		t.Fatalf("decode multistatus: %v\n%s", err, data)
	}
	return res.StatusCode, &ms
}

func cardSyncBody(token string, limit int) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?>
<D:sync-collection xmlns:D="DAV:">
  <D:sync-level>1</D:sync-level>`)
	if token != "" {
		b.WriteString("\n  <D:sync-token>" + token + "</D:sync-token>")
	}
	if limit > 0 {
		b.WriteString("\n  <D:limit><D:nresults>" + strconv.Itoa(limit) + "</D:nresults></D:limit>")
	}
	b.WriteString(`
  <D:prop><D:getetag/></D:prop>
</D:sync-collection>`)
	return b.String()
}

func cardPaths(ms *internal.MultiStatus) (updated, deleted []string) {
	for _, resp := range ms.Responses {
		p, err := resp.Path()
		if err != nil {
			deleted = append(deleted, p)
			continue
		}
		updated = append(updated, p)
	}
	return updated, deleted
}

func assertHas(t *testing.T, got []string, want string) {
	t.Helper()
	for _, s := range got {
		if s == want {
			return
		}
	}
	t.Errorf("expected %q in %v", want, got)
}

func assertMissing(t *testing.T, got []string, want string) {
	t.Helper()
	for _, s := range got {
		if s == want {
			t.Errorf("did not expect %q in %v", want, got)
			return
		}
	}
}

// TestCardSyncDeleteRecreate verifies the empty-token initial sync and that a
// deleted-then-recreated member is reported as an update rather than a 404
// deletion (RFC 6578 3.10).
func TestCardSyncDeleteRecreate(t *testing.T) {
	ctx := context.Background()
	b := newMemSyncBackend()
	if err := b.CreateAddressBook(ctx, &AddressBook{Path: abPath}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{Backend: b}

	x := abPath + "x.vcf"
	if _, err := b.PutAddressObject(ctx, x, testCard("x", "X"), nil); err != nil {
		t.Fatal(err)
	}

	_, ms := doCardSyncReport(t, h, abPath, cardSyncBody("", 0))
	token := ms.SyncToken
	if token == "" {
		t.Fatal("empty sync token after initial sync")
	}
	updated, deleted := cardPaths(ms)
	assertHas(t, updated, x)

	if err := b.DeleteAddressObject(ctx, x); err != nil {
		t.Fatal(err)
	}
	_, ms = doCardSyncReport(t, h, abPath, cardSyncBody(token, 0))
	afterDelete := ms.SyncToken
	_, deleted = cardPaths(ms)
	assertHas(t, deleted, x)

	if _, err := b.PutAddressObject(ctx, x, testCard("x", "X Reborn"), nil); err != nil {
		t.Fatal(err)
	}
	_, ms = doCardSyncReport(t, h, abPath, cardSyncBody(afterDelete, 0))
	updated, deleted = cardPaths(ms)
	assertHas(t, updated, x)
	assertMissing(t, deleted, x)
}

// TestCardSyncWrongCollection verifies a token from another address book (or a
// recreated one) is rejected with DAV:valid-sync-token.
func TestCardSyncWrongCollection(t *testing.T) {
	ctx := context.Background()
	b := newMemSyncBackend()
	h := &Handler{Backend: b}

	other := "/user/contacts/other/"
	for _, p := range []string{abPath, other} {
		if err := b.CreateAddressBook(ctx, &AddressBook{Path: p}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.PutAddressObject(ctx, other+"y.vcf", testCard("y", "Y"), nil); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("REPORT", other, strings.NewReader(cardSyncBody("", 0)))
	req.Header.Set("Content-Type", "application/xml")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var otherMS internal.MultiStatus
	if err := xml.Unmarshal(w.Body.Bytes(), &otherMS); err != nil {
		t.Fatal(err)
	}

	status, _ := doCardSyncReport(t, h, abPath, cardSyncBody(otherMS.SyncToken, 0))
	if status != 403 {
		t.Fatalf("foreign token: expected 403, got %d", status)
	}

	_, own := doCardSyncReport(t, h, abPath, cardSyncBody("", 0))
	old := own.SyncToken
	if err := b.DeleteAddressBook(ctx, abPath); err != nil {
		t.Fatal(err)
	}
	if err := b.CreateAddressBook(ctx, &AddressBook{Path: abPath}); err != nil {
		t.Fatal(err)
	}
	status, _ = doCardSyncReport(t, h, abPath, cardSyncBody(old, 0))
	if status != 403 {
		t.Fatalf("recreated collection token: expected 403, got %d", status)
	}
}

// TestCardSyncPagination verifies pages are pinned to one snapshot and the
// final token is only returned after every change is read.
func TestCardSyncPagination(t *testing.T) {
	ctx := context.Background()
	b := newMemSyncBackend()
	if err := b.CreateAddressBook(ctx, &AddressBook{Path: abPath}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{Backend: b}

	const n = 4
	for i := 0; i < n; i++ {
		p := abPath + string(rune('a'+i)) + ".vcf"
		if _, err := b.PutAddressObject(ctx, p, testCard(string(rune('a'+i)), "Card"), nil); err != nil {
			t.Fatal(err)
		}
	}

	seen := map[string]bool{}
	token := ""
	pages := 0
	var finalToken string
	for {
		_, ms := doCardSyncReport(t, h, abPath, cardSyncBody(token, 2))
		for _, resp := range ms.Responses {
			p, err := resp.Path()
			if err != nil {
				t.Fatal(err)
			}
			if seen[p] {
				t.Fatalf("duplicate across pages: %q", p)
			}
			seen[p] = true
		}
		token = ms.SyncToken
		pages++
		if ms.MoreResults == nil {
			finalToken = token
			break
		}
	}

	if len(seen) != n {
		t.Fatalf("delivered %d members, want %d", len(seen), n)
	}
	if pages != 2 {
		t.Fatalf("expected 2 pages, got %d", pages)
	}
	if !strings.HasPrefix(finalToken, "token-") {
		t.Fatalf("final token not committed: %q", finalToken)
	}

	// Mutating the collection while a limited sync is in progress must not
	// leak into the pinned snapshot.
	_, first := doCardSyncReport(t, h, abPath, cardSyncBody("", 2))
	if first.MoreResults == nil {
		t.Fatal("expected more-results")
	}
	if !strings.HasPrefix(first.SyncToken, "page-") {
		t.Fatalf("intermediate token not a page token: %q", first.SyncToken)
	}
	if _, err := b.PutAddressObject(ctx, abPath+"late.vcf", testCard("late", "Late"), nil); err != nil {
		t.Fatal(err)
	}
	_, cont := doCardSyncReport(t, h, abPath, cardSyncBody(first.SyncToken, 2))
	for _, resp := range cont.Responses {
		p, _ := resp.Path()
		if strings.HasSuffix(p, "late.vcf") {
			t.Fatalf("mid-pagination mutation leaked into snapshot: %q", p)
		}
	}
}

// TestCardSyncUnsupported verifies the capability is not advertised and the
// REPORT is rejected when the backend doesn't implement sync-collection.
func TestCardSyncUnsupported(t *testing.T) {
	h := &Handler{Backend: &testBackend{}}

	req := httptest.NewRequest("OPTIONS", abPath, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if strings.Contains(w.Header().Get("DAV"), "sync-collection") {
		t.Fatalf("advertised sync-collection without support: %q", w.Header().Get("DAV"))
	}

	r := httptest.NewRequest("REPORT", abPath, strings.NewReader(cardSyncBody("", 0)))
	r.Header.Set("Content-Type", "application/xml")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, r)
	if rw.Code != 501 {
		t.Fatalf("expected 501, got %d", rw.Code)
	}
}
