// Package mbox parses and formats the mbox file format.
//
// As the mbox file format is not standardized this package expects the least
// common denominator, the so called mboxo format. The mboxrd variant, which
// quotes any line beginning with zero or more ">" followed by "From ", can be
// selected explicitly with NewReaderFormat and NewWriterFormat.
package mbox

var (
	header        = []byte("From ")
	escapedHeader = append([]byte{'>'}, header...)
)

// Format is a variant of the mbox file format.
type Format int

const (
	// Mboxo is the least common denominator mbox format. Only lines starting
	// with "From " are quoted, and a single quoting layer is removed when
	// reading.
	Mboxo Format = iota

	// Mboxrd is the "rd" variant. When writing, one quoting layer is added to
	// any line beginning with zero or more ">" followed by "From "; when
	// reading one layer is removed. Recognition only happens at the start of a
	// line, "From " occurring in the middle of a line is never modified.
	Mboxrd
)

// countPrefixByte returns the number of leading bytes equal to c in b.
func countPrefixByte(b []byte, c byte) int {
	n := 0
	for n < len(b) && b[n] == c {
		n++
	}
	return n
}
