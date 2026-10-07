package internal

import (
	"encoding/xml"
	"net/http"
)

// SyncLevel1 is the only synchronization level supported by RFC 6578.
const SyncLevel1 = "1"

// NewValidSyncTokenError creates an error indicating that the provided sync
// token is invalid or expired, as defined in
// https://tools.ietf.org/html/rfc6578#section-5.2. The server responds with
// HTTP 403 Forbidden and a DAV:valid-sync-token precondition.
func NewValidSyncTokenError() *HTTPError {
	elem := NewRawXMLElement(ValidSyncTokenName, nil, nil)
	return &HTTPError{
		Code: http.StatusForbidden,
		Err: &Error{
			Raw: []RawXMLValue{*elem},
		},
	}
}

// NewSyncLevelError creates an error indicating that the requested sync level
// is not supported, as defined in
// https://tools.ietf.org/html/rfc6578#section-3.3.
func NewSyncLevelError() *HTTPError {
	elem := NewRawXMLElement(xml.Name{Namespace, "sync-level"}, nil, nil)
	return &HTTPError{
		Code: http.StatusForbidden,
		Err: &Error{
			Raw: []RawXMLValue{*elem},
		},
	}
}
