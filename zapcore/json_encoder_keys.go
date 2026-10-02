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
	"fmt"
	"strings"
)

type duplicateKeyScope struct {
	path               []string
	keys               map[string]keyRecord
	pending            *pendingKey
	pendingAfter       *pendingKey
	pendingAfterObject bool
	array              bool
	arrayIndex         int
	closingKey         string
	closingStart       int
	closingInherited   bool
}

type pendingKey struct {
	key       string
	start     int
	inherited bool
}

type keyRecord struct {
	start     int
	end       int
	inherited bool
}

type duplicateKeyTracker struct {
	scopes        []duplicateKeyScope
	pendingScopes []duplicateKeyScope
	inherited     bool
}

func newDuplicateKeyTracker() *duplicateKeyTracker {
	return &duplicateKeyTracker{scopes: []duplicateKeyScope{{keys: make(map[string]keyRecord)}}}
}

func (t *duplicateKeyTracker) clone() *duplicateKeyTracker {
	if t == nil {
		return nil
	}
	clone := &duplicateKeyTracker{
		scopes:        make([]duplicateKeyScope, len(t.scopes)),
		pendingScopes: cloneScopes(t.pendingScopes),
		inherited:     t.inherited,
	}
	clone.scopes = cloneScopes(t.scopes)
	return clone
}

func cloneScopes(scopes []duplicateKeyScope) []duplicateKeyScope {
	clone := make([]duplicateKeyScope, len(scopes))
	for i, scope := range scopes {
		clone[i].path = append([]string(nil), scope.path...)
		clone[i].array = scope.array
		clone[i].arrayIndex = scope.arrayIndex
		clone[i].closingKey = scope.closingKey
		clone[i].closingStart = scope.closingStart
		clone[i].closingInherited = scope.closingInherited
		clone[i].pendingAfterObject = scope.pendingAfterObject
		if scope.keys != nil {
			clone[i].keys = make(map[string]keyRecord, len(scope.keys))
			for key, record := range scope.keys {
				clone[i].keys[key] = record
			}
		}
		if scope.pending != nil {
			pending := *scope.pending
			clone[i].pending = &pending
		}
		if scope.pendingAfter != nil {
			pending := *scope.pendingAfter
			clone[i].pendingAfter = &pending
		}
	}
	return clone
}

func (t *duplicateKeyTracker) beginEntry() {
	if len(t.scopes) > 1 {
		t.pendingScopes = cloneScopes(t.scopes[1:])
		t.scopes = t.scopes[:1]
	}
	for i := range t.scopes {
		for key, record := range t.scopes[i].keys {
			record.inherited = true
			t.scopes[i].keys[key] = record
		}
	}
	for i := range t.pendingScopes {
		for key, record := range t.pendingScopes[i].keys {
			record.inherited = true
			t.pendingScopes[i].keys[key] = record
		}
	}
	t.inherited = false
}

func (t *duplicateKeyTracker) attachEntryScopes() {
	if len(t.pendingScopes) > 0 {
		t.scopes = append(t.scopes, t.pendingScopes...)
		t.pendingScopes = nil
	}
}

func (t *duplicateKeyTracker) shiftPendingInherited(delta int) {
	for i := range t.pendingScopes {
		for key, record := range t.pendingScopes[i].keys {
			record.start += delta
			record.end += delta
			t.pendingScopes[i].keys[key] = record
		}
	}
}

func (t *duplicateKeyTracker) path() string {
	var builder strings.Builder
	builder.WriteByte('$')
	for _, segment := range t.scopes[len(t.scopes)-1].path {
		if strings.HasPrefix(segment, "[") {
			builder.WriteString(segment)
		} else {
			builder.WriteByte('.')
			builder.WriteString(segment)
		}
	}
	return builder.String()
}

func (t *duplicateKeyTracker) prepare(key string, start int, policy DuplicateKeyPolicy, report func(DuplicateKeyInfo)) bool {
	scope := &t.scopes[len(t.scopes)-1]
	if scope.keys == nil {
		scope.keys = make(map[string]keyRecord)
	}
	pending := &pendingKey{key: key, start: start, inherited: t.inherited}
	if _, ok := scope.keys[key]; ok {
		if report != nil {
			report(DuplicateKeyInfo{Path: t.path(), Key: key})
		}
		if policy == DuplicateKeyKeepFirst {
			return false
		}
	}
	scope.pending = pending
	return true
}

func (t *duplicateKeyTracker) finish(end int) {
	scope := &t.scopes[len(t.scopes)-1]
	if scope.pending == nil {
		return
	}
	scope.keys[scope.pending.key] = keyRecord{
		start:     scope.pending.start,
		end:       end,
		inherited: scope.pending.inherited,
	}
	scope.pending = nil
}

