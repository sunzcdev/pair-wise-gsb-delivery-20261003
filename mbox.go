// Package mbox parses and formats the mbox file format.
//
// As the mbox file format is not standardized this package expects the least
// common denominator, the so called mboxo format.
//
// NewReader and NewWriter use the mboxo variant. The mboxrd variant, in which
// a line starting with any number of '>' characters followed by "From " is
// quoted with one additional '>', is available through NewReaderMode and
// NewWriterMode with the MBOXRD mode.
package mbox

// Mode selects the escaping variant used by a Reader or Writer.
type Mode int

const (
	// MBOXO is the default mboxo variant. Only lines starting with
	// "From " are escaped on writing, and a single leading '>' in front
	// of "From " is removed on reading.
	MBOXO Mode = iota
	// MBOXRD is the mboxrd variant. Any number of leading '>' characters
	// followed by "From " is quoted with one additional '>' on writing,
	// and one leading '>' is removed on reading. Recognition only happens
	// at the beginning of a line.
	MBOXRD
)

var (
	header = []byte("From ")
)

const headerLen = len("From ")
