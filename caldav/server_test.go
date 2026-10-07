package caldav

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav"
)

var propFindSupportedCalendarComponentRequest = `
<d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">
  <d:prop>
     <c:supported-calendar-component-set />
  </d:prop>
</d:propfind>
`

var testPropFindSupportedCalendarComponentCases = map[*Calendar][]string{
	&Calendar{Path: "/user/calendars/cal"}:                                                     []string{"VEVENT"},
	&Calendar{Path: "/user/calendars/cal", SupportedComponentSet: []string{"VTODO"}}:           []string{"VTODO"},
	&Calendar{Path: "/user/calendars/cal", SupportedComponentSet: []string{"VEVENT", "VTODO"}}: []string{"VEVENT", "VTODO"},
}

func TestPropFindSupportedCalendarComponent(t *testing.T) {
	for calendar, expected := range testPropFindSupportedCalendarComponentCases {
		req := httptest.NewRequest("PROPFIND", calendar.Path, nil)
		req.Body = io.NopCloser(strings.NewReader(propFindSupportedCalendarComponentRequest))
		req.Header.Set("Content-Type", "application/xml")
		w := httptest.NewRecorder()
		handler := Handler{Backend: testBackend{calendars: []Calendar{*calendar}}}
		handler.ServeHTTP(w, req)

		res := w.Result()
		defer res.Body.Close()
		data, err := ioutil.ReadAll(res.Body)
		if err != nil {
			t.Error(err)
		}
		resp := string(data)
		for _, comp := range expected {
			// Would be nicer to do a proper XML-decoding here, but this is probably good enough for now.
			if !strings.Contains(resp, comp) {
				t.Errorf("Expected component: %v not found in response:\n%v", comp, resp)
			}
		}
	}
}

var propFindUserPrincipal = `
<?xml version="1.0" encoding="UTF-8"?>
<A:propfind xmlns:A="DAV:">
  <A:prop>
    <A:current-user-principal/>
    <A:principal-URL/>
    <A:resourcetype/>
  </A:prop>
</A:propfind>
`

func TestPropFindRoot(t *testing.T) {
	req := httptest.NewRequest("PROPFIND", "/", strings.NewReader(propFindUserPrincipal))
	req.Header.Set("Content-Type", "application/xml")
	w := httptest.NewRecorder()
	calendar := &Calendar{}
	handler := Handler{Backend: testBackend{calendars: []Calendar{*calendar}}}
	handler.ServeHTTP(w, req)

	res := w.Result()
	defer res.Body.Close()
	data, err := ioutil.ReadAll(res.Body)
	if err != nil {
		t.Error(err)
	}
	resp := string(data)
	if !strings.Contains(resp, `<current-user-principal xmlns="DAV:"><href>/user/</href></current-user-principal>`) {
		t.Errorf("No user-principal returned when doing a PROPFIND against root, response:\n%s", resp)
	}
}

var reportCalendarData = `
<?xml version="1.0" encoding="UTF-8"?>
<B:calendar-multiget xmlns:A="DAV:" xmlns:B="urn:ietf:params:xml:ns:caldav">
  <A:prop>
    <B:calendar-data/>
  </A:prop>
  <A:href>%s</A:href>
</B:calendar-multiget>
`

