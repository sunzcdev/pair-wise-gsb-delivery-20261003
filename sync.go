package webdav

import (
	"context"
	"net/http"

	"github.com/emersion/go-webdav/internal"
)

// CapabilitySyncCollection is the DAV compliance class advertised in the DAV
// response header by servers supporting WebDAV Collection Synchronization
// (RFC 6578).
const CapabilitySyncCollection Capability = "sync-token"

// SyncCollectionRequest is a WebDAV sync-collection REPORT request, as
// defined in RFC 6578 section 6.1.
type SyncCollectionRequest struct {
	// SyncToken is the sync token returned by a previous sync-collection
	// REPORT. An empty SyncToken requests an initial synchronization, which
	// returns the full state of the collection along with a new token.
	SyncToken string
	// Limit, if greater than zero, limits the number of member URIs returned
	// in a single response. When the result is truncated, the returned token
	// can be used to continue the same REPORT.
	Limit int
}

// SyncCollectionChange describes a collection member change. If Deleted is
// true, the resource identified by FileInfo.Path has been removed.
type SyncCollectionChange struct {
	FileInfo FileInfo
	Deleted  bool
}

// SyncCollectionResult is the result of a sync-collection REPORT.
type SyncCollectionResult struct {
	// Changes lists added, modified and deleted collection members.
	Changes []SyncCollectionChange
	// SyncToken is the new sync token.
	//
	// When the result is truncated (see SyncCollectionRequest.Limit), the
	// token MUST be a continuation token bound to the same collection
	// snapshot, allowing the client to fetch the remaining changes without
	// losing or duplicating updates. It MUST NOT be the collection's current
	// token until all changes have been returned, in which case the token
	// becomes the snapshot's final token.
	SyncToken string
}

// SyncCollectionFileSystem is an optional FileSystem extension providing
// support for the sync-collection REPORT defined in RFC 6578. FileSystems
// which do not implement it keep their original behavior: the server does
// not advertise the "sync-token" DAV class and REPORT requests result in a
// 405 Method Not Allowed response.
type SyncCollectionFileSystem interface {
	FileSystem

	// CurrentSyncToken returns the current sync token of the collection.
	// It backs the DAV:current-sync-token live property.
	CurrentSyncToken(ctx context.Context, name string) (string, error)

	// SyncCollection synchronizes a collection.
	//
	// An empty SyncToken requests an initial synchronization; otherwise the
	// changes since the given token are returned. An unknown or expired
	// token MUST result in an error created with NewValidSyncTokenError.
	//
	// When a Limit is set and more changes are available, the result's
	// SyncToken must be a continuation token pinned to the snapshot taken at
	// the start of the operation; changes happening while the client
	// paginates MUST NOT be included or skipped. The final page carries the
	// snapshot's committed token.
	SyncCollection(ctx context.Context, name string, req *SyncCollectionRequest) (*SyncCollectionResult, error)
}

// NewValidSyncTokenError creates an error indicating that the provided sync
// token is unknown, malformed or expired. It is reported to the client as an
// HTTP 403 Forbidden response with the DAV:valid-sync-token precondition, as
// defined in RFC 6578 section 5.2.
func NewValidSyncTokenError() error {
	return internal.NewValidSyncTokenError()
}

func (b *backend) Report(w http.ResponseWriter, r *http.Request) error {
	var query internal.SyncCollectionQuery
	if err := internal.DecodeXMLRequest(r, &query); err != nil {
		return err
	}

	syncFS, ok := b.FileSystem.(SyncCollectionFileSystem)
	if !ok {
		return internal.HTTPErrorf(http.StatusMethodNotAllowed, "webdav: sync-collection REPORT is not supported")
	}

	if query.SyncLevel != internal.SyncLevel1 {
		return internal.NewSyncLevelError()
	}

	fi, err := syncFS.Stat(r.Context(), r.URL.Path)
	if err != nil {
		return err
	}
	if !fi.IsDir {
		return internal.HTTPErrorf(http.StatusForbidden, "webdav: sync-collection REPORT can only be performed on a collection")
	}

	var limit int
	if query.Limit != nil {
		limit = int(query.Limit.NResults)
	}

	result, err := syncFS.SyncCollection(r.Context(), r.URL.Path, &SyncCollectionRequest{
		SyncToken: query.SyncToken,
		Limit:     limit,
	})
	if err != nil {
		return err
	}

	propfind := &internal.PropFind{Prop: query.Prop}
	if query.Prop == nil {
		propfind.AllProp = &struct{}{}
	}

	resps := make([]internal.Response, 0, len(result.Changes))
	for i := range result.Changes {
		change := &result.Changes[i]
		if change.Deleted {
			resps = append(resps, *internal.NewDeletedResponse(change.FileInfo.Path))
			continue
		}

		resp, err := b.propFindFile(r.Context(), propfind, &change.FileInfo)
		if err != nil {
			return err
		}
		resps = append(resps, *resp)
	}

	ms := internal.NewMultiStatus(resps...)
	ms.SyncToken = result.SyncToken
	return internal.ServeMultiStatus(w, ms)
}
