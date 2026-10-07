package carddav

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-vcard"
	"github.com/emersion/go-webdav/internal"
)

type memSyncEntry struct {
	rev    uint64
	path   string
	object *AddressObject
}

type memSyncAddressBook struct {
	id      string
	path    string
	rev     uint64
	base    map[string]*AddressObject
	entries []memSyncEntry
}

type memSyncBackend struct {
	mu        sync.Mutex
	books     map[string]*memSyncAddressBook
	ids       map[string]*memSyncAddressBook
	nextID    int
	rev       uint64
	principal string
	homeSet   string
}

func newMemSyncBackend() *memSyncBackend {
	return &memSyncBackend{
		books:     map[string]*memSyncAddressBook{},
		ids:       map[string]*memSyncAddressBook{},
		principal: "/user/",
		homeSet:   "/user/contacts/",
	}
}

func (b *memSyncBackend) addressBook(path string) (*memSyncAddressBook, error) {
	ab, ok := b.books[path]
	if !ok {
		return nil, internal.HTTPErrorf(http.StatusNotFound, "carddav: address book not found: %q", path)
	}
	return ab, nil
}

func committedToken(id string, rev uint64) string {
	return fmt.Sprintf("token-%s@%d", id, rev)
}

func pageToken(id string, fromRev, snapshotRev uint64, offset int) string {
	return fmt.Sprintf("page-%s@%d@%d@%d", id, fromRev, snapshotRev, offset)
}

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
		off, err := strconv.ParseInt(parts[3], 10, 64)
		if !ok1 || !ok2 || err != nil {
			return "", 0, 0, 0, false
		}
		return parts[0], fromRev, snapshotRev, int(off), true
	}
	return "", 0, 0, 0, false
}

func (b *memSyncBackend) CurrentUserPrincipal(ctx context.Context) (string, error) {
	return b.principal, nil
}

func (b *memSyncBackend) AddressBookHomeSetPath(ctx context.Context) (string, error) {
	return b.homeSet, nil
}

func (b *memSyncBackend) CreateAddressBook(ctx context.Context, ab *AddressBook) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	c := &memSyncAddressBook{
		id:   fmt.Sprintf("ab%d", b.nextID),
		path: ab.Path,
		base: map[string]*AddressObject{},
	}
	b.books[ab.Path] = c
	b.ids[c.id] = c
	return nil
}

