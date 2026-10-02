// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package bizerror

import (
	"encoding/json"
	"fmt"
	"reflect"
)

// decoder reconstructs a local error graph from an envelope.
type decoder struct {
	reg    *Registry
	nodes  map[int]*wireNode
	built  map[int]error
	active map[int]bool // ids currently on the reconstruction stack
}

// decodeRoot validates the envelope and rebuilds the root error. A malformed
// envelope yields a plain error carrying [ErrBadReference]-style context;
// it never panics and never returns nil for a non-empty envelope.
func decodeRoot(reg *Registry, env wireEnvelope) (error, bool) {
	if env.Version != envelopeVersion || len(env.Nodes) == 0 {
		return nil, false
	}
	if len(env.Nodes) > MaxNodes {
		return nil, false
	}

	d := &decoder{
		reg:    reg,
		nodes:  make(map[int]*wireNode, len(env.Nodes)),
		built:  make(map[int]error, len(env.Nodes)),
		active: make(map[int]bool),
	}
	for i := range env.Nodes {
		n := env.Nodes[i]
		if n.ID <= 0 {
			return nil, false
		}
		if _, dup := d.nodes[n.ID]; dup {
			return nil, false
		}
		// Field overflow is structural and rejected; cause overflow is
		// tolerated and truncated during reconstruction so an over-full node
		// never discards the whole envelope.
		if len(n.Fields) > MaxFields {
			return nil, false
		}
		d.nodes[n.ID] = &n
	}
	root, ok := d.nodes[env.Root]
	if !ok {
		return nil, false
	}
	return d.build(root, 0), true
}

// build reconstructs one node. depth guards against maliciously deep chains
// even though aliases are resolved in constant depth.
func (d *decoder) build(node *wireNode, depth int) error {
	if depth > MaxDepth {
		return &placeholder{cause: ErrLimitExceeded}
	}

	// Alias: point back at a previously defined node.
	if node.Ref > 0 {
		target, ok := d.nodes[node.Ref]
		if !ok {
			return &placeholder{cause: ErrBadReference}
		}
		if d.active[node.Ref] {
			// Back-edge on the reconstruction stack: break the cycle with a
			// recognisable terminal error instead of recursing forever.
			return &placeholder{cause: cycleSentinel(target)}
		}
		return d.build(target, depth+1)
	}

	if err, ok := d.built[node.ID]; ok {
		return err
	}

	// A built-in terminal marker node (truncation, cycle, bad reference).
	if node.Sentinel != "" {
		if marker, ok := sentinelError(node.Sentinel); ok {
			err := &placeholder{cause: marker}
			d.built[node.ID] = err
			return err
		}
	}

	// Mark this node in-progress BEFORE descending: any cause that points
	// (directly or through an alias) back at it is a back-edge and becomes
	// a terminal ErrCyclicReference placeholder, guaranteeing an acyclic
	// reconstructed graph.
	d.active[node.ID] = true
	defer delete(d.active, node.ID)

	var causes []error
	for i, ref := range node.Causes {
		if i >= MaxCauses {
			causes = append(causes, &placeholder{cause: ErrLimitExceeded})
			break
		}
		child, ok := d.nodes[ref]
		if !ok {
			causes = append(causes, &placeholder{cause: ErrBadReference})
			continue
		}
		if child.Ref == 0 && d.active[ref] {
			causes = append(causes, &placeholder{cause: cycleSentinel(child)})
			continue
		}
		causes = append(causes, d.build(child, depth+1))
	}

	var built error
	if node.TypeID != "" {
		if entry, ok := d.reg.lookupTypeID(node.TypeID); ok {
			built = d.buildKnown(entry, node, causes)
		} else {
			built = d.buildUnknown(node, causes)
		}
	} else {
		built = d.buildPlain(node, causes)
	}

	d.built[node.ID] = built
	return built
}

func cycleSentinel(node *wireNode) error {
	if node.TypeID == "" {
		return ErrCyclicReference
	}
	return fmt.Errorf("%w (type %q)", ErrCyclicReference, node.TypeID)
}

