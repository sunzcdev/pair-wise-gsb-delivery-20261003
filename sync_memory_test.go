package webdav

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// memorySyncFileSystem is an in-memory FileSystem implementing
// SyncCollectionFileSystem. Each collection has a monotonically increasing
// revision and keeps the full history of member changes, allowing old sync
// tokens (but not foreign or malformed ones) to be replayed.
type memorySyncFileSystem struct {
	mu          sync.Mutex
	files       map[string]*memoryFile
	collections map[string]*memoryCollection
}

type memoryFile struct {
	path    string
	data    []byte
	modTime time.Time
	etag    string
}

type memoryCollection struct {
	revision int64
	events   []memoryChange
}

type memoryChange struct {
	revision int64
	path     string
	deleted  bool
	file     *memoryFile // nil when deleted
}

// Token syntax (opaque to clients):
//   - committed:     "<collection>#<revision>"
//   - continuation:  "<collection>@<revision>:<offset>"

func newMemorySyncFileSystem() *memorySyncFileSystem {
	return &memorySyncFileSystem{
		files:       make(map[string]*memoryFile),
		collections: make(map[string]*memoryCollection),
	}
}

func (fs *memorySyncFileSystem) mkdir(name string) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.collections[path.Clean(name)] = &memoryCollection{}
}

func (fs *memorySyncFileSystem) put(name, data string) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	name = path.Clean(name)
	col := path.Dir(name)
	c := fs.collections[col]
	c.revision++

	f := &memoryFile{
		path:    name,
		data:    []byte(data),
		modTime: time.Unix(c.revision, 0).UTC(),
		etag:    fmt.Sprintf("etag-%d", c.revision),
	}
	fs.files[name] = f
	c.events = append(c.events, memoryChange{revision: c.revision, path: name, file: f})
}

func (fs *memorySyncFileSystem) remove(name string) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	name = path.Clean(name)
	col := path.Dir(name)
	c := fs.collections[col]
	c.revision++

	delete(fs.files, name)
	c.events = append(c.events, memoryChange{revision: c.revision, path: name, deleted: true})
}

func (f *memoryFile) fileInfo() *FileInfo {
	return &FileInfo{
		Path:    f.path,
		Size:    int64(len(f.data)),
		ModTime: f.modTime,
		ETag:    f.etag,
	}
}

func (fs *memorySyncFileSystem) Open(ctx context.Context, name string) (io.ReadCloser, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	f, ok := fs.files[path.Clean(name)]
	if !ok {
		return nil, NewHTTPError(http.StatusNotFound, fmt.Errorf("not found"))
	}
	return io.NopCloser(bytes.NewReader(f.data)), nil
}

func (fs *memorySyncFileSystem) Stat(ctx context.Context, name string) (*FileInfo, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	name = path.Clean(name)
	if c, ok := fs.collections[name]; ok {
		return &FileInfo{Path: name, IsDir: true, ModTime: time.Unix(c.revision, 0).UTC()}, nil
	}
	if f, ok := fs.files[name]; ok {
		return f.fileInfo(), nil
	}
	return nil, NewHTTPError(http.StatusNotFound, fmt.Errorf("not found"))
}

func (fs *memorySyncFileSystem) ReadDir(ctx context.Context, name string, recursive bool) ([]FileInfo, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	var result []FileInfo
	prefix := strings.TrimSuffix(path.Clean(name), "/") + "/"
	for p, f := range fs.files {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		if !recursive && path.Dir(p) != path.Clean(name) {
			continue
		}
		result = append(result, *f.fileInfo())
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, nil
}

func (fs *memorySyncFileSystem) Create(ctx context.Context, name string, body io.ReadCloser, opts *CreateOptions) (*FileInfo, bool, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, false, err
	}
	fs.put(name, string(data))
	fi, _ := fs.Stat(ctx, name)
	return fi, true, nil
}

func (fs *memorySyncFileSystem) RemoveAll(ctx context.Context, name string, opts *RemoveAllOptions) error {
	fs.remove(name)
	return nil
}

func (fs *memorySyncFileSystem) Mkdir(ctx context.Context, name string) error {
	fs.mkdir(name)
	return nil
}

func (fs *memorySyncFileSystem) Copy(ctx context.Context, name, dest string, options *CopyOptions) (bool, error) {
	r, err := fs.Open(ctx, name)
	if err != nil {
		return false, err
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		return false, err
	}
	fs.put(dest, string(data))
	return true, nil
}

func (fs *memorySyncFileSystem) Move(ctx context.Context, name, dest string, options *MoveOptions) (bool, error) {
	r, err := fs.Open(ctx, name)
	if err != nil {
		return false, err
	}
	data, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		return false, err
	}
	fs.remove(name)
	fs.put(dest, string(data))
	return true, nil
}

