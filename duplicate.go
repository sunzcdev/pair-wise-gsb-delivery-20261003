package fastjson

import (
	"fmt"
	"strings"
)

// DuplicateKeyPolicy determines how the Parser handles an object containing
// two members with the same key.
//
// Keys are compared after unescaping, so "name" and "\u006eame" are treated
// as the same key. The policy is applied independently to every parsed
// object; members of distinct objects never affect each other.
type DuplicateKeyPolicy int

const (
	// DuplicateKeyDefault keeps every member, including duplicate ones.
	//
	// This is the default behavior used when no policy is set: Len, Visit
	// and serialization see all the members in the original order, while
	// Object.Get returns the first member with the matching key.
	DuplicateKeyDefault DuplicateKeyPolicy = iota

	// DuplicateKeyReject returns a *DuplicateKeyError when an object
	// contains two members with the same key.
	DuplicateKeyReject

	// DuplicateKeyKeepFirst keeps the first member with a duplicated key and
	// drops later members. The kept member retains the original position of the
	// first member.
	DuplicateKeyKeepFirst

	// DuplicateKeyKeepLast keeps the last member with a duplicated key and
	// drops earlier members. The kept member retains the original position of
	// the last member.
	DuplicateKeyKeepLast
)

// String returns a human-readable representation of p.
func (p DuplicateKeyPolicy) String() string {
	switch p {
	case DuplicateKeyDefault:
		return "default"
	case DuplicateKeyReject:
		return "reject"
	case DuplicateKeyKeepFirst:
		return "keep_first"
	case DuplicateKeyKeepLast:
		return "keep_last"
	default:
		return fmt.Sprintf("unknown(%d)", int(p))
	}
}

// DuplicateKeyError is returned by Parser.Parse when an object contains two
// members with the same key and the parser is configured with
// DuplicateKeyReject.
//
// Positions are byte offsets relative to the start of the parsed JSON and
// point at the opening quote of each key.
type DuplicateKeyError struct {
	// Key is the unescaped key appearing more than once.
	Key string

	// FirstOffset is the byte offset of the first key's opening quote.
	FirstOffset int

	// FirstLine is the 1-based line of the first key.
	FirstLine int

	// FirstColumn is the 1-based column of the first key.
	FirstColumn int

	// DuplicateOffset is the byte offset of the duplicated key's opening quote.
	DuplicateOffset int

	// DuplicateLine is the 1-based line of the duplicated key.
	DuplicateLine int

	// DuplicateColumn is the 1-based column of the duplicated key.
	DuplicateColumn int
}

func (e *DuplicateKeyError) Error() string {
	return fmt.Sprintf("duplicate key %q at line %d, column %d (offset %d); previously seen at line %d, column %d (offset %d)",
		e.Key, e.DuplicateLine, e.DuplicateColumn, e.DuplicateOffset,
		e.FirstLine, e.FirstColumn, e.FirstOffset)
}

// lineColumn converts a byte offset in s into a 1-based line and column.
func lineColumn(s string, offset int) (line, column int) {
	if offset < 0 {
		offset = 0
	}
	if offset > len(s) {
		offset = len(s)
	}
	line = 1 + strings.Count(s[:offset], "\n")
	if i := strings.LastIndexByte(s[:offset], '\n'); i >= 0 {
		column = offset - i
	} else {
		column = offset + 1
	}
	return line, column
}
