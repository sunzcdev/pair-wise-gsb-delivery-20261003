package versioned

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"sync"
)

// Reserved wire keys. They carry the data format's own metadata and must never
// be used as stable field identities (field ids are always >= 1).
const (
	schemaKey  int64 = -1
	versionKey int64 = -2
)

const structTag = "versioned"

type fieldSpec struct {
	id    int64
	index int
	name  string // Go field name, used only for errors
}

type versionSpec struct {
	schema  string
	version uint32
	typ     reflect.Type
	fields  map[int64]fieldSpec
}

type migration struct {
	from, to uint32
	fn       MigrateFunc
}

// Registry holds explicit schema/version registrations and explicit
// version-to-version migrations for a single process.
//
// A zero Registry is ready to use. A Registry is safe for concurrent use once
// registration is complete; registration is typically performed at package
// init time.
type Registry struct {
	mu sync.RWMutex

	versions map[string]map[uint32]*versionSpec
	byType   map[reflect.Type]*versionSpec

	// migrations is keyed by schema and maps (from -> list of edges).
	migrations map[string]map[uint32][]migration
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		versions:   make(map[string]map[uint32]*versionSpec),
		byType:     make(map[reflect.Type]*versionSpec),
		migrations: make(map[string]map[uint32][]migration),
	}
}

// Register associates a Go struct type with a logical schema and one version
// of its wire format.
//
// Exported struct fields opt in by carrying a `versioned:"<id>"` tag whose id
// is a positive integer. The id is the stable wire identity of the field and
// must stay the same even when the Go field is renamed. Fields without a tag
// are not part of the versioned format. Fields tagged `versioned:"-"` are
// explicitly skipped.
//
// Register is explicit by design: there is no name-based matching and no
// inference of any kind.
func (r *Registry) Register(schema string, version uint32, model interface{}) error {
	if schema == "" {
		return fmt.Errorf("versioned: Register: schema id must not be empty")
	}
	if version == 0 {
		return fmt.Errorf("versioned: Register: version must be >= 1")
	}
	if model == nil {
		return fmt.Errorf("versioned: Register(%s v%d): model is nil", schema, version)
	}

	typ := reflect.TypeOf(model)
	if typ.Kind() != reflect.Struct {
		return fmt.Errorf("versioned: Register(%s v%d): %s must be a struct, got %s",
			schema, version, typ, typ.Kind())
	}

	spec, err := buildVersionSpec(schema, version, typ)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.versions == nil {
		r.versions = make(map[string]map[uint32]*versionSpec)
		r.byType = make(map[reflect.Type]*versionSpec)
		r.migrations = make(map[string]map[uint32][]migration)
	}

	if existing := r.byType[typ]; existing != nil {
		return fmt.Errorf("versioned: type %s already registered as %s v%d",
			typ, existing.schema, existing.version)
	}
	byVersion, ok := r.versions[schema]
	if !ok {
		byVersion = make(map[uint32]*versionSpec)
		r.versions[schema] = byVersion
	}
	if dup := byVersion[version]; dup != nil {
		return fmt.Errorf("versioned: %s v%d already registered as %s",
			schema, version, dup.typ)
	}
	byVersion[version] = spec
	r.byType[typ] = spec

	return nil
}

// MustRegister is like Register but panics on error. It is intended for
// package-init-time registrations.
func (r *Registry) MustRegister(schema string, version uint32, model interface{}) {
	if err := r.Register(schema, version, model); err != nil {
		panic(err)
	}
}

func buildVersionSpec(schema string, version uint32, typ reflect.Type) (*versionSpec, error) {
	spec := &versionSpec{
		schema:  schema,
		version: version,
		typ:     typ,
		fields:  make(map[int64]fieldSpec),
	}

	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.PkgPath != "" && !field.Anonymous {
			continue // unexported
		}
		tag, ok := field.Tag.Lookup(structTag)
		if !ok {
			continue
		}
		if tag == "-" {
			continue
		}
		id, err := strconv.ParseInt(tag, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("versioned: %s v%d field %s: invalid field id %q: %w",
				schema, version, field.Name, tag, err)
		}
		if id < 1 {
			return nil, fmt.Errorf("versioned: %s v%d field %s: field id must be >= 1, got %d",
				schema, version, field.Name, id)
		}
		if _, dup := spec.fields[id]; dup {
			return nil, fmt.Errorf("versioned: %s v%d: duplicate field id %d",
				schema, version, id)
		}
		spec.fields[id] = fieldSpec{id: id, index: i, name: field.Name}
	}

	return spec, nil
}

// lookupSpecByType returns the registered spec for the concrete struct type
// behind v.
func (r *Registry) lookupSpecByType(v reflect.Value) (*versionSpec, error) {
	typ := v.Type()
	for typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
		if !v.IsNil() {
			v = v.Elem()
		}
	}

	r.mu.RLock()
	spec := r.byType[typ]
	r.mu.RUnlock()

	if spec == nil {
		return nil, fmt.Errorf("versioned: type %s is not registered", typ)
	}
	return spec, nil
}

func (r *Registry) lookupVersion(schema string, version uint32) (*versionSpec, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	byVersion, ok := r.versions[schema]
	if !ok {
		return nil, &SchemaNotFoundError{Schema: schema}
	}
	spec := byVersion[version]
	if spec == nil {
		return nil, &VersionNotFoundError{Schema: schema, Version: version}
	}
	return spec, nil
}

// sortedVersions returns the registered versions of schema ascending.
func (r *Registry) sortedVersions(schema string) []uint32 {
	r.mu.RLock()
	defer r.mu.RUnlock()

	byVersion := r.versions[schema]
	out := make([]uint32, 0, len(byVersion))
	for v := range byVersion {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
