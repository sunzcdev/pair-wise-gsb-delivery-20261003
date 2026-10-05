package fasthttp

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newMultiRangeTestHandler(t *testing.T, name string, content []byte) RequestHandler {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
		t.Fatalf("cannot create test file: %v", err)
	}

	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })

	fs := &FS{
		Root:            dir,
		AcceptByteRange: true,
		CleanStop:       stop,
	}
	return fs.NewRequestHandler()
}

func doRangeRequest(t *testing.T, h RequestHandler, uri, rangeHeader string, headers map[string]string, head bool) *Response {
	t.Helper()

	var ctx RequestCtx
	ctx.Init(&Request{}, nil, nil)
	ctx.Request.SetRequestURI(uri)
	if head {
		ctx.Request.Header.SetMethod(MethodHead)
	}
	if rangeHeader != "" {
		ctx.Request.Header.Set(HeaderRange, rangeHeader)
	}
	for k, v := range headers {
		ctx.Request.Header.Set(k, v)
	}
	h(&ctx)

	resp := &Response{}
	resp.SkipBody = head
	s := ctx.Response.String()
	br := bufio.NewReader(bytes.NewBufferString(s))
	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error reading response: %v", err)
	}
	return resp
}

type multiRangePart struct {
	startPos int
	endPos   int
	body     []byte
}

func parseMultipartResponse(t *testing.T, resp *Response, fileSize int) []multiRangePart {
	t.Helper()

	contentType := resp.Header.ContentType()
	mediaType, params, err := mime.ParseMediaType(string(contentType))
	if err != nil {
		t.Fatalf("cannot parse response content type %q: %v", contentType, err)
	}
	if mediaType != "multipart/byteranges" {
		t.Fatalf("unexpected media type %q. Expecting multipart/byteranges", mediaType)
	}
	boundary := params["boundary"]
	if boundary == "" {
		t.Fatalf("missing boundary in content type %q", contentType)
	}

	mr := multipart.NewReader(bytes.NewReader(resp.Body()), boundary)
	var parts []multiRangePart
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("cannot read multipart part: %v", err)
		}
		body, err := io.ReadAll(p)
		if err != nil {
			t.Fatalf("cannot read part body: %v", err)
		}
		var startPos, endPos, size int
		cr := p.Header.Get(HeaderContentRange)
		if _, err := fmt.Sscanf(cr, "bytes %d-%d/%d", &startPos, &endPos, &size); err != nil {
			t.Fatalf("cannot parse part Content-Range %q: %v", cr, err)
		}
		if size != fileSize {
			t.Fatalf("unexpected part Content-Range size %d. Expecting %d", size, fileSize)
		}
		if endPos-startPos+1 != len(body) {
			t.Fatalf("part body size %d doesn't match Content-Range %q", len(body), cr)
		}
		if p.Header.Get(HeaderContentType) == "" {
			t.Fatalf("missing part Content-Type for range %q", cr)
		}
		parts = append(parts, multiRangePart{startPos: startPos, endPos: endPos, body: body})
	}
	if len(parts) == 0 {
		t.Fatalf("no parts found in multipart response")
	}
	return parts
}

func checkPartsAgainstContent(t *testing.T, parts []multiRangePart, content []byte, expected [][2]int) {
	t.Helper()

	if len(parts) != len(expected) {
		t.Fatalf("unexpected parts count %d. Expecting %d", len(parts), len(expected))
	}
	for i, p := range parts {
		if p.startPos != expected[i][0] || p.endPos != expected[i][1] {
			t.Fatalf("part %d: unexpected range %d-%d. Expecting %d-%d",
				i, p.startPos, p.endPos, expected[i][0], expected[i][1])
		}
		expectedBody := content[p.startPos : p.endPos+1]
		if !bytes.Equal(p.body, expectedBody) {
			t.Fatalf("part %d: unexpected body %q. Expecting %q", i, p.body, expectedBody)
		}
	}
}

func makePatternContent(size int) []byte {
	content := make([]byte, size)
	for i := range content {
		content[i] = byte(i*31 + 7)
	}
	return content
}

