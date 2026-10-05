package fasthttp

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

func TestParseByteRangesSuccess(t *testing.T) {
	t.Parallel()

	type expectedRange struct {
		start int
		end   int
	}

	testCases := []struct {
		name          string
		header        string
		contentLength int
		ranges        []expectedRange
	}{
		{
			name:          "single explicit range",
			header:        "bytes=1234-6789",
			contentLength: 10000,
			ranges:        []expectedRange{{1234, 6789}},
		},
		{
			name:          "single open-ended range",
			header:        "bytes=123-",
			contentLength: 456,
			ranges:        []expectedRange{{123, 455}},
		},
		{
			name:          "single suffix range",
			header:        "bytes=-123",
			contentLength: 456,
			ranges:        []expectedRange{{333, 455}},
		},
		{
			name:          "suffix larger than the file returns the whole file",
			header:        "bytes=-567",
			contentLength: 56,
			ranges:        []expectedRange{{0, 55}},
		},
		{
			name:          "multiple disjoint ranges keep request order sorted",
			header:        "bytes=10-20,30-40,0-5",
			contentLength: 100,
			ranges:        []expectedRange{{0, 5}, {10, 20}, {30, 40}},
		},
		{
			name:          "overlapping ranges are merged",
			header:        "bytes=0-10,5-15,14-30",
			contentLength: 100,
			ranges:        []expectedRange{{0, 30}},
		},
		{
			name:          "nested ranges are merged",
			header:        "bytes=100-200,120-180",
			contentLength: 300,
			ranges:        []expectedRange{{100, 200}},
		},
		{
			name:          "adjacent ranges are merged",
			header:        "bytes=0-9,10-19",
			contentLength: 100,
			ranges:        []expectedRange{{0, 19}},
		},
		{
			name:          "duplicate ranges are merged",
			header:        "bytes=5-9,5-9,5-9",
			contentLength: 100,
			ranges:        []expectedRange{{5, 9}},
		},
		{
			name:          "end beyond content is clipped",
			header:        "bytes=90-1000",
			contentLength: 100,
			ranges:        []expectedRange{{90, 99}},
		},
		{
			name:          "last byte",
			header:        "bytes=-1",
			contentLength: 10,
			ranges:        []expectedRange{{9, 9}},
		},
		{
			name:          "first byte",
			header:        "bytes=0-0",
			contentLength: 10,
			ranges:        []expectedRange{{0, 0}},
		},
		{
			name:          "whitespace around specs is tolerated",
			header:        "bytes=0-4, 10-14 ,	20-24",
			contentLength: 100,
			ranges:        []expectedRange{{0, 4}, {10, 14}, {20, 24}},
		},
		{
			name:          "fully and partially out of range specs",
			header:        "bytes=100-200,0-4",
			contentLength: 50,
			ranges:        []expectedRange{{0, 4}},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ranges, err := ParseByteRanges([]byte(tc.header), tc.contentLength, maxByteRanges)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(ranges) != len(tc.ranges) {
				t.Fatalf("unexpected ranges count: %d. Expecting %d. ranges=%v", len(ranges), len(tc.ranges), ranges)
			}
			for i, expected := range tc.ranges {
				if ranges[i].Start != expected.start || ranges[i].End != expected.end {
					t.Fatalf("unexpected range #%d: %d-%d. Expecting %d-%d", i, ranges[i].Start, ranges[i].End, expected.start, expected.end)
				}
			}
		})
	}
}

