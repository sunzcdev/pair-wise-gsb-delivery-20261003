//  Copyright (c) 2017 Couchbase, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 		http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package vellum

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// ErrPageTokenCorrupt is returned when a page token cannot be decoded,
// e.g. it was truncated, tampered with, or produced by an incompatible
// version of this library.
var ErrPageTokenCorrupt = errors.New("vellum: page token is corrupt")

// ErrPageTokenMismatch is returned when a page token was issued for a
// different FST, a different query identifier, or a different key range
// than the ones provided by the caller resuming the iteration.
var ErrPageTokenMismatch = errors.New("vellum: page token does not match FST or query")

// pageTokenVersion is the serialization version of page tokens.
const pageTokenVersion = 1

var pageTokenMagic = []byte{'V', 'T'}

// pageToken is the decoded form of an opaque, serializable pagination
// token.  It binds the identity of the FST content, the caller-provided
// query identifier, the key range of the query, and the last key that was
// delivered to the caller.  It contains no internal addresses and no
// references to iterator buffers, so it is safe to serialize and hand to
// another process.
type pageToken struct {
	fingerprint uint64
	queryID     string
	start       []byte // nil means unbounded
	end         []byte // nil means unbounded
	last        []byte // last delivered key, valid only if hasLast
	hasLast     bool
}

// marshal encodes the token as:
//
//	magic(2) | version(1) | fingerprint(8 LE) |
//	queryID(uvarint len + bytes) |
//	start(nullable bytes) | end(nullable bytes) | last(nullable bytes) |
//	crc32-ieee(4 LE) over all preceding bytes
//
// Nullable byte slices are encoded as uvarint(0) when nil, otherwise as
// uvarint(len+1) followed by the raw bytes, so that nil, empty, and
// zero-byte-containing keys are all preserved exactly.
func (t *pageToken) marshal() []byte {
	var buf []byte
	buf = append(buf, pageTokenMagic...)
	buf = append(buf, pageTokenVersion)
	buf = binary.LittleEndian.AppendUint64(buf, t.fingerprint)
	// queryID is always present; encode as uvarint(len+1) + bytes so
	// that an empty query id round-trips distinctly from a missing one
	buf = binary.AppendUvarint(buf, uint64(len(t.queryID))+1)
	buf = append(buf, t.queryID...)
	buf = appendTokenBytes(buf, t.start)
	buf = appendTokenBytes(buf, t.end)
	if t.hasLast {
		// an empty last key must round-trip distinctly from "no key
		// delivered yet", so normalize nil to a non-nil empty slice
		if t.last == nil {
			t.last = []byte{}
		}
		buf = appendTokenBytes(buf, t.last)
	} else {
		buf = appendTokenBytes(buf, nil)
	}
	return binary.LittleEndian.AppendUint32(buf, crc32.ChecksumIEEE(buf))
}

func appendTokenBytes(buf, b []byte) []byte {
	if b == nil {
		return binary.AppendUvarint(buf, 0)
	}
	buf = binary.AppendUvarint(buf, uint64(len(b))+1)
	return append(buf, b...)
}

func parsePageToken(data []byte) (*pageToken, error) {
	headerLen := len(pageTokenMagic) + 1 + 8
	if len(data) < headerLen+4 {
		return nil, fmt.Errorf("%w: token too short", ErrPageTokenCorrupt)
	}
	if !bytes.Equal(data[:len(pageTokenMagic)], pageTokenMagic) {
		return nil, fmt.Errorf("%w: bad magic", ErrPageTokenCorrupt)
	}
	if data[len(pageTokenMagic)] != pageTokenVersion {
		return nil, fmt.Errorf("%w: unsupported version %d",
			ErrPageTokenCorrupt, data[len(pageTokenMagic)])
	}
	body := data[:len(data)-4]
	if crc32.ChecksumIEEE(body) != binary.LittleEndian.Uint32(data[len(data)-4:]) {
		return nil, fmt.Errorf("%w: checksum mismatch", ErrPageTokenCorrupt)
	}

	t := &pageToken{
		fingerprint: binary.LittleEndian.Uint64(body[len(pageTokenMagic)+1:]),
	}
	rest := body[headerLen:]

	queryID, rest, err := takeTokenBytes(rest)
	if err != nil {
		return nil, err
	}
	if queryID == nil {
		return nil, fmt.Errorf("%w: missing query id", ErrPageTokenCorrupt)
	}
	t.queryID = string(queryID)

	if t.start, rest, err = takeTokenBytes(rest); err != nil {
		return nil, err
	}
	if t.end, rest, err = takeTokenBytes(rest); err != nil {
		return nil, err
	}
	if t.last, rest, err = takeTokenBytes(rest); err != nil {
		return nil, err
	}
	t.hasLast = t.last != nil
	if len(rest) != 0 {
		return nil, fmt.Errorf("%w: trailing bytes", ErrPageTokenCorrupt)
	}
	return t, nil
}

