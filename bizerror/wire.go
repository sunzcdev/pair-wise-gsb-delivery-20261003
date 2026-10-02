// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package bizerror

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

// envelopeVersion is the wire format version carried by every envelope.
const envelopeVersion = 1

// wireEnvelope is the transport independent representation of one business
// error tree. It is JSON encoded for both net/rpc and gRPC, which keeps the
// protocol usable by non-Go plugins as well.
type wireEnvelope struct {
	Version int        `json:"version"`
	Root    int        `json:"root"`
	Nodes   []wireNode `json:"nodes"`
}

type wireNode struct {
	ID      int                        `json:"id"`
	TypeID  string                     `json:"type_id,omitempty"`
	Message string                     `json:"message"`
	Fields  map[string]json.RawMessage `json:"fields,omitempty"`
	Causes  []int                      `json:"causes,omitempty"`

	// Ref > 0 turns the node into an alias pointing back at Nodes[Ref]
	// (IDs are 1-based). Aliases exist solely to render cyclic or
	// diamond-shaped error graphs without infinite recursion.
	Ref int `json:"ref,omitempty"`

	// Sentinel names one of the protocol's built-in terminal markers that
	// replaces dropped content. It is a stable string so non-Go plugins can
	// emit it too.
	Sentinel string `json:"sentinel,omitempty"`
}

const (
	sentinelLimit  = "limit_exceeded"
	sentinelCycle  = "cyclic_reference"
	sentinelBadRef = "bad_reference"
)

// sentinelError maps a wire sentinel name back to the package marker.
func sentinelError(name string) (error, bool) {
	switch name {
	case sentinelLimit:
		return ErrLimitExceeded, true
	case sentinelCycle:
		return ErrCyclicReference, true
	case sentinelBadRef:
		return ErrBadReference, true
	default:
		return nil, false
	}
}

// encoder walks a local error graph into wire nodes, applying the registry
// export policy and the protocol bounds.
type encoder struct {
	reg     *Registry
	nodes   []*wireNode
	indexed map[uintptr]int // pointer identity -> node id
	depth   int

	// truncated records that some part of the tree could not be represented
	// within the limits; decoding adds ErrLimitExceeded at those points.
	truncated bool

	// business reports whether the tree contains at least one registered
	// or generic business error. A tree of only plain errors must not be
	// turned into an envelope (otherwise every error would look business).
	business bool
}

// encodeRoot converts a single error into an envelope. ok is false when the
// error (and its whole chain) contains no registered business error, in
// which case callers must leave the error on its original transport path.
func encodeRoot(reg *Registry, err error) (env wireEnvelope, ok bool) {
	e := &encoder{
		reg:     reg,
		indexed: make(map[uintptr]int),
	}

	rootID := e.encode(err)
	if rootID <= 0 || !e.business {
		return wireEnvelope{}, false
	}

	nodes := make([]wireNode, len(e.nodes))
	for i, n := range e.nodes {
		nodes[i] = *n
	}
	return wireEnvelope{Version: envelopeVersion, Root: rootID, Nodes: nodes}, true
}

func (e *encoder) encode(err error) int {
	if err == nil {
		return 0
	}
	if e.depth >= MaxDepth {
		e.truncated = true
		return e.placeholderNode(ErrLimitExceeded)
	}

	// Pointer-identical errors share one node; the second visit becomes a
	// reference. This is what makes cycles (and diamonds) encodable.
	key, hasKey := identityKey(err)
	if hasKey {
		if id, seen := e.indexed[key]; seen {
			return e.refNode(id)
		}
	}

	node := &wireNode{ID: len(e.nodes) + 1, Message: safeMessage(err)}
	if hasKey {
		e.indexed[key] = node.ID
	}

	// A generic *Error is emitted verbatim: a plugin can raise an agreed
	// type identifier without a local Go registration (or emit a type the
	// receiver does not know yet). Explicitly supplied fields are the
	// emitter's own choice, so they bypass the local allow-list but still
	// obey the size/count bounds.
	if generic, isGeneric := err.(*Error); isGeneric {
		e.business = true
		node.TypeID = generic.TypeID
		node.Fields = e.marshalGenericFields(generic.Fields)
		e.nodes = append(e.nodes, node)
		e.encodeCauses(node, generic.causes)
		return node.ID
	}

	known := false
	if entry, matched := e.reg.lookupByGoType(err); matched {
		e.business = true
		e.populateRegistered(node, entry, err)
		known = true
	} else {
		for _, entry := range e.reg.fallbackHooks() {
			fields, ok := e.safeEncode(entry, err)
			if !ok {
				continue
			}
			if fields == nil {
				continue
			}
			node.TypeID = entry.TypeID
			e.populateFromFields(node, entry, fields)
			known = true
			break
		}
		if known {
			e.business = true
		}
	}

	// An unregistered plain error still has to be carried when it sits
	// inside a business error tree (e.g. a cause), but it carries no
	// fields and no type.
	e.nodes = append(e.nodes, node)

	e.encodeCauses(node, causesOf(err))

	// Plain wrappers (multi-cause groups, links) become business nodes when
	// at least one of their causes did.
	if len(node.Causes) > 0 {
		e.business = true
	}

	_ = known
	return node.ID
}