func TestParseByteRangesOverflow(t *testing.T) {
	t.Parallel()

	// Values at and beyond the integer limits must saturate instead of
	// wrapping around on either 32-bit or 64-bit builds.
	maxInt := uint64(math.MaxInt64)
	if strconv.IntSize == 32 {
		maxInt = math.MaxInt32
	}

	testCases := []struct {
		name   string
		header string
		start  int
		end    int
	}{
		{
			name:   "start position at MaxInt64 is out of range but suffix range still applies",
			header: fmt.Sprintf("bytes=%d-,-10", uint64(math.MaxInt64)),
			start:  90,
			end:    99,
		},
		{
			name:   "end position overflows but is clipped to content end",
			header: fmt.Sprintf("bytes=0-%d", uint64(math.MaxInt64)),
			start:  0,
			end:    99,
		},
		{
			name:   "start overflows uint64 digits, open end, merged to whole file",
			header: "bytes=99999999999999999999999999-,0-99999999999999999999999999",
			start:  0,
			end:    99,
		},
		{
			name:   "suffix range with saturated length returns whole file",
			header: "bytes=-99999999999999999999999999",
			start:  0,
			end:    99,
		},
		{
			name:   "huge range touching max build int",
			header: fmt.Sprintf("bytes=%d-%d", maxInt, maxInt),
			start:  -1, // no intersecting ranges expected
		},
		{
			name:   "start equals MaxInt64-1 open end intersects nothing at size 100",
			header: "bytes=9223372036854775806-",
			start:  -1,
		},
		{
			name:   "suffix 100 on file of 100 returns whole file",
			header: "bytes=-100",
			start:  0,
			end:    99,
		},
		{
			name:   "suffix 101 on file of 100 returns whole file",
			header: "bytes=-101",
			start:  0,
			end:    99,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ranges, err := ParseByteRanges([]byte(tc.header), 100, maxByteRanges)
			if tc.start < 0 {
				if err == nil {
					t.Fatalf("expecting error for non-intersecting range, got ranges=%v", ranges)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(ranges) != 1 || ranges[0].Start != tc.start || ranges[0].End != tc.end {
				t.Fatalf("unexpected ranges: %+v. Expecting %d-%d", ranges, tc.start, tc.end)
			}
		})
	}
}

func TestParseByteRangesError(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name          string
		header        string
		contentLength int
	}{
		{name: "unsupported units", header: "foobar=1-34", contentLength: 600},
		{name: "missing equals", header: "bytes 1-2", contentLength: 10},
		{name: "missing dash", header: "bytes=1234", contentLength: 1235},
		{name: "empty specs", header: "bytes=", contentLength: 1235},
		{name: "non-numeric start", header: "bytes=foobar", contentLength: 123},
		{name: "non-numeric end", header: "bytes=1-foobar", contentLength: 123},
		{name: "double dash", header: "bytes=1--3", contentLength: 123},
		{name: "empty spec", header: "bytes=1-2,,3-4", contentLength: 123},
		{name: "start beyond content", header: "bytes=123-", contentLength: 12},
		{name: "all specs beyond content", header: "bytes=100-200,300-400", contentLength: 12},
		{name: "suffix on empty content", header: "bytes=-5", contentLength: 0},
		{name: "explicit range on empty content", header: "bytes=0-0", contentLength: 0},
		{name: "zero length suffix", header: "bytes=-0", contentLength: 100},
		{name: "reversed range", header: "bytes=123-34", contentLength: 1234},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, err := ParseByteRanges([]byte(tc.header), tc.contentLength, maxByteRanges); err == nil {
				t.Fatalf("expecting error when parsing byte range %q", tc.header)
			}
		})
	}
}

func TestParseByteRangesLimit(t *testing.T) {
	t.Parallel()

	if _, err := ParseByteRanges([]byte("bytes=0-0,1-1,2-2"), 100, 2); err == nil {
		t.Fatalf("expecting error when range count exceeds the limit")
	}

	// Negative maxRanges disables the limit.
	ranges, err := ParseByteRanges([]byte("bytes=0-0,2-2,4-4,6-6"), 100, -1)
	if err != nil {
		t.Fatalf("unexpected error with disabled limit: %v", err)
	}
	if len(ranges) != 4 {
		t.Fatalf("unexpected ranges count: %d, expecting 4", len(ranges))
	}

	// The default limit of maxByteRanges is enforced and the saturated
	// adjacent merge cannot let a request emit more parts than allowed.
	headerParts := make([]string, maxByteRanges+1)
	for i := range headerParts {
		headerParts[i] = fmt.Sprintf("%d-%d", i*2, i*2)
	}
	header := "bytes=" + strings.Join(headerParts, ",")
	if _, err := ParseByteRanges([]byte(header), (maxByteRanges+1)*2, maxByteRanges); err == nil {
		t.Fatalf("expecting error for more than %d ranges", maxByteRanges)
	}
}