func TestFSMultiByteRange(t *testing.T) {
	t.Parallel()

	content := makePatternContent(1000)
	h := newMultiRangeTestHandler(t, "file.txt", content)

	resp := doRangeRequest(t, h, "/file.txt", "bytes=100-199, 400-499, -50, 800-899", nil, false)
	if resp.StatusCode() != StatusPartialContent {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusPartialContent)
	}
	if resp.Header.ContentLength() != len(resp.Body()) {
		t.Fatalf("unexpected Content-Length %d. Expecting %d", resp.Header.ContentLength(), len(resp.Body()))
	}

	parts := parseMultipartResponse(t, resp, len(content))
	checkPartsAgainstContent(t, parts, content, [][2]int{
		{100, 199},
		{400, 499},
		{800, 899},
		{950, 999},
	})
}

func TestFSMultiByteRangeBigFile(t *testing.T) {
	t.Parallel()

	// Bigger than maxSmallFileSize, so the big file (sendfile) reader is used.
	content := makePatternContent(100 * 1024)
	h := newMultiRangeTestHandler(t, "big.bin", content)

	resp := doRangeRequest(t, h, "/big.bin", "bytes=0-999, 50000-50999, -2000, 90000-90999", nil, false)
	if resp.StatusCode() != StatusPartialContent {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusPartialContent)
	}
	if resp.Header.ContentLength() != len(resp.Body()) {
		t.Fatalf("unexpected Content-Length %d. Expecting %d", resp.Header.ContentLength(), len(resp.Body()))
	}

	size := len(content)
	parts := parseMultipartResponse(t, resp, size)
	checkPartsAgainstContent(t, parts, content, [][2]int{
		{0, 999},
		{50000, 50999},
		{90000, 90999},
		{size - 2000, size - 1},
	})
}

func TestFSMultiByteRangeOverlappingMerge(t *testing.T) {
	t.Parallel()

	content := makePatternContent(300)
	h := newMultiRangeTestHandler(t, "file.txt", content)

	// Overlapping and adjacent ranges are merged into a single range served
	// via the simple (non-multipart) path.
	resp := doRangeRequest(t, h, "/file.txt", "bytes=50-150, 0-100, 140-200", nil, false)
	if resp.StatusCode() != StatusPartialContent {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusPartialContent)
	}
	expectedCR := "bytes 0-200/300"
	if cr := string(resp.Header.Peek(HeaderContentRange)); cr != expectedCR {
		t.Fatalf("unexpected content-range %q. Expecting %q", cr, expectedCR)
	}
	if bytes.Contains(resp.Header.ContentType(), []byte("multipart")) {
		t.Fatalf("unexpected multipart content type %q for merged single range", resp.Header.ContentType())
	}
	if !bytes.Equal(resp.Body(), content[0:201]) {
		t.Fatalf("unexpected body %q. Expecting %q", resp.Body(), content[0:201])
	}
}

func TestFSMultiByteRangePartialOverlapMerge(t *testing.T) {
	t.Parallel()

	content := makePatternContent(1000)
	h := newMultiRangeTestHandler(t, "file.txt", content)

	// The first two ranges overlap and are merged; the third one stays apart.
	resp := doRangeRequest(t, h, "/file.txt", "bytes=0-100, 50-160, 500-600", nil, false)
	if resp.StatusCode() != StatusPartialContent {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusPartialContent)
	}
	parts := parseMultipartResponse(t, resp, len(content))
	checkPartsAgainstContent(t, parts, content, [][2]int{
		{0, 160},
		{500, 600},
	})
}

func TestFSMultiByteRangeUnsatisfiableSkipped(t *testing.T) {
	t.Parallel()

	content := makePatternContent(300)
	h := newMultiRangeTestHandler(t, "file.txt", content)

	// Unsatisfiable ranges are skipped; the satisfiable ones are served.
	resp := doRangeRequest(t, h, "/file.txt", "bytes=10000-20000, 10-20, 5000-", nil, false)
	if resp.StatusCode() != StatusPartialContent {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusPartialContent)
	}
	expectedCR := "bytes 10-20/300"
	if cr := string(resp.Header.Peek(HeaderContentRange)); cr != expectedCR {
		t.Fatalf("unexpected content-range %q. Expecting %q", cr, expectedCR)
	}
	if !bytes.Equal(resp.Body(), content[10:21]) {
		t.Fatalf("unexpected body %q. Expecting %q", resp.Body(), content[10:21])
	}
}

