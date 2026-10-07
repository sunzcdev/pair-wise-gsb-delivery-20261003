package caldav

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav/internal"
)

// memSyncEntry records one mutation of a calendar object. Object is the object
// state after the mutation, or nil when the object was deleted.
type memSyncEntry struct {
	rev    uint64
	path   string
	object *CalendarObject
}

// memSyncCalendar is a calendar collection bound to one immutable snapshot
// lineage. Recreating a calendar at the same path allocates a new ID, which
// invalidates tokens issued by the previous calendar.
type memSyncCalendar struct {
	id      string
	path    string
	rev     uint64
	base    map[string]*CalendarObject // objects present when the calendar was (re)created
	entries []memSyncEntry
}

// memSyncBackend is a thread-safe in-memory Backend implementing
// SyncCollectionBackend. It keeps an append-only change log per calendar so
// that every sync page is computed against a single fixed snapshot.
type memSyncBackend struct {
	mu        sync.Mutex
	calendars map[string]*memSyncCalendar // keyed by path
	ids       map[string]*memSyncCalendar // keyed by calendar ID
	nextID    int
	rev       uint64
}

func newMemSyncBackend() *memSyncBackend {
	return &memSyncBackend{
		calendars: map[string]*memSyncCalendar{},
		ids:       map[string]*memSyncCalendar{},
	}
}

func (b *memSyncBackend) calendar(path string) (*memSyncCalendar, error) {
	cal, ok := b.calendars[path]
	if !ok {
		return nil, internal.HTTPErrorf(http.StatusNotFound, "caldav: calendar not found: %q", path)
	}
	return cal, nil
}

// committedToken is a sync token once all changes up to rev have been
// delivered to the client.
func committedToken(calID string, rev uint64) string {
	return fmt.Sprintf("token-%s@%d", calID, rev)
}

// pageToken is an intermediate token carrying the snapshot the first page was
// computed against, the starting revision of the change window (-1 for an
// initial sync) and the position of the next change. It keeps later pages
// pinned to the exact same snapshot even if the collection is mutated.
func pageToken(calID string, fromRev, snapshotRev uint64, offset int) string {
	return fmt.Sprintf("page-%s@%d@%d@%d", calID, fromRev, snapshotRev, offset)
}

// parseToken returns (calendarID, fromRev, snapshotRev, offset, ok).
// fromRev/snapshotRev/offset describe a page token; for a committed token
// fromRev and snapshotRev are the token revision and offset is -1.
func parseToken(token string) (id string, fromRev, snapshotRev uint64, offset int, ok bool) {
	parseU64 := func(s string) (uint64, bool) {
		v, err := strconv.ParseUint(s, 10, 64)
		return v, err == nil
	}
	if rest, found := strings.CutPrefix(token, "token-"); found {
		idStr, revStr, found := strings.Cut(rest, "@")
		rev, valid := parseU64(revStr)
		if !found || !valid {
			return "", 0, 0, 0, false
		}
		return idStr, rev, rev, -1, true
	}
	if rest, found := strings.CutPrefix(token, "page-"); found {
		parts := strings.Split(rest, "@")
		if len(parts) != 4 {
			return "", 0, 0, 0, false
		}
		fromRev, ok1 := parseU64(parts[1])
		snapshotRev, ok2 := parseU64(parts[2])
		off64, ok3 := strconv.ParseInt(parts[3], 10, 64)
		if !ok1 || !ok2 || ok3 != nil {
			return "", 0, 0, 0, false
		}
		return parts[0], fromRev, snapshotRev, int(off64), true
	}
	return "", 0, 0, 0, false
}

func (b *memSyncBackend) CurrentUserPrincipal(ctx context.Context) (string, error) {
	return "/user/", nil
}

func (b *memSyncBackend) CalendarHomeSetPath(ctx context.Context) (string, error) {
	return "/user/calendars/", nil
}

func (b *memSyncBackend) CreateCalendar(ctx context.Context, cal *Calendar) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	c := &memSyncCalendar{
		id:   fmt.Sprintf("cal%d", b.nextID),
		path: cal.Path,
		base: map[string]*CalendarObject{},
	}
	b.calendars[cal.Path] = c
	b.ids[c.id] = c
	return nil
}