func takeTokenBytes(data []byte) ([]byte, []byte, error) {
	n, size := binary.Uvarint(data)
	if size <= 0 {
		return nil, nil, fmt.Errorf("%w: bad length prefix", ErrPageTokenCorrupt)
	}
	data = data[size:]
	if n == 0 {
		return nil, data, nil
	}
	n--
	if uint64(len(data)) < n {
		return nil, nil, fmt.Errorf("%w: truncated bytes", ErrPageTokenCorrupt)
	}
	return data[:n], data[n:], nil
}

// checkMatch verifies that a token decoded from a previous page was issued
// for the same FST content, query identifier, and key range as the ones
// the resuming caller provided.
func (t *pageToken) checkMatch(prev *pageToken) error {
	if t.fingerprint != prev.fingerprint {
		return fmt.Errorf("%w: token was issued for different FST content",
			ErrPageTokenMismatch)
	}
	if t.queryID != prev.queryID {
		return fmt.Errorf("%w: token was issued for query %q, not %q",
			ErrPageTokenMismatch, prev.queryID, t.queryID)
	}
	if !sameBound(t.start, prev.start) {
		return fmt.Errorf("%w: token was issued for a different start key",
			ErrPageTokenMismatch)
	}
	if !sameBound(t.end, prev.end) {
		return fmt.Errorf("%w: token was issued for a different end key",
			ErrPageTokenMismatch)
	}
	return nil
}

func sameBound(a, b []byte) bool {
	return (a == nil) == (b == nil) && bytes.Equal(a, b)
}

// SearchPage returns up to limit key/value pairs from this FST, in
// lexicographic order, restricted to
// [startKeyInclusive, endKeyExclusive) and to keys accepted by aut
// (aut may be nil to match everything).
//
// Pagination is driven by an opaque, serializable token: pass a nil token
// to read the first page, and pass the token returned by the previous call
// to read the next page.  Iteration is exhausted when the returned
// nextToken is nil.  Resuming continues strictly after the last key of the
// previous page, so keys are never delivered twice.
//
// The token binds the identity of the FST content, the queryID, and the
// key range.  When resuming, the caller MUST provide the same queryID,
// startKeyInclusive, and endKeyExclusive, and an FST with the same content
// (e.g. the same file re-opened in another process); otherwise
// ErrPageTokenMismatch is returned.  A token that cannot be decoded
// returns ErrPageTokenCorrupt.  The automaton itself is NOT serialized
// into the token: callers using an automaton should incorporate its
// identity into queryID and provide an equivalent automaton when resuming.
//
// The returned keys are copies and remain valid after subsequent calls.
func (f *FST) SearchPage(aut Automaton, queryID string,
	startKeyInclusive, endKeyExclusive, token []byte,
	limit int) (keys [][]byte, vals []uint64, nextToken []byte, err error) {

	if limit < 1 {
		return nil, nil, nil,
			fmt.Errorf("vellum: page limit must be >= 1, got %d", limit)
	}

	resume := &pageToken{
		fingerprint: f.fingerprint(),
		queryID:     queryID,
		start:       startKeyInclusive,
		end:         endKeyExclusive,
	}

	if token != nil {
		prev, err := parsePageToken(token)
		if err != nil {
			return nil, nil, nil, err
		}
		if err := resume.checkMatch(prev); err != nil {
			return nil, nil, nil, err
		}
		resume.hasLast = prev.hasLast
		resume.last = prev.last
	}

	itr, err := f.Search(aut, startKeyInclusive, endKeyExclusive)
	if err == ErrIteratorDone {
		// no keys in range at all
		return nil, nil, nil, nil
	}
	if err != nil {
		return nil, nil, nil, err
	}
	defer func() {
		_ = itr.Close()
	}()

	if resume.hasLast {
		// continue strictly after the last delivered key
		err = itr.Seek(resume.last)
		if err == nil {
			if cur, _ := itr.Current(); bytes.Equal(cur, resume.last) {
				err = itr.Next()
			}
			// if the key is not found (only possible with a colliding
			// fingerprint), the iterator already points past it
		}
		if err == ErrIteratorDone {
			return nil, nil, nil, nil
		}
		if err != nil {
			return nil, nil, nil, err
		}
	}

	for err == nil && len(keys) < limit {
		k, v := itr.Current()
		// copy the key: Current() borrows the iterator's internal buffer,
		// which must never leak into the token or the caller's results
		keys = append(keys, append([]byte(nil), k...))
		vals = append(vals, v)
		err = itr.Next()
	}
	if err != nil && err != ErrIteratorDone {
		return nil, nil, nil, err
	}
	if len(keys) == 0 || err == ErrIteratorDone {
		// range exhausted (possibly exactly at the page boundary or
		// right before the end key): no further pages
		return keys, vals, nil, nil
	}

	resume.last = keys[len(keys)-1]
	resume.hasLast = true
	return keys, vals, resume.marshal(), nil
}
