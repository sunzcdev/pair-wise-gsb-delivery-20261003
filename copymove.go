package webdav

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/emersion/go-webdav/internal"
)

// CopyMoveParams holds the fully resolved parameters of a COPY or MOVE
// request after all server-side preconditions have been verified. Backends
// implementing AtomicCopyMover receive those parameters and are expected to
// perform the operation atomically (e.g. by delegating it to the object
// storage service), so none of the checks performed by the server need to be
// repeated.
type CopyMoveParams struct {
	// Src is the decoded, slash-normalized path of the source resource.
	Src string
	// Dst is the decoded, slash-normalized path of the destination
	// resource.
	Dst string
	// SrcIsDir indicates whether the source resource is a collection.
	SrcIsDir bool
	// DstExists indicates whether a resource already exists at the
	// destination path.
	DstExists bool
	// DstIsDir indicates whether an existing destination is a collection.
	DstIsDir bool
	// Move is true for a MOVE request and false for a COPY request.
	Move bool
	// Recursive indicates whether the operation applies to the source
	// resource and all of its members. It is always true for MOVE
	// requests and reflects the Depth header for COPY requests.
	Recursive bool
	// Overwrite indicates whether an existing destination resource may be
	// replaced. When it is false, DstExists is guaranteed to be false.
	Overwrite bool
}

// AtomicCopyMover is implemented by FileSystem backends which can execute a
// COPY or MOVE operation atomically. The provided params are the result of
// the server-side destination resolution and preflight checks. The returned
// created value is true when a new resource has been created at the
// destination, and false when an existing resource has been overwritten.
type AtomicCopyMover interface {
	CopyMove(ctx context.Context, params *CopyMoveParams) (created bool, err error)
}

func httpError(code int, cause error) error {
	return &internal.HTTPError{Code: code, Err: cause}
}

// resolveHrefPath validates and canonicalizes an href that targets this
// service. It returns the decoded absolute path together with a flag
// indicating whether the href ends with a slash (i.e. addresses a
// collection by convention).
//
// Comparison and lookup must be done on the decoded path segments rather
// than on the raw string: a name may legitimately contain characters which
// are percent-encoded in the URL, and a trailing slash only denotes
// collection semantics.
func resolveHrefPath(r *http.Request, href *internal.Href) (p string, trailingSlash bool, err error) {
	u := (*url.URL)(href)

	if u.Opaque != "" || u.User != nil {
		return "", false, httpError(http.StatusBadRequest, nil)
	}

	// RFC 4918 section 9.3: the Destination URI must reference a resource
	// on the same server. A different host is a 502 Bad Gateway.
	if u.Host != "" && !strings.EqualFold(u.Host, r.Host) {
		return "", false, httpError(http.StatusBadGateway, nil)
	}

	// A destination which carries a query or fragment cannot be resolved
	// to a resource path.
	if u.RawQuery != "" || u.Fragment != "" {
		return "", false, httpError(http.StatusBadRequest, nil)
	}

	return canonicalizeURLPath(u.EscapedPath())
}

// canonicalizeURLPath turns an escaped URL path into a decoded, clean
// absolute path. Each segment is unescaped separately so that encoded
// names are preserved, while encoded or decoded slash characters are
// rejected instead of collapsing the segment boundary.
func canonicalizeURLPath(escaped string) (string, bool, error) {
	if escaped == "" {
		return "", false, httpError(http.StatusBadRequest, nil)
	}

	trailingSlash := strings.HasSuffix(escaped, "/")
	if trailingSlash && len(escaped) > 1 {
		escaped = escaped[:len(escaped)-1]
	}
	if !strings.HasPrefix(escaped, "/") {
		return "", false, httpError(http.StatusBadGateway, nil)
	}

	if escaped == "/" {
		return "/", trailingSlash, nil
	}

	rawSegments := strings.Split(escaped[1:], "/")
	segments := make([]string, 0, len(rawSegments))
	for _, raw := range rawSegments {
		if raw == "" || raw == "." || raw == ".." {
			return "", false, httpError(http.StatusBadRequest, nil)
		}
		segment, err := url.PathUnescape(raw)
		if err != nil {
			return "", false, httpError(http.StatusBadRequest, err)
		}
		if strings.Contains(segment, "/") {
			return "", false, httpError(http.StatusBadRequest, nil)
		}
		segments = append(segments, segment)
	}

	var b strings.Builder
	for _, segment := range segments {
		b.WriteByte('/')
		b.WriteString(segment)
	}
	return b.String(), trailingSlash, nil
}

