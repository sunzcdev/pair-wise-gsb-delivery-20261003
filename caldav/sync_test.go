package caldav

import (
	"context"
	"encoding/xml"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav/internal"
)

const calPath = "/user/calendars/cal/"

func testCalendar(uid string) *ical.Calendar {
	ev := ical.NewEvent()
	ev.Name = ical.CompEvent
	ev.Props.SetText(ical.PropUID, uid)
	ev.Props.SetDateTime(ical.PropDateTimeStamp, time.Now().UTC())
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropProductID, "-//test//EN")
	cal.Children = []*ical.Component{ev.Component}
	return cal
}

func doSyncReport(t *testing.T, h *Handler, body string) (int, *internal.MultiStatus) {
	t.Helper()
	req := httptest.NewRequest("REPORT", calPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/xml")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	res := w.Result()
	defer res.Body.Close()
	data, err := ioutil.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}

	if res.StatusCode != 207 {
		var errElt internal.Error
		if xml.Unmarshal(data, &errElt) == nil {
			return res.StatusCode, nil
		}
		t.Fatalf("unexpected status %d: %s", res.StatusCode, data)
	}

	var ms internal.MultiStatus
	if err := xml.Unmarshal(data, &ms); err != nil {
		t.Fatalf("decode multistatus: %v\n%s", err, data)
	}
	return res.StatusCode, &ms
}

func syncBody(token string, limit int) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>
<D:sync-collection xmlns:D="DAV:">
  <D:sync-level>1</D:sync-level>`)
	if token != "" {
		b.WriteString("\n  <D:sync-token>" + token + "</D:sync-token>")
	}
	if limit > 0 {
		b.WriteString("\n  <D:limit><D:nresults>")
		b.WriteString(strconv.Itoa(limit))
		b.WriteString("</D:nresults></D:limit>")
	}
	b.WriteString(`
  <D:prop>
    <D:getetag/>
  </D:prop>