func (t *duplicateKeyTracker) take(key string) (keyRecord, bool) {
	scope := &t.scopes[len(t.scopes)-1]
	record, ok := scope.keys[key]
	if ok {
		delete(scope.keys, key)
	}
	return record, ok
}

func (t *duplicateKeyTracker) openObject() {
	parent := &t.scopes[len(t.scopes)-1]
	path := append([]string(nil), parent.path...)
	var pendingAfter *pendingKey
	if parent.array {
		path = append(path, fmt.Sprintf("[%d]", parent.arrayIndex))
	} else if parent.pending != nil {
		path = append(path, parent.pending.key)
		pendingAfter = parent.pending
		parent.pending = nil
	}
	child := duplicateKeyScope{path: path, keys: make(map[string]keyRecord), pendingAfter: pendingAfter}
	child.pendingAfterObject = true
	t.scopes = append(t.scopes, child)
}

func (t *duplicateKeyTracker) openArray() {
	parent := &t.scopes[len(t.scopes)-1]
	path := append([]string(nil), parent.path...)
	var pendingAfter *pendingKey
	if parent.pending != nil {
		path = append(path, parent.pending.key)
		pendingAfter = parent.pending
		parent.pending = nil
	}
	t.scopes = append(t.scopes, duplicateKeyScope{
		path:               path,
		keys:               make(map[string]keyRecord),
		array:              true,
		arrayIndex:         -1,
		pendingAfter:       pendingAfter,
		pendingAfterObject: false,
	})
}

func (t *duplicateKeyTracker) arrayElement() {
	scope := &t.scopes[len(t.scopes)-1]
	if scope.array {
		scope.arrayIndex++
	}
}

func (t *duplicateKeyTracker) closeObject(end int) {
	if len(t.scopes) <= 1 {
		return
	}
	child := t.scopes[len(t.scopes)-1]
	t.scopes = t.scopes[:len(t.scopes)-1]
	if child.pendingAfter != nil && child.pendingAfterObject {
		t.scopes[len(t.scopes)-1].pending = child.pendingAfter
		t.finish(end)
	}
}

func (t *duplicateKeyTracker) closeArray(end int) {
	if len(t.scopes) <= 1 {
		return
	}
	array := t.scopes[len(t.scopes)-1]
	t.scopes = t.scopes[:len(t.scopes)-1]
	if array.pendingAfter != nil && !array.pendingAfterObject {
		t.scopes[len(t.scopes)-1].pending = array.pendingAfter
		t.finish(end)
	}
}

func (t *duplicateKeyTracker) openNamespace() {
	scope := &t.scopes[len(t.scopes)-1]
	if scope.pending == nil {
		return
	}
	key := scope.pending.key
	start := scope.pending.start
	inherited := scope.pending.inherited
	scope.keys[key] = keyRecord{start: start, end: start, inherited: inherited}
	scope.pending = nil
	path := append([]string(nil), scope.path...)
	path = append(path, key)
	t.scopes = append(t.scopes, duplicateKeyScope{
		path:             path,
		keys:             make(map[string]keyRecord),
		closingKey:       key,
		closingStart:     start,
		closingInherited: inherited,
	})
}

func (t *duplicateKeyTracker) closeNamespace(end int) {
	if len(t.scopes) <= 1 {
		return
	}
	child := t.scopes[len(t.scopes)-1]
	t.scopes = t.scopes[:len(t.scopes)-1]
	if child.closingKey != "" {
		parent := &t.scopes[len(t.scopes)-1]
		parent.keys[child.closingKey] = keyRecord{start: child.closingStart, end: end, inherited: child.closingInherited}
	}
}

func (t *duplicateKeyTracker) remove(record keyRecord, data []byte) (int, int) {
	start, end := record.start, record.end
	if start > 0 {
		for start > 0 && data[start-1] == ' ' {
			start--
		}
		if start > 0 && data[start-1] == ',' {
			start--
		}
	}
	for i := range t.scopes {
		for key, existing := range t.scopes[i].keys {
			if existing.start == record.start {
				delete(t.scopes[i].keys, key)
			} else if existing.start >= end {
				existing.start -= end - start
				existing.end -= end - start
				t.scopes[i].keys[key] = existing
			}
		}
	}
	return start, end
}

func (t *duplicateKeyTracker) shiftInherited(position, delta int) {
	for i := range t.scopes {
		for key, record := range t.scopes[i].keys {
			if !record.inherited {
				continue
			}
			if record.start >= position {
				record.start += delta
				record.end += delta
				t.scopes[i].keys[key] = record
			}
		}
	}
}