func TestFSMultiByteRangeNotSatisfiable(t *testing.T) {
	t.Parallel()

	content := makePatternContent(300)
	h := newMultiRangeTestHandler(t, "file.txt", content)

	for _, rangeHeader := range []string{
		"bytes=10000-20000",
		"bytes=10000-20000, 30000-",
		"bytes=500-400",
		"bytes=-0",
		"bytes=foobar",
		"bytes=10",
		"items=0-10",
	} {
		resp := doRangeRequest(t, h, "/file.txt", rangeHeader, nil, false)
		if resp.StatusCode() != StatusRequestedRangeNotSatisfiable {
			t.Fatalf("unexpected status code: %d. Expecting %d. range=%q",
				resp.StatusCode(), StatusRequestedRangeNotSatisfiable, rangeHeader)
		}
		expectedCR := "bytes */300"
		if cr := string(resp.Header.Peek(HeaderContentRange)); cr != expectedCR {
			t.Fatalf("unexpected content-range %q. Expecting %q. range=%q", cr, expectedCR, rangeHeader)
		}
	}
}

func TestFSMultiByteRangeOverflow(t *testing.T) {
	t.Parallel()

	content := makePatternContent(300)
	h := newMultiRangeTestHandler(t, "file.txt", content)

	// Numbers overflowing int must be rejected, not wrapped around.
	unsatisfiable := []string{
		"bytes=0-99999999999999999999999999",
		"bytes=99999999999999999999999999-",
		"bytes=-99999999999999999999999999",
		"bytes=0-18446744073709551616",
		"bytes=18446744073709551615-",
		"bytes=9223372036854775807-",
		"bytes=9223372036854775806-9223372036854775807",
		"bytes=0-10, 99999999999999999999999999-",
	}
	for _, rangeHeader := range unsatisfiable {
		resp := doRangeRequest(t, h, "/file.txt", rangeHeader, nil, false)
		if resp.StatusCode() != StatusRequestedRangeNotSatisfiable {
			t.Fatalf("unexpected status code: %d. Expecting %d. range=%q",
				resp.StatusCode(), StatusRequestedRangeNotSatisfiable, rangeHeader)
		}
		expectedCR := "bytes */300"
		if cr := string(resp.Header.Peek(HeaderContentRange)); cr != expectedCR {
			t.Fatalf("unexpected content-range %q. Expecting %q. range=%q", cr, expectedCR, rangeHeader)
		}
	}

	// A large but representable end position is clamped to the content size.
	resp := doRangeRequest(t, h, "/file.txt", "bytes=0-2000000000", nil, false)
	if resp.StatusCode() != StatusPartialContent {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusPartialContent)
	}
	expectedCR := "bytes 0-299/300"
	if cr := string(resp.Header.Peek(HeaderContentRange)); cr != expectedCR {
		t.Fatalf("unexpected content-range %q. Expecting %q", cr, expectedCR)
	}
	if !bytes.Equal(resp.Body(), content) {
		t.Fatalf("unexpected body size %d. Expecting %d", len(resp.Body()), len(content))
	}

	// A suffix longer than the content is clamped to the whole content.
	resp = doRangeRequest(t, h, "/file.txt", "bytes=-2000000000", nil, false)
	if resp.StatusCode() != StatusPartialContent {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusPartialContent)
	}
	if cr := string(resp.Header.Peek(HeaderContentRange)); cr != expectedCR {
		t.Fatalf("unexpected content-range %q. Expecting %q", cr, expectedCR)
	}
	if !bytes.Equal(resp.Body(), content) {
		t.Fatalf("unexpected body size %d. Expecting %d", len(resp.Body()), len(content))
	}
}

func TestFSMultiByteRangeTooMany(t *testing.T) {
	t.Parallel()

	content := makePatternContent(3000)
	h := newMultiRangeTestHandler(t, "file.txt", content)

	// More than maxByteRangeCount ranges: the Range header is ignored and
	// the whole file is served.
	var specs []string
	for i := 0; i < maxByteRangeCount+1; i++ {
		specs = append(specs, fmt.Sprintf("%d-%d", i*10, i*10+5))
	}
	resp := doRangeRequest(t, h, "/file.txt", "bytes="+strings.Join(specs, ", "), nil, false)
	if resp.StatusCode() != StatusOK {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusOK)
	}
	if !bytes.Equal(resp.Body(), content) {
		t.Fatalf("unexpected body size %d. Expecting %d", len(resp.Body()), len(content))
	}

	// Exactly maxByteRangeCount ranges are still served as multipart.
	resp = doRangeRequest(t, h, "/file.txt", "bytes="+strings.Join(specs[:maxByteRangeCount], ", "), nil, false)
	if resp.StatusCode() != StatusPartialContent {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusPartialContent)
	}
	parts := parseMultipartResponse(t, resp, len(content))
	if len(parts) != maxByteRangeCount {
		t.Fatalf("unexpected parts count %d. Expecting %d", len(parts), maxByteRangeCount)
	}
	for i, p := range parts {
		expectedBody := content[i*10 : i*10+6]
		if !bytes.Equal(p.body, expectedBody) {
			t.Fatalf("part %d: unexpected body %q. Expecting %q", i, p.body, expectedBody)
		}
	}
}