func TestParseByteRangesDefaultLimitIntegrated(t *testing.T) {
	t.Parallel()

	// Enough file room so the limit, not unsatisfiability, triggers the
	// rejection.
	parts := make([]string, maxByteRanges+1)
	for i := range parts {
		parts[i] = fmt.Sprintf("%d-%d", i*2, i*2)
	}
	h, _, uri := setupMultirangeTestFile(t, (maxByteRanges+1)*2, &FS{})
	resp := doRangeRequest(t, h, uri, "bytes="+strings.Join(parts, ","), false)
	if resp.StatusCode() != StatusRequestedRangeNotSatisfiable {
		t.Fatalf("unexpected status: %d. Expecting %d", resp.StatusCode(), StatusRequestedRangeNotSatisfiable)
	}
}

// parseMultipartByteranges parses a multipart/byteranges response body with
// the standard library reader and returns each part's Content-Range header
// and payload, in wire order.
func parseMultipartByteranges(t *testing.T, contentType string, body []byte) []multipartPart {
	t.Helper()

	const prefix = "multipart/byteranges; boundary="
	if !strings.HasPrefix(contentType, prefix) {
		t.Fatalf("unexpected content type %q. Expecting prefix %q", contentType, prefix)
	}
	boundary := strings.TrimPrefix(contentType, prefix)

	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var parts []multipartPart
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("unexpected error while reading multipart part: %v", err)
		}
		payload, err := io.ReadAll(part)
		if err != nil {
			t.Fatalf("unexpected error while reading part payload: %v", err)
		}
		parts = append(parts, multipartPart{
			contentRange: part.Header.Get(textproto.CanonicalMIMEHeaderKey(HeaderContentRange)),
			contentType:  part.Header.Get(textproto.CanonicalMIMEHeaderKey(HeaderContentType)),
			payload:      payload,
		})
	}
	if len(parts) == 0 {
		t.Fatalf("multipart response contained no parts")
	}
	return parts
}

type multipartPart struct {
	contentRange string
	contentType  string
	payload      []byte
}

func setupMultirangeTestFile(t *testing.T, size int, fsConfig *FS) (RequestHandler, []byte, string) {
	t.Helper()

	dir := t.TempDir()
	content := make([]byte, size)
	for i := range content {
		content[i] = byte(i % 251)
	}
	name := "ranges.bin"
	if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
		t.Fatalf("cannot create test file: %v", err)
	}

	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })

	fsConfig.Root = dir
	fsConfig.AcceptByteRange = true
	fsConfig.SkipCache = true
	fsConfig.CleanStop = stop

	return fsConfig.NewRequestHandler(), content, "/" + name
}

func doRangeRequest(t *testing.T, h RequestHandler, uri, rangeHeader string, head bool) *Response {
	t.Helper()

	var ctx RequestCtx
	ctx.Init(&Request{}, nil, nil)
	ctx.Request.SetRequestURI(uri)
	if rangeHeader != "" {
		ctx.Request.Header.Set(HeaderRange, rangeHeader)
	}
	ctx.Request.Header.SetMethod("GET")
	if head {
		ctx.Request.Header.SetMethod("HEAD")
	}
	h(&ctx)
	return readResponseFromCtx(t, &ctx, head)
}

func assertPartContent(t *testing.T, part multipartPart, file []byte, start, end, size int) {
	t.Helper()

	expectedRange := fmt.Sprintf("bytes %d-%d/%d", start, end, size)
	if part.contentRange != expectedRange {
		t.Fatalf("unexpected part Content-Range %q. Expecting %q", part.contentRange, expectedRange)
	}
	if !bytes.Equal(part.payload, file[start:end+1]) {
		t.Fatalf("part payload mismatch for range %d-%d: got %d bytes, expected %d",
			start, end, len(part.payload), end-start+1)
	}
}

