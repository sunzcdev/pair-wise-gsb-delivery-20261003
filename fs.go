package fasthttp

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"maps"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
	"github.com/valyala/bytebufferpool"

	brotli "github.com/molecule-man/go-brrr"
)

// ServeFileBytesUncompressed returns HTTP response containing file contents
// from the given path.
//
// Directory contents is returned if path points to directory.
//
// ServeFileBytes may be used for saving network traffic when serving files
// with good compression ratio.
//
// See also RequestCtx.SendFileBytes.
//
// WARNING: do not pass any user supplied paths to this function!
// WARNING: if path is based on user input users will be able to request
// any file on your filesystem! Use fasthttp.FS with a sane Root instead.
func ServeFileBytesUncompressed(ctx *RequestCtx, path []byte) {
	ServeFileUncompressed(ctx, b2s(path))
}

// ServeFileUncompressed returns HTTP response containing file contents
// from the given path.
//
// Directory contents is returned if path points to directory.
//
// ServeFile may be used for saving network traffic when serving files
// with good compression ratio.
//
// See also RequestCtx.SendFile.
//
// WARNING: do not pass any user supplied paths to this function!
// WARNING: if path is based on user input users will be able to request
// any file on your filesystem! Use fasthttp.FS with a sane Root instead.
func ServeFileUncompressed(ctx *RequestCtx, path string) {
	ctx.Request.Header.DelBytes(strAcceptEncoding)
	ServeFile(ctx, path)
}

// ServeFileBytes returns HTTP response containing compressed file contents
// from the given path.
//
// HTTP response may contain uncompressed file contents in the following cases:
//
//   - Missing 'Accept-Encoding: gzip' request header.
//   - No write access to directory containing the file.
//
// Directory contents is returned if path points to directory.
//
// Use ServeFileBytesUncompressed is you don't need serving compressed
// file contents.
//
// See also RequestCtx.SendFileBytes, ServeFileLiteral.
//
// WARNING: do not pass any user supplied paths to this function!
// WARNING: if path is based on user input users will be able to request
// any file on your filesystem! Use fasthttp.FS with a sane Root instead.
func ServeFileBytes(ctx *RequestCtx, path []byte) {
	ServeFile(ctx, b2s(path))
}

// ServeFile returns HTTP response containing compressed file contents
// from the given path.
//
// HTTP response may contain uncompressed file contents in the following cases:
//
//   - Missing 'Accept-Encoding: gzip' request header.
//   - No write access to directory containing the file.
//
// Directory contents is returned if path points to directory.
//
// Use ServeFileUncompressed is you don't need serving compressed file contents.
//
// ServeFile interprets path as a URI path internally. Percent-encoded
// sequences may be decoded, and '?' or '#' may be treated as URI delimiters.
// Use ServeFileLiteral if you need literal path semantics.
//
// See also RequestCtx.SendFile, ServeFileLiteral.
//
// WARNING: do not pass any user supplied paths to this function!
// WARNING: if path is based on user input users will be able to request
// any file on your filesystem! Use fasthttp.FS with a sane Root instead.
func ServeFile(ctx *RequestCtx, path string) {
	rootFSOnce.Do(func() {
		rootFSHandler = rootFS.NewRequestHandler()
	})

	path, ok := normalizeServeFilePath(ctx, path)
	if !ok {
		return
	}
	ctx.Request.SetRequestURI(path)
	rootFSHandler(ctx)
}

// ServeFileLiteral returns HTTP response containing compressed file contents
// from the given path using literal path semantics.
//
// Reserved URI characters in path such as '%', '?' and '#' are preserved
// instead of being interpreted during internal request URI processing.
//
// HTTP response may contain uncompressed file contents in the following cases:
//
//   - Missing 'Accept-Encoding: gzip' request header.
//   - No write access to directory containing the file.
//
// Directory contents is returned if path points to directory.
//
// Use ServeFileUncompressed if you don't need serving compressed file contents.
//
// See also RequestCtx.SendFileLiteral, ServeFile.
//
// WARNING: do not pass any user supplied paths to this function!
// WARNING: if path is based on user input users will be able to request
// any file on your filesystem! Use fasthttp.FS with a sane Root instead.
func ServeFileLiteral(ctx *RequestCtx, path string) {
	rootFSOnce.Do(func() {
		rootFSHandler = rootFS.NewRequestHandler()
	})

	path, ok := normalizeServeFilePath(ctx, path)
	if !ok {
		return
	}
	ctx.Request.SetRequestURIBytes(appendQuotedPath(nil, s2b(path)))
	rootFSHandler(ctx)
}

var (
	rootFSOnce sync.Once
	rootFS     = &FS{
		Root:               "",
		AllowEmptyRoot:     true,
		GenerateIndexPages: true,
		Compress:           true,
		CompressBrotli:     true,
		CompressZstd:       true,
		AcceptByteRange:    true,
	}
	rootFSHandler RequestHandler
)

// ServeFS returns HTTP response containing compressed file contents from the given fs.FS's path.
//
// HTTP response may contain uncompressed file contents in the following cases:
//
//   - Missing 'Accept-Encoding: gzip' request header.
//   - No write access to directory containing the file.
//
// Directory contents is returned if path points to directory.
//
// ServeFS interprets path as a URI path internally. Percent-encoded
// sequences may be decoded, and '?' or '#' may be treated as URI delimiters.
// Use ServeFSLiteral if you need literal path semantics.
//
// See also ServeFile, ServeFSLiteral.
func ServeFS(ctx *RequestCtx, filesystem fs.FS, path string) {
	serveFS(ctx, filesystem, path, false)
}

// ServeFSLiteral returns HTTP response containing compressed file contents
// from the given fs.FS's path using literal path semantics.
//
// Reserved URI characters in path such as '%', '?' and '#' are preserved
// instead of being interpreted during internal request URI processing.
//
// HTTP response may contain uncompressed file contents in the following cases:
//
//   - Missing 'Accept-Encoding: gzip' request header.
//   - No write access to directory containing the file.
//
// Directory contents is returned if path points to directory.
//
// See also ServeFS, ServeFileLiteral.
func ServeFSLiteral(ctx *RequestCtx, filesystem fs.FS, path string) {
	serveFS(ctx, filesystem, path, true)
}

func serveFS(ctx *RequestCtx, filesystem fs.FS, path string, literal bool) {
	f := &FS{
		FS:                 filesystem,
		Root:               "",
		AllowEmptyRoot:     true,
		GenerateIndexPages: true,
		Compress:           true,
		CompressBrotli:     true,
		CompressZstd:       true,
		AcceptByteRange:    true,
		// This FS serves exactly one request, so its cache can never be hit
		// again. Skipping it drops a cleaner goroutine per call and releases the
		// file when the response body closes instead of at GC time.
		SkipCache: true,
	}
	handler := f.NewRequestHandler()

	if literal {
		ctx.Request.SetRequestURIBytes(appendQuotedPath(nil, s2b(path)))
	} else {
		ctx.Request.SetRequestURI(path)
	}
	handler(ctx)
}

func normalizeServeFilePath(ctx *RequestCtx, path string) (string, bool) {
	if path == "" || !filepath.IsAbs(path) {
		// extend relative path to absolute path
		hasTrailingSlash := path != "" && (path[len(path)-1] == '/' || path[len(path)-1] == '\\')

		var err error
		path = filepath.FromSlash(path)
		if path, err = filepath.Abs(path); err != nil {
			ctx.Logger().Printf("cannot resolve path %q to absolute file path: %v", path, err)
			ctx.Error("Internal Server Error", StatusInternalServerError)
			return "", false
		}
		if hasTrailingSlash {
			path += "/"
		}
	}

	// convert the path to forward slashes regardless the OS in order to set the URI properly
	// the handler will convert back to OS path separator before opening the file
	return filepath.ToSlash(path), true
}

// PathRewriteFunc must return new request path based on arbitrary ctx
// info such as ctx.Path().
//
// Path rewriter is used in FS for translating the current request
// to the local filesystem path relative to FS.Root.
//
// The returned path must not contain '..' path segments due to security reasons,
// since such paths may refer files outside FS.Root.
//
// The returned path may refer to ctx members. For example, ctx.Path().
type PathRewriteFunc func(ctx *RequestCtx) []byte

// NewVHostPathRewriter returns path rewriter, which strips slashesCount
// leading slashes from the path and prepends the path with request's host,
// thus simplifying virtual hosting for static files.
//
// Examples:
//
//   - host=foobar.com, slashesCount=0, original path="/foo/bar".
//     Resulting path: "/foobar.com/foo/bar"
//
//   - host=img.aaa.com, slashesCount=1, original path="/images/123/456.jpg"
//     Resulting path: "/img.aaa.com/123/456.jpg"
func NewVHostPathRewriter(slashesCount int) PathRewriteFunc {
	return func(ctx *RequestCtx) []byte {
		path := stripLeadingSlashes(ctx.Path(), slashesCount)
		host := ctx.Host()
		if n := bytes.IndexByte(host, '/'); n >= 0 {
			host = nil
		}
		if len(host) == 0 {
			host = strInvalidHost
		}
		b := bytebufferpool.Get()
		b.B = append(b.B, '/')
		b.B = append(b.B, host...)
		b.B = append(b.B, path...)
		ctx.URI().SetPathBytes(b.B)
		bytebufferpool.Put(b)

		return ctx.Path()
	}
}

var strInvalidHost = []byte("invalid-host")

// NewPathSlashesStripper returns path rewriter, which strips slashesCount
// leading slashes from the path.
//
// Examples:
//
//   - slashesCount = 0, original path: "/foo/bar", result: "/foo/bar"
//   - slashesCount = 1, original path: "/foo/bar", result: "/bar"
//   - slashesCount = 2, original path: "/foo/bar", result: ""
//
// The returned path rewriter may be used as FS.PathRewrite .
func NewPathSlashesStripper(slashesCount int) PathRewriteFunc {
	return func(ctx *RequestCtx) []byte {
		return stripLeadingSlashes(ctx.Path(), slashesCount)
	}
}

// NewPathPrefixStripper returns path rewriter, which removes prefixSize bytes
// from the path prefix.
//
// Examples:
//
//   - prefixSize = 0, original path: "/foo/bar", result: "/foo/bar"
//   - prefixSize = 3, original path: "/foo/bar", result: "o/bar"
//   - prefixSize = 7, original path: "/foo/bar", result: "r"
//
// The returned path rewriter may be used as FS.PathRewrite .
func NewPathPrefixStripper(prefixSize int) PathRewriteFunc {
	return func(ctx *RequestCtx) []byte {
		path := ctx.Path()
		if len(path) >= prefixSize {
			path = path[prefixSize:]
		}
		return path
	}
}