</D:sync-collection>`)
	return b.String()
}

func responsePaths(ms *internal.MultiStatus) (updated, deleted []string) {
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

func assertContains(t *testing.T, got []string, want string) {
	t.Helper()
	for _, s := range got {
		if s == want {
			return
		}
	}
	t.Errorf("expected %q in %v", want, got)
}

func assertNotContains(t *testing.T, got []string, want string) {
	t.Helper()
	for _, s := range got {
		if s == want {
			t.Errorf("did not expect %q in %v", want, got)
			return
		}
	}
}

// TestSyncCollectionInitialAndIncremental verifies the empty-token initial
// sync, the delete-and-recreate collapse rule (RFC 6578 3.10), explicit 404
// deletions and token advancement.
func TestSyncCollectionInitialAndIncremental(t *testing.T) {
	ctx := context.Background()
	b := newMemSyncBackend()
	if err := b.CreateCalendar(ctx, &Calendar{Path: calPath}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{Backend: b}

	a := calPath + "a.ics"
	c := calPath + "c.ics"
	if _, err := b.PutCalendarObject(ctx, a, testCalendar("a"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := b.PutCalendarObject(ctx, c, testCalendar("c"), nil); err != nil {
		t.Fatal(err)
	}

	// Initial sync with an empty token.
	_, ms := doSyncReport(t, h, syncBody("", 0))
	if ms.SyncToken == "" {
		t.Fatal("initial sync returned empty sync token")
	}
	initialToken := ms.SyncToken
	updated, deleted := responsePaths(ms)
	if len(deleted) != 0 {
		t.Fatalf("initial sync reported deletions: %v", deleted)
	}
	assertContains(t, updated, a)
	assertContains(t, updated, c)

	// Modify a, delete c.
	if _, err := b.PutCalendarObject(ctx, a, testCalendar("a-mod"), nil); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteCalendarObject(ctx, c); err != nil {
		t.Fatal(err)
	}

	_, ms = doSyncReport(t, h, syncBody(initialToken, 0))
	tokenAfterDelete := ms.SyncToken
	updated, deleted = responsePaths(ms)
	assertContains(t, updated, a)
	assertContains(t, deleted, c)

	// Recreate c at the same path: an incremental sync must report it as an
	// update, never as a deletion, so clients don't drop a live resource.
	if _, err := b.PutCalendarObject(ctx, c, testCalendar("c-recreated"), nil); err != nil {
		t.Fatal(err)
	}
	_, ms = doSyncReport(t, h, syncBody(tokenAfterDelete, 0))
	updated, deleted = responsePaths(ms)
	assertContains(t, updated, c)
	assertNotContains(t, deleted, c)
}

// TestSyncCollectionTokenForWrongCollection verifies a token issued by one
// collection is rejected with DAV:valid-sync-token when presented to another,
// including after the original collection was deleted and recreated.
func TestSyncCollectionTokenForWrongCollection(t *testing.T) {
	ctx := context.Background()
	b := newMemSyncBackend()
	h := &Handler{Backend: b}

	otherPath := "/user/calendars/other/"
	for _, p := range []string{calPath, otherPath} {
		if err := b.CreateCalendar(ctx, &Calendar{Path: p}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.PutCalendarObject(ctx, otherPath+"x.ics", testCalendar("x"), nil); err != nil {
		t.Fatal(err)
	}

	// Obtain a token for the "other" calendar.
	req := httptest.NewRequest("REPORT", otherPath, strings.NewReader(syncBody("", 0)))
	req.Header.Set("Content-Type", "application/xml")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var otherMS internal.MultiStatus
	if err := xml.Unmarshal(w.Body.Bytes(), &otherMS); err != nil {
		t.Fatal(err)
	}

	// Present the foreign token against calPath: expect HTTP 403.
	status, _ := doSyncReport(t, h, syncBody(otherMS.SyncToken, 0))
	if status != 403 {
		t.Fatalf("wrong-collection token: expected 403, got %d", status)
	}

	// Recreating calPath at the same location allocates a new collection ID,
	// so its old tokens must now be invalid too.
	_, ownMS := doSyncReport(t, h, syncBody("", 0))
	oldToken := ownMS.SyncToken
	if err := b.DeleteCalendar(ctx, calPath); err != nil {
		t.Fatal(err)
	}
	if err := b.CreateCalendar(ctx, &Calendar{Path: calPath}); err != nil {
		t.Fatal(err)
	}
	status, _ = doSyncReport(t, h, syncBody(oldToken, 0))
	if status != 403 {
		t.Fatalf("token after collection recreate: expected 403, got %d", status)
	}
}

// TestSyncCollectionPaginationSameSnapshot verifies that all pages are bound
// to one snapshot: intermediate pages carry more-results and an intermediate
// token; no change is dropped or duplicated; the final committed token is only
// returned after every change has been read, even when the collection mutates
// mid-sequence.
func TestSyncCollectionPaginationSameSnapshot(t *testing.T) {
	ctx := context.Background()
	b := newMemSyncBackend()
	if err := b.CreateCalendar(ctx, &Calendar{Path: calPath}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{Backend: b}

	const n = 5
	for i := 0; i < n; i++ {
		path := calPath + string(rune('a'+i)) + ".ics"
		if _, err := b.PutCalendarObject(ctx, path, testCalendar(string(rune('a'+i))), nil); err != nil {
			t.Fatal(err)
		}
	}

	var seen []string
	token := ""
	pages := 0
	for {
		_, ms := doSyncReport(t, h, syncBody(token, 2))
		for _, resp := range ms.Responses {
			p, err := resp.Path()
			if err != nil {
				t.Fatalf("unexpected deletion during initial pagination: %v", err)
			}
			seen = append(seen, p)
		}
		token = ms.SyncToken
		pages++
		if ms.MoreResults == nil {
			break
		}
		if pages > n {
			t.Fatal("pagination did not terminate")
		}
	}

	if len(seen) != n {
		t.Fatalf("paginated initial sync delivered %d responses, want %d", len(seen), n)
	}
	dup := map[string]bool{}
	for _, p := range seen {
		if dup[p] {
			t.Fatalf("duplicate response across pages: %q", p)
		}
		dup[p] = true
	}
	if pages != 3 {
		t.Fatalf("expected 3 pages for 5 items with limit 2, got %d", pages)
	}

	// Start a fresh limited sync and mutate after the first page. The pending
	// pages must still be computed against the original snapshot.
	_, ms := doSyncReport(t, h, syncBody("", 2))
	if ms.MoreResults == nil {
		t.Fatal("expected more-results on first limited page")
	}
	firstToken := ms.SyncToken
	if strings.HasPrefix(firstToken, "token-") {
		t.Fatal("intermediate page returned a committed token")
	}

	if _, err := b.PutCalendarObject(ctx, calPath+"late.ics", testCalendar("late"), nil); err != nil {
		t.Fatal(err)
	}

	var paged []string
	token = firstToken
	for {
		_, pageMS := doSyncReport(t, h, syncBody(token, 2))
		for _, resp := range pageMS.Responses {
			p, err := resp.Path()
			if err != nil {
				t.Fatal(err)
			}
			paged = append(paged, p)
		}
		token = pageMS.SyncToken
		if pageMS.MoreResults == nil {
			break
		}
	}
	for _, p := range paged {
		if strings.HasSuffix(p, "late.ics") {
			t.Fatalf("mutation during pagination leaked into the old snapshot: %q", p)
		}
	}

	// Now that the first sync completed and committed its token, the late
	// mutation must be visible in the next incremental sync.
	_, ms = doSyncReport(t, h, syncBody(token, 0))
	updated, _ := responsePaths(ms)
	assertContains(t, updated, calPath+"late.ics")
}

// TestSyncCollectionUnsupportedBackend verifies a backend that doesn't
// implement SyncCollectionBackend keeps the old capability declaration and
// rejects sync-collection REPORTs.
func TestSyncCollectionUnsupportedBackend(t *testing.T) {
	h := &Handler{Backend: testBackend{}}

	req := httptest.NewRequest("OPTIONS", calPath, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if strings.Contains(w.Header().Get("DAV"), "sync-collection") {
		t.Fatalf("DAV header advertised sync-collection without backend support: %q", w.Header().Get("DAV"))
	}

	status, _ := func() (int, *internal.MultiStatus) {
		r := httptest.NewRequest("REPORT", calPath, strings.NewReader(syncBody("", 0)))
		r.Header.Set("Content-Type", "application/xml")
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, r)
		return rw.Code, nil
	}()
	if status != 501 {
		t.Fatalf("unsupported backend: expected 501, got %d", status)
	}
}

// TestSyncCollectionAdvertisedAndTokenProp verifies the DAV feature is
// advertised and DAV:sync-token is exposed via PROPFIND on the calendar.
func TestSyncCollectionAdvertisedAndTokenProp(t *testing.T) {
	ctx := context.Background()
	b := newMemSyncBackend()
	if err := b.CreateCalendar(ctx, &Calendar{Path: calPath}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{Backend: b}

	req := httptest.NewRequest("OPTIONS", calPath, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if !strings.Contains(w.Header().Get("DAV"), "sync-collection") {
		t.Fatalf("DAV header missing sync-collection: %q", w.Header().Get("DAV"))
	}

	propfind := `<?xml version="1.0"?>
