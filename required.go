package toml

import (
	"reflect"
	"strconv"
	"sync"

	"github.com/pelletier/go-toml/v2/unstable"
)

// reqSeg is one segment of the instance path being tracked: a field, table,
// or map key name, annotated with the element index when the segment is
// reached through an array table or an array value (-1 otherwise).
type reqSeg struct {
	name string
	idx  int32
}

// reqInstance records a struct instance that the document provided, so that
// the required fields of its type can be checked once the whole document has
// been decoded.
type reqInstance struct {
	// path holds the display segments of the instance, e.g. ["servers[1]"]
	// for the second element of an array table. Empty for the root.
	path []string
	plan *structPlan
	// provided holds the TOML names of the required fields the document
	// explicitly provided for this instance.
	provided  map[string]struct{}
	highlight unstable.Range
}

// reqPushSeg appends a segment to the current instance path.
func (d *decoder) reqPushSeg(name string, idx int32) {
	d.reqPath = append(d.reqPath, reqSeg{name: name, idx: idx})
}

// reqPop removes the last segment of the current instance path.
func (d *decoder) reqPop() {
	d.reqPath = d.reqPath[:len(d.reqPath)-1]
}

// reqAnnotate sets the array element index of the last segment of the
// current instance path.
func (d *decoder) reqAnnotate(idx int32) {
	if len(d.reqPath) > 0 {
		d.reqPath[len(d.reqPath)-1].idx = idx
	}
}

// reqPathKey encodes the current instance path into the decoder's scratch
// buffer. The result is only valid until the next call. Names cannot contain
// NUL (control characters are rejected by the parser), so NUL and \x01 are
// unambiguous separators.
func (d *decoder) reqPathKey() []byte {
	b := d.reqScratch[:0]
	for i, s := range d.reqPath {
		if i > 0 {
			b = append(b, 0)
		}
		b = append(b, s.name...)
		if s.idx >= 0 {
			b = append(b, 1)
			b = strconv.AppendInt(b, int64(s.idx), 10)
		}
	}
	d.reqScratch = b
	return b
}

// reqProvide records that the document explicitly provided field f of the
// current instance. Only required fields are tracked: they are the only ones
// ever looked up, and a required field implies its instance was registered.
func (d *decoder) reqProvide(f *structField) {
	if !f.required {
		return
	}
	if inst, ok := d.reqInst[string(d.reqPathKey())]; ok {
		inst.provided[f.tomlName] = struct{}{}
	}
}

// reqRegisterInstance registers the struct instance at the current instance
// path, so that its required fields are checked at the end of the decode.
// Only struct types decoded field-by-field that declare required fields are
// registered: scalars, generic containers, and types decoded through the
// Unmarshaler interface have no required fields to check. Registration is
// idempotent per path.
func (d *decoder) reqRegisterInstance(t reflect.Type, expr *unstable.Node) {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	if d.unmarshalerInterface &&
		(t.Implements(unmarshalerType) || reflect.PtrTo(t).Implements(unmarshalerType)) {
		return
	}
	plan := planForType(t)
	if len(plan.required) == 0 {
		return
	}
	key := string(d.reqPathKey())
	if _, ok := d.reqInst[key]; ok {
		return
	}
	if d.reqInst == nil {
		d.reqInst = map[string]*reqInstance{}
	}
	path := make([]string, len(d.reqPath))
	for i, s := range d.reqPath {
		if s.idx >= 0 {
			path[i] = s.name + "[" + strconv.Itoa(int(s.idx)) + "]"
		} else {
			path[i] = s.name
		}
	}
	inst := &reqInstance{
		path:     path,
		plan:     plan,
		provided: map[string]struct{}{},
	}
	if expr != nil {
		inst.highlight = keyLocation(expr)
	}
	d.reqInst[key] = inst
	d.reqOrder = append(d.reqOrder, inst)
}

// missingRequiredError returns the MissingRequiredError cumulated over all
// the struct instances the document provided, or nil when every required
// field was provided. Instances are visited in registration order and fields
// in declaration order, so the result is deterministic.
func (d *decoder) missingRequiredError(document []byte) error {
	if !d.reqEnabled {
		return nil
	}
	var errs []DecodeError
	for _, inst := range d.reqOrder {
		for _, name := range inst.plan.required {
			if _, ok := inst.provided[name]; ok {
				continue
			}
			key := make(Key, 0, len(inst.path)+1)
			key = append(key, inst.path...)
			key = append(key, name)
			display := key[0]
			for _, p := range key[1:] {
				display += "." + p
			}
			highlight := document[inst.highlight.Offset : inst.highlight.Offset+inst.highlight.Length]
			errs = append(errs, *newDecodeError(document, highlight, key, "missing required field "+display))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return &MissingRequiredError{Errors: errs}
}

var requiredTypeCache sync.Map // reflect.Type -> bool

// typeHasRequired reports whether the type tree rooted at t contains at
// least one field tagged `toml:"...,required"`. The result is cached per
// type: it only depends on the (static) type tree.
func typeHasRequired(t reflect.Type) bool {
	if b, ok := requiredTypeCache.Load(t); ok {
		return b.(bool)
	}
	b := hasRequiredFields(t, map[reflect.Type]bool{})
	requiredTypeCache.Store(t, b)
	return b
}

func hasRequiredFields(t reflect.Type, visiting map[reflect.Type]bool) bool {
	for t.Kind() == reflect.Ptr || t.Kind() == reflect.Slice ||
		t.Kind() == reflect.Array || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return false
	}
	if visiting[t] {
		// A recursive type without required fields on the cycle itself.
		return false
	}
	if len(planForType(t).required) > 0 {
		return true
	}
	visiting[t] = true
	defer delete(visiting, t)
	for i := 0; i < t.NumField(); i++ {
		if hasRequiredFields(t.Field(i).Type, visiting) {
			return true
		}
	}
	return false
}