func (b *memSyncBackend) ListAddressBooks(ctx context.Context) ([]AddressBook, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]AddressBook, 0, len(b.books))
	for _, ab := range b.books {
		out = append(out, AddressBook{Path: ab.path})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (b *memSyncBackend) GetAddressBook(ctx context.Context, path string) (*AddressBook, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ab, err := b.addressBook(path)
	if err != nil {
		return nil, err
	}
	return &AddressBook{Path: ab.path}, nil
}

func (b *memSyncBackend) DeleteAddressBook(ctx context.Context, path string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	ab, ok := b.books[path]
	if !ok {
		return internal.HTTPErrorf(http.StatusNotFound, "carddav: address book not found: %q", path)
	}
	delete(b.books, path)
	delete(b.ids, ab.id)
	return nil
}

func (b *memSyncBackend) appendChange(ab *memSyncAddressBook, path string, obj *AddressObject) {
	b.rev++
	ab.rev = b.rev
	ab.entries = append(ab.entries, memSyncEntry{rev: b.rev, path: path, object: obj})
}

func parentPath(p string) string {
	p = strings.TrimSuffix(p, "/")
	idx := strings.LastIndex(p, "/")
	if idx < 0 {
		return "/"
	}
	return p[:idx+1]
}

func (b *memSyncBackend) PutAddressObject(ctx context.Context, path string, card vcard.Card, opts *PutAddressObjectOptions) (*AddressObject, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ab, err := b.addressBook(parentPath(path))
	if err != nil {
		return nil, err
	}
	obj := &AddressObject{
		Path:    path,
		Card:    card,
		ETag:    fmt.Sprintf("etag-%d", b.rev+1),
		ModTime: time.Unix(int64(b.rev+1), 0).UTC(),
	}
	b.appendChange(ab, path, obj)
	return obj, nil
}

func (b *memSyncBackend) DeleteAddressObject(ctx context.Context, path string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	ab, err := b.addressBook(parentPath(path))
	if err != nil {
		return err
	}
	b.appendChange(ab, path, nil)
	return nil
}

func (b *memSyncBackend) GetAddressObject(ctx context.Context, path string, req *AddressDataRequest) (*AddressObject, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ab := range b.books {
		if obj, ok := b.stateAt(ab, b.rev)[path]; ok {
			cp := *obj
			return &cp, nil
		}
	}
	return nil, internal.HTTPErrorf(http.StatusNotFound, "carddav: object not found: %q", path)
}

func (b *memSyncBackend) ListAddressObjects(ctx context.Context, path string, req *AddressDataRequest) ([]AddressObject, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ab, err := b.addressBook(path)
	if err != nil {
		return nil, err
	}
	state := b.stateAt(ab, ab.rev)
	out := make([]AddressObject, 0, len(state))
	for _, obj := range state {
		out = append(out, *obj)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (b *memSyncBackend) QueryAddressObjects(ctx context.Context, path string, query *AddressBookQuery) ([]AddressObject, error) {
	return b.ListAddressObjects(ctx, path, &query.DataRequest)
}

func (b *memSyncBackend) stateAt(ab *memSyncAddressBook, snapshotRev uint64) map[string]*AddressObject {
	state := make(map[string]*AddressObject, len(ab.base))
	for path, obj := range ab.base {
		state[path] = obj
	}
	for _, e := range ab.entries {
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

func sortChanges(changes []memChange) {
	sort.SliceStable(changes, func(i, j int) bool {
		if changes[i].deleted != changes[j].deleted {
			return !changes[i].deleted
		}
		return changes[i].path < changes[j].path
	})
}

func (b *memSyncBackend) changesBetween(ab *memSyncAddressBook, fromRev, toRev uint64) []memChange {
	window := map[string]*AddressObject{}
	var order []string
	for _, e := range ab.entries {
		if e.rev <= fromRev || e.rev > toRev {
			continue
		}
		if _, seen := window[e.path]; !seen {
			order = append(order, e.path)
		}
		window[e.path] = e.object
	}
	before := b.stateAt(ab, fromRev)
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

func (b *memSyncBackend) SyncCollection(ctx context.Context, path string, query *SyncQuery) (*SyncResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	ab, err := b.addressBook(path)
	if err != nil {
		return nil, err
	}

	var changes []memChange
	var offset int
	var fromRev, snapshotRev uint64
	initial := false
	if query.SyncToken == "" {
		fromRev = ^uint64(0)
		snapshotRev = ab.rev
		initial = true
	} else {
		id, tokenFromRev, tokenSnapshotRev, tokenOffset, ok := parseToken(query.SyncToken)
		if !ok {
			return nil, InvalidSyncTokenError(fmt.Errorf("carddav: malformed sync token %q", query.SyncToken))
		}
		tokenAB, known := b.ids[id]
		if !known || tokenAB != ab || tokenSnapshotRev > ab.rev {
			return nil, InvalidSyncTokenError(fmt.Errorf("carddav: sync token %q is not valid for this collection", query.SyncToken))
		}
		fromRev = tokenFromRev
		if tokenOffset >= 0 {
			snapshotRev = tokenSnapshotRev
			offset = tokenOffset
		} else {
			snapshotRev = ab.rev
		}
		initial = tokenFromRev == ^uint64(0)
	}

	if initial {
		for p := range b.stateAt(ab, snapshotRev) {
			changes = append(changes, memChange{path: p})
		}
		sortChanges(changes)
	} else {
		changes = b.changesBetween(ab, fromRev, snapshotRev)
	}

	if offset > len(changes) {
		return nil, InvalidSyncTokenError(fmt.Errorf("carddav: sync token %q points past the snapshot", query.SyncToken))
	}

	resp := &SyncResponse{}
	end := len(changes)
	if query.Limit > 0 && offset+query.Limit < end {
		end = offset + query.Limit
	}

	state := b.stateAt(ab, snapshotRev)
	for _, c := range changes[offset:end] {
		if c.deleted {
			resp.Deleted = append(resp.Deleted, c.path)
			continue
		}
		obj, ok := state[c.path]
		if !ok {
			return nil, internal.HTTPErrorf(http.StatusInternalServerError, "carddav: inconsistent snapshot for %q", c.path)
		}
		resp.Updated = append(resp.Updated, *obj)
	}

	if end < len(changes) {
		resp.SyncToken = pageToken(ab.id, fromRev, snapshotRev, end)
		resp.MoreResults = true
		return resp, nil
	}
	resp.SyncToken = committedToken(ab.id, snapshotRev)
	return resp, nil
}

func (b *memSyncBackend) CurrentSyncToken(ctx context.Context, path string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ab, err := b.addressBook(path)
	if err != nil {
		return "", err
	}
	return committedToken(ab.id, ab.rev), nil
}