// FS represents settings for request handler serving static files
// from the local filesystem.
//
// It is prohibited copying FS values. Create new values instead.
type FS struct {
	noCopy noCopy

	// FS is filesystem to serve files from. eg: embed.FS os.DirFS
	FS fs.FS

	// Path rewriting function.
	//
	// By default request path is not modified.
	PathRewrite PathRewriteFunc

	// PathNotFound fires when file is not found in filesystem
	// this functions tries to replace "Cannot open requested path"
	// server response giving to the programmer the control of server flow.
	//
	// By default PathNotFound returns
	// "Cannot open requested path"
	PathNotFound RequestHandler

	// Suffixes list to add to compressedFileSuffix depending on encoding
	//
	// This value has sense only if Compress is set.
	//
	// FSCompressedFileSuffixes is used by default.
	CompressedFileSuffixes map[string]string

	// If CleanStop is set, the channel can be closed to stop the cleanup handlers
	// for the FS RequestHandlers created with NewRequestHandler.
	// NEVER close this channel while the handler is still being used!
	CleanStop chan struct{}

	h RequestHandler

	// Path to the root directory to serve files from.
	Root string

	// Path to the compressed root directory to serve files from. If this value
	// is empty, Root is used.
	CompressRoot string

	// Suffix to add to the name of cached compressed file.
	//
	// This value has sense only if Compress is set.
	//
	// FSCompressedFileSuffix is used by default.
	CompressedFileSuffix string

	// List of index file names to try opening during directory access.
	//
	// For example:
	//
	//     * index.html
	//     * index.htm
	//     * my-super-index.xml
	//
	// By default the list is empty.
	IndexNames []string

	// Expiration duration for inactive file handlers.
	//
	// FSHandlerCacheDuration is used by default.
	CacheDuration time.Duration

	once sync.Once

	// AllowEmptyRoot controls what happens when Root is empty. When false (default) it will default to the
	// current working directory. An empty root is mostly useful when you want to use absolute paths
	// on windows that are on different filesystems. On linux setting your Root to "/" already allows you to use
	// absolute paths on any filesystem.
	AllowEmptyRoot bool

	// Uses brotli encoding and fallbacks to zstd or gzip in responses if set to true, uses zstd or gzip if set to false.
	//
	// This value has sense only if Compress is set.
	//
	// Brotli encoding is disabled by default.
	CompressBrotli bool

	// Uses zstd encoding and fallbacks to gzip in responses if set to true, uses gzip if set to false.
	//
	// This value has sense only if Compress is set.
	//
	// zstd encoding is disabled by default.
	CompressZstd bool

	// Index pages for directories without files matching IndexNames
	// are automatically generated if set.
	//
	// Directory index generation may be quite slow for directories
	// with many files (more than 1K), so it is discouraged enabling
	// index pages' generation for such directories.
	//
	// By default index pages aren't generated.
	GenerateIndexPages bool

	// Transparently compresses responses if set to true.
	//
	// The server tries minimizing CPU usage by caching compressed files.
	// It adds CompressedFileSuffix suffix to the original file name and
	// tries saving the resulting compressed file under the new file name.
	// So it is advisable to give the server write access to Root
	// and to all inner folders in order to minimize CPU usage when serving
	// compressed responses.
	//
	// Transparent compression is disabled by default.
	Compress bool

	// Enables byte range requests if set to true.
	//
	// Byte range requests are disabled by default.
	AcceptByteRange bool

	// MaxByteRanges limits the number of byte ranges accepted in a single
	// request. Overlapping or adjacent ranges are merged before the limit is
	// enforced on the response, but this bounds how many range specs a client
	// may send and therefore the per-request multipart framing overhead.
	//
	// Zero means the default limit (100). Negative values disable the limit
	// (not recommended). Requests exceeding the limit get a
	// 416 Range Not Satisfiable response.
	MaxByteRanges int

	// SkipCache if true, will cache no file handler.
	//
	// By default is false.
	SkipCache bool
}

// FSCompressedFileSuffix is the suffix FS adds to the original file names
// when trying to store compressed file under the new file name.
// See FS.Compress for details.
const FSCompressedFileSuffix = ".fasthttp.gz"

// FSCompressedFileSuffixes is the suffixes FS adds to the original file names depending on encoding
// when trying to store compressed file under the new file name.
// See FS.Compress for details.
//
// This map is read during FSHandler initialization (which runs in a sync.Once).
// It is not safe for concurrent modification. Set any custom suffixes in an
// init() function or before the first call to FS.NewRequestHandler, and do not
// modify the map afterwards.
var FSCompressedFileSuffixes = map[string]string{
	"gzip": ".fasthttp.gz",
	"br":   ".fasthttp.br",
	"zstd": ".fasthttp.zst",
}

// FSHandlerCacheDuration is the default expiration duration for inactive
// file handlers opened by FS.
const FSHandlerCacheDuration = 10 * time.Second

// FSHandler returns request handler serving static files from
// the given root folder.
//
// stripSlashes indicates how many leading slashes must be stripped
// from requested path before searching requested file in the root folder.
// Examples:
//
//   - stripSlashes = 0, original path: "/foo/bar", result: "/foo/bar"
//   - stripSlashes = 1, original path: "/foo/bar", result: "/bar"
//   - stripSlashes = 2, original path: "/foo/bar", result: ""
//
// The returned request handler automatically generates index pages
// for directories without index.html.
//
// The returned handler caches requested file handles
// for FSHandlerCacheDuration.
// Make sure your program has enough 'max open files' limit aka
// 'ulimit -n' if root folder contains many files.
//
// Do not create multiple request handler instances for the same
// (root, stripSlashes) arguments - just reuse a single instance.
// Otherwise goroutine leak will occur.
func FSHandler(root string, stripSlashes int) RequestHandler {
	fs := &FS{
		Root:               root,
		IndexNames:         []string{"index.html"},
		GenerateIndexPages: true,
		AcceptByteRange:    true,
	}
	if stripSlashes > 0 {
		fs.PathRewrite = NewPathSlashesStripper(stripSlashes)
	}
	return fs.NewRequestHandler()
}

// NewRequestHandler returns new request handler with the given FS settings.
//
// The returned handler caches requested file handles
// for FS.CacheDuration.
// Make sure your program has enough 'max open files' limit aka
// 'ulimit -n' if FS.Root folder contains many files.
//
// Do not create multiple request handlers from a single FS instance -
// just reuse a single request handler.
func (fs *FS) NewRequestHandler() RequestHandler {
	fs.once.Do(fs.initRequestHandler)
	return fs.h
}

func (fs *FS) normalizeRoot(root string) string {
	// fs.FS uses relative paths, that paths are slash-separated on all systems, even Windows.
	if fs.FS == nil {
		// Serve files from the current working directory if Root is empty or if Root is a relative path.
		if (!fs.AllowEmptyRoot && root == "") || (root != "" && !filepath.IsAbs(root)) {
			path, err := os.Getwd()
			if err != nil {
				path = "."
			}
			root = path + "/" + root
		}

		// convert the root directory slashes to the native format
		root = filepath.FromSlash(root)
	} else {
		if root == "" {
			return root
		}

		// Normalize fs.FS roots to slash-separated relative paths.
		root = strings.ReplaceAll(root, "\\", "/")
		root = strings.TrimLeft(root, "/")
		if root == "" {
			return root
		}
		root = path.Clean(root)
		if root == "." {
			return "."
		}
		return root
	}

	// strip trailing slashes from the root path
	for root != "" && root[len(root)-1] == os.PathSeparator {
		root = root[:len(root)-1]
	}
	return root
}

func (fs *FS) initRequestHandler() {
	root := fs.normalizeRoot(fs.Root)

	compressRoot := fs.CompressRoot
	if compressRoot == "" {
		compressRoot = root
	} else {
		compressRoot = fs.normalizeRoot(compressRoot)
	}

	compressedFileSuffixes := fs.CompressedFileSuffixes
	if compressedFileSuffixes["br"] == "" || compressedFileSuffixes["gzip"] == "" ||
		compressedFileSuffixes["zstd"] == "" || compressedFileSuffixes["br"] == compressedFileSuffixes["gzip"] ||
		compressedFileSuffixes["br"] == compressedFileSuffixes["zstd"] ||
		compressedFileSuffixes["gzip"] == compressedFileSuffixes["zstd"] {
		// Copy global map
		compressedFileSuffixes = make(map[string]string, len(FSCompressedFileSuffixes))
		maps.Copy(compressedFileSuffixes, FSCompressedFileSuffixes)
	}

	if fs.CompressedFileSuffix != "" {
		compressedFileSuffixes["gzip"] = fs.CompressedFileSuffix
		compressedFileSuffixes["br"] = FSCompressedFileSuffixes["br"]
		compressedFileSuffixes["zstd"] = FSCompressedFileSuffixes["zstd"]
	}

	h := &fsHandler{
		filesystem:             fs.FS,
		root:                   root,
		indexNames:             fs.IndexNames,
		pathRewrite:            fs.PathRewrite,
		generateIndexPages:     fs.GenerateIndexPages,
		compress:               fs.Compress,
		compressBrotli:         fs.CompressBrotli,
		compressZstd:           fs.CompressZstd,
		compressRoot:           compressRoot,
		pathNotFound:           fs.PathNotFound,
		acceptByteRange:        fs.AcceptByteRange,
		maxByteRanges:          fs.MaxByteRanges,
		compressedFileSuffixes: compressedFileSuffixes,
	}

	h.cacheManager = newCacheManager(fs)

	if h.filesystem == nil {
		h.filesystem = &osFS{} // It provides os.Open and os.Stat
	}

	if fs.SkipCache {
		// noopCacheManager.Close is a no-op, so there is nothing to stop.
		fs.h = h.handleRequest
		return
	}

	// Use a >16-byte backing array so the cleanup owner doesn't fall under
	// runtime's tiny pointer-free allocation batching.
	cacheCleaner := make([]byte, 32)
	runtime.AddCleanup(&cacheCleaner[0], cacheManager.Close, h.cacheManager)
	fs.h = func(ctx *RequestCtx) {
		h.handleRequest(ctx)
		// Keep the cleanup owner captured by fs.h and alive until
		// h.handleRequest returns, since its cleanup stops the cache cleaner.
		runtime.KeepAlive(cacheCleaner)
	}
}

type fsHandler struct {
	smallFileReaderPool sync.Pool
	filesystem          fs.FS

	cacheManager cacheManager

	pathRewrite            PathRewriteFunc
	pathNotFound           RequestHandler
	compressedFileSuffixes map[string]string

	root               string
	compressRoot       string
	indexNames         []string
	generateIndexPages bool
	compress           bool
	compressBrotli     bool
	compressZstd       bool
	acceptByteRange    bool
	maxByteRanges      int
}