func (fs *memorySyncFileSystem) CurrentSyncToken(ctx context.Context, name string) (string, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	c, ok := fs.collections[path.Clean(name)]
	if !ok {
		return "", NewHTTPError(http.StatusNotFound, fmt.Errorf("not found"))
	}
	return committedToken(path.Clean(name), c.revision), nil
}

func committedToken(collection string, revision int64) string {
	return fmt.Sprintf("%s#%d", collection, revision)
}

func continuationToken(collection string, revision int64, offset int) string {
	return fmt.Sprintf("%s@%d:%d", collection, revision, offset)
}

type parsedToken struct {
	collection   string
	revision     int64
	offset       int
	continuation bool
}

func parseToken(token string) (parsedToken, bool) {
	i := strings.IndexAny(token, "#@")
	if i < 0 {
		return parsedToken{}, false
	}
	t := parsedToken{collection: token[:i], continuation: token[i] == '@'}
	rest := token[i+1:]

	if t.continuation {
		parts := strings.SplitN(rest, ":", 2)
		if len(parts) != 2 {
			return parsedToken{}, false
		}
		rev, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return parsedToken{}, false
		}
		off, err := strconv.Atoi(parts[1])
		if err != nil {
			return parsedToken{}, false
		}
		t.revision, t.offset = rev, off
		return t, true
	}

	rev, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return parsedToken{}, false
	}
	t.revision = rev
	return t, true
}

func (fs *memorySyncFileSystem) SyncCollection(ctx context.Context, name string, req *SyncCollectionRequest) (*SyncCollectionResult, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	collection := path.Clean(name)
	c, ok := fs.collections[collection]
	if !ok {
		return nil, NewHTTPError(http.StatusNotFound, fmt.Errorf("not found"))
	}

	var (
		snapshot int64
		baseline int64
		start    int
		initial  bool
	)

	if req.SyncToken == "" {
		initial = true
		snapshot = c.revision
	} else {
		t, ok := parseToken(req.SyncToken)
		if !ok || t.collection != collection {
			return nil, NewValidSyncTokenError()
		}
		if t.continuation {
			if t.revision < 0 || t.revision > c.revision || t.offset < 0 {
				return nil, NewValidSyncTokenError()
			}
			snapshot, start = t.revision, t.offset
			// An initial-sync continuation: the member list is the
			// snapshot state rather than an event-log diff.
			initial = true
		} else {
			if t.revision < 0 || t.revision > c.revision {
				return nil, NewValidSyncTokenError()
			}
			snapshot = c.revision
			baseline = t.revision
		}
	}

	changes := fs.collapse(c, collection, snapshot, baseline, initial)

	end := len(changes)
	if req.Limit > 0 && start+req.Limit < end {
		end = start + req.Limit
	}

	page := changes[start:end]
	result := &SyncCollectionResult{
		Changes:   make([]SyncCollectionChange, len(page)),
		SyncToken: committedToken(collection, snapshot),
	}
	for i, ch := range page {
		change := SyncCollectionChange{
			FileInfo: FileInfo{Path: ch.path},
			Deleted:  ch.deleted,
		}
		if ch.file != nil {
			change.FileInfo = *ch.file.fileInfo()
		}
		result.Changes[i] = change
	}

	if end < len(changes) {
		// Truncated result: keep the continuation bound to the same
		// snapshot, and do not emit the snapshot's committed token until
		// the client has read every page.
		result.SyncToken = continuationToken(collection, snapshot, end)
	}
	return result, nil
}

// collapse reduces the event log to one entry per member.
//
// On an initial sync (initial == true) it enumerates every member present at
// the snapshot as an addition. On an incremental sync it replays events in
// (baseline, snapshot], keeping each member's final state (or a tombstone if
// it was deleted).
func (fs *memorySyncFileSystem) collapse(c *memoryCollection, collection string, snapshot, baseline int64, initial bool) []memoryChange {
	index := make(map[string]int)
	var result []memoryChange

	add := func(ch memoryChange) {
		if i, ok := index[ch.path]; ok {
			result[i] = ch
			return
		}
		index[ch.path] = len(result)
		result = append(result, ch)
	}

	if initial {
		// Reconstruct the state at snapshot by replaying all events up to
		// snapshot, then keeping only live members in their snapshot state.
		states := make(map[string]*memoryFile)
		for _, ev := range c.events {
			if ev.revision > snapshot {
				break
			}
			if path.Dir(ev.path) != collection {
				continue
			}
			if ev.deleted {
				delete(states, ev.path)
			} else {
				states[ev.path] = ev.file
			}
		}
		var paths []string
		for p := range states {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			add(memoryChange{path: p, file: states[p]})
		}
		return result
	}

	for _, ev := range c.events {
		if ev.revision <= baseline {
			continue
		}
		if ev.revision > snapshot {
			break
		}
		if path.Dir(ev.path) != collection {
			continue
		}
		add(ev)
	}
	return result
}