func TestFSByteRangesMultipartSmallFile(t *testing.T) {
	t.Parallel()

	h, file, uri := setupMultirangeTestFile(t, 500, &FS{})
	size := len(file)

	resp := doRangeRequest(t, h, uri, "bytes=0-9,100-119,495-", false)
	if resp.StatusCode() != StatusPartialContent {
		t.Fatalf("unexpected status: %d. Expecting %d", resp.StatusCode(), StatusPartialContent)
	}

	contentType := string(resp.Header.ContentType())
	body := resp.Body()
	if int(resp.Header.ContentLength()) != len(body) {
		t.Fatalf("content-length %d does not match body length %d", resp.Header.ContentLength(), len(body))
	}

	parts := parseMultipartByteranges(t, contentType, body)
	if len(parts) != 3 {
		t.Fatalf("unexpected parts count: %d. Expecting 3", len(parts))
	}
	assertPartContent(t, parts[0], file, 0, 9, size)
	assertPartContent(t, parts[1], file, 100, 119, size)
	assertPartContent(t, parts[2], file, 495, 499, size)

	// Multipart responses carry no top-level Content-Range.
	if len(resp.Header.Peek(HeaderContentRange)) != 0 {
		t.Fatalf("multipart response must not carry a top-level Content-Range header")
	}
}

func TestFSByteRangesMultipartOverlapNormalized(t *testing.T) {
	t.Parallel()

	h, file, uri := setupMultirangeTestFile(t, 300, &FS{})
	size := len(file)

	// Overlapping and adjacent ranges collapse into a single part, and a
	// server MUST use the simple single-range path for exactly one range.
	resp := doRangeRequest(t, h, uri, "bytes=10-100,50-80,101-150", false)
	if resp.StatusCode() != StatusPartialContent {
		t.Fatalf("unexpected status: %d", resp.StatusCode())
	}
	if ct := string(resp.Header.ContentType()); strings.HasPrefix(ct, "multipart/") {
		t.Fatalf("merged single range must not use multipart encoding, got %q", ct)
	}
	expectedCR := fmt.Sprintf("bytes %d-%d/%d", 10, 150, size)
	if cr := string(resp.Header.Peek(HeaderContentRange)); cr != expectedCR {
		t.Fatalf("unexpected content-range %q. Expecting %q", cr, expectedCR)
	}
	if !bytes.Equal(resp.Body(), file[10:151]) {
		t.Fatalf("merged range payload mismatch")
	}
}

func TestFSByteRangesMultipartBigFileStreamed(t *testing.T) {
	t.Parallel()

	// Bigger than maxSmallFileSize so each part is served through
	// bigFileReader (seeking + sendfile-capable streaming).
	h, file, uri := setupMultirangeTestFile(t, 3*maxSmallFileSize+512, &FS{})
	size := len(file)

	cases := [][2]int{
		{0, 7},
		{maxSmallFileSize + 10, maxSmallFileSize + 99},
		{size - 3, size - 1},
	}
	rangeHeader := fmt.Sprintf("bytes=%d-%d,%d-%d,%d-%d",
		cases[0][0], cases[0][1], cases[1][0], cases[1][1], cases[2][0], cases[2][1])

	resp := doRangeRequest(t, h, uri, rangeHeader, false)
	if resp.StatusCode() != StatusPartialContent {
		t.Fatalf("unexpected status: %d", resp.StatusCode())
	}
	if int(resp.Header.ContentLength()) != len(resp.Body()) {
		t.Fatalf("content-length %d does not match body length %d", resp.Header.ContentLength(), len(resp.Body()))
	}

	parts := parseMultipartByteranges(t, string(resp.Header.ContentType()), resp.Body())
	if len(parts) != len(cases) {
		t.Fatalf("unexpected parts count: %d. Expecting %d", len(parts), len(cases))
	}
	for i, c := range cases {
		assertPartContent(t, parts[i], file, c[0], c[1], size)
	}
}

