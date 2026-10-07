package webdav

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/emersion/go-webdav/internal"
)

func newSyncTestServer(fs FileSystem) *httptest.Server {
	return httptest.NewServer(&Handler{FileSystem: fs})
}

func syncReport(t *testing.T, ts *httptest.Server, collection, token string, limit int) *internal.MultiStatus {
	t.Helper()

	var limitXML string
	if limit > 0 {
		limitXML = fmt.Sprintf("<d:limit><d:nresults>%d</d:nresults></d:limit>", limit)
	}

	body := `<?xml version="1.0" encoding="utf-8"?>
<d:sync-collection xmlns:d="DAV:">
  <d:sync-token>` + token + `</d:sync-token>
  ` + limitXML + `
  <d:sync-level>1</d:sync-level>
  <d:prop>
    <d:getetag/>
  </d:prop>
</d:sync-collection>`

	req, err := http.NewRequest("REPORT", ts.URL+collection, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/xml")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMultiStatus {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("REPORT sync-collection: expected 207, got %v: %s", resp.Status, data)
	}

	var ms internal.MultiStatus
	if err := xml.NewDecoder(resp.Body).Decode(&ms); err != nil {
		t.Fatal(err)
	}
	return &ms
}

func rawSyncReport(t *testing.T, ts *httptest.Server, collection, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("REPORT", ts.URL+collection, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/xml")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func syncReportBody(token string) string {
	return `<?xml version="1.0"?>
<d:sync-collection xmlns:d="DAV:">
  <d:sync-token>` + token + `</d:sync-token>
  <d:sync-level>1</d:sync-level>
  <d:prop><d:getetag/></d:prop>
</d:sync-collection>`
}

// statusOf returns the HTTP status embedded in a sync response item, or 200
// when the item only contains propstats (i.e. it exists).
func statusOf(resp *internal.Response) int {
	if resp.Status != nil {
		return resp.Status.Code
	}
	return http.StatusOK
}

func hrefStatuses(ms *internal.MultiStatus) map[string]int {
	m := make(map[string]int)
	for _, r := range ms.Responses {
		for _, h := range r.Hrefs {
			m[h.Path] = statusOf(&r)
		}
	}
	return m
}

func setupSyncedCollection(t *testing.T) (*memorySyncFileSystem, *httptest.Server) {
	t.Helper()
	fs := newMemorySyncFileSystem()
	fs.mkdir("/cal")
	fs.put("/cal/a.ics", "a")
	fs.put("/cal/b.ics", "b")
	return fs, newSyncTestServer(fs)
}

// Initial sync with an empty token enumerates the full collection and returns
// the current token.
func TestSyncCollectionInitial(t *testing.T) {
	fs, ts := setupSyncedCollection(t)
	defer ts.Close()

	ms := syncReport(t, ts, "/cal", "", 0)
	got := hrefStatuses(ms)
	if len(got) != 2 || got["/cal/a.ics"] != http.StatusOK || got["/cal/b.ics"] != http.StatusOK {
		t.Fatalf("initial sync: unexpected members: %v", got)
	}
	wantToken, err := fs.CurrentSyncToken(context.Background(), "/cal")
	if err != nil {
		t.Fatal(err)
	}
	if ms.SyncToken != wantToken {
		t.Fatalf("initial sync: token = %q, want %q", ms.SyncToken, wantToken)
	}
}

// A resource deleted and then re-created before the next sync is reported as
// present (not as a 404 deletion), and a pure deletion is reported with an
// explicit 404 status.
func TestSyncCollectionDeleteRecreate(t *testing.T) {
	fs, ts := setupSyncedCollection(t)
	defer ts.Close()

	initial := syncReport(t, ts, "/cal", "", 0)

	fs.remove("/cal/a.ics")
	fs.put("/cal/a.ics", "a-new") // recreated before the next sync
	fs.remove("/cal/b.ics")

	inc := syncReport(t, ts, "/cal", initial.SyncToken, 0)
	got := hrefStatuses(inc)

	if got["/cal/a.ics"] != http.StatusOK {
		t.Errorf("deleted-then-recreated member: expected 200, got %v", got["/cal/a.ics"])
	}
	if got["/cal/b.ics"] != http.StatusNotFound {
		t.Errorf("deleted member: expected 404, got %v", got["/cal/b.ics"])
	}

	wantToken, err := fs.CurrentSyncToken(context.Background(), "/cal")
	if err != nil {
		t.Fatal(err)
	}
	if inc.SyncToken != wantToken {
		t.Errorf("incremental token = %q, want %q", inc.SyncToken, wantToken)
	}
}

// A token minted for another collection must not be accepted: RFC 6578
// requires a 403 with the DAV:valid-sync-token precondition.
func TestSyncCollectionForeignToken(t *testing.T) {
	fs, ts := setupSyncedCollection(t)
	defer ts.Close()

	fs.mkdir("/other")
	fs.put("/other/x.ics", "x")

	other := syncReport(t, ts, "/other", "", 0)

	resp := rawSyncReport(t, ts, "/cal", syncReportBody(other.SyncToken))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign token: expected 403, got %v", resp.Status)
	}
	data, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(data), "valid-sync-token") {
		t.Fatalf("foreign token: missing valid-sync-token precondition in %s", data)
	}
}

