package webdav

import (
	"context"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path"
	"testing"
)

type recordedCopy struct {
	src, dst string
	opts     CopyOptions
}

type recordedMove struct {
	src, dst string
	opts     MoveOptions
}

// fakeFileSystem is an in-memory FileSystem that records every Copy and Move
// call so tests can verify which operations reach the storage layer.
type fakeFileSystem struct {
	resources map[string]bool // cleaned path -> isDir
	copies    []recordedCopy
	moves     []recordedMove
}

func newFakeFileSystem() *fakeFileSystem {
	return &fakeFileSystem{resources: map[string]bool{"/": true}}
}

func (f *fakeFileSystem) addDir(name string) {
	f.resources[path.Clean(name)] = true
}

func (f *fakeFileSystem) addFile(name string) {
	f.resources[path.Clean(name)] = false
}

func (f *fakeFileSystem) calls() int {
	return len(f.copies) + len(f.moves)
}

func (f *fakeFileSystem) Open(ctx context.Context, name string) (io.ReadCloser, error) {
	return nil, NewHTTPError(http.StatusNotImplemented, nil)
}

func (f *fakeFileSystem) Stat(ctx context.Context, name string) (*FileInfo, error) {
	isDir, ok := f.resources[path.Clean(name)]
	if !ok {
		return nil, NewHTTPError(http.StatusNotFound, fs.ErrNotExist)
	}
	return &FileInfo{Path: name, IsDir: isDir}, nil
}

func (f *fakeFileSystem) ReadDir(ctx context.Context, name string, recursive bool) ([]FileInfo, error) {
	return nil, NewHTTPError(http.StatusNotImplemented, nil)
}

func (f *fakeFileSystem) Create(ctx context.Context, name string, body io.ReadCloser, opts *CreateOptions) (*FileInfo, bool, error) {
	return nil, false, NewHTTPError(http.StatusNotImplemented, nil)
}

func (f *fakeFileSystem) RemoveAll(ctx context.Context, name string, opts *RemoveAllOptions) error {
	return NewHTTPError(http.StatusNotImplemented, nil)
}

func (f *fakeFileSystem) Mkdir(ctx context.Context, name string) error {
	return NewHTTPError(http.StatusNotImplemented, nil)
}

func (f *fakeFileSystem) Copy(ctx context.Context, src, dst string, opts *CopyOptions) (bool, error) {
	f.copies = append(f.copies, recordedCopy{src, dst, *opts})
	dstKey := path.Clean(dst)
	_, exists := f.resources[dstKey]
	if exists && opts.NoOverwrite {
		return false, fs.ErrExist
	}
	f.resources[dstKey] = f.resources[path.Clean(src)]
	return !exists, nil
}

func (f *fakeFileSystem) Move(ctx context.Context, src, dst string, opts *MoveOptions) (bool, error) {
	f.moves = append(f.moves, recordedMove{src, dst, *opts})
	srcKey := path.Clean(src)
	dstKey := path.Clean(dst)
	_, exists := f.resources[dstKey]
	if exists && opts.NoOverwrite {
		return false, fs.ErrExist
	}
	f.resources[dstKey] = f.resources[srcKey]
	delete(f.resources, srcKey)
	return !exists, nil
}

