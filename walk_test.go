package message

import (
	"io"
	"io/ioutil"
	"reflect"
	"strings"
	"testing"
)

const testEmbeddedInner = "Content-Type: multipart/alternative; boundary=INNER\r\n" +
	"\r\n" +
	"--INNER\r\n" +
	"Content-Type: text/plain\r\n" +
	"\r\n" +
	"Inner text\r\n" +
	"--INNER\r\n" +
	"Content-Type: text/html\r\n" +
	"\r\n" +
	"<p>Inner HTML</p>\r\n" +
	"--INNER--\r\n" +
	"Inner epilogue\r\n"

func testEmbeddedMessage(inner string) string {
	return "Mime-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=OUTER\r\n" +
		"\r\n" +
		"--OUTER\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"Outer text\r\n" +
		"--OUTER\r\n" +
		"Content-Type: message/rfc822\r\n" +
		"\r\n" +
		inner +
		"--OUTER\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"after.bin\"\r\n" +
		"\r\n" +
		"AFTER-ATTACHMENT\r\n" +
		"--OUTER--\r\n"
}

var testEmbeddedText = testEmbeddedMessage(testEmbeddedInner)

type testWalkEntityPart struct {
	path      WalkPath
	mediaType string
	body      string
	err       error
}

// walkEntityCollect visits the whole tree, reading leaf bodies completely.
func walkEntityCollect(t *testing.T, e *Entity, opts *WalkOptions) ([]testWalkEntityPart, error) {
	return walkEntityRead(t, e, opts, -1)
}

// walkEntityRead visits the whole tree, reading at most maxBody bytes from
// each leaf body (the whole body if maxBody < 0).
func walkEntityRead(t *testing.T, e *Entity, opts *WalkOptions, maxBody int64) ([]testWalkEntityPart, error) {
	t.Helper()
	var l []testWalkEntityPart
	err := e.WalkWithOptions(func(path WalkPath, part *Entity, err error) error {
		if err != nil {
			l = append(l, testWalkEntityPart{path: path, err: err})
			return nil
		}
		var body string
		if part.MultipartReader() == nil && part.Header.Get("Content-Type") != "message/rfc822" {
			r := part.Body
			if maxBody >= 0 {
				r = io.LimitReader(r, maxBody)
			}
			b, err := ioutil.ReadAll(r)
			if err != nil {
				return err
			}
			body = string(b)
		}
		mediaType, _, _ := part.Header.ContentType()
		l = append(l, testWalkEntityPart{
			path:      path,
			mediaType: mediaType,
			body:      body,
		})
		return nil
	}, opts)
	return l, err
}

func TestWalkWithOptions_embedded(t *testing.T) {
	e, err := Read(strings.NewReader(testEmbeddedText))
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}

	want := []testWalkEntityPart{
		{path: nil, mediaType: "multipart/mixed"},
		{path: WalkPath{{Index: 0}}, mediaType: "text/plain", body: "Outer text"},
		{path: WalkPath{{Index: 1}}, mediaType: "message/rfc822"},
		{path: WalkPath{{Index: 1}, {Index: -1, Embedded: true}}, mediaType: "multipart/alternative"},
		{path: WalkPath{{Index: 1}, {Index: -1, Embedded: true}, {Index: 0}}, mediaType: "text/plain", body: "Inner text"},
		{path: WalkPath{{Index: 1}, {Index: -1, Embedded: true}, {Index: 1}}, mediaType: "text/html", body: "<p>Inner HTML</p>"},
		{path: WalkPath{{Index: 2}}, mediaType: "application/octet-stream", body: "AFTER-ATTACHMENT"},
	}

	got, err := walkEntityCollect(t, e, &WalkOptions{EnterEmbedded: true})
	if err != nil {
		t.Fatalf("Entity.WalkWithOptions() = %v", err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("Entity.WalkWithOptions() =\n%#v\nbut want:\n%#v", got, want)
	}
}

