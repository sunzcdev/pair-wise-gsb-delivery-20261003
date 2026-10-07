// Package carddav provides a client and server CardDAV implementation.
//
// CardDAV is defined in RFC 6352.
package carddav

import (
	"context"
	"time"

	"github.com/emersion/go-vcard"
	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/internal"
)

var CapabilityAddressBook = webdav.Capability("addressbook")

func NewAddressBookHomeSet(path string) webdav.BackendSuppliedHomeSet {
	return &addressbookHomeSet{Href: internal.Href{Path: path}}
}

type AddressDataType struct {
	ContentType string
	Version     string
}

type AddressBook struct {
	Path                 string
	Name                 string
	Description          string
	MaxResourceSize      int64
	SupportedAddressData []AddressDataType
	// ReadOnly reports that the current user may only read this address book.
	// It controls the DAV:current-user-privilege-set reported by the server.
	ReadOnly bool
}

func (ab *AddressBook) SupportsAddressData(contentType, version string) bool {
	if len(ab.SupportedAddressData) == 0 {
		return contentType == "text/vcard" && version == "3.0"
	}
	for _, t := range ab.SupportedAddressData {
		if t.ContentType == contentType && t.Version == version {
			return true
		}
	}
	return false
}

type AddressBookQuery struct {
	DataRequest AddressDataRequest

	PropFilters []PropFilter
	FilterTest  FilterTest // defaults to FilterAnyOf

	Limit int // <= 0 means unlimited
}

type AddressDataRequest struct {
	Props   []string
	AllProp bool
}

type PropFilter struct {
	Name string
	Test FilterTest // defaults to FilterAnyOf

	// if IsNotDefined is set, TextMatches and Params need to be unset
	IsNotDefined bool
	TextMatches  []TextMatch
	Params       []ParamFilter
}

type ParamFilter struct {
	Name string

	// if IsNotDefined is set, TextMatch needs to be unset
	IsNotDefined bool
	TextMatch    *TextMatch
}

type TextMatch struct {
	Text            string
	NegateCondition bool
	MatchType       MatchType // defaults to MatchContains
}

type FilterTest string

const (
	FilterAnyOf FilterTest = "anyof"
	FilterAllOf FilterTest = "allof"
)

type MatchType string

const (
	MatchEquals     MatchType = "equals"
	MatchContains   MatchType = "contains"
	MatchStartsWith MatchType = "starts-with"
	MatchEndsWith   MatchType = "ends-with"
)

type AddressBookMultiGet struct {
	Paths       []string
	DataRequest AddressDataRequest
}

type AddressObject struct {
	Path          string
	ModTime       time.Time
	ContentLength int64
	ETag          string
	Card          vcard.Card
}

// SyncQuery is the query struct represents a sync-collection request
type SyncQuery struct {
	DataRequest AddressDataRequest
	SyncToken   string
	Limit       int // <= 0 means unlimited
}

// SyncResponse contains the result of a sync-collection REPORT.
//
// Updated contains the address object resources that have been added or
// modified. Deleted contains the paths of resources that have been removed.
// The distinction is conveyed in the protocol by an explicit response status,
// as required by RFC 6578 section 6.3.
type SyncResponse struct {
	SyncToken   string
	Updated     []AddressObject
	Deleted     []string
	MoreResults bool
}

// SyncCollectionBackend is an optional Backend extension adding support for
// the RFC 6578 sync-collection REPORT. A Backend that doesn't implement this
// interface keeps its previous behavior: the server doesn't advertise the
// "sync-collection" DAV feature and rejects sync-collection requests.
type SyncCollectionBackend interface {
	// SyncCollection synchronizes the address book collection at path. An
	// empty token requests an initial synchronization; a non-empty token
	// requests the changes that happened since the token was issued. The
	// limit, when positive, bounds the number of returned changes and forces
	// MoreResults to be true when more pages remain.
	SyncCollection(ctx context.Context, path string, query *SyncQuery) (*SyncResponse, error)

	// CurrentSyncToken returns the current sync token of the address book
	// collection at path. It backs the DAV:sync-token live property.
	CurrentSyncToken(ctx context.Context, path string) (string, error)
}

// InvalidSyncTokenError wraps err into an HTTP 403 error carrying the
// DAV:valid-sync-token precondition (RFC 6578 section 3.2). Backends should
// return it when the sync token is unknown, expired, or was issued for another
// collection.
func InvalidSyncTokenError(err error) error {
	return internal.InvalidSyncTokenError(err)
}
