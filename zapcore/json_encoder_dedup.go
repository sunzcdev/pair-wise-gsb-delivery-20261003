// Copyright (c) 2026 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package zapcore

import (
	"sort"
	"strings"
)

// kvSegment records the position of one key/value pair in the encoder buffer.
// It is created before the key is written, so end stays 0 while the value is
// still being appended and is filled when the next field starts or when the
// enclosing object closes.
type kvSegment struct {
	scope    int
	key      string
	start    int // buffer index of the key's opening quote
	colon    int // buffer index of the ':' separating key and value
	end      int // buffer index just past the encoded value (exclusive)
	suppress bool
}

// dedupScope records one object: the root, an opened namespace, a marshaled
// object, or an object nested in an array. name is empty for the root and for
// anonymous objects reached through arrays.
type dedupScope struct {
	parent  int
	name    string
	braceAt int // index of the opening '{', -1 for a context encoder's root
	closeAt int // index of the closing '}'
	// seen tracks live keys in this scope for O(1) KeepFirst suppression. It
	// is populated lazily and nil for policies that resolve at finalize time.
	seen map[string]struct{}
}

// dedupState carries all bookkeeping used to enforce a DuplicateKeyPolicy.
// It is allocated lazily, so encoders configured with AllowDuplicateKeys keep
// their historical, allocation-free shape.
type dedupState struct {
	policy DuplicateKeyPolicy
	onDup  DuplicateKeyFunc

	scopes   []dedupScope
	stack    []int // indexes of currently open objects; scopes[0] is root
	segments []kvSegment
	// rawBuf references the encoder buffer during resolve, for separator
	// inspection and conflict value extraction.
	rawBuf []byte
}

func newDedupState(policy DuplicateKeyPolicy, onDup DuplicateKeyFunc) *dedupState {
	d := &dedupState{policy: policy, onDup: onDup}
	d.scopes = append(d.scopes, dedupScope{parent: -1, braceAt: -1})
	d.stack = append(d.stack, 0)
	return d
}

func (d *dedupState) clone() *dedupState {
	c := &dedupState{policy: d.policy, onDup: d.onDup}
	c.scopes = append(c.scopes, d.scopes...)
	c.stack = append(c.stack, d.stack...)
	c.segments = append(c.segments, d.segments...)
	return c
}

func (d *dedupState) curScope() int {
	return d.stack[len(d.stack)-1]
}

// beginField records a new key/value pair after the element separator has
// been written. keyStart is the buffer index where the key's opening quote
// will be appended. It returns false when KeepFirst suppresses a live
// duplicate, in which case the caller must not write the key or evaluate the
// value.
func (d *dedupState) beginField(key string, keyStart int) bool {
	scope := d.curScope()
	if d.policy == KeepFirst {
		s := &d.scopes[scope]
		if s.seen == nil {
			s.seen = make(map[string]struct{})
		}
		if _, ok := s.seen[key]; ok {
			// Do NOT finalize the previous live segment here: no separator
			// will be written for this suppressed field, so the previous
			// segment's end stays wherever the next real boundary is.
			d.segments = append(d.segments, kvSegment{
				scope:    scope,
				key:      key,
				start:    keyStart,
				end:      keyStart,
				suppress: true,
			})
			return false
		}
		s.seen[key] = struct{}{}
	}
	d.closeOpenSegment(scope, keyStart)
	d.segments = append(d.segments, kvSegment{
		scope: scope,
		key:   key,
		start: keyStart,
	})
	return true
}

// registerHeaderKey claims a key in the current object before any field is
// written, and records the segment that will carry it. It is used for the
// entry's built-in header fields (level, time, message, ...). Under
// KeepFirst the key is added to the object's seen set immediately, so a
// later context or call field with the same key is eagerly suppressed and
// never writes a separator.
func (d *dedupState) registerHeaderKey(scope int, key string, keyStart int) {
	if d.policy == KeepFirst {
		s := &d.scopes[scope]
		if s.seen == nil {
			s.seen = make(map[string]struct{})
		}
		s.seen[key] = struct{}{}
	}
	d.segments = append(d.segments, kvSegment{
		scope: scope,
		key:   key,
		start: keyStart,
	})
}