type fsFile struct {
	lastModified time.Time

	t               time.Time
	f               fs.File
	h               *fsHandler
	filename        string // fs.FileInfo.Name() return filename, isn't filepath.
	contentType     string
	dirIndex        []byte
	lastModifiedStr []byte

	bigFiles      []*bigFileReader
	contentLength int
	readersCount  int

	bigFilesLock sync.Mutex
	compressed   bool
}

func (ff *fsFile) NewReader() (io.Reader, error) {
	if ff.isBig() {
		return ff.bigFileReader()
	}
	return ff.smallFileReader(), nil
}

func (ff *fsFile) smallFileReader() io.Reader {
	v := ff.h.smallFileReaderPool.Get()
	if v == nil {
		v = &fsSmallFileReader{}
	}
	r := v.(*fsSmallFileReader) //nolint:forcetypeassert
	r.ff = ff
	r.endPos = ff.contentLength
	if r.startPos > 0 {
		panic("bug: fsSmallFileReader with non-nil startPos found in the pool")
	}
	return r
}

// Files bigger than this size are sent with sendfile.
const maxSmallFileSize = 2 * 4096

func (ff *fsFile) isBig() bool {
	if _, ok := ff.h.filesystem.(*osFS); !ok { // fs.FS only uses bigFileReader, memory cache uses fsSmallFileReader
		return ff.f != nil
	}
	return ff.contentLength > maxSmallFileSize && len(ff.dirIndex) == 0
}

func (ff *fsFile) bigFileReader() (io.Reader, error) {
	if ff.f == nil {
		return nil, errors.New("bug: ff.f must be non-nil in big file reader")
	}

	var r io.Reader

	ff.bigFilesLock.Lock()
	n := len(ff.bigFiles)
	if n > 0 {
		r = ff.bigFiles[n-1]
		ff.bigFiles = ff.bigFiles[:n-1]
	}
	ff.bigFilesLock.Unlock()

	if r != nil {
		return r, nil
	}

	f, err := ff.h.filesystem.Open(ff.filename)
	if err != nil {
		return nil, fmt.Errorf("cannot open already opened file: %w", err)
	}
	return &bigFileReader{
		f:  f,
		ff: ff,
		r:  f,
	}, nil
}

func (ff *fsFile) Release() {
	if ff.f != nil {
		_ = ff.f.Close()

		if ff.isBig() {
			ff.bigFilesLock.Lock()
			for _, r := range ff.bigFiles {
				_ = r.f.Close()
			}
			ff.bigFilesLock.Unlock()
		}
	}
}

func (ff *fsFile) decReadersCount() {
	ff.h.cacheManager.DecReadersCount(ff)
}

// bigFileReader attempts to trigger sendfile
// for sending big files over the wire.
type bigFileReader struct {
	f  fs.File
	ff *fsFile
	r  io.Reader
	lr io.LimitedReader
}

func (r *bigFileReader) UpdateByteRange(startPos, endPos int) error {
	seeker, ok := r.f.(io.Seeker)
	if !ok {
		return errors.New("must implement seek")
	}
	if _, err := seeker.Seek(int64(startPos), io.SeekStart); err != nil {
		return err
	}
	r.r = &r.lr
	r.lr.R = r.f
	r.lr.N = int64(endPos - startPos + 1)
	return nil
}

func (r *bigFileReader) Read(p []byte) (int, error) {
	return r.r.Read(p)
}

func (r *bigFileReader) WriteTo(w io.Writer) (int64, error) {
	if rf, ok := w.(io.ReaderFrom); ok {
		// fast path. Send file must be triggered
		return rf.ReadFrom(r.r)
	}

	// slow path
	return copyZeroAlloc(w, r.r)
}

func (r *bigFileReader) Close() error {
	r.r = r.f
	seeker, ok := r.f.(io.Seeker)
	if !ok {
		_ = r.f.Close()
		return errors.New("must implement seek")
	}
	n, err := seeker.Seek(0, io.SeekStart)
	if err == nil {
		if n == 0 {
			ff := r.ff
			ff.bigFilesLock.Lock()
			ff.bigFiles = append(ff.bigFiles, r)
			ff.bigFilesLock.Unlock()
		} else {
			_ = r.f.Close()
			err = errors.New("bug: seek to start returned (non-zero, nil)")
		}
	} else {
		_ = r.f.Close()
	}
	r.ff.decReadersCount()
	return err
}

type fsSmallFileReader struct {
	ff       *fsFile
	startPos int
	endPos   int
}

func (r *fsSmallFileReader) Close() error {
	ff := r.ff
	ff.decReadersCount()
	r.ff = nil
	r.startPos = 0
	r.endPos = 0
	ff.h.smallFileReaderPool.Put(r)
	return nil
}

func (r *fsSmallFileReader) UpdateByteRange(startPos, endPos int) error {
	r.startPos = startPos
	r.endPos = endPos + 1
	return nil
}

func (r *fsSmallFileReader) Read(p []byte) (int, error) {
	tailLen := r.endPos - r.startPos
	if tailLen <= 0 {
		return 0, io.EOF
	}
	if len(p) > tailLen {
		p = p[:tailLen]
	}

	ff := r.ff
	if ff.f != nil {
		ra, ok := ff.f.(io.ReaderAt)
		if !ok {
			return 0, errors.New("must implement readat")
		}
		n, err := ra.ReadAt(p, int64(r.startPos))
		r.startPos += n
		return n, err
	}

	n := copy(p, ff.dirIndex[r.startPos:])
	r.startPos += n
	return n, nil
}

func (r *fsSmallFileReader) WriteTo(w io.Writer) (int64, error) {
	ff := r.ff

	var n int
	var err error
	if ff.f == nil {
		n, err = w.Write(ff.dirIndex[r.startPos:r.endPos])
		return int64(n), err
	}

	if rf, ok := w.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}

	curPos := r.startPos
	bufv := copyBufPool.Get()
	buf := bufv.([]byte) //nolint:forcetypeassert
	for err == nil {
		tailLen := r.endPos - curPos
		if tailLen <= 0 {
			break
		}
		if len(buf) > tailLen {
			buf = buf[:tailLen]
		}
		ra, ok := ff.f.(io.ReaderAt)
		if !ok {
			return 0, errors.New("must implement readat")
		}
		n, err = ra.ReadAt(buf, int64(curPos))
		nw, errw := w.Write(buf[:n])
		curPos += nw
		if errw == nil && nw != n {
			errw = errors.New("bug: write returned (n, nil), where n != len(p)")
		}
		if err == nil {
			err = errw
		}
	}
	copyBufPool.Put(bufv)

	if err == io.EOF {
		err = nil
	}
	return int64(curPos - r.startPos), err
}

// multipartByteRangesBoundary is the fixed-length prefix of the boundary
// generated for multipart/byteranges responses.
var multipartByteRangesBoundaryPrefix = []byte("fasthttp")

// multiRangeReader streams the representation as a multipart/byteranges
// message (RFC 7233, Appendix A). The file itself is never buffered: each
// part's payload is read on demand from the underlying reader, which seeks
// and limits reads itself.
type multiRangeReader struct {
	r             io.Reader // underlying per-part reader
	ranges        []ByteRange
	contentType   []byte
	contentLength int
	boundary      []byte

	// partIndex selects the current range; chunkIndex walks the chunks
	// produced for that range (prefix, payload, suffix) and the trailer.
	partIndex  int
	chunkIndex int

	// header holds the prefix of the current part; payload is its file
	// content and suffix the CRLF ending it.
	header []byte
	suffix []byte // CRLF between parts, or CRLF + closing boundary on the last

	headerOffset int
	suffixOffset int
	eof          bool
}

func newMultiRangeReader(
	r io.Reader,
	ranges []ByteRange,
	contentType string,
	contentLength int,
	boundary []byte,
) *multiRangeReader {
	return &multiRangeReader{
		r:             r,
		ranges:        ranges,
		contentType:   []byte(contentType),
		contentLength: contentLength,
		boundary:      boundary,
	}
}

const (
	multiRangeChunkHeader = iota
	multiRangeChunkPayload
	multiRangeChunkSuffix
)

