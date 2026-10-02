package versioning

import (
	"fmt"
	"reflect"
	"sync"

	"github.com/vmihailenco/msgpack/v5"
)

// Conversion migrates one record between two schema versions. It receives the
// fields keyed by stable wire IDs and must return the fields of the next
// version. A conversion must be pure: it should not rely on the Go names of
// fields and must not assume a deployment-specific configuration, since the
// source version travels inside the data itself.
//
// When the type change cannot preserve all information (for example a
// fractional float becoming an int), return an error wrapping ErrLossy.
type Conversion func(in Fields) (Fields, error)

// Registry binds Go struct types to schema versions and holds the explicit
// conversions between versions. A Registry is safe for concurrent use.
type Registry struct {
	mu       sync.RWMutex
	byGoType map[reflect.Type]*typeInfo
	byVer    map[uint64]*typeInfo
	conv     map[[2]uint64]Conversion
	cache    typeCache
}

// NewRegistry creates an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		byGoType: make(map[reflect.Type]*typeInfo),
		byVer:    make(map[uint64]*typeInfo),
		conv:     make(map[[2]uint64]Conversion),
	}
}

// Register marks the struct type of v as version ver. v may be a value or a
// pointer to the struct. Every exported field must carry a unique
// `version:"id"` tag providing its stable wire ID.
func (r *Registry) Register(ver uint64, v interface{}) error {
	if v == nil {
		return fmt.Errorf("versioning: register version %d: nil value", ver)
	}
	typ := reflect.TypeOf(v)
	if typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}

	ti, err := r.cache.get(typ, ver)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.byGoType[typ]; ok && existing.ver != ver {
		return fmt.Errorf("versioning: %s already registered at version %d",
			typ, existing.ver)
	}
	if existing, ok := r.byVer[ver]; ok && existing.typ != typ {
		return fmt.Errorf("versioning: version %d already registered to %s",
			ver, existing.typ)
	}
	r.byGoType[typ] = ti
	r.byVer[ver] = ti
	return nil
}

// MustRegister is like Register but panics on error.
func (r *Registry) MustRegister(ver uint64, v interface{}) {
	if err := r.Register(ver, v); err != nil {
		panic(err)
	}
}

// Conversion registers the function that upgrades records from fromVer to
// toVer. Conversions are the only mechanism for changing schemas: the package
// never infers a migration from field names. The edge must connect two
// registered versions and must advance to a different version.
func (r *Registry) Conversion(fromVer, toVer uint64, fn Conversion) error {
	if fn == nil {
		return fmt.Errorf("versioning: conversion %d->%d is nil", fromVer, toVer)
	}
	if fromVer == toVer {
		return fmt.Errorf("versioning: conversion %d->%d is a no-op", fromVer, toVer)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byVer[fromVer]; !ok {
		return fmt.Errorf("versioning: conversion %d->%d: source version is not registered",
			fromVer, toVer)
	}
	if _, ok := r.byVer[toVer]; !ok {
		return fmt.Errorf("versioning: conversion %d->%d: target version is not registered",
			fromVer, toVer)
	}
	key := [2]uint64{fromVer, toVer}
	if _, dup := r.conv[key]; dup {
		return fmt.Errorf("versioning: conversion %d->%d already registered",
			fromVer, toVer)
	}
	r.conv[key] = fn
	return nil
}

// MustConversion is like Conversion but panics on error.
func (r *Registry) MustConversion(fromVer, toVer uint64, fn Conversion) {
	if err := r.Conversion(fromVer, toVer, fn); err != nil {
		panic(err)
	}
}

// lookupGoType returns the registered type for a reflect type (dereferencing
// pointers).
func (r *Registry) lookupGoType(typ reflect.Type) (*typeInfo, bool) {
	if typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	ti, ok := r.byGoType[typ]
	return ti, ok
}

// path returns the ordered conversions to move from dataVer to targetVer. A
// nil path with no error means the versions already match. Edges may skip
// version numbers; only explicitly registered edges are used.
func (r *Registry) path(dataVer, targetVer uint64) ([]Conversion, error) {
	if dataVer == targetVer {
		return nil, nil
	}

	r.mu.RLock()
	targetOK := r.byVer[targetVer] != nil
	sourceOK := r.byVer[dataVer] != nil
	convs := make(map[[2]uint64]Conversion, len(r.conv))
	next := make(map[uint64][][2]uint64)
	for edge, fn := range r.conv {
		convs[edge] = fn
		next[edge[0]] = append(next[edge[0]], edge)
	}
	r.mu.RUnlock()

	if !targetOK || !sourceOK {
		return nil, fmt.Errorf("%w: have %d want %d", ErrUnknownVersion, dataVer, targetVer)
	}

	// BFS from dataVer, following only explicitly registered edges. Each
	// visited version remembers the conversion that first reached it.
	visited := map[uint64]bool{dataVer: true}
	parentFn := map[uint64]Conversion{}
	parentVer := map[uint64]uint64{}
	queue := []uint64{dataVer}
	found := false
	for len(queue) > 0 && !found {
		cur := queue[0]
		queue = queue[1:]
		for _, edge := range next[cur] {
			to := edge[1]
			if visited[to] {
				continue
			}
			visited[to] = true
			parentFn[to] = convs[edge]
			parentVer[to] = cur
			if to == targetVer {
				found = true
				break
			}
			queue = append(queue, to)
		}
	}
	if !found {
		return nil, fmt.Errorf("%w: from %d to %d", ErrNoConversion, dataVer, targetVer)
	}

	// Rebuild the ordered conversions backwards, then reverse.
	var path []Conversion
	for v := targetVer; v != dataVer; v = parentVer[v] {
		path = append(path, parentFn[v])
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path, nil
}

// Marshal encodes v. When v's type is registered it is written as a versioned
// envelope carrying that type's schema version. Otherwise v is encoded with
// plain msgpack, producing identical bytes to msgpack.Marshal(v).
func (r *Registry) Marshal(v interface{}) ([]byte, error) {
	if v == nil {
		return msgpack.Marshal(v)
	}
	rv := reflect.ValueOf(v)
	ti, ok := r.lookupGoType(rv.Type())
	if !ok {
		return msgpack.Marshal(v)
	}

	fields, err := ti.encodeFields(rv)
	if err != nil {
		return nil, err
	}
	return encodeEnvelope(ti.ver, fields)
}

// Unmarshal decodes data into v, which must be a pointer to a registered
// struct. The schema version is read from the data envelope and the record is
// upgraded to v's version through the registered conversions. Plain
// (non-versioned) MessagePack is rejected here: use msgpack.Unmarshal for data
// that never opted into versioning.
func (r *Registry) Unmarshal(data []byte, v interface{}) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return fmt.Errorf("versioning: Unmarshal requires a non-nil pointer")
	}
	ti, ok := r.lookupGoType(rv.Type())
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotRegistered, rv.Type())
	}

	dataVer, fields, err := decodeEnvelope(data)
	if err != nil {
		return err
	}

	steps, err := r.path(dataVer, ti.ver)
	if err != nil {
		return err
	}
	for i, step := range steps {
		fields, err = step(fields)
		if err != nil {
			return fmt.Errorf("versioning: conversion step %d failed: %w", i, err)
		}
	}
	return ti.decodeFields(rv, fields)
}