func doCopyMove(t *testing.T, f *fakeFileSystem, method, target, destination string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest(method, "http://example.com"+target, nil)
	if destination != "" {
		r.Header.Set("Destination", destination)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}

	h := Handler{FileSystem: f}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func assertNoCommit(t *testing.T, f *fakeFileSystem) {
	t.Helper()
	if f.calls() != 0 {
		t.Fatalf("expected no storage commit, got %d copy and %d move calls", len(f.copies), len(f.moves))
	}
}

func TestMoveCollectionIntoOwnDescendant(t *testing.T) {
	f := newFakeFileSystem()
	f.addDir("/a")
	f.addDir("/a/b")

	rec := doCopyMove(t, f, "MOVE", "/a/", "http://example.com/a/b/c/", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
	assertNoCommit(t, f)
}

func TestMoveCollectionToPrefixSiblingAllowed(t *testing.T) {
	// "/ab" shares the string prefix "/a" but is not a descendant of "/a":
	// the move must be accepted.
	f := newFakeFileSystem()
	f.addDir("/a")

	rec := doCopyMove(t, f, "MOVE", "/a/", "http://example.com/ab", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", rec.Code)
	}
	if len(f.moves) != 1 {
		t.Fatalf("expected exactly one move commit, got %d", len(f.moves))
	}
}

func TestCopyMoveSameResource(t *testing.T) {
	cases := []struct {
		name        string
		method      string
		target      string
		destination string
		setup       func(f *fakeFileSystem)
	}{
		{
			name:        "move collection trailing slash",
			method:      "MOVE",
			target:      "/a/",
			destination: "http://example.com/a",
			setup:       func(f *fakeFileSystem) { f.addDir("/a") },
		},
		{
			name:        "copy identical file",
			method:      "COPY",
			target:      "/a/b.txt",
			destination: "http://example.com/a/b.txt",
			setup: func(f *fakeFileSystem) {
				f.addDir("/a")
				f.addFile("/a/b.txt")
			},
		},
		{
			name:        "move encoded names",
			method:      "MOVE",
			target:      "/a%20b/x.txt",
			destination: "http://example.com/a%20b/x.txt",
			setup: func(f *fakeFileSystem) {
				f.addDir("/a b")
				f.addFile("/a b/x.txt")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeFileSystem()
			tc.setup(f)

			rec := doCopyMove(t, f, tc.method, tc.target, tc.destination, nil)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected 403, got %d", rec.Code)
			}
			assertNoCommit(t, f)
		})
	}
}

func TestCopyMoveMissingDestinationParent(t *testing.T) {
	for _, method := range []string{"COPY", "MOVE"} {
		f := newFakeFileSystem()
		f.addDir("/a")
		f.addFile("/a/x.txt")

		rec := doCopyMove(t, f, method, "/a/x.txt", "http://example.com/nope/y.txt", nil)
		if rec.Code != http.StatusConflict {
			t.Fatalf("%s: expected 409, got %d", method, rec.Code)
		}
		assertNoCommit(t, f)
	}
}

func TestCopyMoveForeignDestination(t *testing.T) {
	for _, method := range []string{"COPY", "MOVE"} {
		f := newFakeFileSystem()
		f.addDir("/a")
		f.addFile("/a/x.txt")

		rec := doCopyMove(t, f, method, "/a/x.txt", "http://other.example/y.txt", nil)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("%s: expected 502, got %d", method, rec.Code)
		}
		assertNoCommit(t, f)
	}
}

func TestCopyMoveMissingSource(t *testing.T) {
	f := newFakeFileSystem()
	f.addDir("/a")

	rec := doCopyMove(t, f, "MOVE", "/gone.txt", "http://example.com/a/y.txt", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
	assertNoCommit(t, f)
}

func TestCopyOverwriteCommitsOnce(t *testing.T) {
	f := newFakeFileSystem()
	f.addDir("/a")
	f.addFile("/a/x.txt")
	f.addFile("/a/y.txt")

	rec := doCopyMove(t, f, "COPY", "/a/x.txt", "http://example.com/a/y.txt", map[string]string{
		"Overwrite": "T",
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rec.Code)
	}
	if len(f.copies) != 1 {
		t.Fatalf("expected exactly one copy commit, got %d", len(f.copies))
	}
	if f.copies[0].opts.NoOverwrite {
		t.Fatal("expected NoOverwrite to be false")
	}
}

func TestCopyNoOverwriteExistingDestination(t *testing.T) {
	f := newFakeFileSystem()
	f.addDir("/a")
	f.addFile("/a/x.txt")
	f.addFile("/a/y.txt")

	rec := doCopyMove(t, f, "COPY", "/a/x.txt", "http://example.com/a/y.txt", map[string]string{
		"Overwrite": "F",
	})
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected 412, got %d", rec.Code)
	}
	if len(f.copies) != 1 {
		t.Fatalf("expected exactly one copy commit, got %d", len(f.copies))
	}
	if !f.copies[0].opts.NoOverwrite {
		t.Fatal("expected NoOverwrite to be true")
	}
}

func TestCopyDepthOneRejected(t *testing.T) {
	f := newFakeFileSystem()
	f.addDir("/a")

	rec := doCopyMove(t, f, "COPY", "/a", "http://example.com/b", map[string]string{
		"Depth": "1",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	assertNoCommit(t, f)
}

func TestMoveDepthZeroRejected(t *testing.T) {
	f := newFakeFileSystem()
	f.addDir("/a")

	rec := doCopyMove(t, f, "MOVE", "/a", "http://example.com/b", map[string]string{
		"Depth": "0",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	assertNoCommit(t, f)
}

func TestMoveEncodedDestination(t *testing.T) {
	f := newFakeFileSystem()
	f.addDir("/a")
	f.addDir("/b")
	f.addFile("/a/é.txt")

	rec := doCopyMove(t, f, "MOVE", "/a/%C3%A9.txt", "http://example.com/b/%C3%A9.txt", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", rec.Code)
	}
	if len(f.moves) != 1 {
		t.Fatalf("expected exactly one move commit, got %d", len(f.moves))
	}
	if f.moves[0].dst != "/b/é.txt" {
		t.Fatalf("expected decoded destination %q, got %q", "/b/é.txt", f.moves[0].dst)
	}
}