func (mr *multiRangeReader) Close() error {
	if closer, ok := mr.r.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// SupportsBodyWriteTo marks the reader as safe for the WriteTo fast path.
func (mr *multiRangeReader) SupportsBodyWriteTo() bool {
	return true
}

// preparePart points the underlying reader at the current range and builds
// the corresponding multipart part header.
func (mr *multiRangeReader) preparePart() error {
	br := mr.ranges[mr.partIndex]
	if updater, ok := mr.r.(byteRangeUpdater); ok {
		if err := updater.UpdateByteRange(br.Start, br.End); err != nil {
			return err
		}
	}

	h := appendMultipartPartHeader(nil, mr.boundary, mr.contentType, br, mr.contentLength)

	mr.header = h
	mr.headerOffset = 0
	mr.suffixOffset = 0
	mr.suffix = mr.suffix[:0]
	mr.suffix = append(mr.suffix, strCRLF...)
	if mr.partIndex == len(mr.ranges)-1 {
		mr.suffix = append(mr.suffix, '-', '-')
		mr.suffix = append(mr.suffix, mr.boundary...)
		mr.suffix = append(mr.suffix, '-', '-')
	}
	mr.chunkIndex = multiRangeChunkHeader
	return nil
}

func (mr *multiRangeReader) Read(p []byte) (int, error) {
	if mr.eof {
		return 0, io.EOF
	}
	for {
		if mr.partIndex >= len(mr.ranges) {
			mr.eof = true
			return 0, io.EOF
		}

		if mr.header == nil {
			if err := mr.preparePart(); err != nil {
				return 0, err
			}
		}

		switch mr.chunkIndex {
		case multiRangeChunkHeader:
			if mr.headerOffset < len(mr.header) {
				n := copy(p, mr.header[mr.headerOffset:])
				mr.headerOffset += n
				return n, nil
			}
			mr.chunkIndex = multiRangeChunkPayload
			continue
		case multiRangeChunkPayload:
			n, err := mr.r.Read(p)
			if n > 0 {
				return n, nil
			}
			if err != nil && err != io.EOF {
				return 0, err
			}
			if n == 0 && err == nil {
				return 0, io.ErrNoProgress
			}
			mr.chunkIndex = multiRangeChunkSuffix
			continue
		case multiRangeChunkSuffix:
			if mr.suffixOffset < len(mr.suffix) {
				n := copy(p, mr.suffix[mr.suffixOffset:])
				mr.suffixOffset += n
				return n, nil
			}
		}

		mr.partIndex++
		mr.header = nil
	}
}

// WriteTo streams the whole multipart message to w without buffering the
// file. When w is a flushing buffered writer, the buffer is drained before
// each part payload so large file regions may take zero-copy paths such as
// sendfile.
func (mr *multiRangeReader) WriteTo(w io.Writer) (int64, error) {
	mr.partIndex = 0
	mr.eof = false
	var written int64

	writeBytes := func(b []byte) error {
		for len(b) > 0 {
			n, err := w.Write(b)
			if n > 0 {
				written += int64(n)
				b = b[n:]
			}
			if err != nil {
				return err
			}
			if n == 0 {
				return io.ErrShortWrite
			}
		}
		return nil
	}

	flusher, canFlush := w.(interface{ Flush() error })

	for i := range mr.ranges {
		mr.partIndex = i
		if err := mr.preparePart(); err != nil {
			return written, err
		}
		if err := writeBytes(mr.header); err != nil {
			return written, err
		}

		// Drain buffered framing bytes before handing the file region to the
		// writer so zero-copy transfers (e.g. sendfile) stay possible.
		if canFlush {
			if err := flusher.Flush(); err != nil {
				return written, err
			}
		}

		n, err := mr.writePayload(w)
		written += n
		if err != nil && err != io.EOF {
			return written, err
		}
		if err := writeBytes(mr.suffix); err != nil {
			return written, err
		}
	}
	mr.eof = true
	return written, nil
}

// writePayload streams one part's file bytes to w. The underlying reader is
// already bounded to the part by UpdateByteRange (both bigFileReader and
// fsSmallFileReader limit themselves), so no extra io.LimitedReader is
// needed: this keeps sendfile-capable readers on their fast path.
func (mr *multiRangeReader) writePayload(w io.Writer) (int64, error) {
	// Prefer the reader's WriteTo so sendfile-capable readers keep their
	// fast path (bigFileReader.WriteTo honors its own LimitedReader;
	// fsSmallFileReader.WriteTo copies exactly its configured window).
	if wt, ok := mr.r.(io.WriterTo); ok {
		n, err := wt.WriteTo(w)
		if err == io.EOF {
			err = nil
		}
		return n, err
	}

	br := mr.ranges[mr.partIndex]
	remaining := int64(br.End - br.Start + 1)
	vbuf := copyBufPool.Get()
	buf := vbuf.([]byte) //nolint:forcetypeassert
	var written int64
	for remaining > 0 {
		toRead := int64(len(buf))
		if toRead > remaining {
			toRead = remaining
		}
		n, err := mr.r.Read(buf[:toRead])
		if n > 0 {
			nw, werr := w.Write(buf[:n])
			written += int64(nw)
			remaining -= int64(nw)
			if werr != nil {
				copyBufPool.Put(vbuf)
				return written, werr
			}
			if nw != n {
				copyBufPool.Put(vbuf)
				return written, io.ErrShortWrite
			}
		}
		if err != nil {
			copyBufPool.Put(vbuf)
			if err == io.EOF && remaining == 0 {
				return written, nil
			}
			return written, err
		}
		if n == 0 && err == nil {
			copyBufPool.Put(vbuf)
			return written, io.ErrNoProgress
		}
	}
	copyBufPool.Put(vbuf)
	return written, nil
}

// appendMultipartPartHeader appends the framing header of a single
// multipart/byteranges part:
//
//	--BOUNDARY\r\n
//	Content-Type: <contentType>\r\n
//	Content-Range: bytes start-end/size\r\n
//	\r\n
func appendMultipartPartHeader(dst, boundary, contentType []byte, br ByteRange, contentLength int) []byte {
	dst = append(dst, '-', '-')
	dst = append(dst, boundary...)
	dst = append(dst, strCRLF...)
	dst = append(dst, HeaderContentType...)
	dst = append(dst, ':')
	dst = append(dst, contentType...)
	dst = append(dst, strCRLF...)
	dst = append(dst, HeaderContentRange...)
	dst = append(dst, ':', ' ')
	dst = append(dst, strBytes...)
	dst = append(dst, ' ')
	dst = AppendUint(dst, br.Start)
	dst = append(dst, '-')
	dst = AppendUint(dst, br.End)
	dst = append(dst, '/')
	dst = AppendUint(dst, contentLength)
	dst = append(dst, strCRLF...)
	dst = append(dst, strCRLF...)
	return dst
}

// multipartByteRangesSize computes the total wire size of a
// multipart/byteranges response for the given ranges.
func multipartByteRangesSize(ranges []ByteRange, contentType string, contentLength int, boundary []byte) int {
	// Per-part framing is everything appendMultipartPartHeader writes plus
	// the separating CRLF. Digit counts vary per range, so the numeric parts
	// are added individually; all fixed pieces are folded into framingBase.
	framingBase := 2 + len(boundary) + 2 + // "--BOUNDARY\r\n"
		len(HeaderContentType) + 1 + len(contentType) + 2 + // "Content-Type: CT\r\n"
		len(HeaderContentRange) + 2 + len(strBytes) + 1 + // "Content-Range: bytes "
		1 + 1 + // '-' between offsets and '/' before the complete length
		2 + // CRLF ending the Content-Range header line
		2 + // blank line terminating the part headers
		2 // CRLF separating the payload from the next boundary

	total := 0
	for _, br := range ranges {
		total += framingBase
		total += decimalLen(br.Start) + decimalLen(br.End) + decimalLen(contentLength)
		total += br.End - br.Start + 1
	}
	// closing "--BOUNDARY--"
	total += 2 + len(boundary) + 2
	return total
}

// decimalLen returns the number of decimal digits in n >= 0.
func decimalLen(n int) int {
	if n == 0 {
		return 1
	}
	d := 0
	for n > 0 {
		n /= 10
		d++
	}
	return d
}

// newMultipartBoundary returns a cryptographically random boundary that
// never needs quoting and is extremely unlikely to appear in the payload.
func newMultipartBoundary() ([]byte, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	const hex = "0123456789abcdef"
	boundary := make([]byte, 0, len(multipartByteRangesBoundaryPrefix)+len(raw)*2)
	boundary = append(boundary, multipartByteRangesBoundaryPrefix...)
	for _, c := range raw {
		boundary = append(boundary, hex[c>>4], hex[c&0xf])
	}
	return boundary, nil
}

type cacheManager interface {
	Lock()
	Unlock()
	Close()
	DecReadersCount(ff *fsFile)
	GetFileFromCache(cacheKind CacheKind, path []byte) (*fsFile, bool)
	SetFileToCache(cacheKind CacheKind, path []byte, ff *fsFile) *fsFile
}

var (
	_ cacheManager = (*inMemoryCacheManager)(nil)
	_ cacheManager = (*noopCacheManager)(nil)
)

type CacheKind uint8

const (
	defaultCacheKind CacheKind = iota
	brotliCacheKind
	gzipCacheKind
	zstdCacheKind
)

func newCacheManager(fs *FS) cacheManager {
	if fs.SkipCache {
		return &noopCacheManager{}
	}

	cacheDuration := fs.CacheDuration
	if cacheDuration <= 0 {
		cacheDuration = FSHandlerCacheDuration
	}

	instance := &inMemoryCacheManager{
		cacheDuration: cacheDuration,
		cache:         make(map[string]*fsFile),
		cacheBrotli:   make(map[string]*fsFile),
		cacheGzip:     make(map[string]*fsFile),
		cacheZstd:     make(map[string]*fsFile),
		cleanStop:     make(chan struct{}),
	}

	go instance.handleCleanCache(fs.CleanStop)

	return instance
}

type noopCacheManager struct {
	cacheLock sync.Mutex
}

func (n *noopCacheManager) Lock() {
	n.cacheLock.Lock()
}

func (n *noopCacheManager) Unlock() {
	n.cacheLock.Unlock()
}

func (*noopCacheManager) Close() {}

func (n *noopCacheManager) DecReadersCount(ff *fsFile) {
	release := false

	n.cacheLock.Lock()
	ff.readersCount--
	if ff.readersCount < 0 {
		n.cacheLock.Unlock()
		panic("bug: fsFile.readersCount < 0")
	}
	if ff.readersCount == 0 {
		release = true
	}
	n.cacheLock.Unlock()

	if release {
		ff.Release()
	}
}

func (*noopCacheManager) GetFileFromCache(cacheKind CacheKind, path []byte) (*fsFile, bool) {
	return nil, false
}

func (n *noopCacheManager) SetFileToCache(cacheKind CacheKind, path []byte, ff *fsFile) *fsFile {
	n.cacheLock.Lock()
	ff.readersCount++
	n.cacheLock.Unlock()
	return ff
}

type inMemoryCacheManager struct {
	cache         map[string]*fsFile
	cacheBrotli   map[string]*fsFile
	cacheGzip     map[string]*fsFile
	cacheZstd     map[string]*fsFile
	cacheDuration time.Duration
	cleanStop     chan struct{}
	cleanStopOnce sync.Once
	pendingFiles  []*fsFile
	closed        bool
	cacheLock     sync.Mutex
}

func (cm *inMemoryCacheManager) Lock() {
	cm.cacheLock.Lock()
}

func (cm *inMemoryCacheManager) Unlock() {
	cm.cacheLock.Unlock()
}

func (cm *inMemoryCacheManager) Close() {
	filesToRelease := cm.close()
	for _, ff := range filesToRelease {
		ff.Release()
	}
}

func (cm *inMemoryCacheManager) close() []*fsFile {
	closedNow := false
	cm.cleanStopOnce.Do(func() {
		close(cm.cleanStop)
		closedNow = true
	})
	if !closedNow {
		return nil
	}

	cm.cacheLock.Lock()
	cm.closed = true
	filesToRelease := cm.collectAllFilesToReleaseNolock(nil)
	cm.cacheLock.Unlock()

	return filesToRelease
}

func (cm *inMemoryCacheManager) DecReadersCount(ff *fsFile) {
	release := false

	cm.cacheLock.Lock()
	ff.readersCount--
	if ff.readersCount < 0 {
		cm.cacheLock.Unlock()
		panic("bug: fsFile.readersCount < 0")
	}
	if cm.closed && ff.readersCount == 0 {
		release = true
		cm.removePendingFileNolock(ff)
	}
	cm.cacheLock.Unlock()

	if release {
		ff.Release()
	}
}

func (cm *inMemoryCacheManager) getFsCache(cacheKind CacheKind) map[string]*fsFile {
	fileCache := cm.cache
	switch cacheKind {
	case brotliCacheKind:
		fileCache = cm.cacheBrotli
	case gzipCacheKind:
		fileCache = cm.cacheGzip
	case zstdCacheKind:
		fileCache = cm.cacheZstd
	}

	return fileCache
}

func (cm *inMemoryCacheManager) GetFileFromCache(cacheKind CacheKind, path []byte) (*fsFile, bool) {
	cm.cacheLock.Lock()
	var ff *fsFile
	var ok bool
	if !cm.closed {
		fileCache := cm.getFsCache(cacheKind)
		ff, ok = fileCache[string(path)]
	}
	if ok {
		ff.readersCount++
	}
	cm.cacheLock.Unlock()

	return ff, ok
}

func (cm *inMemoryCacheManager) SetFileToCache(cacheKind CacheKind, path []byte, ff *fsFile) *fsFile {
	cm.cacheLock.Lock()
	if cm.closed {
		ff.readersCount++
		cm.cacheLock.Unlock()
		return ff
	}

	fileCache := cm.getFsCache(cacheKind)
	ff1, ok := fileCache[string(path)]
	if !ok {
		fileCache[string(path)] = ff
		ff.readersCount++
	} else {
		ff1.readersCount++
	}
	cm.cacheLock.Unlock()

	if ok {
		// The file has been already opened by another
		// goroutine, so close the current file and use
		// the file opened by another goroutine instead.
		ff.Release()
		ff = ff1
	}

	return ff
}

func (cm *inMemoryCacheManager) handleCleanCache(cleanStop chan struct{}) {
	clean := func() {
		filesToRelease := cm.cleanCache()
		for _, ff := range filesToRelease {
			ff.Release()
		}
	}

	t := time.NewTicker(cm.cacheDuration / 2)
	defer t.Stop()

	for {
		select {
		case <-t.C:
			clean()
		case <-cm.cleanStop:
			return
		case _, stillOpen := <-cleanStop:
			// Ignore values send on the channel, only stop when it is closed.
			if !stillOpen {
				filesToRelease := cm.close()
				for _, ff := range filesToRelease {
					ff.Release()
				}
				return
			}
		}
	}
}

func (cm *inMemoryCacheManager) cleanCache() []*fsFile {
	var filesToRelease []*fsFile

	cm.cacheLock.Lock()
	if cm.closed {
		cm.cacheLock.Unlock()
		return nil
	}

	// Close files which couldn't be closed before due to non-zero
	// readers count on the previous run.
	var remainingFiles []*fsFile
	for _, ff := range cm.pendingFiles {
		if ff.readersCount > 0 {
			remainingFiles = append(remainingFiles, ff)
		} else {
			filesToRelease = append(filesToRelease, ff)
		}
	}
	cm.pendingFiles = remainingFiles

	filesToRelease = cm.cleanCacheNolock(cm.cache, filesToRelease)
	filesToRelease = cm.cleanCacheNolock(cm.cacheBrotli, filesToRelease)
	filesToRelease = cm.cleanCacheNolock(cm.cacheGzip, filesToRelease)
	filesToRelease = cm.cleanCacheNolock(cm.cacheZstd, filesToRelease)

	cm.cacheLock.Unlock()

	return filesToRelease
}

func (cm *inMemoryCacheManager) cleanCacheNolock(cache map[string]*fsFile, filesToRelease []*fsFile) []*fsFile {
	t := time.Now()
	for k, ff := range cache {
		if t.Sub(ff.t) > cm.cacheDuration {
			filesToRelease = cm.addFileToReleaseNolock(filesToRelease, ff)
			delete(cache, k)
		}
	}
	return filesToRelease
}

func (cm *inMemoryCacheManager) collectAllFilesToReleaseNolock(filesToRelease []*fsFile) []*fsFile {
	filesToRelease = cm.collectCacheFilesToReleaseNolock(cm.cache, filesToRelease)
	filesToRelease = cm.collectCacheFilesToReleaseNolock(cm.cacheBrotli, filesToRelease)
	filesToRelease = cm.collectCacheFilesToReleaseNolock(cm.cacheGzip, filesToRelease)
	filesToRelease = cm.collectCacheFilesToReleaseNolock(cm.cacheZstd, filesToRelease)

	pendingFiles := cm.pendingFiles
	cm.pendingFiles = nil
	for _, ff := range pendingFiles {
		filesToRelease = cm.addFileToReleaseNolock(filesToRelease, ff)
	}
	return filesToRelease
}

func (cm *inMemoryCacheManager) collectCacheFilesToReleaseNolock(cache map[string]*fsFile, filesToRelease []*fsFile) []*fsFile {
	for k, ff := range cache {
		filesToRelease = cm.addFileToReleaseNolock(filesToRelease, ff)
		delete(cache, k)
	}
	return filesToRelease
}

func (cm *inMemoryCacheManager) addFileToReleaseNolock(filesToRelease []*fsFile, ff *fsFile) []*fsFile {
	if ff.readersCount > 0 {
		// There are pending readers on the file handle, so we cannot close it.
		// Put it into pendingFiles so it will be closed later.
		cm.pendingFiles = append(cm.pendingFiles, ff)
		return filesToRelease
	}
	return append(filesToRelease, ff)
}

func (cm *inMemoryCacheManager) removePendingFileNolock(ff *fsFile) {
	for i, pendingFile := range cm.pendingFiles {
		if pendingFile == ff {
			copy(cm.pendingFiles[i:], cm.pendingFiles[i+1:])
			cm.pendingFiles[len(cm.pendingFiles)-1] = nil
			cm.pendingFiles = cm.pendingFiles[:len(cm.pendingFiles)-1]
			return
		}
	}
}

func (h *fsHandler) pathToFilePath(path []byte, hasTrailingSlash bool) string {
	if hasTrailingSlash {
		path = path[:len(path)-1]
	}
	hasLeadingSlash := len(path) > 0 && path[0] == '/'

	if _, ok := h.filesystem.(*osFS); !ok {
		root := h.root
		if root == "." {
			root = ""
		}
		if len(path) < 1 || (hasLeadingSlash && len(path) == 1) {
			if h.root == "." {
				return "."
			}
			return root
		}

		if root == "" {
			if hasLeadingSlash {
				return string(path[1:])
			}
			return string(path)
		}

		// Use byte buffer pool to avoid string concatenation allocations.
		b := bytebufferpool.Get()
		defer bytebufferpool.Put(b)

		b.B = append(b.B, root...)
		b.B = append(b.B, '/')
		if hasLeadingSlash {
			b.B = append(b.B, path[1:]...)
		} else {
			b.B = append(b.B, path...)
		}

		return string(b.B)
	}

	// Use byte buffer pool to avoid string concatenation allocations
	b := bytebufferpool.Get()
	defer bytebufferpool.Put(b)

	b.B = append(b.B, h.root...)
	if hasLeadingSlash {
		b.B = append(b.B, path...)
	} else {
		if h.root != "" && len(path) > 0 {
			b.B = append(b.B, '/')
		}
		b.B = append(b.B, path...)
	}

	return filepath.FromSlash(string(b.B))
}

func (h *fsHandler) filePathToCompressed(filePath string) string {
	if h.root == h.compressRoot {
		return filePath
	}
	if !strings.HasPrefix(filePath, h.root) {
		return filePath
	}
	return filepath.FromSlash(h.compressRoot + filePath[len(h.root):])
}

func (h *fsHandler) handleRequest(ctx *RequestCtx) {
	var path []byte
	if h.pathRewrite != nil {
		path = h.pathRewrite(ctx)
	} else {
		path = ctx.Path()
	}
	hasTrailingSlash := len(path) > 0 && path[len(path)-1] == '/'

	if n := bytes.IndexByte(path, 0); n >= 0 {
		ctx.Logger().Printf("cannot serve path with nil byte at position %d: %q", n, path)
		ctx.Error("Are you a hacker?", StatusBadRequest)
		return
	}
	// Prevent request paths from reaching NTFS alternate data streams through
	// the default osFS. Custom filesystems may define their own colon syntax.
	if _, ok := h.filesystem.(*osFS); ok && hasWindowsReservedPathColon(path, h.root == "") {
		ctx.Logger().Printf("cannot serve path with a Windows-reserved ':' character: %q", path)
		ctx.Error("Forbidden", StatusForbidden)
		return
	}

	// Rewritten paths bypass ctx.Path()'s normalization, so they must be
	// checked for '..' segments here. On Windows every path is checked
	// regardless: ctx.Path() normalization only treats '/' as a separator,
	// so backslash traversal such as `\..\` and `\../` survives it and
	// filepath.FromSlash later turns it into a real parent-directory jump
	// (https://github.com/valyala/fasthttp/issues/1691).
	if h.pathRewrite != nil || filepath.Separator == '\\' {
		if hasDotDotPathSegment(path) {
			ctx.Logger().Printf("cannot serve path with '..' path segment due to security reasons: %q", path)
			ctx.Error("Internal Server Error", StatusInternalServerError)
			return
		}
	}

	mustCompress := false
	fileCacheKind := defaultCacheKind
	fileEncoding := ""
	byteRange := ctx.Request.Header.peek(strRange)
	if len(byteRange) == 0 && h.compress {
		switch {
		case h.compressBrotli && ctx.Request.Header.HasAcceptEncodingBytes(strBr):
			mustCompress = true
			fileCacheKind = brotliCacheKind
			fileEncoding = "br"
		case h.compressZstd && ctx.Request.Header.HasAcceptEncodingBytes(strZstd):
			mustCompress = true
			fileCacheKind = zstdCacheKind
			fileEncoding = "zstd"
		case ctx.Request.Header.HasAcceptEncodingBytes(strGzip):
			mustCompress = true
			fileCacheKind = gzipCacheKind
			fileEncoding = "gzip"
		}
	}

	ff, ok := h.cacheManager.GetFileFromCache(fileCacheKind, path)
	if !ok {
		filePath := h.pathToFilePath(path, hasTrailingSlash)

		var err error
		ff, err = h.openFSFile(filePath, mustCompress, fileEncoding)
		if mustCompress && err == errNoCreatePermission {
			ctx.Logger().Printf("insufficient permissions for saving compressed file for %q. Serving uncompressed file. "+
				"Allow write access to the directory with this file in order to improve fasthttp performance", filePath)
			mustCompress = false
			ff, err = h.openFSFile(filePath, mustCompress, fileEncoding)
		}

		if errors.Is(err, errDirIndexRequired) {
			if !hasTrailingSlash {
				ctx.RedirectBytes(append(path, '/'), StatusFound)
				return
			}
			ff, err = h.openIndexFile(ctx, filePath, mustCompress, fileEncoding)
			if err != nil {
				ctx.Logger().Printf("cannot open dir index %q: %v", filePath, err)
				ctx.Error("Directory index is forbidden", StatusForbidden)
				return
			}
		} else if err != nil {
			ctx.Logger().Printf("cannot open file %q: %v", filePath, err)
			if h.pathNotFound == nil {
				ctx.Error("Cannot open requested path", StatusNotFound)
			} else {
				ctx.SetStatusCode(StatusNotFound)
				h.pathNotFound(ctx)
			}
			return
		}

		ff = h.cacheManager.SetFileToCache(fileCacheKind, path, ff)
	}

	if !ctx.IfModifiedSince(ff.lastModified) {
		ff.decReadersCount()
		ctx.NotModified()
		return
	}

	r, err := ff.NewReader()
	if err != nil {
		ff.decReadersCount()
		ctx.Logger().Printf("cannot obtain file reader for path=%q: %v", path, err)
		ctx.Error("Internal Server Error", StatusInternalServerError)
		return
	}

	hdr := &ctx.Response.Header
	if ff.compressed {
		switch fileEncoding {
		case "br":
			hdr.SetContentEncodingBytes(strBr)
			hdr.addVaryBytes(strAcceptEncoding)
		case "gzip":
			hdr.SetContentEncodingBytes(strGzip)
			hdr.addVaryBytes(strAcceptEncoding)
		case "zstd":
			hdr.SetContentEncodingBytes(strZstd)
			hdr.addVaryBytes(strAcceptEncoding)
		}
	}

	statusCode := StatusOK
	contentLength := ff.contentLength
	if h.acceptByteRange {
		hdr.setNonSpecial(strAcceptRanges, strBytes)
		if len(byteRange) > 0 && ifRangeMatches(ctx, ff) {
			ranges, err := ParseByteRanges(byteRange, contentLength, h.maxByteRanges)
			if err != nil {
				if rc, ok := r.(io.Closer); ok {
					_ = rc.Close()
				}
				ctx.Logger().Printf("cannot parse byte range %q for path=%q: %v", byteRange, path, err)
				sendRangeNotSatisfiable(ctx, contentLength)
				return
			}

			if len(ranges) == 1 {
				startPos, endPos := ranges[0].Start, ranges[0].End
				if err = r.(byteRangeUpdater).UpdateByteRange(startPos, endPos); err != nil { //nolint:forcetypeassert
					if rc, ok := r.(io.Closer); ok {
						_ = rc.Close()
					}
					ctx.Logger().Printf("cannot seek byte range %q for path=%q: %v", byteRange, path, err)
					ctx.Error("Internal Server Error", StatusInternalServerError)
					return
				}

				hdr.SetContentRange(startPos, endPos, contentLength)
				contentLength = endPos - startPos + 1
				statusCode = StatusPartialContent
			} else {
				boundary, err := newMultipartBoundary()
				if err != nil {
					if rc, ok := r.(io.Closer); ok {
						_ = rc.Close()
					}
					ctx.Logger().Printf("cannot generate multipart boundary for path=%q: %v", path, err)
					ctx.Error("Internal Server Error", StatusInternalServerError)
					return
				}

				mr := newMultiRangeReader(r, ranges, ff.contentType, contentLength, boundary)
				contentLength = multipartByteRangesSize(ranges, ff.contentType, contentLength, boundary)

				mt := make([]byte, 0, len(strMultipartByteranges)+len(boundary)+11)
				mt = append(mt, strMultipartByteranges...)
				mt = append(mt, ';', ' ')
				mt = append(mt, strBoundary...)
				mt = append(mt, '=')
				mt = append(mt, boundary...)
				hdr.SetContentTypeBytes(mt)

				r = mr
				statusCode = StatusPartialContent
			}
		}
	}

	hdr.setNonSpecial(strLastModified, ff.lastModifiedStr)
	if !ctx.IsHead() {
		ctx.SetBodyStream(r, contentLength)
	} else {
		ctx.Response.ResetBody()
		ctx.Response.SkipBody = true
		ctx.Response.Header.SetContentLength(contentLength)
		if rc, ok := r.(io.Closer); ok {
			if err := rc.Close(); err != nil {
				ctx.Logger().Printf("cannot close file reader: %v", err)
				ctx.Error("Internal Server Error", StatusInternalServerError)
				return
			}
		}
	}
	hdr.noDefaultContentType = true
	if len(hdr.ContentType()) == 0 {
		ctx.SetContentType(ff.contentType)
	}
	ctx.SetStatusCode(statusCode)
}

type byteRangeUpdater interface {
	UpdateByteRange(startPos, endPos int) error
}

// ifRangeMatches reports whether a present 'If-Range' precondition still
// matches the representation. fasthttp serves Last-Modified timestamps but
// no entity tags, so only the HTTP-date form may match: a matching date means
// the Range request may be evaluated, anything else makes the server ignore
// the Range header and return the full representation (RFC 7233 3.2/4.3).
func ifRangeMatches(ctx *RequestCtx, ff *fsFile) bool {
	ifRange := ctx.Request.Header.peek(strIfRange)
	if len(ifRange) == 0 {
		return true
	}
	// Entity tags (including the weak form) never match since the FS handler
	// does not generate ETags.
	if bytes.HasPrefix(ifRange, []byte("\"")) || bytes.HasPrefix(ifRange, []byte("W/\"")) {
		return false
	}
	t, err := ParseHTTPDate(ifRange)
	if err != nil {
		return false
	}
	lastModified := ff.lastModified.Truncate(time.Second)
	return !t.Before(lastModified) && !t.After(lastModified)
}

// sendRangeNotSatisfiable responds with 416 and the required
// 'Content-Range: bytes */<complete-length>' header (RFC 7233, section 4.4).
func sendRangeNotSatisfiable(ctx *RequestCtx, contentLength int) {
	ctx.Response.Reset()
	ctx.SetStatusCode(StatusRequestedRangeNotSatisfiable)
	ctx.SetContentTypeBytes(defaultContentType)
	ctx.Response.Header.setNonSpecial(strAcceptRanges, strBytes)

	cr := make([]byte, 0, len(strBytes)+2+16)
	cr = append(cr, strBytes...)
	cr = append(cr, ' ', '*', '/')
	cr = AppendUint(cr, contentLength)
	ctx.Response.Header.setNonSpecial(strContentRange, cr)
	ctx.SetBodyString("Range Not Satisfiable")
}

// ByteRange is a single byte range [Start, End] (inclusive). Both fields
// are byte offsets within the representation, so End-Start+1 is the range
// length.
type ByteRange struct {
	Start int
	End   int
}

// maxByteRanges bounds the number of ranges accepted in a single request so
// a client cannot force the server to build a response consisting of huge
// numbers of tiny parts.
const maxByteRanges = 100

// ParseByteRanges parses 'Range: bytes=...' header value, which may contain
// one or more comma-separated byte-range specs as defined by RFC 7233
// section 2.1. At most maxRanges specs are accepted. A zero value falls
// back to the built-in maxByteRanges limit; a negative value disables the
// limit entirely:
//
//   - 'bytes=start-end' for an explicit range,
//   - 'bytes=start-'    for an open-ended range (up to the end),
//   - 'bytes=-suffix'   for a suffix range (last suffix bytes).
//
// Returned ranges are clipped to contentLength, sorted by their start
// position and merged when they overlap or are adjacent. Range specs that do
// not intersect the content are dropped. An error is returned only if the
// header is malformed, more than maxByteRanges specs were given, or none of
// the requested ranges intersects the content.
//
// Numeric values are parsed as unsigned 64-bit integers. Overflowing values
// saturate instead of failing, which mirrors the open-ended semantics of
// excessively large positions and prevents signed integer wraparound on
// 32-bit builds.
func ParseByteRanges(byteRange []byte, contentLength, maxRanges int) ([]ByteRange, error) {
	if maxRanges == 0 {
		maxRanges = maxByteRanges
	}
	if contentLength <= 0 {
		return nil, fmt.Errorf("byte range %q is invalid for empty content", byteRange)
	}
	if !bytes.HasPrefix(byteRange, strBytes) {
		return nil, fmt.Errorf("unsupported range units: %q: expecting %q", byteRange, strBytes)
	}
	b := byteRange[len(strBytes):]
	if len(b) == 0 || b[0] != '=' {
		return nil, fmt.Errorf("missing byte range in %q", byteRange)
	}
	b = b[1:]
	if len(b) == 0 {
		return nil, fmt.Errorf("missing byte range in %q", byteRange)
	}

	ranges := make([]ByteRange, 0, 4)
	specCount := 0
	for _, spec := range bytes.Split(b, []byte{','}) {
		spec = trimByteRangeSpec(spec)
		if len(spec) == 0 {
			return nil, fmt.Errorf("empty byte range spec in %q", byteRange)
		}
		specCount++
		if maxRanges >= 0 && specCount > maxRanges {
			return nil, fmt.Errorf("too many byte ranges in %q: maximum allowed is %d", byteRange, maxRanges)
		}

		dash := bytes.IndexByte(spec, '-')
		if dash < 0 {
			return nil, fmt.Errorf("missing the end position of byte range in %q", byteRange)
		}

		if dash == 0 {
			suffixLen, ok := parseByteRangeUint(spec[1:])
			if !ok {
				return nil, fmt.Errorf("invalid suffix byte range in %q", byteRange)
			}
			// A zero-length suffix is unsatisfiable: it denotes an empty
			// suffix of the representation (RFC 7233, section 2.1).
			if suffixLen == 0 {
				continue
			}
			start := contentLength
			if suffixLen < uint64(contentLength) {
				start = int(suffixLen)
			}
			start = contentLength - start
			ranges = append(ranges, ByteRange{Start: start, End: contentLength - 1})
			continue
		}

		start, ok := parseByteRangeUint(spec[:dash])
		if !ok {
			return nil, fmt.Errorf("invalid start position of byte range in %q", byteRange)
		}
		if start >= uint64(contentLength) {
			// This range does not intersect the representation.
			continue
		}
		br := ByteRange{Start: int(start), End: contentLength - 1}

		endSpec := trimByteRangeSpec(spec[dash+1:])
		if len(endSpec) > 0 {
			end, ok := parseByteRangeUint(endSpec)
			if !ok {
				return nil, fmt.Errorf("invalid end position of byte range in %q", byteRange)
			}
			if end < start {
				return nil, fmt.Errorf("the start position of byte range cannot exceed the end position. byte range %q", byteRange)
			}
			if end < uint64(contentLength) {
				br.End = int(end)
			}
		}
		ranges = append(ranges, br)
	}

	if len(ranges) == 0 {
		return nil, fmt.Errorf("none of the requested ranges overlap content of length %d. byte range %q", contentLength, byteRange)
	}

	return mergeByteRanges(ranges), nil
}

// trimByteRangeSpec strips optional whitespace around a byte-range spec.
// RFC 7233 allows a single SP or HTAB around the comma separator.
func trimByteRangeSpec(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t') {
		b = b[1:]
	}
	for len(b) > 0 {
		c := b[len(b)-1]
		if c != ' ' && c != '\t' {
			break
		}
		b = b[:len(b)-1]
	}
	return b
}