// safeEncode runs an Encode hook with panic protection so a buggy hook
// cannot crash the plugin while handling an unrelated error.
func (e *encoder) safeEncode(entry *registration, err error) (fields map[string]any, ok bool) {
	defer func() {
		if recover() != nil {
			fields = nil
			ok = false
		}
	}()
	got, err := entry.Encode(err)
	return got, err == nil
}

func (e *encoder) encodeCauses(node *wireNode, causes []error) {
	// Reserve the final slot for the truncation sentinel when needed, so the
	// wire form never lists more than MaxCauses cause references.
	for i, cause := range causes {
		moreAfter := i < len(causes)-1
		// Keep one slot free for the sentinel if any cause would otherwise
		// be dropped.
		capReal := MaxCauses
		if moreAfter {
			capReal = MaxCauses - 1
		}
		if len(node.Causes) >= capReal || len(e.nodes) >= MaxNodes {
			node.Causes = append(node.Causes, e.placeholderNode(ErrLimitExceeded))
			e.truncated = true
			return
		}
		e.depth++
		id := e.encode(cause)
		e.depth--
		if id > 0 {
			node.Causes = append(node.Causes, id)
		}
	}
}

func (e *encoder) marshalGenericFields(fields map[string]any) map[string]json.RawMessage {
	if len(fields) == 0 {
		return nil
	}
	out := make(map[string]json.RawMessage, len(fields))
	for name, value := range fields {
		if len(out) >= MaxFields {
			e.truncated = true
			break
		}
		raw, err := json.Marshal(value)
		if err != nil || len(raw) > MaxFieldBytes {
			if err == nil {
				e.truncated = true
			}
			continue
		}
		out[name] = raw
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (e *encoder) populateRegistered(node *wireNode, entry *registration, err error) {
	node.TypeID = entry.TypeID

	switch {
	case entry.Encode != nil:
		func() {
			defer func() { _ = recover() }()
			fields, hookErr := entry.Encode(err)
			if hookErr != nil {
				// A failing hook must not expose anything by accident.
				return
			}
			e.populateFromFields(node, entry, fields)
		}()
	case entry.goType != nil:
		node.Fields = reflectiveFields(err, entry.fieldAllow)
	}
}

func (e *encoder) populateFromFields(node *wireNode, entry *registration, fields map[string]any) {
	out := make(map[string]json.RawMessage, len(fields))
	for name, value := range fields {
		if len(out) >= MaxFields {
			e.truncated = true
			break
		}
		if _, allowed := entry.fieldAllow[name]; !allowed {
			// Default-deny: hooks cannot leak fields they did not declare.
			continue
		}
		raw, err := json.Marshal(value)
		if err != nil {
			continue
		}
		if len(raw) > MaxFieldBytes {
			e.truncated = true
			continue
		}
		out[name] = raw
	}
	if len(out) > 0 {
		node.Fields = out
	}
}

func reflectiveFields(err error, allow map[string]struct{}) map[string]json.RawMessage {
	if len(allow) == 0 {
		return nil
	}
	v := reflect.ValueOf(err).Elem()
	t := v.Type()
	out := make(map[string]json.RawMessage, len(allow))
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.PkgPath != "" {
			continue // unexported
		}
		if _, ok := allow[field.Name]; !ok {
			continue
		}
		raw, err := json.Marshal(v.Field(i).Interface())
		if err != nil || len(raw) > MaxFieldBytes {
			continue
		}
		out[field.Name] = raw
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (e *encoder) refNode(target int) int {
	node := &wireNode{ID: len(e.nodes) + 1, Ref: target}
	e.nodes = append(e.nodes, node)
	return node.ID
}

func (e *encoder) placeholderNode(marker error) int {
	node := &wireNode{ID: len(e.nodes) + 1, Message: marker.Error(), Sentinel: sentinelName(marker)}
	node.Causes = nil
	e.nodes = append(e.nodes, node)
	return node.ID
}

func sentinelName(marker error) string {
	switch {
	case errors.Is(marker, ErrLimitExceeded):
		return sentinelLimit
	case errors.Is(marker, ErrCyclicReference):
		return sentinelCycle
	case errors.Is(marker, ErrBadReference):
		return sentinelBadRef
	default:
		return ""
	}
}

// identityKey returns a stable key for pointer-backed error values. Slice
// headers (multi-cause groups) deliberately key on their backing array.
func identityKey(err error) (uintptr, bool) {
	switch v := err.(type) {
	case *Error:
		return reflect.ValueOf(v).Pointer(), true
	}
	rv := reflect.ValueOf(err)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func:
		return rv.Pointer(), rv.Kind() == reflect.Pointer
	}
	return 0, false
}

// causesOf implements the standard single/multi Unwrap contract without
// depending on Go version-specific helpers.
func causesOf(err error) []error {
	switch u := err.(type) {
	case interface{ Unwrap() []error }:
		return u.Unwrap()
	case interface{ Unwrap() error }:
		if c := u.Unwrap(); c != nil {
			return []error{c}
		}
	}
	return nil
}

func safeMessage(err error) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			msg = fmt.Sprintf("bizerror: error message panicked: %v", r)
		}
	}()
	return err.Error()
}