func (b *memSyncBackend) ListCalendars(ctx context.Context) ([]Calendar, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Calendar, 0, len(b.calendars))
	for _, cal := range b.calendars {
		out = append(out, Calendar{Path: cal.path})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (b *memSyncBackend) GetCalendar(ctx context.Context, path string) (*Calendar, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cal, err := b.calendar(path)
	if err != nil {
		return nil, err
	}
	return &Calendar{Path: cal.path}, nil
}

func (b *memSyncBackend) DeleteCalendar(ctx context.Context, path string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	cal, ok := b.calendars[path]
	if !ok {
		return internal.HTTPErrorf(http.StatusNotFound, "caldav: calendar not found: %q", path)
	}
	delete(b.calendars, path)
	delete(b.ids, cal.id)
	return nil
}

func (b *memSyncBackend) appendChange(cal *memSyncCalendar, path string, object *CalendarObject) {
	b.rev++
	cal.rev = b.rev
	cal.entries = append(cal.entries, memSyncEntry{
		rev:    b.rev,
		path:   path,
		object: object,
	})
}

func parentCollectionPath(objectPath string) string {
	p := strings.TrimSuffix(objectPath, "/")
	idx := strings.LastIndex(p, "/")
	if idx < 0 {
		return "/"
	}
	return p[:idx+1]
}

func (b *memSyncBackend) PutCalendarObject(ctx context.Context, path string, data *ical.Calendar, opts *PutCalendarObjectOptions) (*CalendarObject, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	cal, err := b.calendar(parentCollectionPath(path))
	if err != nil {
		return nil, err
	}

	obj := &CalendarObject{
		Path:    path,
		Data:    data,
		ETag:    fmt.Sprintf("etag-%d", b.rev+1),
		ModTime: time.Unix(int64(b.rev+1), 0).UTC(),
	}
	b.appendChange(cal, path, obj)
	return obj, nil
}

func (b *memSyncBackend) DeleteCalendarObject(ctx context.Context, path string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	cal, err := b.calendar(parentCollectionPath(path))
	if err != nil {
		return err
	}
	b.appendChange(cal, path, nil)
	return nil
}

func (b *memSyncBackend) GetCalendarObject(ctx context.Context, path string, req *CalendarCompRequest) (*CalendarObject, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, cal := range b.calendars {
		state := b.stateAt(cal, b.rev)
		if obj, ok := state[path]; ok {
			return cloneObject(obj), nil
		}
	}
	return nil, internal.HTTPErrorf(http.StatusNotFound, "caldav: object not found: %q", path)
}

func (b *memSyncBackend) ListCalendarObjects(ctx context.Context, path string, req *CalendarCompRequest) ([]CalendarObject, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cal, err := b.calendar(path)
	if err != nil {
		return nil, err
	}
	state := b.stateAt(cal, cal.rev)
	out := make([]CalendarObject, 0, len(state))
	for _, obj := range state {
		out = append(out, *cloneObject(obj))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (b *memSyncBackend) QueryCalendarObjects(ctx context.Context, path string, query *CalendarQuery) ([]CalendarObject, error) {
	return b.ListCalendarObjects(ctx, path, &query.CompRequest)
}

// stateAt reconstructs the collection contents as of snapshotRev by folding
// the append-only change log over the base contents. Entries newer than
// snapshotRev are ignored so that pages stay pinned to one snapshot.
func (b *memSyncBackend) stateAt(cal *memSyncCalendar, snapshotRev uint64) map[string]*CalendarObject {
	state := make(map[string]*CalendarObject, len(cal.base))
	for path, obj := range cal.base {
		state[path] = obj
	}
	for _, e := range cal.entries {
		if e.rev > snapshotRev {
			continue
		}
		if e.object == nil {
			delete(state, e.path)
		} else {
			state[e.path] = e.object
		}
	}
	return state
}

type memChange struct {
	path    string
	deleted bool
}

// changesBetween lists the changes in (fromRev, toRev]. A path deleted and
// recreated within the window is reported once as an update, never as a
// deletion (RFC 6578 section 3.10).
func (b *memSyncBackend) changesBetween(cal *memSyncCalendar, fromRev, toRev uint64) []memChange {
	window := map[string]*CalendarObject{}
	var order []string
	for _, e := range cal.entries {
		if e.rev <= fromRev || e.rev > toRev {
			continue
		}
		if _, seen := window[e.path]; !seen {
			order = append(order, e.path)
		}
		window[e.path] = e.object
	}

	before := b.stateAt(cal, fromRev)
	var changes []memChange
	for _, path := range order {
		if window[path] == nil {
			if _, existed := before[path]; existed {
				changes = append(changes, memChange{path: path, deleted: true})
			}
		} else {
			changes = append(changes, memChange{path: path})
		}
	}
	sortChanges(changes)
	return changes
}

func sortChanges(changes []memChange) {
	sort.SliceStable(changes, func(i, j int) bool {
		if changes[i].deleted != changes[j].deleted {
			return !changes[i].deleted
		}
		return changes[i].path < changes[j].path
	})
}

func (b *memSyncBackend) SyncCollection(ctx context.Context, path string, query *SyncQuery) (*SyncResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	cal, err := b.calendar(path)
	if err != nil {
		return nil, err
	}

	var changes []memChange
	var offset int
	var fromRev, snapshotRev uint64
	initial := false
	if query.SyncToken == "" {
		// Initial sync: bind to the current snapshot.
		fromRev = ^uint64(0)
		snapshotRev = cal.rev
		initial = true
	} else {
		id, tokenFromRev, tokenSnapshotRev, tokenOffset, ok := parseToken(query.SyncToken)
		if !ok {
			return nil, InvalidSyncTokenError(fmt.Errorf("caldav: malformed sync token %q", query.SyncToken))
		}
		tokenCal, known := b.ids[id]
		if !known || tokenCal != cal || tokenSnapshotRev > cal.rev {
			return nil, InvalidSyncTokenError(fmt.Errorf("caldav: sync token %q is not valid for this collection", query.SyncToken))
		}
		fromRev = tokenFromRev
		if tokenOffset >= 0 {
			// Continuing a paginated sync: stay pinned to the first page's
			// snapshot, even if newer revisions exist.
			snapshotRev = tokenSnapshotRev
			offset = tokenOffset
		} else {
			// A committed token: synchronize up to the current revision.
			snapshotRev = cal.rev
		}
		initial = tokenFromRev == ^uint64(0)
	}

	// Initial sync (first page or continuation) reports the whole snapshot.
	if initial {
		state := b.stateAt(cal, snapshotRev)
		for p := range state {
			changes = append(changes, memChange{path: p})
		}
		sortChanges(changes)
	} else {
		changes = b.changesBetween(cal, fromRev, snapshotRev)
	}

	if offset > len(changes) {
		return nil, InvalidSyncTokenError(fmt.Errorf("caldav: sync token %q points past the snapshot", query.SyncToken))
	}

	resp := &SyncResponse{}
	end := len(changes)
	if query.Limit > 0 && offset+query.Limit < end {
		end = offset + query.Limit
	}

	state := b.stateAt(cal, snapshotRev)
	for _, c := range changes[offset:end] {
		if c.deleted {
			resp.Deleted = append(resp.Deleted, c.path)
			continue
		}
		obj, ok := state[c.path]
		if !ok {
			return nil, internal.HTTPErrorf(http.StatusInternalServerError, "caldav: inconsistent snapshot for %q", c.path)
		}
		resp.Updated = append(resp.Updated, *cloneObject(obj))
	}

	if end < len(changes) {
		// Intermediate page: pin the client to the same snapshot and window.
		resp.SyncToken = pageToken(cal.id, fromRev, snapshotRev, end)
		resp.MoreResults = true
		return resp, nil
	}

	// The final token is committed only once the whole snapshot is delivered.
	resp.SyncToken = committedToken(cal.id, snapshotRev)
	return resp, nil
}

func (b *memSyncBackend) CurrentSyncToken(ctx context.Context, path string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cal, err := b.calendar(path)
	if err != nil {
		return "", err
	}
	return committedToken(cal.id, cal.rev), nil
}

func cloneObject(obj *CalendarObject) *CalendarObject {
	if obj == nil {
		return nil
	}
	cp := *obj
	return &cp
}