// parseByteRangeUint parses an unsigned decimal integer, saturating at
// math.MaxUint64 when the value overflows instead of returning an error.
// It returns ok=false only for empty or non-numeric input.
func parseByteRangeUint(b []byte) (v uint64, ok bool) {
	if len(b) == 0 {
		return 0, false
	}
	const maxUint64 = uint64(1<<64 - 1)
	const cutoff = maxUint64 / 10
	for _, c := range b {
		k := c - '0'
		if k > 9 {
			return 0, false
		}
		if v > cutoff || (v == cutoff && uint64(k) > maxUint64-cutoff*10) {
			v = maxUint64
			continue
		}
		v = 10*v + uint64(k)
	}
	return v, true
}

// mergeByteRanges sorts the given ranges by start position and merges
// overlapping or adjacent ones, so every byte of the representation appears
// at most once in the response.
func mergeByteRanges(ranges []ByteRange) []ByteRange {
	sort.Slice(ranges, func(i, j int) bool {
		if ranges[i].Start == ranges[j].Start {
			return ranges[i].End < ranges[j].End
		}
		return ranges[i].Start < ranges[j].Start
	})

	merged := ranges[:1]
	for i := 1; i < len(ranges); i++ {
		last := &merged[len(merged)-1]
		cur := ranges[i]
		// Adjacent ranges (cur.Start == last.End+1) are merged too: their
		// multipart framing costs bytes without saving any payload.
		if cur.Start <= last.End+1 {
			if cur.End > last.End {
				last.End = cur.End
			}
			continue
		}
		merged = append(merged, cur)
	}
	return merged
}

