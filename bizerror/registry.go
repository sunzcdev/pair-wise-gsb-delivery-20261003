// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

// Package bizerror defines an opt-in protocol for sending structured
// "business errors" between a go-plugin host and a plugin process.
//
// # Why this exists
//
// By default only the textual message of an error crosses a plugin
// boundary. A host that needs to tell "resource not found" apart from
// "temporarily unavailable" is forced to match error strings, which is
// brittle. This package lets both sides agree on error types identified by
// stable strings and reconstruct them on the receiving side so the host can
// classify them with errors.Is and errors.As.
//
// The protocol is entirely optional:
//
//   - Plugins that do not participate send plain errors exactly as before.
//   - Error types that were not registered on the receiving side are not
//     guessed at; they come back as [Error] values carrying [ErrUnknownType]
//     instead of being silently turned into some unrelated local type.
//   - Transport errors (dial failures, dead connections, plugin crashes,
//     handshake failures, ...) never look like business errors and keep
//     their existing classification.
//
// # Registering a type
//
// Both processes register the types they understand, using an identifier
// that must never be tied to a Go type name:
//
//	type NotFoundError struct {
//		Resource string
//		Secret   string // must not leave the plugin process
//	}
//
//	func (e *NotFoundError) Error() string { return e.Resource + " not found" }
//
//	reg := bizerror.NewRegistry()
//	reg.MustRegister(bizerror.Registration{
//		TypeID: "kv.v1.not_found",
//		Prototype: func() *NotFoundError { return &NotFoundError{} },
//		// Only these fields are put on the wire. Secret stays local.
//		ExportedFields: []string{"Resource"},
//	})
//
// Custom encode/decode hooks ([Registration.Encode] /
// [Registration.Decode]) allow complete control over the exported field set
// without exposing struct internals.
//
// Sending and receiving is transport specific; see [NetRPCEncode] and
// [GRPCServerInterceptor] / [GRPCClientInterceptor].
package bizerror

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
)

// FieldName is the name of an exported field, exactly as it appears on the
// Go struct (not a wire name). A type may expose at most [MaxFields] fields.
type FieldName = string

// Registration describes one agreed business error type.
type Registration struct {
	// TypeID is the stable, transport independent identifier agreed by
	// host and plugin, for example "kv.v1.not_found". It must be unique
	// within a registry and non-empty.
	TypeID string

	// Prototype returns a fresh zero-value instance of the error type.
	// When non-nil, Prototype is used both to recognise errors returned by
	// the local implementation (by assignable type) and to allocate the
	// value that received fields are written into.
	//
	// Prototype and Encode are mutually exclusive on registration; Decode
	// may be combined with Prototype to post-process a reflectively
	// populated value.
	Prototype func() error

	// Encode converts an error of this type into its stable wire form. It
	// is invoked for every error assignable to the type recognised through
	// Prototype, or for every error when Prototype is nil.
	//
	// The returned fields are filtered again against ExportedFields, so a
	// hook can never leak a field that was not explicitly declared.
	Encode func(error) (fields map[string]any, err error)

	// Decode builds an error of this type from the fields that crossed the
	// boundary (already limited to ExportedFields on the sender). When nil,
	// the value produced by Prototype is filled in reflectively and used
	// directly.
	Decode func(fields map[string]any) (error, error)

	// ExportedFields is the allow-list of field names allowed on the wire.
	// Fields produced by Encode or read reflectively from Prototype that
	// are not in this list are dropped. An empty/nil list means no fields
	// cross at all: only the type and message are shared. This is the
	// default-deny rule that keeps sensitive data inside the process that
	// owns it.
	ExportedFields []FieldName
}

type registration struct {
	Registration

	goType     reflect.Type // when Prototype is set
	fieldAllow map[string]struct{}
}

// Registry holds the business error types understood by one process. A
// registry is safe for concurrent use after [Registry.Register] calls have
// finished; the typical pattern is to build one during startup (or in an
// init helper) and share it afterwards.
type Registry struct {
	mu      sync.RWMutex
	byType  map[string]*registration
	byGoID  map[reflect.Type]*registration
	ordered []*registration // stable matching order for Prototype-less hooks
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		byType: make(map[string]*registration),
		byGoID: make(map[reflect.Type]*registration),
	}
}

// Register adds one type. It returns an error for malformed or duplicate
// registrations rather than changing existing state.
func (r *Registry) Register(reg Registration) error {
	if reg.TypeID == "" {
		return errors.New("bizerror: registration requires a non-empty TypeID")
	}
	if reg.Prototype == nil && reg.Encode == nil {
		return fmt.Errorf("bizerror: registration %q requires Prototype or Encode", reg.TypeID)
	}

	entry := &registration{Registration: reg}
	entry.fieldAllow = make(map[string]struct{}, len(reg.ExportedFields))
	for _, name := range reg.ExportedFields {
		entry.fieldAllow[name] = struct{}{}
	}

	if reg.Prototype != nil {
		proto := reg.Prototype()
		if proto == nil {
			return fmt.Errorf("bizerror: registration %q Prototype returned nil", reg.TypeID)
		}
		t := reflect.TypeOf(proto)
		if t.Kind() != reflect.Pointer || t.Elem().Kind() != reflect.Struct {
			return fmt.Errorf("bizerror: registration %q Prototype must return a pointer to a struct", reg.TypeID)
		}
		entry.goType = t
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.byType[reg.TypeID]; exists {
		return fmt.Errorf("bizerror: duplicate type registration %q", reg.TypeID)
	}
	if entry.goType != nil {
		if _, exists := r.byGoID[entry.goType]; exists {
			return fmt.Errorf("bizerror: duplicate Go type registration for %q", reg.TypeID)
		}
	}

	r.byType[reg.TypeID] = entry
	if entry.goType != nil {
		r.byGoID[entry.goType] = entry
	}
	r.ordered = append(r.ordered, entry)
	return nil
}

// MustRegister panics when Register fails; intended for package-level setup.
func (r *Registry) MustRegister(reg Registration) {
	if err := r.Register(reg); err != nil {
		panic(err)
	}
}

// lookupByGoType returns the registration responsible for err's concrete
// (pointer) type.
func (r *Registry) lookupByGoType(err error) (*registration, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	t := reflect.TypeOf(err)
	entry, ok := r.byGoID[t]
	return entry, ok
}

// fallbackHooks returns Prototype-less Encode hooks in registration order.
// Each hook decides per error whether it owns it by returning a non-nil
// fields map (which may be empty).
func (r *Registry) fallbackHooks() []*registration {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var out []*registration
	for _, entry := range r.ordered {
		if entry.goType == nil && entry.Encode != nil {
			out = append(out, entry)
		}
	}
	return out
}

// lookupTypeID returns the registration for a stable type identifier.
func (r *Registry) lookupTypeID(typeID string) (*registration, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.byType[typeID]
	return entry, ok
}

// RegisteredTypeIDs reports the type identifiers currently known, sorted.
func (r *Registry) RegisteredTypeIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ids := make([]string, 0, len(r.byType))
	for id := range r.byType {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

var defaultRegistry = NewRegistry()

// Register registers a type on the process-wide default registry.
func Register(reg Registration) error { return defaultRegistry.Register(reg) }

// MustRegister registers a type on the process-wide default registry or
// panics; intended for init-time setup shared by host and plugin packages.
func MustRegister(reg Registration) { defaultRegistry.MustRegister(reg) }