func TestFSByteRangesBigFileMultipleRequests(t *testing.T) {
	// Each request opens a fresh bigFileReader, which is seeked once per
	// part. Running several requests back to back exercises repeated seeks
	// across real file handles.
	t.Parallel()

	dir := t.TempDir()
	size := maxSmallFileSize*2 + 1000
	file := make([]byte, size)
	for i := range file {
		file[i] = byte((i * 37) % 251)
	}
	name := "big-ranges.bin"
	if err := os.WriteFile(filepath.Join(dir, name), file, 0o600); err != nil {
		t.Fatalf("cannot create test file: %v", err)
	}

	stop := make(chan struct{})
	fs := &FS{
		Root:            dir,
		AcceptByteRange: true,
		SkipCache:       true,
		CleanStop:       stop,
	}
	h := fs.NewRequestHandler()

	requests := []string{
		"bytes=0-99,5000-5099",
		"bytes=10000-10099,8000-8050,15000-",
		"bytes=-5000,-100",
	}
	for _, rangeHeader := range requests {
		resp := doRangeRequest(t, h, "/"+name, rangeHeader, false)
		if resp.StatusCode() != StatusPartialContent {
			t.Fatalf("unexpected status for %q: %d", rangeHeader, resp.StatusCode())
		}
		ranges, err := ParseByteRanges([]byte(rangeHeader), size, maxByteRanges)
		if err != nil {
			t.Fatalf("unexpected parse error for %q: %v", rangeHeader, err)
		}
		if len(ranges) == 1 {
			if !bytes.Equal(resp.Body(), file[ranges[0].Start:ranges[0].End+1]) {
				t.Fatalf("simple path payload mismatch for %q", rangeHeader)
			}
			continue
		}

		parts := parseMultipartByteranges(t, string(resp.Header.ContentType()), resp.Body())
		if len(parts) != len(ranges) {
			t.Fatalf("unexpected parts count for %q: %d, expecting %d", rangeHeader, len(parts), len(ranges))
		}
		for i, br := range ranges {
			assertPartContent(t, parts[i], file, br.Start, br.End, size)
		}
	}

	close(stop)
}

func TestFSByteRangesHEAD(t *testing.T) {
	t.Parallel()

	h, _, uri := setupMultirangeTestFile(t, 4096, &FS{})

	getResp := doRangeRequest(t, h, uri, "bytes=0-9,100-109", false)
	headResp := doRangeRequest(t, h, uri, "bytes=0-9,100-109", true)

	if getResp.StatusCode() != StatusPartialContent || headResp.StatusCode() != StatusPartialContent {
		t.Fatalf("unexpected statuses: GET=%d HEAD=%d", getResp.StatusCode(), headResp.StatusCode())
	}
	if getResp.Header.ContentLength() != headResp.Header.ContentLength() {
		t.Fatalf("HEAD content-length %d differs from GET content-length %d",
			headResp.Header.ContentLength(), getResp.Header.ContentLength())
	}
	if !strings.HasPrefix(string(headResp.Header.ContentType()), "multipart/byteranges; boundary=") ||
		!strings.HasPrefix(string(getResp.Header.ContentType()), "multipart/byteranges; boundary=") {
		t.Fatalf("HEAD content type %q differs from GET %q",
			headResp.Header.ContentType(), getResp.Header.ContentType())
	}
	if len(headResp.Body()) != 0 {
		t.Fatalf("HEAD response must have no body, got %d bytes", len(headResp.Body()))
	}
}

func TestFSByteRangesNotSatisfiable(t *testing.T) {
	t.Parallel()

	h, file, uri := setupMultirangeTestFile(t, 100, &FS{})

	resp := doRangeRequest(t, h, uri, "bytes=1000-2000", false)
	if resp.StatusCode() != StatusRequestedRangeNotSatisfiable {
		t.Fatalf("unexpected status: %d. Expecting %d", resp.StatusCode(), StatusRequestedRangeNotSatisfiable)
	}
	expectedCR := fmt.Sprintf("bytes */%d", len(file))
	if cr := string(resp.Header.Peek(HeaderContentRange)); cr != expectedCR {
		t.Fatalf("unexpected content-range %q. Expecting %q", cr, expectedCR)
	}
	if ar := string(resp.Header.Peek(HeaderAcceptRanges)); ar != "bytes" {
		t.Fatalf("unexpected accept-ranges %q. Expecting %q", ar, "bytes")
	}

	// Malformed range header yields the same status.
	resp = doRangeRequest(t, h, uri, "bytes=abc-def", false)
	if resp.StatusCode() != StatusRequestedRangeNotSatisfiable {
		t.Fatalf("unexpected status for malformed range: %d", resp.StatusCode())
	}
}

