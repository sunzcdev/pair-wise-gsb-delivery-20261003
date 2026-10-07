package webdav

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newLocalHandler(t *testing.T) (http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	return &Handler{FileSystem: LocalFileSystem(dir)}, dir
}

func doPut(t *testing.T, h http.Handler, target string, body []byte, headers map[string]string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, target, bytes.NewReader(body))
	req.RequestURI = target
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Result()
}

func putInitial(t *testing.T, h http.Handler, target, content string) string {
	t.Helper()
	resp := doPut(t, h, target, []byte(content), nil)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("seed PUT %s: status %d", target, resp.StatusCode)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatalf("initial PUT %s: missing ETag", target)
	}
	return etag
}

// Two clients presenting the same old If-Match tag concurrently must have
// exactly one successful write: the condition is evaluated atomically at
// commit time, so the loser observes the winner's new entity-tag.
func TestLocalFileSystemConcurrentIfMatch(t *testing.T) {
	h, dir := newLocalHandler(t)
	target := "/concurrent.txt"
	old := putInitial(t, h, target, "v0")

	const writers = 16
	start := make(chan struct{})
	var wg sync.WaitGroup
	statuses := make([]int, writers)
	bodies := make([][]byte, writers)
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			body := []byte("writer-" + strings.Repeat("x", 4088) + "-")
			body = append(body, []byte{byte('0' + i/10), byte('0' + i%10), '\n'}...)
			bodies[i] = body
			resp := doPut(t, h, target, body, map[string]string{"If-Match": old})
			statuses[i] = resp.StatusCode
			resp.Body.Close()
		}()
	}
	close(start)
	wg.Wait()

	var ok, failed int
	for _, code := range statuses {
		switch code {
		case http.StatusNoContent:
			ok++
		case http.StatusPreconditionFailed:
			failed++
		default:
			t.Fatalf("unexpected status %d", code)
		}
	}
	if ok != 1 {
		t.Fatalf("exactly one writer must win, got %d success and %d precondition failures", ok, failed)
	}
	if failed != writers-1 {
		t.Fatalf("all other writers must fail, got %d failures", failed)
	}

	// The committed content is exactly one of the winning bodies; no
	// truncation or torn writes, and no staging files left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") || strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("staging file leaked: %s", entry.Name())
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash("concurrent.txt")))
	if err != nil {
		t.Fatal(err)
	}
	matched := false
	for _, body := range bodies {
		if bytes.Equal(data, body) {
			matched = true
			break
		}
	}
	if !matched {
		t.Fatalf("committed content does not match any winning body")
	}

	// The winner produced a new tag.
	fi, err := os.Stat(filepath.Join(dir, "concurrent.txt"))
	if err != nil {
		t.Fatal(err)
	}
	newTag := fileInfoFromOS(target, fi).ETag
	if newTag == strings.Trim(old, `"`) {
		t.Fatalf("winning write did not change the entity-tag")
	}
}

// If-None-Match: * on a non-existent resource succeeds; a second attempt
// fails and neither changes nor partially creates anything.
func TestLocalFileSystemNoneMatchWildcardCreate(t *testing.T) {
	h, dir := newLocalHandler(t)
	target := "/new.txt"

	resp := doPut(t, h, target, []byte("first"), map[string]string{"If-None-Match": "*"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create with If-None-Match: *: status %d", resp.StatusCode)
	}

	resp = doPut(t, h, target, []byte("second"), map[string]string{"If-None-Match": "*"})
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("second create: status %d, want 412", resp.StatusCode)
	}

	data, err := os.ReadFile(filepath.Join(dir, "new.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "first" {
		t.Fatalf("failed create modified the resource: %q", data)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("failed create left staging files: %v", entries)
	}

	// A new resource is created even when If-None-Match carries a tag
	// list: every listed tag refers to representations that don't exist.
	listTarget := "/listed.txt"
	resp = doPut(t, h, listTarget, []byte("fresh"),
		map[string]string{"If-None-Match": `"a", "b", W/"c"`})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create with If-None-Match list: status %d, want 201", resp.StatusCode)
	}
	// Repeating with the current tag (presented weakly, inside a list)
	// must fail under the weak comparison and leave the content untouched.
	currentTag := putInitial(t, h, listTarget, "fresh")
	resp = doPut(t, h, listTarget, []byte("clobber"),
		map[string]string{"If-None-Match": `"other", W/` + currentTag})
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("repeat create with non-matching list: status %d, want 412", resp.StatusCode)
	}
	data, err = os.ReadFile(filepath.Join(dir, "listed.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "fresh" {
		t.Fatalf("failed list condition changed the resource: %q", data)
	}
}