// ParseByteRange parses 'Range: bytes=...' header value containing a single
// byte range.
//
// It follows https://www.w3.org/Protocols/rfc2616/rfc2616-sec14.html#sec14.35 .
func ParseByteRange(byteRange []byte, contentLength int) (startPos, endPos int, err error) {
	ranges, err := ParseByteRanges(byteRange, contentLength, maxByteRanges)
	if err != nil {
		return 0, 0, err
	}
	if len(ranges) != 1 {
		return 0, 0, fmt.Errorf("expecting a single byte range in %q", byteRange)
	}
	return ranges[0].Start, ranges[0].End, nil
}

func (h *fsHandler) openIndexFile(ctx *RequestCtx, dirPath string, mustCompress bool, fileEncoding string) (*fsFile, error) {
	for _, indexName := range h.indexNames {
		indexFilePath := indexName
		if dirPath != "" {
			indexFilePath = dirPath + "/" + indexName
		}

		ff, err := h.openFSFile(indexFilePath, mustCompress, fileEncoding)
		if err == nil {
			return ff, nil
		}
		if mustCompress && err == errNoCreatePermission {
			ctx.Logger().Printf("insufficient permissions for saving compressed file for %q. Serving uncompressed file. "+
				"Allow write access to the directory with this file in order to improve fasthttp performance", indexFilePath)
			mustCompress = false
			return h.openFSFile(indexFilePath, mustCompress, fileEncoding)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("cannot open file %q: %w", indexFilePath, err)
		}
	}

	if !h.generateIndexPages {
		return nil, fmt.Errorf("cannot access directory without index page: directory %q", dirPath)
	}

	return h.createDirIndex(ctx, dirPath, mustCompress, fileEncoding)
}

