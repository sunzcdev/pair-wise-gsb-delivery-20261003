// Package webdav provides a client and server WebDAV filesystem implementation.
//
// WebDAV is defined in RFC 4918.
package webdav

import (
	"time"
)

// FileInfo holds information about a WebDAV file.
type FileInfo struct {
	Path     string
	Size     int64
	ModTime  time.Time
	IsDir    bool
	MIMEType string
	ETag     string
}

type CreateOptions struct {
	// Conditions holds the parsed If-Match and If-None-Match headers.
	// Backends must evaluate them atomically when creating or replacing the
	// resource; see ErrConditionsNotSupported.
	Conditions Conditions
}

type RemoveAllOptions struct {
	// Conditions holds the parsed If-Match and If-None-Match headers.
	// Backends must evaluate them atomically when removing the resource; see
	// ErrConditionsNotSupported.
	Conditions Conditions
}

type CopyOptions struct {
	NoRecursive bool
	NoOverwrite bool
}

type MoveOptions struct {
	NoOverwrite bool
}