func TestFSMultiByteRangeOverheadFallback(t *testing.T) {
	t.Parallel()

	content := makePatternContent(300)
	h := newMultiRangeTestHandler(t, "file.txt", content)

	// The ranges cover nearly the whole file, so the multipart overhead
	// outweighs the savings and the whole file is served instead.
	resp := doRangeRequest(t, h, "/file.txt", "bytes=0-139, 150-299", nil, false)
	if resp.StatusCode() != StatusOK {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusOK)
	}
	if !bytes.Equal(resp.Body(), content) {
		t.Fatalf("unexpected body size %d. Expecting %d", len(resp.Body()), len(content))
	}
}

func TestFSMultiByteRangeHead(t *testing.T) {
	t.Parallel()

	content := makePatternContent(1000)
	h := newMultiRangeTestHandler(t, "file.txt", content)

	rangeHeader := "bytes=100-199, 400-499, -50"
	getResp := doRangeRequest(t, h, "/file.txt", rangeHeader, nil, false)
	if getResp.StatusCode() != StatusPartialContent {
		t.Fatalf("unexpected status code: %d. Expecting %d", getResp.StatusCode(), StatusPartialContent)
	}

	headResp := doRangeRequest(t, h, "/file.txt", rangeHeader, nil, true)
	if headResp.StatusCode() != StatusPartialContent {
		t.Fatalf("unexpected status code: %d. Expecting %d", headResp.StatusCode(), StatusPartialContent)
	}
	if headResp.Header.ContentLength() != len(getResp.Body()) {
		t.Fatalf("unexpected HEAD Content-Length %d. Expecting %d",
			headResp.Header.ContentLength(), len(getResp.Body()))
	}
	if ct := headResp.Header.ContentType(); !bytes.Contains(ct, []byte("multipart/byteranges")) {
		t.Fatalf("unexpected HEAD content type %q. Expecting multipart/byteranges", ct)
	}
	if len(headResp.Body()) != 0 {
		t.Fatalf("unexpected HEAD body size %d. Expecting 0", len(headResp.Body()))
	}
}

func TestFSMultiByteRangeIfRange(t *testing.T) {
	t.Parallel()

	content := makePatternContent(1000)
	h := newMultiRangeTestHandler(t, "file.txt", content)

	rangeHeader := "bytes=100-199, 400-499"

	// Obtain the Last-Modified validator with a plain request.
	resp := doRangeRequest(t, h, "/file.txt", "", nil, false)
	lastModified := string(resp.Header.Peek(HeaderLastModified))
	if lastModified == "" {
		t.Fatalf("missing Last-Modified header")
	}

	// Matching If-Range: the ranged response is sent.
	resp = doRangeRequest(t, h, "/file.txt", rangeHeader, map[string]string{HeaderIfRange: lastModified}, false)
	if resp.StatusCode() != StatusPartialContent {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusPartialContent)
	}
	parts := parseMultipartResponse(t, resp, len(content))
	checkPartsAgainstContent(t, parts, content, [][2]int{
		{100, 199},
		{400, 499},
	})

	// Stale If-Range date: the whole file is sent.
	staleHeaders := map[string]string{HeaderIfRange: "Mon, 02 Jan 2006 15:04:05 GMT"}
	resp = doRangeRequest(t, h, "/file.txt", rangeHeader, staleHeaders, false)
	if resp.StatusCode() != StatusOK {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusOK)
	}
	if !bytes.Equal(resp.Body(), content) {
		t.Fatalf("unexpected body size %d. Expecting %d", len(resp.Body()), len(content))
	}

	// If-Range with an entity tag cannot match: the whole file is sent.
	etagHeaders := map[string]string{HeaderIfRange: `"some-etag"`}
	resp = doRangeRequest(t, h, "/file.txt", rangeHeader, etagHeaders, false)
	if resp.StatusCode() != StatusOK {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusOK)
	}
	if !bytes.Equal(resp.Body(), content) {
		t.Fatalf("unexpected body size %d. Expecting %d", len(resp.Body()), len(content))
	}
}