// A malformed or expired token yields the same 403 precondition.
func TestSyncCollectionInvalidToken(t *testing.T) {
	_, ts := setupSyncedCollection(t)
	defer ts.Close()

	for _, token := range []string{"garbage", "/cal#9999"} {
		resp := rawSyncReport(t, ts, "/cal", syncReportBody(token))
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "valid-sync-token") {
			t.Fatalf("token %q: expected 403 valid-sync-token, got %v: %s", token, resp.Status, body)
		}
	}
}

// All pages of a limited sync are bound to the same snapshot. Changes made
// while paginating neither leak in nor get lost, and the committed token is
// only returned on the final page.
func TestSyncCollectionPaginationSnapshot(t *testing.T) {
	fs, ts := setupSyncedCollection(t)
	defer ts.Close()

	// Two members at snapshot revision 2; one result per page.
	page1 := syncReport(t, ts, "/cal", "", 1)
	if len(page1.Responses) != 1 {
		t.Fatalf("page 1: expected 1 response, got %d", len(page1.Responses))
	}
	if !strings.Contains(page1.SyncToken, "@2:") {
		t.Fatalf("page 1: expected continuation token pinned at revision 2, got %q", page1.SyncToken)
	}

	// Mutate the collection while the client is still paging. These revision
	// 3 changes must not appear in the in-flight snapshot.
	fs.remove("/cal/a.ics")
	fs.put("/cal/c.ics", "c")

	page2 := syncReport(t, ts, "/cal", page1.SyncToken, 1)

	var all []internal.Response
	all = append(all, page1.Responses...)
	all = append(all, page2.Responses...)

	got := map[string]int{}
	for _, r := range all {
		for _, h := range r.Hrefs {
			got[h.Path] = statusOf(&r)
		}
	}
	if len(got) != 2 || got["/cal/a.ics"] != http.StatusOK || got["/cal/b.ics"] != http.StatusOK {
		t.Fatalf("paged sync: expected snapshot {a,b}, got %v", got)
	}

	// The final page commits revision 2, not the current revision 3.
	if page2.SyncToken != committedToken("/cal", 2) {
		t.Fatalf("page 2: expected committed snapshot token %q, got %q", committedToken("/cal", 2), page2.SyncToken)
	}

	// Continuing from the committed revision-2 token surfaces revision 3: a
	// 404 deletion of a.ics and an addition of c.ics.
	inc := syncReport(t, ts, "/cal", page2.SyncToken, 0)
	next := hrefStatuses(inc)
	if next["/cal/a.ics"] != http.StatusNotFound {
		t.Errorf("post-pagination: a.ics expected 404, got %v", next["/cal/a.ics"])
	}
	if next["/cal/c.ics"] != http.StatusOK {
		t.Errorf("post-pagination: c.ics expected 200, got %v", next["/cal/c.ics"])
	}
}

