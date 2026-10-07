package vellum

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrInvalidPageToken is returned when a serialized page token cannot be
// parsed or fails its own structural validation.
var ErrInvalidPageToken = errors.New("vellum: invalid page token")

// ErrPageTokenMismatch is returned when a page token was issued for a
// different FST or a different query than the one being resumed.
var ErrPageTokenMismatch = errors.New("vellum: page token does not match this FST or query")

// pageTokenVersion identifies the token encoding, so that tokens produced
// by an incompatible version are rejected instead of misinterpreted.
const pageTokenVersion = 1

// PageToken is a serializable cursor that allows paginated iteration of an
// FST to be suspended in one process and resumed in another.  It records
// the identity of the FST contents, the key range, a caller-provided query
// identifier and a copy of the last delivered key.  It contains no internal
// state addresses and no references to iterator buffers, so it is safe to
// encode, hand off and use against a freshly opened instance of the same
// FST.
//
// The token binds the key range and the query identifier, but it cannot
// capture an arbitrary Automaton.  Callers must supply the same automaton
// (when any) and the same query identifier when resuming; a mismatch of the
// query identifier is reported as ErrPageTokenMismatch.
type PageToken struct {
	Version     int    `json:"version"`
	Fingerprint []byte `json:"fingerprint"`
	QueryID     string `json:"queryID,omitempty"`
	Start       []byte `json:"start,omitempty"`
	End         []byte `json:"end,omitempty"`
	Last        []byte `json:"last,omitempty"`
	HasLast     bool   `json:"hasLast,omitempty"`
}

// fingerprint returns a content hash of this FST, computed lazily and
// cached for the lifetime of the instance.
func (f *FST) fingerprint() []byte {
	f.hashOnce.Do(func() {
		sum := sha256.Sum256(f.data)
		f.hash = sum[:]
	})
	return f.hash
}

// NewPageToken returns the initial token for paging through the results of
// a search over this FST, restricted to
// [startKeyInclusive, endKeyExclusive).  queryID is an opaque,
// caller-provided identifier of the query (e.g. a hash of the automaton or
// other search criteria); it is verified on resume so that a token is never
// applied to a different query.
func (f *FST) NewPageToken(queryID string, startKeyInclusive, endKeyExclusive []byte) *PageToken {
	return &PageToken{
		Version:     pageTokenVersion,
		Fingerprint: f.fingerprint(),
		QueryID:     queryID,
		Start:       append([]byte(nil), startKeyInclusive...),
		End:         append([]byte(nil), endKeyExclusive...),
	}
}

// Encode serializes the token into a byte slice that can be transferred to
// another process and later restored with DecodePageToken.
func (t *PageToken) Encode() ([]byte, error) {
	return json.Marshal(t)
}

// DecodePageToken restores a token previously serialized with Encode.
// Corrupted or structurally invalid input yields ErrInvalidPageToken.
func DecodePageToken(data []byte) (*PageToken, error) {
	var t PageToken
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidPageToken, err)
	}
	if err := t.validate(); err != nil {
		return nil, err
	}
	return &t, nil
}

func (t *PageToken) validate() error {
	if t.Version != pageTokenVersion {
		return fmt.Errorf("%w: unsupported version %d", ErrInvalidPageToken, t.Version)
	}
	if len(t.Fingerprint) != sha256.Size {
		return fmt.Errorf("%w: malformed fingerprint", ErrInvalidPageToken)
	}
	return nil
}

func (f *FST) checkPageToken(t *PageToken, queryID string) error {
	if err := t.validate(); err != nil {
		return err
	}
	if !bytes.Equal(t.Fingerprint, f.fingerprint()) {
		return fmt.Errorf("%w: token was issued for a different FST", ErrPageTokenMismatch)
	}
	if t.QueryID != queryID {
		return fmt.Errorf("%w: token query ID %q does not match query ID %q",
			ErrPageTokenMismatch, t.QueryID, queryID)
	}
	return nil
}

// NextPage delivers the next page of up to pageSize key/value pairs of the
// search described by token, resuming strictly after the last key delivered
// by the previous page so no key is ever returned twice.  The automaton
// (nil to match everything) and queryID must be the same as for the
// previous pages of this query; a different FST or queryID is rejected with
// ErrPageTokenMismatch.
//
// The returned keys are copies and remain valid after the call.  The
// returned token replaces the input token and should be used for the
// following page; when the range is exhausted an empty page is returned and
// the token no longer advances.
func (f *FST) NextPage(aut Automaton, queryID string, pageSize int, token *PageToken) ([][]byte, []uint64, *PageToken, error) {
	if token == nil {
		return nil, nil, nil, fmt.Errorf("%w: token is nil", ErrInvalidPageToken)
	}
	if pageSize < 1 {
		return nil, nil, nil, fmt.Errorf("vellum: pageSize must be >= 1, got %d", pageSize)
	}
	if err := f.checkPageToken(token, queryID); err != nil {
		return nil, nil, nil, err
	}

	itr, err := f.Search(aut, token.Start, token.End)
	if err == ErrIteratorDone {
		// empty range, nothing was ever or is left to be delivered
		return nil, nil, token, nil
	}
	if err != nil {
		return nil, nil, nil, err
	}
	defer itr.Close()

	if token.HasLast {
		err = itr.Seek(token.Last)
		if err == ErrIteratorDone {
			return nil, nil, token, nil
		}
		if err != nil {
			return nil, nil, nil, err
		}
		// The last delivered key exists in this FST, so the seek lands
		// exactly on it; step past it to avoid delivering it again.
		if k, _ := itr.Current(); bytes.Equal(k, token.Last) {
			err = itr.Next()
			if err == ErrIteratorDone {
				return nil, nil, token, nil
			}
			if err != nil {
				return nil, nil, nil, err
			}
		}
	}

	keys := make([][]byte, 0, pageSize)
	vals := make([]uint64, 0, pageSize)
	for len(keys) < pageSize {
		k, v := itr.Current()
		// the key buffer returned by Current is reused by the iterator,
		// so a copy must be kept in the page and in the next token
		keys = append(keys, append([]byte(nil), k...))
		vals = append(vals, v)
		if err = itr.Next(); err != nil {
			break
		}
	}
	if err != nil && err != ErrIteratorDone {
		return nil, nil, nil, err
	}

	if len(keys) == 0 {
		return nil, nil, token, nil
	}

	next := &PageToken{
		Version:     token.Version,
		Fingerprint: token.Fingerprint,
		QueryID:     token.QueryID,
		Start:       token.Start,
		End:         token.End,
		Last:        keys[len(keys)-1],
		HasLast:     true,
	}
	return keys, vals, next, nil
}