var (
	errDirIndexRequired   = errors.New("directory index required")
	errNoCreatePermission = errors.New("no 'create file' permissions")
)

func (h *fsHandler) createDirIndex(ctx *RequestCtx, dirPath string, mustCompress bool, fileEncoding string) (*fsFile, error) {
	w := &bytebufferpool.ByteBuffer{}

	base := ctx.URI()

	// io/fs doesn't support ReadDir with empty path.
	if dirPath == "" {
		dirPath = "."
	}

	basePathEscaped := html.EscapeString(string(base.Path()))
	_, _ = fmt.Fprintf(w, "<html><head><title>%s</title><style>.dir { font-weight: bold }</style></head><body>", basePathEscaped)
	_, _ = fmt.Fprintf(w, "<h1>%s</h1>", basePathEscaped)
	_, _ = fmt.Fprintf(w, "<ul>")

	if len(basePathEscaped) > 1 {
		var parentURI URI
		base.CopyTo(&parentURI)
		parentURI.Update(string(base.Path()) + "/..")
		parentPathEscaped := html.EscapeString(string(parentURI.Path()))
		_, _ = fmt.Fprintf(w, `<li><a href="%s" class="dir">..</a></li>`, parentPathEscaped)
	}

	dirEntries, err := fs.ReadDir(h.filesystem, dirPath)
	if err != nil {
		return nil, err
	}

	fm := make(map[string]fs.FileInfo, len(dirEntries))
	filenames := make([]string, 0, len(dirEntries))
nestedContinue:
	for _, de := range dirEntries {
		name := de.Name()
		for _, cfs := range h.compressedFileSuffixes {
			if strings.HasSuffix(name, cfs) {
				// Do not show compressed files on index page.
				continue nestedContinue
			}
		}
		fi, err := de.Info()
		if err != nil {
			ctx.Logger().Printf("cannot fetch information from dir entry %q: %v, skip", name, err)

			continue nestedContinue
		}

		fm[name] = fi
		filenames = append(filenames, name)
	}

	var u URI
	base.CopyTo(&u)
	u.Update(string(u.Path()) + "/")

	sort.Strings(filenames)
	for _, name := range filenames {
		u.Update(name)
		pathEscaped := html.EscapeString(string(u.Path()))
		fi := fm[name]
		auxStr := "dir"
		className := "dir"
		if !fi.IsDir() {
			auxStr = fmt.Sprintf("file, %d bytes", fi.Size())
			className = "file"
		}
		_, _ = fmt.Fprintf(w, `<li><a href="%s" class="%s">%s</a>, %s, last modified %s</li>`,
			pathEscaped, className, html.EscapeString(name), auxStr, fsModTime(fi.ModTime()))
	}

	_, _ = fmt.Fprintf(w, "</ul></body></html>")

	if mustCompress {
		var zbuf bytebufferpool.ByteBuffer
		switch fileEncoding {
		case "br":
			zbuf.B = AppendBrotliBytesLevel(zbuf.B, w.B, CompressDefaultCompression)
		case "gzip":
			zbuf.B = AppendGzipBytesLevel(zbuf.B, w.B, CompressDefaultCompression)
		case "zstd":
			zbuf.B = AppendZstdBytesLevel(zbuf.B, w.B, CompressZstdDefault)
		}
		w = &zbuf
	}

	dirIndex := w.B
	lastModified := time.Now()
	ff := &fsFile{
		h:               h,
		dirIndex:        dirIndex,
		contentType:     "text/html; charset=utf-8",
		contentLength:   len(dirIndex),
		compressed:      mustCompress,
		lastModified:    lastModified,
		lastModifiedStr: AppendHTTPDate(nil, lastModified),

		t: lastModified,
	}
	return ff, nil
}

const (
	fsMinCompressRatio        = 0.8
	fsMaxCompressibleFileSize = 8 * 1024 * 1024
)

func (h *fsHandler) compressAndOpenFSFile(filePath, fileEncoding string) (*fsFile, error) {
	f, err := h.filesystem.Open(filePath)
	if err != nil {
		return nil, err
	}

	fileInfo, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("cannot obtain info for file %q: %w", filePath, err)
	}

	if fileInfo.IsDir() {
		_ = f.Close()
		return nil, errDirIndexRequired
	}

	if strings.HasSuffix(filePath, h.compressedFileSuffixes[fileEncoding]) ||
		fileInfo.Size() > fsMaxCompressibleFileSize ||
		!isFileCompressible(f, fsMinCompressRatio) {
		return h.newFSFile(f, fileInfo, false, filePath, "")
	}

	compressedFilePath := h.filePathToCompressed(filePath)

	if _, ok := h.filesystem.(*osFS); !ok {
		return h.newCompressedFSFileCache(f, fileInfo, compressedFilePath, fileEncoding)
	}

	if compressedFilePath != filePath {
		if err := os.MkdirAll(filepath.Dir(compressedFilePath), 0o750); err != nil {
			return nil, err
		}
	}
	compressedFilePath += h.compressedFileSuffixes[fileEncoding]

	absPath, err := filepath.Abs(compressedFilePath)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("cannot determine absolute path for %q: %v", compressedFilePath, err)
	}

	flock := acquireFileLock(absPath)
	flock.mu.Lock()
	defer func() {
		flock.mu.Unlock()
		releaseFileLock(absPath, flock)
	}()
	return h.compressFileNolock(f, fileInfo, filePath, compressedFilePath, fileEncoding)
}

func (h *fsHandler) compressFileNolock(
	f fs.File, fileInfo fs.FileInfo, filePath, compressedFilePath, fileEncoding string,
) (*fsFile, error) {
	// Attempt to open compressed file created by another concurrent
	// goroutine.
	// It is safe opening such a file, since the file creation
	// is guarded by file mutex - see getFileLock call.
	if _, err := os.Stat(compressedFilePath); err == nil {
		_ = f.Close()
		return h.newCompressedFSFile(compressedFilePath, fileEncoding)
	}

	// Create temporary file, so concurrent goroutines don't use
	// it until it is created.
	//
	// os.CreateTemp gives the file a random name and opens it with O_EXCL and
	// 0600, so a symlink pre-planted at the otherwise predictable temp path
	// can't be followed to truncate an arbitrary file, and the cache file
	// isn't left group/world-readable.
	zf, err := os.CreateTemp(filepath.Dir(compressedFilePath), filepath.Base(compressedFilePath)+".tmp-*")
	if err != nil {
		_ = f.Close()
		if !errors.Is(err, fs.ErrPermission) {
			return nil, fmt.Errorf("cannot create temporary file for %q: %w", compressedFilePath, err)
		}
		return nil, errNoCreatePermission
	}
	tmpFilePath := zf.Name()
	switch fileEncoding {
	case "br":
		zw := acquireStacklessBrotliWriter(zf, CompressDefaultCompression)
		_, err = copyZeroAlloc(zw, f)
		if errf := zw.Flush(); err == nil {
			err = errf
		}
		releaseStacklessBrotliWriter(zw, CompressDefaultCompression)
	case "gzip":
		zw := acquireStacklessGzipWriter(zf, CompressDefaultCompression)
		_, err = copyZeroAlloc(zw, f)
		if errf := zw.Flush(); err == nil {
			err = errf
		}
		releaseStacklessGzipWriter(zw, CompressDefaultCompression)
	case "zstd":
		zw := acquireStacklessZstdWriter(zf, CompressZstdDefault)
		_, err = copyZeroAlloc(zw, f)
		if errf := zw.Flush(); err == nil {
			err = errf
		}
		releaseStacklessZstdWriter(zw, CompressZstdDefault)
	}
	_ = zf.Close()
	_ = f.Close()
	if err != nil {
		_ = os.Remove(tmpFilePath)
		return nil, fmt.Errorf("error when compressing file %q to %q: %w", filePath, tmpFilePath, err)
	}
	if err = os.Chtimes(tmpFilePath, time.Now(), fileInfo.ModTime()); err != nil {
		_ = os.Remove(tmpFilePath)
		return nil, fmt.Errorf("cannot change modification time to %v for tmp file %q: %v",
			fileInfo.ModTime(), tmpFilePath, err)
	}
	if err = os.Rename(tmpFilePath, compressedFilePath); err != nil {
		_ = os.Remove(tmpFilePath)
		return nil, fmt.Errorf("cannot move compressed file from %q to %q: %w", tmpFilePath, compressedFilePath, err)
	}
	return h.newCompressedFSFile(compressedFilePath, fileEncoding)
}

