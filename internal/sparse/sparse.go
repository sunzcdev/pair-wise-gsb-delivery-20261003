// Package sparse defines the sparse-file transfer extension used to negotiate
// sparse transfer support and to discover and create holes in files.
//
// The extension is advertised during the SSH_FXP_INIT / SSH_FXP_VERSION
// exchange under the name sparse-file@pkg.github.com. Its extension data is a
// comma-separated list of capability tokens.
//
// Two extended requests are defined, both keyed by an open file handle:
//
//   - extents@sftp.pkg.github.com reports the ranges of a file that carry
//     allocated (non-hole) data.
//   - punch-hole@sftp.pkg.github.com deallocates a byte range, turning it into
//     a hole without changing the logical length of the file.
//
// Discovering extents and creating holes are independently negotiated: a peer
// may be able to do one but not the other. A peer that advertises neither is a
// plain protocol-3 peer and dense transfer still works.
package sparse

import "strings"

// ExtensionName is the name under which this extension is negotiated.
const ExtensionName = "sparse-file@pkg.github.com"

// Capability is an independently negotiable sparse-transfer feature.
type Capability uint32

const (
	// CapExtents means the side holding a file open for reading can report
	// which byte ranges carry allocated data and which are holes.
	CapExtents Capability = 1 << iota
	// CapPunch means the side holding a file open for writing can turn a
	// byte range into a hole without shrinking the file.
	CapPunch
)

// CapAll is the set of every defined capability.
const CapAll = CapExtents | CapPunch

// ExtensionData renders the given capabilities into the extension-data string
// used in the SSH_FXP_INIT / SSH_FXP_VERSION exchange.
func ExtensionData(caps Capability) string {
	var tokens []string
	if caps&CapExtents != 0 {
		tokens = append(tokens, "extents")
	}
	if caps&CapPunch != 0 {
		tokens = append(tokens, "punch")
	}
	return strings.Join(tokens, ",")
}

// ParseExtensionData parses an extension-data string into a capability set.
// Unknown tokens are ignored so that future capabilities stay negotiable.
func ParseExtensionData(data string) Capability {
	var caps Capability
	for _, token := range strings.Split(data, ",") {
		switch strings.TrimSpace(token) {
		case "extents":
			caps |= CapExtents
		case "punch":
			caps |= CapPunch
		}
	}
	return caps
}

// Extent describes one allocated (non-hole), half-open byte range
// [Offset, Offset+Length) of a file.
type Extent struct {
	Offset uint64
	Length uint64
}

// ExtentsRequest is the extents@sftp.pkg.github.com extended request.
//
// It asks for the allocated extents that overlap the half-open range
// [Offset, Offset+Length). A Length of zero means "to the end of the file".
const ExtentsRequest = "extents@sftp.pkg.github.com"

// PunchRequest is the punch-hole@sftp.pkg.github.com extended request.
//
// It requests that the half-open range [Offset, Offset+Length) be turned into
// a hole. It MUST NOT change the logical size of the file; reads from the
// range afterwards return zeroes.
const PunchRequest = "punch-hole@sftp.pkg.github.com"