func TestMultiCalendarBackend(t *testing.T) {
	calendarB := Calendar{Path: "/user/calendars/b", SupportedComponentSet: []string{"VTODO"}}
	calendars := []Calendar{
		Calendar{Path: "/user/calendars/a"},
		calendarB,
	}
	eventSummary := "This is a todo"
	event := ical.NewEvent()
	event.Name = ical.CompToDo
	event.Props.SetText(ical.PropUID, "46bbf47a-1861-41a3-ae06-8d8268c6d41e")
	event.Props.SetDateTime(ical.PropDateTimeStamp, time.Now())
	event.Props.SetText(ical.PropSummary, eventSummary)
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropProductID, "-//xyz Corp//NONSGML PDA Calendar Version 1.0//EN")
	cal.Children = []*ical.Component{
		event.Component,
	}
	object := CalendarObject{
		Path: "/user/calendars/b/test.ics",
		Data: cal,
	}
	req := httptest.NewRequest("PROPFIND", "/user/calendars/", strings.NewReader(propFindUserPrincipal))
	req.Header.Set("Content-Type", "application/xml")
	w := httptest.NewRecorder()
	handler := Handler{Backend: testBackend{
		calendars: calendars,
		objectMap: map[string][]CalendarObject{
			calendarB.Path: []CalendarObject{object},
		},
	}}
	handler.ServeHTTP(w, req)

	res := w.Result()
	defer res.Body.Close()
	data, err := ioutil.ReadAll(res.Body)
	if err != nil {
		t.Error(err)
	}
	resp := string(data)
	for _, calendar := range calendars {
		if !strings.Contains(resp, fmt.Sprintf(`<response xmlns="DAV:"><href>%s</href>`, calendar.Path)) {
			t.Errorf("Calendar: %v not returned in PROPFIND, response:\n%s", calendar, resp)
		}
	}

	// Now do a PROPFIND for the last calendar
	req = httptest.NewRequest("PROPFIND", calendarB.Path, strings.NewReader(propFindSupportedCalendarComponentRequest))
	req.Header.Set("Content-Type", "application/xml")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	res = w.Result()
	defer res.Body.Close()
	data, err = ioutil.ReadAll(res.Body)
	if err != nil {
		t.Error(err)
	}
	resp = string(data)
	if !strings.Contains(resp, "VTODO") {
		t.Errorf("Expected component: VTODO not found in response:\n%v", resp)
	}
	if !strings.Contains(resp, object.Path) {
		t.Errorf("Expected calendar object: %v not found in response:\n%v", object, resp)
	}

	// Now do a REPORT to get the actual data for the event
	req = httptest.NewRequest("REPORT", calendarB.Path, strings.NewReader(fmt.Sprintf(reportCalendarData, object.Path)))
	req.Header.Set("Content-Type", "application/xml")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	res = w.Result()
	defer res.Body.Close()
	data, err = ioutil.ReadAll(res.Body)
	if err != nil {
		t.Error(err)
	}
	resp = string(data)
	if !strings.Contains(resp, fmt.Sprintf("SUMMARY:%s", eventSummary)) {
		t.Errorf("ICAL content not properly returned in response:\n%v", resp)
	}
}

type testBackend struct {
	calendars []Calendar
	objectMap map[string][]CalendarObject
}

func (t testBackend) CreateCalendar(ctx context.Context, calendar *Calendar) error {
	return nil
}

func (t testBackend) ListCalendars(ctx context.Context) ([]Calendar, error) {
	return t.calendars, nil
}

func (t testBackend) GetCalendar(ctx context.Context, path string) (*Calendar, error) {
	for _, cal := range t.calendars {
		if cal.Path == path {
			return &cal, nil
		}
	}
	return nil, fmt.Errorf("Calendar for path: %s not found", path)
}

func (t testBackend) CalendarHomeSetPath(ctx context.Context) (string, error) {
	return "/user/calendars/", nil
}

func (t testBackend) CurrentUserPrincipal(ctx context.Context) (string, error) {
	return "/user/", nil
}

func (t testBackend) DeleteCalendarObject(ctx context.Context, path string) error {
	return nil
}

func (t testBackend) GetCalendarObject(ctx context.Context, path string, req *CalendarCompRequest) (*CalendarObject, error) {
	for _, objs := range t.objectMap {
		for _, obj := range objs {
			if obj.Path == path {
				return &obj, nil
			}
		}
	}
	return nil, fmt.Errorf("Couldn't find calendar object at: %s", path)
}

func (t testBackend) PutCalendarObject(ctx context.Context, path string, calendar *ical.Calendar, opts *PutCalendarObjectOptions) (*CalendarObject, error) {
	return nil, nil
}