func TestFSByteRangesTooManyRanges(t *testing.T) {
	t.Parallel()

	h, _, uri := setupMultirangeTestFile(t, 100000, &FS{MaxByteRanges: 3})

	resp := doRangeRequest(t, h, uri, "bytes=0-1,2-3,4-5,6-7", false)
	if resp.StatusCode() != StatusRequestedRangeNotSatisfiable {
		t.Fatalf("unexpected status: %d. Expecting %d", resp.StatusCode(), StatusRequestedRangeNotSatisfiable)
	}

	// Three specs that collapse into one part are allowed: the limit applies
	// to requested specs.
	resp = doRangeRequest(t, h, uri, "bytes=0-10,2-9,5-20", false)
	if resp.StatusCode() != StatusPartialContent {
		t.Fatalf("unexpected status: %d. Expecting %d", resp.StatusCode(), StatusPartialContent)
	}
}

func TestFSByteRangesIfRange(t *testing.T) {
	t.Parallel()

	h, file, uri := setupMultirangeTestFile(t, 500, &FS{})

	var firstCtx RequestCtx
	firstCtx.Init(&Request{}, nil, nil)
	firstCtx.Request.SetRequestURI(uri)
	h(&firstCtx)
	lastModified := string(firstCtx.Response.Header.Peek(HeaderLastModified))
	if lastModified == "" {
		t.Fatalf("test file response must carry Last-Modified")
	}

	doConditional := func(ifRange string) *Response {
		t.Helper()

		var ctx RequestCtx
		ctx.Init(&Request{}, nil, nil)
		ctx.Request.SetRequestURI(uri)
		ctx.Request.Header.Set(HeaderRange, "bytes=0-9,100-109")
		ctx.Request.Header.Set(HeaderIfRange, ifRange)
		h(&ctx)
		return readResponseFromCtx(t, &ctx, false)
	}

	// Matching If-Range date: multipart partial content.
	resp := doConditional(lastModified)
	if resp.StatusCode() != StatusPartialContent {
		t.Fatalf("matching If-Range must honor the range, got status %d", resp.StatusCode())
	}
	if !strings.HasPrefix(string(resp.Header.ContentType()), "multipart/byteranges") {
		t.Fatalf("expecting multipart content type, got %q", resp.Header.ContentType())
	}

	// Stale If-Range date: ignore the Range header, return the full file.
	resp = doConditional("Mon, 01 Jan 0001 00:00:00 GMT")
	if resp.StatusCode() != StatusOK {
		t.Fatalf("stale If-Range must return 200, got %d", resp.StatusCode())
	}
	if !bytes.Equal(resp.Body(), file) {
		t.Fatalf("stale If-Range must return the full representation")
	}

	// A date in the future also fails the validator comparison.
	resp = doConditional("Mon, 01 Jan 9999 00:00:00 GMT")
	if resp.StatusCode() != StatusOK {
		t.Fatalf("future If-Range must return 200, got %d", resp.StatusCode())
	}

	// Unparseable If-Range: behave like a mismatch.
	resp = doConditional("not-a-date")
	if resp.StatusCode() != StatusOK {
		t.Fatalf("unparseable If-Range must return 200, got %d", resp.StatusCode())
	}

	// Entity tag form can never match, since the handler sets no ETags.
	resp = doConditional(`"v1"`)
	if resp.StatusCode() != StatusOK {
		t.Fatalf("etag If-Range without ETag support must return 200, got %d", resp.StatusCode())
	}
}

func TestFSByteRangesSingleRangeSimplePath(t *testing.T) {
	t.Parallel()

	h, file, uri := setupMultirangeTestFile(t, 500, &FS{})

	resp := doRangeRequest(t, h, uri, "bytes=10-20", false)
	if resp.StatusCode() != StatusPartialContent {
		t.Fatalf("unexpected status: %d", resp.StatusCode())
	}
	if strings.HasPrefix(string(resp.Header.ContentType()), "multipart/") {
		t.Fatalf("single range must not use multipart encoding, got %q", resp.Header.ContentType())
	}
	if cr := string(resp.Header.Peek(HeaderContentRange)); cr != "bytes 10-20/500" {
		t.Fatalf("unexpected content-range %q", cr)
	}
	if !bytes.Equal(resp.Body(), file[10:21]) {
		t.Fatalf("single range payload mismatch")
	}
	if resp.Header.ContentLength() != 11 {
		t.Fatalf("unexpected content-length: %d", resp.Header.ContentLength())
	}
}