// newCompressedFSFileCache use memory cache compressed files.
func (h *fsHandler) newCompressedFSFileCache(f fs.File, fileInfo fs.FileInfo, filePath, fileEncoding string) (*fsFile, error) {
	var (
		w   = &bytebufferpool.ByteBuffer{}
		err error
	)

	switch fileEncoding {
	case "br":
		zw := acquireStacklessBrotliWriter(w, CompressDefaultCompression)
		_, err = copyZeroAlloc(zw, f)
		if errf := zw.Flush(); err == nil {
			err = errf
		}
		releaseStacklessBrotliWriter(zw, CompressDefaultCompression)
	case "gzip":
		zw := acquireStacklessGzipWriter(w, CompressDefaultCompression)
		_, err = copyZeroAlloc(zw, f)
		if errf := zw.Flush(); err == nil {
			err = errf
		}
		releaseStacklessGzipWriter(zw, CompressDefaultCompression)
	case "zstd":
		zw := acquireStacklessZstdWriter(w, CompressZstdDefault)
		_, err = copyZeroAlloc(zw, f)
		if errf := zw.Flush(); err == nil {
			err = errf
		}
		releaseStacklessZstdWriter(zw, CompressZstdDefault)
	}
	defer func() { _ = f.Close() }()

	if err != nil {
		return nil, fmt.Errorf("error when compressing file %q: %w", filePath, err)
	}

	seeker, ok := f.(io.Seeker)
	if !ok {
		return nil, errors.New("seek is not implemented")
	}
	if _, err = seeker.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	ext := fileExtension(fileInfo.Name(), false, h.compressedFileSuffixes[fileEncoding])
	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		data, err := readFileHeader(f, false, fileEncoding)
		if err != nil {
			return nil, fmt.Errorf("cannot read header of the file %q: %w", fileInfo.Name(), err)
		}
		contentType = http.DetectContentType(data)
	}

	dirIndex := w.B
	lastModified := fileInfo.ModTime()
	ff := &fsFile{
		h:               h,
		dirIndex:        dirIndex,
		contentType:     contentType,
		contentLength:   len(dirIndex),
		compressed:      true,
		lastModified:    lastModified,
		lastModifiedStr: AppendHTTPDate(nil, lastModified),

		t: time.Now(),
	}

	return ff, nil
}

func (h *fsHandler) newCompressedFSFile(filePath, fileEncoding string) (*fsFile, error) {
	f, err := h.filesystem.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("cannot open compressed file %q: %w", filePath, err)
	}
	fileInfo, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("cannot obtain info for compressed file %q: %w", filePath, err)
	}
	return h.newFSFile(f, fileInfo, true, filePath, fileEncoding)
}

func (h *fsHandler) openFSFile(filePath string, mustCompress bool, fileEncoding string) (*fsFile, error) {
	filePathOriginal := filePath
	if mustCompress {
		filePath += h.compressedFileSuffixes[fileEncoding]
	}
	f, err := h.filesystem.Open(filePath)
	if err != nil {
		if mustCompress && errors.Is(err, fs.ErrNotExist) {
			return h.compressAndOpenFSFile(filePathOriginal, fileEncoding)
		}

		// If the file is not found and the path is empty, let's return errDirIndexRequired error.
		if filePath == "" && (errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrInvalid)) {
			return nil, errDirIndexRequired
		}

		return nil, err
	}

	fileInfo, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("cannot obtain info for file %q: %w", filePath, err)
	}

	if fileInfo.IsDir() {
		_ = f.Close()
		if mustCompress {
			return nil, fmt.Errorf("directory with unexpected suffix found: %q: suffix: %q",
				filePath, h.compressedFileSuffixes[fileEncoding])
		}
		return nil, errDirIndexRequired
	}

	if mustCompress {
		fileInfoOriginal, err := fs.Stat(h.filesystem, filePathOriginal)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("cannot obtain info for original file %q: %w", filePathOriginal, err)
		}

		// Only re-create the compressed file if there was more than a second between the mod times.
		// On macOS the gzip seems to truncate the nanoseconds in the mod time causing the original file
		// to look newer than the gzipped file.
		if fileInfoOriginal.ModTime().Sub(fileInfo.ModTime()) >= time.Second {
			// The compressed file became stale. Re-create it.
			_ = f.Close()
			_ = os.Remove(filePath)
			return h.compressAndOpenFSFile(filePathOriginal, fileEncoding)
		}
	}

	return h.newFSFile(f, fileInfo, mustCompress, filePath, fileEncoding)
}

func (h *fsHandler) newFSFile(f fs.File, fileInfo fs.FileInfo, compressed bool, filePath, fileEncoding string) (*fsFile, error) {
	n := fileInfo.Size()
	contentLength := int(n)
	if n != int64(contentLength) {
		_ = f.Close()
		return nil, fmt.Errorf("too big file: %d bytes", n)
	}

	// detect content-type
	ext := fileExtension(fileInfo.Name(), compressed, h.compressedFileSuffixes[fileEncoding])
	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		data, err := readFileHeader(f, compressed, fileEncoding)
		if err != nil {
			return nil, fmt.Errorf("cannot read header of the file %q: %w", fileInfo.Name(), err)
		}
		contentType = http.DetectContentType(data)
	}

	lastModified := fileInfo.ModTime()
	ff := &fsFile{
		h:               h,
		f:               f,
		filename:        filePath,
		contentType:     contentType,
		contentLength:   contentLength,
		compressed:      compressed,
		lastModified:    lastModified,
		lastModifiedStr: AppendHTTPDate(nil, lastModified),

		t: time.Now(),
	}
	return ff, nil
}

func readFileHeader(f io.Reader, compressed bool, fileEncoding string) ([]byte, error) {
	r := f
	var (
		br  *brotli.Reader
		zr  *gzip.Reader
		zsr *zstd.Decoder
	)
	if compressed {
		var err error
		switch fileEncoding {
		case "br":
			br = acquireBrotliReader(f)
			r = br
		case "gzip":
			if zr, err = acquireGzipReader(f); err != nil {
				return nil, err
			}
			r = zr
		case "zstd":
			if zsr, err = acquireZstdReader(f); err != nil {
				return nil, err
			}
			r = zsr
		}
	}

	lr := &io.LimitedReader{
		R: r,
		N: 512,
	}
	data, err := io.ReadAll(lr)
	seeker, ok := f.(io.Seeker)
	if !ok {
		return nil, errors.New("must implement seek")
	}
	if _, err := seeker.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	if br != nil {
		releaseBrotliReader(br)
	}

	if zr != nil {
		releaseGzipReader(zr)
	}

	if zsr != nil {
		releaseZstdReader(zsr)
	}

	return data, err
}

func stripLeadingSlashes(path []byte, stripSlashes int) []byte {
	for stripSlashes > 0 && len(path) > 0 {
		if path[0] != '/' {
			// developer sanity-check
			panic("BUG: path must start with slash")
		}
		n := bytes.IndexByte(path[1:], '/')
		if n < 0 {
			path = path[:0]
			break
		}
		path = path[n+1:]
		stripSlashes--
	}
	return path
}

func hasDotDotPathSegment(path []byte) bool {
	segmentStart := 0
	for i, c := range path {
		if c != '/' && (filepath.Separator != '\\' || c != '\\') {
			continue
		}
		if i-segmentStart == 2 && path[segmentStart] == '.' && path[segmentStart+1] == '.' {
			return true
		}
		segmentStart = i + 1
	}
	return len(path)-segmentStart == 2 &&
		path[segmentStart] == '.' &&
		path[segmentStart+1] == '.'
}

func fileExtension(path string, compressed bool, compressedFileSuffix string) string {
	if compressed && strings.HasSuffix(path, compressedFileSuffix) {
		path = path[:len(path)-len(compressedFileSuffix)]
	}
	n := strings.LastIndexByte(path, '.')
	if n < 0 {
		return ""
	}
	return path[n:]
}

// FileLastModified returns last modified time for the file.
func FileLastModified(path string) (time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return zeroTime, err
	}
	fileInfo, err := f.Stat()
	_ = f.Close()
	if err != nil {
		return zeroTime, err
	}
	return fsModTime(fileInfo.ModTime()), nil
}

func fsModTime(t time.Time) time.Time {
	return t.In(time.UTC).Truncate(time.Second)
}

var (
	filesLockMu  sync.Mutex
	filesLockMap = make(map[string]*fileLock)
)

type fileLock struct {
	mu sync.Mutex
	// refs counts goroutines that hold or are waiting on mu.
	refs int
}

func acquireFileLock(absPath string) *fileLock {
	filesLockMu.Lock()
	flock := filesLockMap[absPath]
	if flock == nil {
		flock = &fileLock{}
		filesLockMap[absPath] = flock
	}
	flock.refs++
	filesLockMu.Unlock()
	return flock
}

func releaseFileLock(absPath string, flock *fileLock) {
	filesLockMu.Lock()
	flock.refs--
	if flock.refs == 0 && filesLockMap[absPath] == flock {
		delete(filesLockMap, absPath)
	}
	filesLockMu.Unlock()
}

var _ fs.FS = (*osFS)(nil)

type osFS struct{}

func (o *osFS) Open(name string) (fs.File, error)     { return os.Open(name) }
func (o *osFS) Stat(name string) (fs.FileInfo, error) { return os.Stat(name) }