func (t testBackend) ListCalendarObjects(ctx context.Context, path string, req *CalendarCompRequest) ([]CalendarObject, error) {
	return t.objectMap[path], nil
}

func (t testBackend) QueryCalendarObjects(ctx context.Context, path string, query *CalendarQuery) ([]CalendarObject, error) {
	return nil, nil
}

func calSyncObject(name string) CalendarObject {
	event := ical.NewEvent()
	event.Name = ical.CompEvent
	event.Props.SetText(ical.PropUID, name)
	event.Props.SetDateTime(ical.PropDateTimeStamp, time.Unix(0, 0).UTC())
	event.Props.SetText(ical.PropSummary, "Event "+name)
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropProductID, "-//test//EN")
	cal.Children = []*ical.Component{event.Component}
	return CalendarObject{Path: calPath + "/" + name, Data: cal}
}

func TestSyncCalendarObjects(t *testing.T) {
	backend := newSyncTestBackend()
	backend.put(calSyncObject("a.ics"))
	backend.put(calSyncObject("b.ics"))

	ts := httptest.NewServer(&Handler{Backend: backend})
	defer ts.Close()

	client, err := NewClient(nil, ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Initial sync with an empty token.
	initial, err := client.SyncCollection(ctx, calPath, &SyncQuery{})
	if err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	if len(initial.Updated) != 2 {
		t.Fatalf("initial sync: expected 2 updated, got %d", len(initial.Updated))
	}
	if len(initial.Deleted) != 0 {
		t.Fatalf("initial sync: expected 0 deleted, got %d", len(initial.Deleted))
	}
	if initial.SyncToken != backend.makeToken(2) {
		t.Fatalf("initial sync: token = %q, want %q", initial.SyncToken, backend.makeToken(2))
	}

	// Delete a.ics, recreate it, delete b.ics.
	backend.del(calPath + "/a.ics")
	backend.put(calSyncObject("a.ics"))
	backend.del(calPath + "/b.ics")

	inc, err := client.SyncCollection(ctx, calPath, &SyncQuery{SyncToken: initial.SyncToken})
	if err != nil {
		t.Fatalf("incremental sync: %v", err)
	}

	updated := map[string]bool{}
	for _, o := range inc.Updated {
		updated[o.Path] = true
	}
	if !updated[calPath+"/a.ics"] {
		t.Errorf("incremental sync: recreated a.ics missing from updated: %v", updated)
	}
	deleted := map[string]bool{}
	for _, p := range inc.Deleted {
		deleted[p] = true
	}
	if !deleted[calPath+"/b.ics"] {
		t.Errorf("incremental sync: b.ics missing from deleted: %v", inc.Deleted)
	}

	// A token minted for calPath used against another existing collection
	// triggers the valid-sync-token precondition (403).
	_, err = client.SyncCollection(ctx, calPath2, &SyncQuery{SyncToken: initial.SyncToken})
	if err == nil {
		t.Fatal("expected error for foreign token, got nil")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("foreign token: expected 403, got %v", err)
	}

	// A garbage token triggers the same 403.
	_, err = client.SyncCollection(ctx, calPath, &SyncQuery{SyncToken: "nonsense"})
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("invalid token: expected 403, got %v", err)
	}
}

func TestSyncCalendarOptions(t *testing.T) {
	backend := newSyncTestBackend()
	ts := httptest.NewServer(&Handler{Backend: backend})
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodOptions, ts.URL+calPath, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if !strings.Contains(resp.Header.Get("Dav"), "sync-token") {
		t.Errorf("calendar missing sync-token DAV class: %q", resp.Header.Get("Dav"))
	}

	// A backend without the sync extension keeps the original declaration.
	plain := httptest.NewServer(&Handler{Backend: testBackend{}})
	defer plain.Close()

	req2, _ := http.NewRequest(http.MethodOptions, plain.URL+calPath, nil)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if strings.Contains(resp2.Header.Get("Dav"), "sync-token") {
		t.Errorf("plain backend unexpectedly advertised sync-token: %q", resp2.Header.Get("Dav"))
	}
}

