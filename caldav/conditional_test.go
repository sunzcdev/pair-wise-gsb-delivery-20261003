package caldav

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav"
)

// putCapturingBackend records the options handed to PutCalendarObject.
type putCapturingBackend struct {
	testBackend
	got *PutCalendarObjectOptions
}

func (b *putCapturingBackend) PutCalendarObject(ctx context.Context, path string, cal *ical.Calendar, opts *PutCalendarObjectOptions) (*CalendarObject, error) {
	b.got = opts
	return &CalendarObject{Path: path, ETag: `"new"`}, nil
}

func newCalObject() *ical.Calendar {
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropProductID, "-//test//EN")
	event := ical.NewEvent()
	event.Name = ical.CompEvent
	event.Props.SetText(ical.PropUID, "uid@example.com")
	event.Props.SetDateTime(ical.PropDateTimeStamp, time.Unix(0, 0).UTC())
	cal.Children = []*ical.Component{event.Component}
	return cal
}

// The client forwards conditional headers verbatim.
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

	opts := &PutCalendarObjectOptions{}
	opts.IfMatch, err = webdav.ParseConditionalMatch(`"v1", W/"other"`)
	if err != nil {
		t.Fatal(err)
	}
	opts.IfNoneMatch, err = webdav.ParseConditionalMatch("*")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.PutCalendarObject(context.Background(), "/cal/e.ics", newCalObject(), opts); err != nil {
		t.Fatal(err)
	}

	if got := seen.Get("If-Match"); got != `"v1", W/"other"` {
		t.Errorf("If-Match = %q", got)
	}
	if got := seen.Get("If-None-Match"); got != "*" {
		t.Errorf("If-None-Match = %q", got)
	}
}

// Malformed conditional headers are rejected with 400 before the iCalendar
// body is parsed or the backend is invoked.
func TestServerPutMalformedHeaderRejectsEarly(t *testing.T) {
	b := &putCapturingBackend{}
	h := &Handler{Backend: b}
	body := "BEGIN:VCALENDAR\r\nthis is intentionally not valid icalendar\r\n"
	req := httptest.NewRequest(http.MethodPut, "/cal/e.ics", strings.NewReader(body))
	req.Header.Set("Content-Type", "text/calendar")
	req.Header.Set("If-Match", `"oops`)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if b.got != nil {
		t.Fatalf("backend must not be invoked when the header fails to parse")
	}
}