<D:propfind xmlns:D="DAV:"><D:prop><D:sync-token/></D:prop></D:propfind>`
	req = httptest.NewRequest("PROPFIND", calPath, strings.NewReader(propfind))
	req.Header.Set("Content-Type", "application/xml")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "<sync-token") {
		t.Fatalf("PROPFIND did not return DAV:sync-token:\n%s", w.Body.String())
	}
}

// TestSyncCollectionBadLevel verifies an unsupported sync-level is rejected.
func TestSyncCollectionBadLevel(t *testing.T) {
	ctx := context.Background()
	b := newMemSyncBackend()
	if err := b.CreateCalendar(ctx, &Calendar{Path: calPath}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{Backend: b}

	bad := strings.Replace(syncBody("", 0), "<D:sync-level>1</D:sync-level>", "<D:sync-level>infinity</D:sync-level>", 1)
	req := httptest.NewRequest("REPORT", calPath, strings.NewReader(bad))
	req.Header.Set("Content-Type", "application/xml")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for sync-level infinity, got %d", w.Code)
	}
}

// TestSyncCollectionClientEndToEnd drives the server through the real CalDAV
// client, following more-results pages and verifying that deletions arrive as
// explicit removed members.
func TestSyncCollectionClientEndToEnd(t *testing.T) {
	ctx := context.Background()
	b := newMemSyncBackend()
	if err := b.CreateCalendar(ctx, &Calendar{Path: calPath}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(&Handler{Backend: b})
	defer ts.Close()

	client, err := NewClient(nil, ts.URL)
	if err != nil {
		t.Fatal(err)
	}

	paths := []string{"1.ics", "2.ics", "3.ics"}
	for i, name := range paths {
		if _, err := b.PutCalendarObject(ctx, calPath+name, testCalendar(name), nil); err != nil {
			t.Fatal(err)
		}
		_ = i
	}

	// Initial sync with a page limit, following pages via MoreResults.
	token := ""
	var updated []CalendarObject
	for {
		resp, err := client.SyncCollection(ctx, calPath, &SyncQuery{
			SyncToken:   token,
			Limit:       2,
			CompRequest: CalendarCompRequest{Name: ical.CompCalendar, AllProps: true, AllComps: true},
		})
		if err != nil {
			t.Fatal(err)
		}
		updated = append(updated, resp.Updated...)
		token = resp.SyncToken
		if !resp.MoreResults {
			break
		}
	}
	if len(updated) != len(paths) {
		t.Fatalf("initial paged sync delivered %d objects, want %d", len(updated), len(paths))
	}

	// Delete one and incrementally synchronize.
	deletedPath := calPath + "2.ics"
	if err := b.DeleteCalendarObject(ctx, calPath+"2.ics"); err != nil {
		t.Fatal(err)
	}
	resp, err := client.SyncCollection(ctx, calPath, &SyncQuery{SyncToken: token})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Deleted) != 1 || resp.Deleted[0] != deletedPath {
		t.Fatalf("expected single deletion of %q, got %v", deletedPath, resp.Deleted)
	}
	if resp.MoreResults {
		t.Fatal("did not expect more-results on the incremental sync")
	}
}