// syncTestBackend is a testBackend extended with RFC 6578 sync support.
type syncTestBackend struct {
	testBackend
	mu      sync.Mutex
	path    string
	token   int
	events  []syncTestEvent
	objects map[string]CalendarObject
}

type syncTestEvent struct {
	revision int
	path     string
	deleted  bool
	object   *CalendarObject
}

const calPath = "/user/calendars/sync"

func newSyncTestBackend() *syncTestBackend {
	return &syncTestBackend{
		testBackend: testBackend{calendars: []Calendar{{Path: calPath}, {Path: calPath2}}},
		path:        calPath,
		objects:     make(map[string]CalendarObject),
	}
}

const calPath2 = "/user/calendars/sync-other"

func (b *syncTestBackend) makeToken(revision int) string {
	return fmt.Sprintf("%s#%d", b.path, revision)
}

func parseCalToken(collection, token string) (int, bool) {
	i := strings.IndexByte(token, '#')
	if i < 0 || token[:i] != collection {
		return 0, false
	}
	rev, err := strconv.Atoi(token[i+1:])
	if err != nil {
		return 0, false
	}
	return rev, true
}

func (b *syncTestBackend) put(obj CalendarObject) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.token++
	obj.ModTime = time.Unix(int64(b.token), 0).UTC()
	obj.ETag = fmt.Sprintf("etag-%d", b.token)
	b.objects[obj.Path] = obj
	b.events = append(b.events, syncTestEvent{revision: b.token, path: obj.Path, object: &obj})
}

func (b *syncTestBackend) del(path string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.token++
	delete(b.objects, path)
	b.events = append(b.events, syncTestEvent{revision: b.token, path: path, deleted: true})
}

func (b *syncTestBackend) CurrentCalendarSyncToken(ctx context.Context, path string) (string, error) {
	if path != b.path && path != calPath2 {
		return "", webdav.NewHTTPError(http.StatusNotFound, fmt.Errorf("not found"))
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return fmt.Sprintf("%s#%d", path, b.token), nil
}

func (b *syncTestBackend) GetCalendarObject(ctx context.Context, path string, req *CalendarCompRequest) (*CalendarObject, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if obj, ok := b.objects[path]; ok {
		return &obj, nil
	}
	return nil, webdav.NewHTTPError(http.StatusNotFound, fmt.Errorf("not found"))
}

func (b *syncTestBackend) SyncCalendarObjects(ctx context.Context, path string, req *SyncQuery) (*SyncResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if path != b.path && path != calPath2 {
		return nil, webdav.NewHTTPError(http.StatusNotFound, fmt.Errorf("not found"))
	}

	result := &SyncResponse{SyncToken: fmt.Sprintf("%s#%d", path, b.token)}

	if req.SyncToken == "" {
		paths := make([]string, 0, len(b.objects))
		for p := range b.objects {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			result.Updated = append(result.Updated, b.objects[p])
		}
		return result, nil
	}

	baseline, ok := parseCalToken(path, req.SyncToken)
	if !ok || baseline < 0 || baseline > b.token {
		return nil, webdav.NewValidSyncTokenError()
	}

	// Collapse events in (baseline, token] to each member's final state.
	final := map[string]syncTestEvent{}
	var order []string
	for _, ev := range b.events {
		if ev.revision <= baseline {
			continue
		}
		if _, seen := final[ev.path]; !seen {
			order = append(order, ev.path)
		}
		final[ev.path] = ev
	}
	for _, p := range order {
		ev := final[p]
		if ev.deleted {
			result.Deleted = append(result.Deleted, p)
		} else {
			result.Updated = append(result.Updated, *ev.object)
		}
	}
	return result, nil
}