func (d *decoder) buildKnown(entry *registration, node *wireNode, causes []error) error {
	fields := decodeFields(node)

	switch {
	case entry.Decode != nil:
		got, err := entry.Decode(fields)
		if err == nil && got != nil {
			return attachCauses(got, causes)
		}
		// A registered hook that refuses its own payload must not be
		// papered over as an unknown type; surface a generic node.
	case entry.Prototype != nil:
		if got := reflectiveAssign(entry, fields); got != nil {
			return attachCauses(got, causes)
		}
	}

	return d.genericNode(node, fields, causes, true)
}

func (d *decoder) buildUnknown(node *wireNode, causes []error) error {
	return d.genericNode(node, decodeFields(node), causes, false)
}

func (d *decoder) buildPlain(node *wireNode, causes []error) error {
	return attachCauses(plainString(node.Message), causes)
}

func (d *decoder) genericNode(node *wireNode, fields map[string]any, causes []error, known bool) error {
	e := &Error{
		TypeID:  node.TypeID,
		Message: node.Message,
		Fields:  fields,
		Known:   known,
		causes:  causes,
	}
	return e
}

func decodeFields(node *wireNode) map[string]any {
	if len(node.Fields) == 0 {
		return nil
	}
	out := make(map[string]any, len(node.Fields))
	for name, raw := range node.Fields {
		if len(out) >= MaxFields {
			break
		}
		if len(raw) > MaxFieldBytes {
			continue
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			continue
		}
		out[name] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// reflectiveAssign populates a Prototype value from the decoded fields using
// the exported field names. JSON numbers land as float64; assignable numeric
// kinds are converted. Incompatible values are skipped.
func reflectiveAssign(entry *registration, fields map[string]any) error {
	proto := entry.Prototype()
	rv := reflect.ValueOf(proto)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return nil
	}
	elem := rv.Elem()
	t := elem.Type()

	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if sf.PkgPath != "" {
			continue
		}
		raw, ok := fields[sf.Name]
		if !ok {
			continue
		}
		fv := elem.Field(i)
		if !fv.CanSet() {
			continue
		}
		assignReflect(fv, raw)
	}
	return proto
}

func assignReflect(dst reflect.Value, raw any) {
	rv := reflect.ValueOf(raw)
	if rv.IsValid() && rv.Type().AssignableTo(dst.Type()) {
		dst.Set(rv)
		return
	}

	// Numeric conversions from float64 (encoding/json's default).
	if f, ok := raw.(float64); ok {
		switch dst.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			dst.SetInt(int64(f))
			return
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			dst.SetUint(uint64(f))
			return
		case reflect.Float32, reflect.Float64:
			dst.SetFloat(f)
			return
		}
	}

	if s, ok := raw.(string); ok && dst.Kind() == reflect.String {
		dst.SetString(s)
		return
	}
	if b, ok := raw.(bool); ok && dst.Kind() == reflect.Bool {
		dst.SetBool(b)
	}
}

// plainString is an untyped error for nodes that carried no TypeID; it is a
// distinct type so it never accidentally matches a registered sentinel.
type plainString string

func (s plainString) Error() string { return string(s) }

// attachCauses presents a concrete reconstructed error together with its
// reconstructed causes. The concrete error is the primary value (so
// errors.As on the root succeeds) and the causes are attached through a
// single-cause Unwrap chain, which the standard library traverses without
// ever revisiting a node: the reconstructed graph is acyclic by design.
//
// When there is exactly one cause we use Unwrap() error; several causes use
// the slice form. The concrete error's own local Unwrap (if any) is not
// trusted across the boundary - the wire causes are the source of truth.
func attachCauses(err error, causes []error) error {
	if len(causes) == 0 {
		return err
	}
	return &withCauses{primary: err, causes: causes}
}

type withCauses struct {
	primary error
	causes  []error
}

func (w *withCauses) Error() string { return w.primary.Error() }

func (w *withCauses) Unwrap() []error {
	out := make([]error, 0, len(w.causes)+1)
	out = append(out, w.primary)
	out = append(out, w.causes...)
	return out
}