// lastOpenSegment returns the currently open segment in the given scope, if
// any.
func (d *dedupState) lastOpenSegment(scope int) *kvSegment {
	for i := len(d.segments) - 1; i >= 0; i-- {
		seg := &d.segments[i]
		if seg.scope != scope {
			continue
		}
		if seg.end == 0 {
			return seg
		}
		return nil
	}
	return nil
}

// markColon records the colon position for the currently open segment.
func (d *dedupState) markColon(scope, pos int) {
	if seg := d.lastOpenSegment(scope); seg != nil {
		seg.colon = pos
	}
}

// closeOpenSegment finalizes the open segment in the given scope at pos. For
// a normal field, pos precedes the new separator. For the last field of an
// object (closed at the closing brace), pass -1 so the segment is marked
// "ends at brace" and finalized later in fixupSegmentEnds, where dangling
// separators left by eager suppression are handled.
func (d *dedupState) closeOpenSegment(scope, pos int) {
	if seg := d.lastOpenSegment(scope); seg != nil {
		seg.end = pos
	}
}

func (d *dedupState) pushScope(name string, braceAt int) int {
	idx := len(d.scopes)
	d.scopes = append(d.scopes, dedupScope{
		parent:  d.curScope(),
		name:    name,
		braceAt: braceAt,
	})
	d.stack = append(d.stack, idx)
	return idx
}

func (d *dedupState) popScope(closeAt int) {
	top := d.curScope()
	d.closeOpenSegment(top, -1)
	d.scopes[top].closeAt = closeAt
	d.stack = d.stack[:len(d.stack)-1]
}

// closeTopNamespace closes the namespace on top of the stack. At the top
// level the root itself is being closed by EncodeEntry, which records the
// root's position separately via setRootClose.
func (d *dedupState) closeTopNamespace(closeAt int) {
	if len(d.stack) <= 1 {
		if s := &d.scopes[0]; s.closeAt == 0 {
			s.closeAt = closeAt
		}
		return
	}
	d.popScope(closeAt)
}

// setRootClose records the position of the root object's closing brace.
func (d *dedupState) setRootClose(closeAt int) {
	d.scopes[0].closeAt = closeAt
}

// importFrom folds another encoder's accumulated context into this encoder's
// bookkeeping. The source bytes are copied starting at offset, so every
// recorded position is shifted by offset. Header fields already recorded in
// d are considered earlier than every imported field.
func (d *dedupState) importFrom(src *dedupState, offset int) {
	scopeMap := make([]int, len(src.scopes))
	// The source root object is the same object as d's root (the entry's
	// top-level fields and accumulated context share it).
	scopeMap[0] = 0
	for i := range src.scopes {
		if i == 0 {
			continue
		}
		s := &src.scopes[i]
		parent := scopeMap[s.parent]
		braceAt := s.braceAt
		if braceAt >= 0 {
			braceAt += offset
		}
		closeAt := 0
		if s.closeAt > 0 {
			closeAt = s.closeAt + offset
		}
		scopeMap[i] = len(d.scopes)
		d.scopes = append(d.scopes, dedupScope{
			parent:  parent,
			name:    s.name,
			braceAt: braceAt,
			closeAt: closeAt,
		})
	}
	for i := range src.segments {
		s := &src.segments[i]
		seg := *s
		seg.scope = scopeMap[s.scope]
		seg.start += offset
		if seg.colon > 0 {
			seg.colon += offset
		}
		// Re-resolve value boundaries in the final buffer; positions copied
		// from the context buffer were finalized without knowledge of which
		// segment ends up last in the merged object.
		seg.end = 0
		d.segments = append(d.segments, seg)
	}
	// Reopen the source's still-open namespace stack.
	for _, srcIdx := range src.stack[1:] {
		d.stack = append(d.stack, scopeMap[srcIdx])
	}

	// Merge live-key sets so KeepFirst keeps suppressing duplicates across
	// the header/context boundary. Only still-open objects can receive more
	// fields; closed objects only need their segments copied.
	openSet := map[int]struct{}{}
	for _, srcIdx := range src.stack {
		openSet[srcIdx] = struct{}{}
	}
	for srcIdx := range openSet {
		srcScope := &src.scopes[srcIdx]
		if len(srcScope.seen) == 0 {
			continue
		}
		dst := &d.scopes[scopeMap[srcIdx]]
		if dst.seen == nil {
			dst.seen = make(map[string]struct{}, len(srcScope.seen))
		}
		for k := range srcScope.seen {
			dst.seen[k] = struct{}{}
		}
	}
}