func TestFSByteRangesOverflowEndClipped(t *testing.T) {
	t.Parallel()

	h, file, uri := setupMultirangeTestFile(t, 100, &FS{})

	// End positions beyond both the file and the host integer width must be
	// clipped to the last byte instead of failing or wrapping.
	resp := doRangeRequest(t, h, uri, "bytes=90-99999999999999999999999999999", false)
	if resp.StatusCode() != StatusPartialContent {
		t.Fatalf("unexpected status: %d", resp.StatusCode())
	}
	if cr := string(resp.Header.Peek(HeaderContentRange)); cr != "bytes 90-99/100" {
		t.Fatalf("unexpected content-range %q", cr)
	}
	if !bytes.Equal(resp.Body(), file[90:]) {
		t.Fatalf("clipped overflow range payload mismatch")
	}
}

func TestFSByteRangesMultipartContentLengthExact(t *testing.T) {
	t.Parallel()

	// Various shapes including tiny parts and boundary-near offsets; the
	// advertised Content-Length must equal the exact wire length.
	h, file, uri := setupMultirangeTestFile(t, 64, &FS{})

	// "0-0,1-2,-1" collapse into "0-2" because the last byte is adjacent to
	// the preceding part.
	resp := doRangeRequest(t, h, uri, "bytes=0-0,1-2,-1,10-12,30-", false)
	if resp.StatusCode() != StatusPartialContent {
		t.Fatalf("unexpected status: %d", resp.StatusCode())
	}
	parts := parseMultipartByteranges(t, string(resp.Header.ContentType()), resp.Body())
	wantRanges := []ByteRange{{0, 2}, {10, 12}, {30, 63}}
	if len(parts) != len(wantRanges) {
		t.Fatalf("unexpected parts count: %d, expecting %d", len(parts), len(wantRanges))
	}
	for i, br := range wantRanges {
		assertPartContent(t, parts[i], file, br.Start, br.End, len(file))
	}
}

func TestFSByteRangesSuffixStaysSeparate(t *testing.T) {
	t.Parallel()

	h, file, uri := setupMultirangeTestFile(t, 64, &FS{})

	// The suffix is disjoint from the other ranges and must remain its own
	// part instead of being silently merged.
	resp := doRangeRequest(t, h, uri, "bytes=0-2,10-12,-10", false)
	if resp.StatusCode() != StatusPartialContent {
		t.Fatalf("unexpected status: %d", resp.StatusCode())
	}
	parts := parseMultipartByteranges(t, string(resp.Header.ContentType()), resp.Body())
	wantRanges := []ByteRange{{0, 2}, {10, 12}, {54, 63}}
	if len(parts) != len(wantRanges) {
		t.Fatalf("unexpected parts count: %d, expecting %d", len(parts), len(wantRanges))
	}
	for i, br := range wantRanges {
		assertPartContent(t, parts[i], file, br.Start, br.End, len(file))
	}
}

func TestFSByteRangesMultipartMapFS(t *testing.T) {
	t.Parallel()

	// A non-os filesystem always goes through bigFileReader, even for small
	// payloads, so this covers the seek + io.LimitedReader path for a custom
	// fs.FS implementation.
	size := 400
	file := make([]byte, size)
	for i := range file {
		file[i] = byte((i*13 + 5) % 251)
	}
	mapFS := fstest.MapFS{
		"f.bin": {Data: file},
	}
	stop := make(chan struct{})
	defer close(stop)
	h := (&FS{
		FS:              mapFS,
		AcceptByteRange: true,
		CleanStop:       stop,
	}).NewRequestHandler()

	resp := doRangeRequest(t, h, "/f.bin", "bytes=0-9,50-59,390-", false)
	if resp.StatusCode() != StatusPartialContent {
		t.Fatalf("unexpected status: %d", resp.StatusCode())
	}
	parts := parseMultipartByteranges(t, string(resp.Header.ContentType()), resp.Body())
	wantRanges := []ByteRange{{0, 9}, {50, 59}, {390, 399}}
	if len(parts) != len(wantRanges) {
		t.Fatalf("unexpected parts count: %d, expecting %d", len(parts), len(wantRanges))
	}
	for i, br := range wantRanges {
		assertPartContent(t, parts[i], file, br.Start, br.End, size)
	}
}