// The default options must not enter embedded messages, matching Walk.
func TestWalkWithOptions_default(t *testing.T) {
	e, err := Read(strings.NewReader(testEmbeddedText))
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}

	got, err := walkEntityCollect(t, e, nil)
	if err != nil {
		t.Fatalf("Entity.WalkWithOptions() = %v", err)
	}

	for _, p := range got {
		for _, step := range p.path {
			if step.Embedded {
				t.Errorf("Entity.WalkWithOptions() entered an embedded message with default options: %#v", got)
			}
		}
	}
	if len(got) != 4 {
		t.Fatalf("Entity.WalkWithOptions() visited %v entities, want 4", len(got))
	}
	if got[3].mediaType != "application/octet-stream" || got[3].body != "AFTER-ATTACHMENT" {
		t.Errorf("Entity.WalkWithOptions() last part = %#v", got[3])
	}
}

// The old Walk must keep treating message/rfc822 parts as leaves.
func TestWalk_embeddedUnchanged(t *testing.T) {
	e, err := Read(strings.NewReader(testEmbeddedText))
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}

	var paths [][]int
	var embeddedBody string
	err = e.Walk(func(path []int, part *Entity, err error) error {
		if err != nil {
			return err
		}
		var pathCopy []int
		if len(path) > 0 {
			pathCopy = make([]int, len(path))
			copy(pathCopy, path)
		}
		paths = append(paths, pathCopy)
		if mediaType, _, _ := part.Header.ContentType(); mediaType == "message/rfc822" {
			b, err := ioutil.ReadAll(part.Body)
			if err != nil {
				return err
			}
			embeddedBody = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Entity.Walk() = %v", err)
	}

	wantPaths := [][]int{nil, {0}, {1}, {2}}
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Errorf("Entity.Walk() paths = %#v, want %#v", paths, wantPaths)
	}
	// The CRLF preceding the enclosing boundary is a separator and is not
	// part of the body.
	wantEmbeddedBody := strings.TrimSuffix(testEmbeddedInner, "\r\n")
	if embeddedBody != wantEmbeddedBody {
		t.Errorf("Entity.Walk() embedded body = %q, want %q", embeddedBody, wantEmbeddedBody)
	}
}

// A malformed embedded message must be reported to the walk function without
// losing the stream position of the enclosing multipart.
func TestWalkWithOptions_embeddedCorrupt(t *testing.T) {
	raw := testEmbeddedMessage("this is not a valid header\r\n" +
		"\r\n" +
		"Garbage embedded body\r\n")

	e, err := Read(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}

	got, err := walkEntityCollect(t, e, &WalkOptions{EnterEmbedded: true})
	if err != nil {
		t.Fatalf("Entity.WalkWithOptions() = %v", err)
	}

	var corrupt *testWalkEntityPart
	for i := range got {
		if got[i].err != nil {
			corrupt = &got[i]
		}
	}
	if corrupt == nil {
		t.Fatalf("Entity.WalkWithOptions() did not report the corrupt embedded message: %#v", got)
	}
	wantPath := WalkPath{{Index: 1}, {Index: -1, Embedded: true}}
	if !reflect.DeepEqual(corrupt.path, wantPath) {
		t.Errorf("corrupt embedded message path = %#v, want %#v", corrupt.path, wantPath)
	}

	last := got[len(got)-1]
	if last.mediaType != "application/octet-stream" || last.body != "AFTER-ATTACHMENT" {
		t.Errorf("Entity.WalkWithOptions() last part = %#v, stream position lost", last)
	}
}