// The continuation token differs from the committed snapshot token, and only
// the last page returns the collection's current (committed) token.
func TestSyncCollectionContinuationFinalizes(t *testing.T) {
	fs, ts := setupSyncedCollection(t)
	defer ts.Close()

	page1 := syncReport(t, ts, "/cal", "", 1)
	page2 := syncReport(t, ts, "/cal", page1.SyncToken, 1)

	if !strings.HasPrefix(page1.SyncToken, "/cal@") {
		t.Fatalf("page 1 token must be a continuation token, got %q", page1.SyncToken)
	}
	if page1.SyncToken == page2.SyncToken {
		t.Fatalf("continuation token must differ from the committed token")
	}
	final, err := fs.CurrentSyncToken(context.Background(), "/cal")
	if err != nil {
		t.Fatal(err)
	}
	if page2.SyncToken != final {
		t.Fatalf("last page token = %q, want current token %q", page2.SyncToken, final)
	}
}

// A FileSystem without the sync extension keeps its original capability
// declaration: no sync-token class and REPORT stays disallowed.
func TestSyncCollectionCapabilityNotAdvertised(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(dir+"/cal", 0o755); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(&Handler{FileSystem: LocalFileSystem(dir)})
	defer ts.Close()

	req, err := http.NewRequest(http.MethodOptions, ts.URL+"/cal", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if strings.Contains(resp.Header.Get("Dav"), "sync-token") {
		t.Errorf("plain backend unexpectedly advertised sync-token: %q", resp.Header.Get("Dav"))
	}
	if strings.Contains(resp.Header.Get("Allow"), "REPORT") {
		t.Errorf("plain backend unexpectedly allowed REPORT: %q", resp.Header.Get("Allow"))
	}

	r2 := rawSyncReport(t, ts, "/cal", syncReportBody(""))
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("plain backend REPORT: expected 405, got %v", r2.Status)
	}
}

// A FileSystem with the sync extension advertises sync-token, REPORT and the
// current-sync-token live property.
func TestSyncCollectionCapabilityAdvertised(t *testing.T) {
	_, ts := setupSyncedCollection(t)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodOptions, ts.URL+"/cal", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if !strings.Contains(resp.Header.Get("Dav"), "sync-token") {
		t.Errorf("sync backend missing sync-token DAV class: %q", resp.Header.Get("Dav"))
	}
	if !strings.Contains(resp.Header.Get("Allow"), "REPORT") {
		t.Errorf("sync backend missing REPORT in Allow: %q", resp.Header.Get("Allow"))
	}

	// current-sync-token PROPFIND
	pf := `<?xml version="1.0"?>
<d:propfind xmlns:d="DAV:"><d:prop><d:current-sync-token/></d:prop></d:propfind>`
	pfreq, err := http.NewRequest("PROPFIND", ts.URL+"/cal", strings.NewReader(pf))
	if err != nil {
		t.Fatal(err)
	}
	pfreq.Header.Set("Depth", "0")
	pfreq.Header.Set("Content-Type", "application/xml")
	pfresp, err := http.DefaultClient.Do(pfreq)
	if err != nil {
		t.Fatal(err)
	}
	defer pfresp.Body.Close()
	data, _ := io.ReadAll(pfresp.Body)
	if !strings.Contains(string(data), ">/cal#2</d:sync-token>") && !strings.Contains(string(data), ">/cal#2</sync-token>") {
		t.Fatalf("PROPFIND current-sync-token: missing token element: %s", data)
	}
}