func TestMultiRangeReaderByteByByte(t *testing.T) {
	t.Parallel()

	// Exercises the plain Read path with 1-byte buffers, which forces many
	// header/payload/suffix state transitions.
	ranges := []ByteRange{{0, 2}, {10, 12}, {60, 63}}
	content := make([]byte, 64)
	for i := range content {
		content[i] = byte(7 * i)
	}

	src := &staticRangeReader{content: content}
	mr := newMultiRangeReader(src, ranges, "application/octet-stream", len(content), []byte("boundary"))

	var got bytes.Buffer
	p := make([]byte, 1)
	for {
		n, err := mr.Read(p)
		if n == 1 {
			got.WriteByte(p[0])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("unexpected read error: %v", err)
		}
	}

	parts := parseMultipartByteranges(t, "multipart/byteranges; boundary=boundary", got.Bytes())
	if len(parts) != len(ranges) {
		t.Fatalf("unexpected parts count: %d, expecting %d", len(parts), len(ranges))
	}
	for i, br := range ranges {
		assertPartContent(t, parts[i], content, br.Start, br.End, len(content))
	}

	// Content-Length prediction must match the emitted wire size.
	predicted := multipartByteRangesSize(ranges, "application/octet-stream", len(content), []byte("boundary"))
	if predicted != got.Len() {
		t.Fatalf("predicted multipart size %d differs from emitted %d", predicted, got.Len())
	}
}

func TestMultiRangeReaderWriteToMatchesRead(t *testing.T) {
	t.Parallel()

	ranges := []ByteRange{{3, 7}, {100, 101}, {250, 260}}
	content := make([]byte, 300)
	for i := range content {
		content[i] = byte(i % 257)
	}

	build := func() *multiRangeReader {
		return newMultiRangeReader(&staticRangeReader{content: content}, ranges, "text/plain", len(content), []byte("bnd"))
	}

	viaRead, err := io.ReadAll(build())
	if err != nil {
		t.Fatalf("Read path error: %v", err)
	}

	var viaWrite bytes.Buffer
	if _, err := build().WriteTo(&viaWrite); err != nil {
		t.Fatalf("WriteTo path error: %v", err)
	}

	if !bytes.Equal(viaRead, viaWrite.Bytes()) {
		t.Fatalf("Read and WriteTo produced different multipart payloads (%d vs %d bytes)", len(viaRead), viaWrite.Len())
	}
	predicted := multipartByteRangesSize(ranges, "text/plain", len(content), []byte("bnd"))
	if predicted != viaWrite.Len() {
		t.Fatalf("predicted multipart size %d differs from WriteTo size %d", predicted, viaWrite.Len())
	}

	parts := parseMultipartByteranges(t, "multipart/byteranges; boundary=bnd", viaWrite.Bytes())
	if len(parts) != len(ranges) {
		t.Fatalf("unexpected parts count: %d", len(parts))
	}
	for i, br := range ranges {
		assertPartContent(t, parts[i], content, br.Start, br.End, len(content))
	}
}

// staticRangeReader is a controllable in-memory implementation of the same
// interfaces as fsSmallFileReader: UpdateByteRange narrows subsequent reads
// to the configured window.
type staticRangeReader struct {
	content  []byte
	startPos int
	endPos   int
}

func (s *staticRangeReader) UpdateByteRange(startPos, endPos int) error {
	s.startPos = startPos
	s.endPos = endPos + 1
	return nil
}

func (s *staticRangeReader) Read(p []byte) (int, error) {
	if s.startPos >= s.endPos {
		return 0, io.EOF
	}
	n := copy(p, s.content[s.startPos:s.endPos])
	s.startPos += n
	if s.startPos >= s.endPos {
		return n, io.EOF
	}
	return n, nil
}