// Not reading embedded message bodies completely must not lose the stream
// position of the enclosing multipart.
func TestWalkWithOptions_embeddedPartialRead(t *testing.T) {
	e, err := Read(strings.NewReader(testEmbeddedText))
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}

	got, err := walkEntityRead(t, e, &WalkOptions{EnterEmbedded: true}, 3)
	if err != nil {
		t.Fatalf("Entity.WalkWithOptions() = %v", err)
	}

	last := got[len(got)-1]
	if last.mediaType != "application/octet-stream" || last.body != "AFT" {
		t.Errorf("Entity.WalkWithOptions() last part = %#v", last)
	}

	// The attachment must still be fully readable afterwards: walk again
	// without reading leaf bodies at all.
	e, err = Read(strings.NewReader(testEmbeddedText))
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}
	var lastBody string
	err = e.WalkWithOptions(func(path WalkPath, part *Entity, err error) error {
		if err != nil {
			return err
		}
		if mediaType, _, _ := part.Header.ContentType(); mediaType == "application/octet-stream" {
			b, err := ioutil.ReadAll(part.Body)
			if err != nil {
				return err
			}
			lastBody = string(b)
		}
		return nil
	}, &WalkOptions{EnterEmbedded: true})
	if err != nil {
		t.Fatalf("Entity.WalkWithOptions() = %v", err)
	}
	if lastBody != "AFTER-ATTACHMENT" {
		t.Errorf("attachment body = %q, want %q, stream position lost", lastBody, "AFTER-ATTACHMENT")
	}
}

func TestWalkWithOptions_skipChildren(t *testing.T) {
	e, err := Read(strings.NewReader(testEmbeddedText))
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}

	var paths []WalkPath
	err = e.WalkWithOptions(func(path WalkPath, part *Entity, err error) error {
		if err != nil {
			return err
		}
		paths = append(paths, path)
		if mediaType, _, _ := part.Header.ContentType(); mediaType == "message/rfc822" {
			return SkipChildren
		}
		return nil
	}, &WalkOptions{EnterEmbedded: true})
	if err != nil {
		t.Fatalf("Entity.WalkWithOptions() = %v", err)
	}

	want := []WalkPath{
		nil,
		{{Index: 0}},
		{{Index: 1}},
		{{Index: 2}},
	}
	if !reflect.DeepEqual(paths, want) {
		t.Errorf("Entity.WalkWithOptions() paths = %#v, want %#v", paths, want)
	}
}

func TestWalkWithOptions_skipChildrenRoot(t *testing.T) {
	e, err := Read(strings.NewReader(testEmbeddedText))
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}

	n := 0
	err = e.WalkWithOptions(func(path WalkPath, part *Entity, err error) error {
		if err != nil {
			return err
		}
		n++
		return SkipChildren
	}, &WalkOptions{EnterEmbedded: true})
	if err != nil {
		t.Fatalf("Entity.WalkWithOptions() = %v", err)
	}
	if n != 1 {
		t.Errorf("Entity.WalkWithOptions() visited %v entities, want 1", n)
	}
}

func TestWalkWithOptions_maxDepth(t *testing.T) {
	e, err := Read(strings.NewReader(testEmbeddedText))
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}

	var paths []WalkPath
	err = e.WalkWithOptions(func(path WalkPath, part *Entity, err error) error {
		if err != nil {
			return err
		}
		paths = append(paths, path)
		return nil
	}, &WalkOptions{EnterEmbedded: true, MaxDepth: 2})
	if err != nil {
		t.Fatalf("Entity.WalkWithOptions() = %v", err)
	}

	want := []WalkPath{
		nil,
		{{Index: 0}},
		{{Index: 1}},
		{{Index: 1}, {Index: -1, Embedded: true}},
		{{Index: 2}},
	}
	if !reflect.DeepEqual(paths, want) {
		t.Errorf("Entity.WalkWithOptions() paths = %#v, want %#v", paths, want)
	}
}

func TestWalkWithOptions_maxBytes(t *testing.T) {
	e, err := Read(strings.NewReader(testEmbeddedText))
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}

	_, err = walkEntityCollect(t, e, &WalkOptions{EnterEmbedded: true, MaxBytes: 10})
	if err != errWalkTooBig {
		t.Fatalf("Entity.WalkWithOptions() = %v, want %v", err, errWalkTooBig)
	}
}