// resolve enforces the policy once the full entry has been encoded. It reports
// conflicts (ReportConflict) and returns the buffer ranges to delete for
// KeepFirst/KeepLast.
func (d *dedupState) resolve(buf []byte) [][2]int {
	d.rawBuf = buf
	// Re-finalize open segments at object boundaries, stripping trailing
	// separators left by eager suppression.
	d.fixupSegmentEnds()
	switch d.policy {
	case ReportConflict:
		d.reportConflicts(buf)
		return nil
	case KeepFirst, KeepLast:
		return d.duplicateRanges()
	default:
		return nil
	}
}

// fixupSegmentEnds corrects the end position of every open (end == 0) or
// comma-trailing segment so that value extraction and dropped-range
// computation see the true value boundary.
func (d *dedupState) fixupSegmentEnds() {
	for i := range d.segments {
		seg := &d.segments[i]
		if seg.suppress {
			continue
		}
		closeAt := d.scopes[seg.scope].closeAt
		if seg.end <= 0 {
			// Last field of an object (or an unresolved segment): end at the
			// closing brace, then strip a dangling separator left by eager
			// suppression.
			end := closeAt
			for end > seg.colon+1 && (d.rawBuf[end-1] == ',' || d.rawBuf[end-1] == ' ') {
				end--
			}
			seg.end = end
		}
		// Value begins just after the colon (and optional space). Its true
		// end is found by scanning one complete JSON value, independent of
		// the (possibly eagerly-suppressed) following segment.
		start := seg.colon + 1
		for start < len(d.rawBuf) && d.rawBuf[start] == ' ' {
			start++
		}
		seg.end = scanJSONValue(d.rawBuf, start)
	}
}

func (d *dedupState) reportConflicts(buf []byte) {
	if d.onDup == nil {
		return
	}
	byScope := d.segmentsByScope()
	for scopeIdx, refs := range byScope {
		first := make(map[string]int, len(refs))
		for _, ref := range refs {
			seg := &d.segments[ref]
			if prior, ok := first[seg.key]; ok {
				d.onDup(DuplicateKeyConflict{
					Path:          d.scopePath(scopeIdx),
					Key:           seg.key,
					ExistingValue: valueText(buf, &d.segments[prior]),
					NewValue:      valueText(buf, seg),
				})
			} else {
				first[seg.key] = ref
			}
		}
	}
}

// segmentsByScope groups segment indexes per scope, preserving write order.
func (d *dedupState) segmentsByScope() map[int][]int {
	out := make(map[int][]int)
	for i := range d.segments {
		out[d.segments[i].scope] = append(out[d.segments[i].scope], i)
	}
	return out
}

