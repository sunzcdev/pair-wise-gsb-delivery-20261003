package webdav

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

type node struct {
	isDir bool
}

type fakeStore struct {
	nodes   map[string]node
	commits []*CopyMoveParams
	copyN   int
	moveN   int
}

func newFakeStore() *fakeStore {
	return &fakeStore{nodes: map[string]node{
		"/":           {isDir: true},
		"/d":          {isDir: true},
		"/d/sub":      {isDir: true},
		"/d/sub/leaf": {},
		"/file.txt":   {},
		"/foo":        {},
		"/foobar":     {},
	}}
}

// atomicFakeFS records the single atomic commit instead of performing
// separate Copy/Move calls.
type atomicFakeFS struct {
	*fakeStore
}

// legacyFakeFS implements only the legacy FileSystem Copy/Move methods.
type legacyFakeFS struct {
	*fakeStore
}

func (s *fakeStore) Stat(ctx context.Context, name string) (*FileInfo, error) {
	n, ok := s.nodes[name]
	if !ok {
		return nil, NewHTTPError(http.StatusNotFound, os.ErrNotExist)
	}
	return &FileInfo{Path: name, IsDir: n.isDir}, nil
}

func (s *fakeStore) Open(ctx context.Context, name string) (io.ReadCloser, error) {
	return nil, NewHTTPError(http.StatusNotFound, os.ErrNotExist)
}

func (s *fakeStore) ReadDir(ctx context.Context, name string, recursive bool) ([]FileInfo, error) {
	return nil, nil
}

func (s *fakeStore) Create(ctx context.Context, name string, body io.ReadCloser, opts *CreateOptions) (*FileInfo, bool, error) {
	return nil, false, errors.New("fakeStore: Create not supported")
}

func (s *fakeStore) RemoveAll(ctx context.Context, name string, opts *RemoveAllOptions) error {
	return errors.New("fakeStore: RemoveAll not supported")
}

func (s *fakeStore) Mkdir(ctx context.Context, name string) error {
	return errors.New("fakeStore: Mkdir not supported")
}

func (s *fakeStore) commit(params *CopyMoveParams) (bool, error) {
	created := !params.DstExists
	s.commits = append(s.commits, params)
	s.nodes[params.Dst] = node{isDir: params.SrcIsDir}
	if params.Move {
		delete(s.nodes, params.Src)
	}
	return created, nil
}

func (fs atomicFakeFS) CopyMove(ctx context.Context, params *CopyMoveParams) (bool, error) {
	return fs.commit(params)
}

func (fs atomicFakeFS) Copy(ctx context.Context, src, dst string, opts *CopyOptions) (bool, error) {
	panic("atomicFakeFS: legacy Copy must not be called")
}

func (fs atomicFakeFS) Move(ctx context.Context, src, dst string, opts *MoveOptions) (bool, error) {
	panic("atomicFakeFS: legacy Move must not be called")
}

func (fs legacyFakeFS) Copy(ctx context.Context, src, dst string, opts *CopyOptions) (bool, error) {
	fs.copyN++
	fi, _ := fs.Stat(ctx, dst)
	if fi != nil && opts.NoOverwrite {
		return false, NewHTTPError(http.StatusPreconditionFailed, os.ErrExist)
	}
	fs.nodes[dst] = fs.nodes[src]
	return fi == nil, nil
}

func (fs legacyFakeFS) Move(ctx context.Context, src, dst string, opts *MoveOptions) (bool, error) {
	fs.moveN++
	fi, _ := fs.Stat(ctx, dst)
	if fi != nil && opts.NoOverwrite {
		return false, NewHTTPError(http.StatusPreconditionFailed, os.ErrExist)
	}
	fs.nodes[dst] = fs.nodes[src]
	delete(fs.nodes, src)
	return fi == nil, nil
}

func doCopyMove(t *testing.T, fs FileSystem, method, src, dest, depth, overwrite string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "http://example.com"+src, nil)
	req.Host = "example.com"
	if dest != "" {
		req.Header.Set("Destination", dest)
	}
	if depth != "" {
		req.Header.Set("Depth", depth)
	}
	if overwrite != "" {
		req.Header.Set("Overwrite", overwrite)
	}
	w := httptest.NewRecorder()
	h := Handler{FileSystem: fs}
	h.ServeHTTP(w, req)
	return w
}

type copyMoveCase struct {
	name      string
	method    string
	src       string
	dest      string
	depth     string
	overwrite string
	wantCode  int
	commits   int
}

var copyMoveCases = []copyMoveCase{
	{name: "move new", method: "MOVE", src: "/file.txt", dest: "http://example.com/moved.txt", wantCode: http.StatusCreated, commits: 1},
	{name: "copy new", method: "COPY", src: "/file.txt", dest: "http://example.com/copied.txt", wantCode: http.StatusCreated, commits: 1},
	{name: "move collection new", method: "MOVE", src: "/d", dest: "http://example.com/moved", wantCode: http.StatusCreated, commits: 1},
	{name: "copy collection infinity", method: "COPY", src: "/d", dest: "http://example.com/copied", depth: "infinity", wantCode: http.StatusCreated, commits: 1},
	{name: "copy overwrite", method: "COPY", src: "/file.txt", dest: "http://example.com/foo", overwrite: "T", wantCode: http.StatusNoContent, commits: 1},
	{name: "copy no overwrite existing", method: "COPY", src: "/file.txt", dest: "http://example.com/foo", overwrite: "F", wantCode: http.StatusPreconditionFailed, commits: 0},
	{name: "move no overwrite existing", method: "MOVE", src: "/file.txt", dest: "http://example.com/foo", overwrite: "F", wantCode: http.StatusPreconditionFailed, commits: 0},
	{name: "move into descendant", method: "MOVE", src: "/d", dest: "http://example.com/d/sub/new", wantCode: http.StatusForbidden, commits: 0},
	{name: "copy infinity into descendant", method: "COPY", src: "/d", dest: "http://example.com/d/sub/new", depth: "infinity", wantCode: http.StatusForbidden, commits: 0},
	{name: "copy depth zero into descendant", method: "COPY", src: "/d", dest: "http://example.com/d/sub/new", depth: "0", wantCode: http.StatusCreated, commits: 1},
	{name: "move self", method: "MOVE", src: "/file.txt", dest: "http://example.com/file.txt", wantCode: http.StatusForbidden, commits: 0},
	{name: "copy self", method: "COPY", src: "/file.txt", dest: "http://example.com/file.txt", wantCode: http.StatusForbidden, commits: 0},
	{name: "move self encoded", method: "MOVE", src: "/a%20b", dest: "http://example.com/a b", wantCode: http.StatusNotFound, commits: 0},
	{name: "missing parent", method: "MOVE", src: "/file.txt", dest: "http://example.com/nope/new.txt", wantCode: http.StatusConflict, commits: 0},
	{name: "parent is non-collection", method: "MOVE", src: "/file.txt", dest: "http://example.com/foo/new.txt", wantCode: http.StatusConflict, commits: 0},
	{name: "source missing", method: "MOVE", src: "/missing.txt", dest: "http://example.com/new.txt", wantCode: http.StatusNotFound, commits: 0},
	{name: "destination on other service", method: "MOVE", src: "/file.txt", dest: "http://other.example.com/new.txt", wantCode: http.StatusBadGateway, commits: 0},
	{name: "sibling not prefix descendant", method: "MOVE", src: "/foo", dest: "http://example.com/foobar2", wantCode: http.StatusCreated, commits: 1},
	{name: "encoded names are equal", method: "MOVE", src: "/foo", dest: "http://example.com/%66oo", wantCode: http.StatusForbidden, commits: 0},
	{name: "encoded slash rejected", method: "MOVE", src: "/file.txt", dest: "http://example.com/a%2Fb", wantCode: http.StatusBadRequest, commits: 0},
	{name: "destination query rejected", method: "MOVE", src: "/file.txt", dest: "http://example.com/new.txt?x=1", wantCode: http.StatusBadRequest, commits: 0},
	{name: "file trailing slash", method: "MOVE", src: "/file.txt", dest: "http://example.com/moved.txt/", wantCode: http.StatusConflict, commits: 0},
	{name: "collection trailing slash", method: "MOVE", src: "/d", dest: "http://example.com/moved/", wantCode: http.StatusCreated, commits: 1},
	{name: "copy depth one", method: "COPY", src: "/d", dest: "http://example.com/new", depth: "1", wantCode: http.StatusBadRequest, commits: 0},
	{name: "move depth zero", method: "MOVE", src: "/d", dest: "http://example.com/new", depth: "0", wantCode: http.StatusBadRequest, commits: 0},
	{name: "invalid overwrite", method: "MOVE", src: "/file.txt", dest: "http://example.com/new.txt", overwrite: "yes", wantCode: http.StatusBadRequest, commits: 0},
	{name: "invalid depth", method: "COPY", src: "/file.txt", dest: "http://example.com/new.txt", depth: "2", wantCode: http.StatusBadRequest, commits: 0},
	{name: "missing destination", method: "MOVE", src: "/file.txt", dest: "", wantCode: http.StatusBadRequest, commits: 0},
	{name: "root destination", method: "MOVE", src: "/file.txt", dest: "http://example.com/", wantCode: http.StatusForbidden, commits: 0},
}

func TestCopyMovePreflight(t *testing.T) {
	for _, tc := range copyMoveCases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			fs := atomicFakeFS{store}

			w := doCopyMove(t, fs, tc.method, tc.src, tc.dest, tc.depth, tc.overwrite)
			if w.Code != tc.wantCode {
				t.Fatalf("status: got %v, want %v", w.Code, tc.wantCode)
			}
			if len(store.commits) != tc.commits {
				t.Fatalf("commits: got %v, want %v", len(store.commits), tc.commits)
			}
		})
	}
}

func TestCopyMoveOverwriteCommitsOnce(t *testing.T) {
	store := newFakeStore()
	fs := atomicFakeFS{store}

	w := doCopyMove(t, fs, "MOVE", "/file.txt", "http://example.com/foo", "", "T")
	if w.Code != http.StatusNoContent {
		t.Fatalf("status: got %v, want %v", w.Code, http.StatusNoContent)
	}
	if len(store.commits) != 1 {
		t.Fatalf("commits: got %v, want 1", len(store.commits))
	}

	params := store.commits[0]
	if !params.Move || !params.Overwrite || !params.DstExists {
		t.Fatalf("unexpected resolved params: %+v", params)
	}
	if params.Src != "/file.txt" || params.Dst != "/foo" {
		t.Fatalf("unexpected paths: %+v", params)
	}
	if _, ok := store.nodes["/file.txt"]; ok {
		t.Fatal("source still present after move")
	}
	if _, ok := store.nodes["/foo"]; !ok {
		t.Fatal("destination missing after move")
	}
}

func TestCopyMoveResolvedParams(t *testing.T) {
	store := newFakeStore()
	fs := atomicFakeFS{store}

	w := doCopyMove(t, fs, "COPY", "/d", "http://example.com/copied", "0", "F")
	if w.Code != http.StatusCreated {
		t.Fatalf("status: got %v, want %v", w.Code, http.StatusCreated)
	}
	params := store.commits[0]
	if params.Move || params.Recursive || params.Overwrite || !params.SrcIsDir || params.DstExists {
		t.Fatalf("unexpected resolved params: %+v", params)
	}
}

func TestCopyMoveLegacyFallback(t *testing.T) {
	store := newFakeStore()
	fs := legacyFakeFS{store}

	w := doCopyMove(t, fs, "MOVE", "/file.txt", "http://example.com/legacy.txt", "", "F")
	if w.Code != http.StatusCreated {
		t.Fatalf("status: got %v, want %v", w.Code, http.StatusCreated)
	}
	if store.moveN != 1 || store.copyN != 0 {
		t.Fatalf("legacy calls: move=%v copy=%v", store.moveN, store.copyN)
	}

	reject := doCopyMove(t, fs, "MOVE", "/d", "http://example.com/d/desc", "", "F")
	if reject.Code != http.StatusForbidden {
		t.Fatalf("status: got %v, want %v", reject.Code, http.StatusForbidden)
	}
	if store.moveN != 1 {
		t.Fatalf("rejected request must not reach the backend, move calls: %v", store.moveN)
	}
}

func TestCopyMoveLocalFileSystem(t *testing.T) {
	dir := t.TempDir()
	fs := LocalFileSystem(dir)

	ctx := context.Background()
	if err := fs.Mkdir(ctx, "/d"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Mkdir(ctx, "/d/sub"); err != nil {
		t.Fatal(err)
	}

	h := Handler{FileSystem: fs}

	req := httptest.NewRequest("MOVE", "http://example.com/d", nil)
	req.Host = "example.com"
	req.Header.Set("Destination", "http://example.com/d/sub/moved")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("descendant move: got %v, want %v", w.Code, http.StatusForbidden)
	}

	req = httptest.NewRequest("MOVE", "http://example.com/d", nil)
	req.Host = "example.com"
	req.Header.Set("Destination", "http://example.com/moved")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("valid move: got %v, want %v", w.Code, http.StatusCreated)
	}

	if exists, _, err := statResource(ctx, fs, "/d"); err != nil || exists {
		t.Fatalf("source still present: exists=%v err=%v", exists, err)
	}
	if exists, isDir, err := statResource(ctx, fs, "/moved"); err != nil || !exists || !isDir {
		t.Fatalf("destination missing: exists=%v isDir=%v err=%v", exists, isDir, err)
	}
}
