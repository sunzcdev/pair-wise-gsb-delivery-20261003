package caldav

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav"
)

// atomicBackend is a CalDAV backend that evaluates request conditions inside
// a mutex-guarded commit, i.e. atomically.
type atomicBackend struct {
	testBackend

	mu     sync.Mutex
	store  map[string]storedObject
	nextID int
}

type storedObject struct {
	data *ical.Calendar
	etag string
}

func newAtomicBackend() *atomicBackend {
	return &atomicBackend{
		testBackend: testBackend{
			calendars: []Calendar{{Path: "/user/calendars/cal"}},
		},
		store: map[string]storedObject{},
	}
}

func (b *atomicBackend) GetCalendarObject(ctx context.Context, path string, req *CalendarCompRequest) (*CalendarObject, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	obj, ok := b.store[path]
	if !ok {
		return nil, webdav.NewHTTPError(http.StatusNotFound, nil)
	}
	return &CalendarObject{Path: path, Data: obj.data, ETag: obj.etag}, nil
}

func (b *atomicBackend) PutCalendarObject(ctx context.Context, path string, cal *ical.Calendar, opts *PutCalendarObjectOptions) (*CalendarObject, error) {
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
	b.store[path] = storedObject{data: cal, etag: etag}
	return &CalendarObject{Path: path, Data: cal, ETag: etag}, nil
}

func validCalendar() *ical.Calendar {
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropProductID, "-//test//EN")
	ev := ical.NewEvent()
	ev.Props.SetText(ical.PropUID, "uid-1@example.com")
	ev.Props.SetDateTime(ical.PropDateTimeStamp, time.Now())
	cal.Children = []*ical.Component{ev.Component}
	return cal
}

// Two clients PUT concurrently with the same (current) ETag: exactly one
// succeeds.
func TestCalendarObjectConcurrentIfMatchRace(t *testing.T) {
	srv := httptest.NewServer(&Handler{Backend: newAtomicBackend()})
	defer srv.Close()

	c, err := NewClient(http.DefaultClient, srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	const path = "/user/calendars/cal/event.ics"
	if _, err := c.PutCalendarObject(context.Background(), path, validCalendar(), nil); err != nil {
		t.Fatal(err)
	}
	obj, err := c.GetCalendarObject(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	etag := obj.ETag

	put := func() int {
		_, err := c.PutCalendarObject(context.Background(), path, validCalendar(), &PutCalendarObjectClientOptions{
			Conditions: webdav.Conditions{IfMatch: webdav.NewConditionalMatch(mustTag(t, etag))},
		})
		return statusFromErr(t, err)
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

// If-None-Match "*" only allows the first create; a second attempt fails and
// does not replace the stored object.
func TestCalendarObjectIfNoneMatchWildcard(t *testing.T) {
	srv := httptest.NewServer(&Handler{Backend: newAtomicBackend()})
	defer srv.Close()

	c, err := NewClient(http.DefaultClient, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	const path = "/user/calendars/cal/new.ics"

	conds := &PutCalendarObjectClientOptions{Conditions: webdav.Conditions{IfNoneMatch: webdav.NewWildcardMatch()}}
	if _, err := c.PutCalendarObject(context.Background(), path, validCalendar(), conds); err != nil {
		t.Fatalf("first create: %v", err)
	}
	first, err := c.GetCalendarObject(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.PutCalendarObject(context.Background(), path, validCalendar(), conds); err == nil {
		t.Fatal("second create with If-None-Match * should fail")
	} else if statusFromErr(t, err) != http.StatusPreconditionFailed {
		t.Fatalf("expected 412, got %v", err)
	}

	second, err := c.GetCalendarObject(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if second.ETag != first.ETag {
		t.Fatalf("failed If-None-Match replaced object: %q -> %q", first.ETag, second.ETag)
	}
}

// A malformed conditional header is rejected with 400 before the iCalendar
// body is parsed.
func TestCalendarObjectMalformedHeader(t *testing.T) {
	h := &Handler{Backend: newAtomicBackend()}

	req := httptest.NewRequest(http.MethodPut, "/user/calendars/cal/event.ics",
		strings.NewReader("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n"))
	req.Header.Set("Content-Type", ical.MIMEType)
	req.Header.Set("If-Match", "garbage")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func mustTag(t *testing.T, s string) webdav.ETag {
	t.Helper()
	tag, err := webdav.ParseETag(s)
	if err != nil {
		t.Fatal(err)
	}
	return tag
}

func statusFromErr(t *testing.T, err error) int {
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