// duplicateRanges computes byte ranges occupied by dropped fields, including
// exactly one adjacent comma (and optional whitespace) so removal keeps the
// JSON valid. It also skips zero-width segments produced by eager
// suppression: those fields were never written, so they occupy no bytes.
func (d *dedupState) duplicateRanges() [][2]int {
	byScope := d.segmentsByScope()
	drop := make(map[int]bool)
	for _, refs := range byScope {
		if d.policy == KeepLast {
			kept := make(map[string]int, len(refs))
			for _, ref := range refs {
				if !d.segments[ref].suppress {
					kept[d.segments[ref].key] = ref
				}
			}
			for _, ref := range refs {
				if !d.segments[ref].suppress &&
					kept[d.segments[ref].key] != ref {
					drop[ref] = true
				}
			}
		} else {
			seen := make(map[string]bool, len(refs))
			for _, ref := range refs {
				if d.segments[ref].suppress {
					// Never written (eager KeepFirst suppression): nothing to
					// remove.
					continue
				}
				key := d.segments[ref].key
				if seen[key] {
					drop[ref] = true
				} else {
					seen[key] = true
				}
			}
		}
	}
	if len(drop) == 0 {
		return nil
	}

	var ranges [][2]int
	for scopeIdx, refs := range byScope {
		for i, ref := range refs {
			if !drop[ref] {
				continue
			}
			seg := &d.segments[ref]

			// Default: remove the key/value and the separator immediately
			// following its value. If this dropped run starts the object
			// (no earlier surviving sibling), there is no trailing separator
			// of our own to consume, so extend to the next surviving
			// sibling's key quote (removing its leading separator).
			start := seg.start
			end := d.afterValueSeparator(seg)

			hasEarlierSurvivor := false
			for j := i - 1; j >= 0; j-- {
				if !d.segments[refs[j]].suppress && !drop[refs[j]] {
					hasEarlierSurvivor = true
					break
				}
			}
			closeAt := d.scopes[scopeIdx].closeAt
			if !hasEarlierSurvivor {
				end = closeAt
				for j := i + 1; j < len(refs); j++ {
					n := &d.segments[refs[j]]
					if n.suppress || drop[refs[j]] {
						continue
					}
					end = n.start
					break
				}
			}
			ranges = append(ranges, [2]int{start, end})
		}
	}

	sort.Slice(ranges, func(i, j int) bool { return ranges[i][0] < ranges[j][0] })
	return mergeRanges(ranges)
}

// afterValueSeparator returns the index just past the element separator
// (", " in spaced mode, "," otherwise) immediately following a segment's
// value, or the value end itself if there is no following separator.
func (d *dedupState) afterValueSeparator(seg *kvSegment) int {
	end := seg.end
	if end < len(d.rawBuf) && d.rawBuf[end] == ',' {
		end++
		if end < len(d.rawBuf) && d.rawBuf[end] == ' ' {
			end++
		}
	}
	return end
}

// scopePath returns the dotted path identifying a scope, using the names of
// its named enclosing scopes. The root path is empty.
func (d *dedupState) scopePath(scopeIdx int) string {
	var parts []string
	for cur := scopeIdx; cur != 0; {
		scope := &d.scopes[cur]
		if scope.name != "" {
			parts = append(parts, scope.name)
		}
		cur = scope.parent
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, ".")
}

// valueText returns the encoded value bytes of a segment for conflict
// reporting. It scans one complete JSON value from the colon, correctly
// skipping commas that appear inside strings, objects, and arrays.
func valueText(buf []byte, seg *kvSegment) string {
	start := seg.colon + 1
	for start < len(buf) && buf[start] == ' ' {
		start++
	}
	return string(buf[start:scanJSONValue(buf, start)])
}

// scanJSONValue returns the index just past the JSON value beginning at i.
func scanJSONValue(buf []byte, i int) int {
	if i >= len(buf) {
		return i
	}
	switch buf[i] {
	case '"':
		// Scanning a string value; the opening quote is at i.
		i++
		for i < len(buf) {
			if buf[i] == '\\' {
				i += 2
				continue
			}
			if buf[i] == '"' {
				return i + 1
			}
			i++
		}
		return i
	case '{':
		return scanJSONContainer(buf, i, '{', '}')
	case '[':
		return scanJSONContainer(buf, i, '[', ']')
	default:
		for i < len(buf) {
			c := buf[i]
			if c == ',' || c == '}' || c == ']' {
				return i
			}
			i++
		}
		return i
	}
}

// scanJSONContainer returns the index just past the balanced container
// starting at i, accounting for strings and nesting.
func scanJSONContainer(buf []byte, i int, open, close byte) int {
	depth := 0
	for i < len(buf) {
		switch buf[i] {
		case '"':
			i = scanJSONValue(buf, i)
			continue
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return i + 1
			}
		}
		i++
	}
	return i
}

func mergeRanges(in [][2]int) [][2]int {
	if len(in) <= 1 {
		return in
	}
	out := in[:1]
	for _, r := range in[1:] {
		last := &out[len(out)-1]
		if r[0] <= last[1] {
			if r[1] > last[1] {
				last[1] = r[1]
			}
			continue
		}
		out = append(out, r)
	}
	return out
}