// Multi-tag lists match a single current tag; a list without the current
// tag fails and leaves the payload unstaged at the target.
func TestLocalFileSystemMultiTagList(t *testing.T) {
	h, dir := newLocalHandler(t)
	target := "/multi.txt"
	etag := putInitial(t, h, target, "orig")

	// Current tag present in a list with other strong tags: update allowed.
	resp := doPut(t, h, target, []byte("updated"),
		map[string]string{"If-Match": `"other-a", ` + etag + `, "other-b"`})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("matching list: status %d, want 204", resp.StatusCode)
	}

	// A stale multi-tag list must fail without truncating the file.
	resp = doPut(t, h, target, []byte("should-not-appear"),
		map[string]string{"If-Match": `"stale-1", "stale-2"`})
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("non-matching list: status %d, want 412", resp.StatusCode)
	}

	// A weak tag in If-Match never satisfies the strong comparison, even
	// when the opaque-tag is identical.
	weak := "W/" + strings.TrimPrefix(etag, "")
	resp = doPut(t, h, target, []byte("should-not-appear"),
		map[string]string{"If-Match": weak})
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("weak If-Match: status %d, want 412", resp.StatusCode)
	}

	data, err := os.ReadFile(filepath.Join(dir, "multi.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "updated" {
		t.Fatalf("failed conditional update changed the resource: %q", data)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("failed update left staging files: %v", entries)
	}
}

// Malformed conditional headers are rejected with 400 before the request
// body is read, and the backend is not invoked.
func TestLocalFileSystemMalformedHeaderRejectedEarly(t *testing.T) {
	h, _ := newLocalHandler(t)

	bodyRead := false
	req := httptest.NewRequest(http.MethodPut, "/x.txt", readerFunc(func(p []byte) (int, error) {
		bodyRead = true
		return 0, io.EOF
	}))
	req.RequestURI = "/x.txt"
	req.Header.Set("If-Match", `"unterminated`)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if bodyRead {
		t.Fatalf("request body must not be read when the header is malformed")
	}
}

// A backend that cannot evaluate conditions atomically must reject requests
// carrying them via the defined 501 path rather than faking atomicity.
type nonAtomicBackend struct{}

func (nonAtomicBackend) Create(ctx context.Context, name string, body io.ReadCloser, opts *CreateOptions) (*FileInfo, bool, error) {
	if opts != nil && (opts.IfMatch.IsSet() || opts.IfNoneMatch.IsSet()) {
		return nil, false, NewConditionalMatchNotSupportedError()
	}
	return &FileInfo{Path: name}, true, nil
}
func (nonAtomicBackend) Open(context.Context, string) (io.ReadCloser, error) {
	return nil, os.ErrNotExist
}
func (nonAtomicBackend) Stat(context.Context, string) (*FileInfo, error) {
	return nil, NewHTTPError(http.StatusNotFound, errors.New("not found"))
}
func (nonAtomicBackend) ReadDir(context.Context, string, bool) ([]FileInfo, error) {
	return nil, nil
}
func (nonAtomicBackend) RemoveAll(context.Context, string, *RemoveAllOptions) error {
	return NewConditionalMatchNotSupportedError()
}
func (nonAtomicBackend) Mkdir(context.Context, string) error { return nil }
func (nonAtomicBackend) Copy(context.Context, string, string, *CopyOptions) (bool, error) {
	return false, nil
}
func (nonAtomicBackend) Move(context.Context, string, string, *MoveOptions) (bool, error) {
	return false, nil
}

func TestNonAtomicBackendRejection(t *testing.T) {
	h := &Handler{FileSystem: nonAtomicBackend{}}
	resp := doPut(t, h, "/y.txt", []byte("data"), map[string]string{"If-None-Match": "*"})
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}

	// Requests without conditions keep working.
	resp = doPut(t, h, "/y.txt", []byte("data"), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("unconditional request: status = %d, want 201", resp.StatusCode)
	}
}

type readerFunc func(p []byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }
