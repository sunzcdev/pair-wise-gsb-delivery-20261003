package webdav

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func newLocalTestServer(t *testing.T) (*httptest.Server, string, *Client) {
	t.Helper()
	dir := t.TempDir()
	srv := httptest.NewServer(&Handler{FileSystem: LocalFileSystem(dir)})
	t.Cleanup(srv.Close)

	c, err := NewClient(http.DefaultClient, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return srv, dir, c
}

func mustWriteFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(name)), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func fileContent(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Two clients concurrently PUT using the same stale-but-current ETag: exactly
// one must succeed and the other must receive 412.
func TestLocalFileSystemConcurrentIfMatchRace(t *testing.T) {
	srv, dir, c := newLocalTestServer(t)

	const name = "/race.txt"
	mustWriteFile(t, dir, name, "initial")

	fi, err := c.Stat(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	etag := fi.ETag

	put := func(body string) int {
		req, err := http.NewRequest(http.MethodPut, srv.URL+name, bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("If-Match", `"`+etag+`"`)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results <- put("update-from-client-" + string(rune('A'+i)))
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)

	var ok, failed int
	for code := range results {
		switch code {
		case http.StatusNoContent:
			ok++
		case http.StatusPreconditionFailed:
			failed++
		default:
			t.Fatalf("unexpected status %d", code)
		}
	}
	if ok != 1 || failed != 1 {
		t.Fatalf("expected exactly one success and one 412, got %d success / %d fail", ok, failed)
	}

	if got := fileContent(t, dir, name); got == "initial" {
		t.Fatal("winning request did not update the file")
	}
}

func TestLocalFileSystemCreateConditions(t *testing.T) {
	srv, dir, _ := newLocalTestServer(t)

	doPut := func(name string, body io.Reader, headers map[string]string) *http.Response {
		req, err := http.NewRequest(http.MethodPut, srv.URL+name, body)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}

	t.Run("wildcard create new succeeds", func(t *testing.T) {
		const name = "/new1.txt"
		resp := doPut(name, bytes.NewBufferString("hello"), map[string]string{"If-None-Match": "*"})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("expected 201, got %d", resp.StatusCode)
		}
		if got := fileContent(t, dir, name); got != "hello" {
			t.Fatalf("content = %q", got)
		}
	})

	t.Run("wildcard create existing fails without overwrite", func(t *testing.T) {
		const name = "/new2.txt"
		mustWriteFile(t, dir, name, "original")
		resp := doPut(name, bytes.NewBufferString("replacement"), map[string]string{"If-None-Match": "*"})
		if resp.StatusCode != http.StatusPreconditionFailed {
			t.Fatalf("expected 412, got %d", resp.StatusCode)
		}
		if got := fileContent(t, dir, name); got != "original" {
			t.Fatalf("failed request modified existing file: %q", got)
		}
	})

	t.Run("multi-tag list matches one current tag", func(t *testing.T) {
		const name = "/multi.txt"
		mustWriteFile(t, dir, name, "v0")
		c, err := NewClient(http.DefaultClient, srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		mfi, err := c.Stat(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		etag := mfi.ETag

		resp := doPut(name, bytes.NewBufferString("v1"), map[string]string{
			"If-Match": `"other1", "` + etag + `", "other2"`,
		})
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("expected 204, got %d", resp.StatusCode)
		}
	})

	t.Run("multi-tag list no match fails without partial resource", func(t *testing.T) {
		const name = "/multi-miss.txt"
		mustWriteFile(t, dir, name, "keep")
		resp := doPut(name, bytes.NewBufferString("data"), map[string]string{
			"If-Match": `"a", "b", "c"`,
		})
		if resp.StatusCode != http.StatusPreconditionFailed {
			t.Fatalf("expected 412, got %d", resp.StatusCode)
		}
		if got := fileContent(t, dir, name); got != "keep" {
			t.Fatalf("file changed after failed If-Match: %q", got)
		}

		// No temporary files must be left behind.
		matches, _ := filepath.Glob(filepath.Join(dir, ".webdav-tmp-*"))
		if len(matches) != 0 {
			t.Fatalf("temporary files left behind: %v", matches)
		}
	})

	t.Run("new resource with non-matching tag list is not created", func(t *testing.T) {
		const name = "/brand-new.txt"
		resp := doPut(name, bytes.NewBufferString("data"), map[string]string{
			"If-Match": `"anything"`,
		})
		if resp.StatusCode != http.StatusPreconditionFailed {
			t.Fatalf("expected 412 for If-Match on missing resource, got %d", resp.StatusCode)
		}
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("resource should not have been created, stat err=%v", err)
		}
	})
}

// A malformed conditional header must be rejected with 400 before the request
// body is read, even when the body is large.
func TestLocalFileSystemMalformedHeaderRejectsBeforeBody(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, dir, "/big.txt", "x")

	handler := &Handler{FileSystem: LocalFileSystem(dir)}

	for _, header := range []struct{ name, value string }{
		{"If-Match", `not-a-valid-etag`},
		{"If-Match", `"v1", `},
		{"If-None-Match", `W/without-quotes`},
	} {
		t.Run(header.name+"="+header.value, func(t *testing.T) {
			tracker := &trackingReader{data: make([]byte, 1<<16)}
			req := httptest.NewRequest(http.MethodPut, "/big.txt", tracker)
			req.Header.Set(header.name, header.value)
			w := httptest.NewRecorder()

			handler.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d", w.Code)
			}
			if tracker.read != 0 {
				t.Fatalf("handler consumed %d bytes of body before rejecting malformed header", tracker.read)
			}
			if got := fileContent(t, dir, "/big.txt"); got != "x" {
				t.Fatalf("existing file changed: %q", got)
			}
		})
	}
}

// A backend without atomic conditional capability surfaces a clear 501 path.
func TestUnsupportedConditionsRejectedWith501(t *testing.T) {
	srv := httptest.NewServer(&Handler{FileSystem: incapableFileSystem{}})
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodPut, srv.URL+"/x", bytes.NewBufferString("data"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("If-Match", `"v1"`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", resp.StatusCode)
	}
}

type trackingReader struct {
	data []byte
	pos  int
	read int
}

func (r *trackingReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	r.read += n
	return n, nil
}

// incapableFileSystem accepts unconditional writes but refuses conditional
// ones instead of performing a non-atomic stat-then-write emulation.
type incapableFileSystem struct{}

func (incapableFileSystem) Open(context.Context, string) (io.ReadCloser, error) {
	return nil, NewHTTPError(http.StatusNotFound, os.ErrNotExist)
}
func (incapableFileSystem) Stat(context.Context, string) (*FileInfo, error) {
	return nil, NewHTTPError(http.StatusNotFound, os.ErrNotExist)
}
func (incapableFileSystem) ReadDir(context.Context, string, bool) ([]FileInfo, error) {
	return nil, nil
}
func (b incapableFileSystem) Create(ctx context.Context, name string, body io.ReadCloser, opts *CreateOptions) (*FileInfo, bool, error) {
	if opts != nil && opts.Conditions.IsSet() {
		return nil, false, NewHTTPError(http.StatusNotImplemented, ErrConditionsNotSupported)
	}
	return &FileInfo{Path: name}, true, nil
}
func (incapableFileSystem) RemoveAll(context.Context, string, *RemoveAllOptions) error {
	return NewHTTPError(http.StatusNotImplemented, ErrConditionsNotSupported)
}
func (incapableFileSystem) Mkdir(context.Context, string) error { return nil }
func (incapableFileSystem) Copy(context.Context, string, string, *CopyOptions) (bool, error) {
	return false, nil
}
func (incapableFileSystem) Move(context.Context, string, string, *MoveOptions) (bool, error) {
	return false, nil
}