// isPathBelow reports whether p is a member of the collection rooted at
// root, where both arguments are canonicalized paths. The check is done on
// path segments rather than string prefixes, so sibling names such as
// "/foobar" are not considered children of "/foo".
func isPathBelow(p, root string) bool {
	if root == "/" {
		return true
	}
	return strings.HasPrefix(p, root+"/")
}

// statResource stats a path and reports whether the resource exists, whether
// it is a collection and the original error for unexpected failures.
func statResource(ctx context.Context, fs FileSystem, name string) (exists, isDir bool, err error) {
	fi, err := fs.Stat(ctx, name)
	if err != nil {
		if internal.IsNotFound(err) {
			return false, false, nil
		}
		return false, false, err
	}
	return true, fi.IsDir, nil
}

// resolveCopyMove performs the destination resolution and preflight checks
// shared by COPY and MOVE. It never mutates the backend.
func (b *backend) resolveCopyMove(r *http.Request, dest *internal.Href, move, recursive, overwrite bool) (*CopyMoveParams, error) {
	src, srcTrailingSlash, err := canonicalizeURLPath(r.URL.EscapedPath())
	if err != nil {
		return nil, err
	}
	dst, dstTrailingSlash, err := resolveHrefPath(r, dest)
	if err != nil {
		return nil, err
	}

	if src == "/" || dst == "/" {
		return nil, httpError(http.StatusForbidden, nil)
	}

	srcExists, srcIsDir, err := statResource(r.Context(), b.FileSystem, src)
	if err != nil {
		return nil, err
	}
	if !srcExists {
		return nil, httpError(http.StatusNotFound, nil)
	}
	if srcTrailingSlash && !srcIsDir {
		return nil, httpError(http.StatusNotFound, nil)
	}

	dstExists, dstIsDir, err := statResource(r.Context(), b.FileSystem, dst)
	if err != nil {
		return nil, err
	}

	// A href ending with a slash addresses a collection: an existing
	// destination must be a collection, and a new destination may only be
	// created when the source itself is a collection.
	if dstTrailingSlash && ((dstExists && !dstIsDir) || (!dstExists && !srcIsDir)) {
		return nil, httpError(http.StatusConflict, nil)
	}

	// A collection cannot atomically replace a non-collection resource and
	// vice versa.
	if dstExists && srcIsDir != dstIsDir {
		return nil, httpError(http.StatusConflict, nil)
	}

	// Source and destination are compared on their canonicalized paths, so
	// percent-encoding differences do not defeat the identity check.
	//
	// RFC 4918 section 9.8/9.9: COPY or MOVE onto the source resource
	// itself is forbidden, as is a MOVE of a collection into one of its
	// descendants or an infinite-depth COPY (which could not terminate). A
	// zero-depth COPY into a descendant collection is permitted.
	if src == dst {
		return nil, httpError(http.StatusForbidden, nil)
	}
	if srcIsDir && isPathBelow(dst, src) && (move || recursive) {
		return nil, httpError(http.StatusForbidden, nil)
	}

	if !dstExists {
		parent := "/"
		if idx := strings.LastIndexByte(dst, '/'); idx > 0 {
			parent = dst[:idx]
		}
		if parent != "/" {
			parentExists, parentIsDir, err := statResource(r.Context(), b.FileSystem, parent)
			if err != nil {
				return nil, err
			}
			if !parentExists || !parentIsDir {
				return nil, httpError(http.StatusConflict, nil)
			}
		}
	} else if !overwrite {
		return nil, httpError(http.StatusPreconditionFailed, nil)
	}

	return &CopyMoveParams{
		Src:       src,
		Dst:       dst,
		SrcIsDir:  srcIsDir,
		DstExists: dstExists,
		DstIsDir:  dstIsDir,
		Move:      move,
		Recursive: recursive,
		Overwrite: overwrite,
	}, nil
}
