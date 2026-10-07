package carddav

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-vcard"
	"github.com/emersion/go-webdav"
)

const abPath = "/test/contacts/sync"
const abPath2 = "/test/contacts/sync-other"

// syncTestBackend implements Backend and SyncAddressBookBackend.
type syncTestBackend struct {
	mu      sync.Mutex
	path    string
	token   int
	events  []abSyncEvent
	objects map[string]AddressObject
}

type abSyncEvent struct {
	revision int
	path     string
	deleted  bool
	object   *AddressObject
}

func newSyncTestBackend() *syncTestBackend {
	return &syncTestBackend{
		path:    abPath,
		objects: make(map[string]AddressObject),
	}
}

func (b *syncTestBackend) put(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.token++

	card := vcard.Card{}
	card.AddValue(vcard.FieldVersion, "4.0")
	card.AddValue(vcard.FieldUID, name)
	card.AddValue(vcard.FieldFormattedName, "Contact "+name)

	obj := AddressObject{
		Path:    abPath + "/" + name,
		ModTime: time.Unix(int64(b.token), 0).UTC(),
		ETag:    fmt.Sprintf("etag-%d", b.token),
		Card:    card,
	}
	b.objects[obj.Path] = obj
	b.events = append(b.events, abSyncEvent{revision: b.token, path: obj.Path, object: &obj})
}

func (b *syncTestBackend) del(path string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.token++
	delete(b.objects, path)
	b.events = append(b.events, abSyncEvent{revision: b.token, path: path, deleted: true})
}

func parseABToken(collection, token string) (int, bool) {
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

func (b *syncTestBackend) CurrentAddressBookSyncToken(ctx context.Context, path string) (string, error) {
	if path != b.path && path != abPath2 {
		return "", webdav.NewHTTPError(http.StatusNotFound, fmt.Errorf("not found"))
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return fmt.Sprintf("%s#%d", path, b.token), nil
}

func (b *syncTestBackend) SyncAddressObjects(ctx context.Context, path string, req *SyncQuery) (*SyncResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if path != b.path && path != abPath2 {
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

	baseline, ok := parseABToken(path, req.SyncToken)
	if !ok || baseline < 0 || baseline > b.token {
		return nil, webdav.NewValidSyncTokenError()
	}

	final := map[string]abSyncEvent{}
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

// The remaining Backend methods use a minimal in-memory implementation.

func (b *syncTestBackend) CurrentUserPrincipal(ctx context.Context) (string, error) {
	return "/test/", nil
}

func (b *syncTestBackend) AddressBookHomeSetPath(ctx context.Context) (string, error) {
	return "/test/contacts/", nil
}

func (b *syncTestBackend) ListAddressBooks(ctx context.Context) ([]AddressBook, error) {
	return []AddressBook{
		{Path: abPath, Name: "Sync book"},
		{Path: abPath2, Name: "Other book"},
	}, nil
}

func (b *syncTestBackend) GetAddressBook(ctx context.Context, path string) (*AddressBook, error) {
	for _, ab := range []string{abPath, abPath2} {
		if path == ab {
			return &AddressBook{Path: path, Name: "Sync book"}, nil
		}
	}
	return nil, webdav.NewHTTPError(http.StatusNotFound, fmt.Errorf("not found"))
}

func (b *syncTestBackend) CreateAddressBook(ctx context.Context, ab *AddressBook) error {
	return nil
}

func (b *syncTestBackend) DeleteAddressBook(ctx context.Context, path string) error {
	return fmt.Errorf("not implemented")
}

func (b *syncTestBackend) GetAddressObject(ctx context.Context, path string, req *AddressDataRequest) (*AddressObject, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if obj, ok := b.objects[path]; ok {
		return &obj, nil
	}
	return nil, webdav.NewHTTPError(http.StatusNotFound, fmt.Errorf("not found"))
}

func (b *syncTestBackend) ListAddressObjects(ctx context.Context, path string, req *AddressDataRequest) ([]AddressObject, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var result []AddressObject
	for p, obj := range b.objects {
		if strings.HasPrefix(p, path+"/") {
			result = append(result, obj)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, nil
}

func (b *syncTestBackend) QueryAddressObjects(ctx context.Context, path string, query *AddressBookQuery) ([]AddressObject, error) {
	return nil, fmt.Errorf("not implemented")
}

func (b *syncTestBackend) PutAddressObject(ctx context.Context, path string, card vcard.Card, opts *PutAddressObjectOptions) (*AddressObject, error) {
	return nil, fmt.Errorf("not implemented")
}

func (b *syncTestBackend) DeleteAddressObject(ctx context.Context, path string) error {
	return fmt.Errorf("not implemented")
}

func TestSyncAddressObjects(t *testing.T) {
	backend := newSyncTestBackend()
	backend.put("a.vcf")
	backend.put("b.vcf")

	ts := httptest.NewServer(&Handler{Backend: backend})
	defer ts.Close()

	client, err := NewClient(nil, ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	initial, err := client.SyncCollection(ctx, abPath, &SyncQuery{})
	if err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	if len(initial.Updated) != 2 || len(initial.Deleted) != 0 {
		t.Fatalf("initial sync: updated=%d deleted=%d", len(initial.Updated), len(initial.Deleted))
	}

	backend.del(abPath + "/a.vcf")
	backend.put("a.vcf") // recreate
	backend.del(abPath + "/b.vcf")

	inc, err := client.SyncCollection(ctx, abPath, &SyncQuery{SyncToken: initial.SyncToken})
	if err != nil {
		t.Fatalf("incremental sync: %v", err)
	}

	updated := map[string]bool{}
	for _, o := range inc.Updated {
		updated[o.Path] = true
	}
	if !updated[abPath+"/a.vcf"] {
		t.Errorf("recreated a.vcf missing from updated: %v", updated)
	}
	deleted := map[string]bool{}
	for _, p := range inc.Deleted {
		deleted[p] = true
	}
	if !deleted[abPath+"/b.vcf"] {
		t.Errorf("b.vcf missing from deleted: %v", inc.Deleted)
	}

	// Foreign collection token.
	if _, err := client.SyncCollection(ctx, abPath2, &SyncQuery{SyncToken: initial.SyncToken}); err == nil {
		t.Fatal("foreign token: expected error, got nil")
	} else if !strings.Contains(err.Error(), "403") {
		t.Fatalf("foreign token: expected 403, got %v", err)
	}

	// Garbage token.
	if _, err := client.SyncCollection(ctx, abPath, &SyncQuery{SyncToken: "nonsense"}); err == nil {
		t.Fatal("invalid token: expected error, got nil")
	} else if !strings.Contains(err.Error(), "403") {
		t.Fatalf("invalid token: expected 403, got %v", err)
	}
}

func TestSyncAddressBookOptions(t *testing.T) {
	backend := newSyncTestBackend()
	ts := httptest.NewServer(&Handler{Backend: backend})
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodOptions, ts.URL+abPath, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Dav"), "sync-token") {
		t.Errorf("address book missing sync-token DAV class: %q", resp.Header.Get("Dav"))
	}

	plain := httptest.NewServer(&Handler{Backend: &testBackend{}})
	defer plain.Close()

	// testBackend routes resource types by path depth; exercise the address
	// book depth (3 components) via a distinct existing book path.
	req2, _ := http.NewRequest(http.MethodOptions, plain.URL+"/test/contacts/plain", nil)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if strings.Contains(resp2.Header.Get("Dav"), "sync-token") {
		t.Errorf("plain backend unexpectedly advertised sync-token: %q", resp2.Header.Get("Dav"))
	}
}
