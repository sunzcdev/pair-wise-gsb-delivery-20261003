// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
)

const (
	maxBusinessErrorNodes  = 128
	maxBusinessErrorDepth  = 32
	maxBusinessErrorFields = 64
)

type businessErrorMeta struct {
	id      string
	message string
	fields  map[string]any
	causes  []error
}

type businessErrorCodec interface {
	encodedBusinessError() *businessErrorMeta
}

// RegisterBusinessError registers a stable business error type and its finite
// field set. Fields mapped to true cross plugin boundaries; false fields stay
// local and are omitted by the encoder.
func RegisterBusinessError[T error](id string, fields map[string]bool) *BusinessErrorType[T] {
	if strings.TrimSpace(id) == "" {
		panic("business error id must not be empty")
	}

	typ := reflect.TypeFor[T]()
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}

	if len(fields) > maxBusinessErrorFields {
		panic(fmt.Sprintf("business error type %q declares too many fields", id))
	}

	known := make(map[string]struct{}, len(fields))
	allowed := make(map[string]struct{}, len(fields))
	for name, exported := range fields {
		if strings.TrimSpace(name) == "" {
			panic("business error field name must not be empty")
		}
		known[name] = struct{}{}
		if exported {
			allowed[name] = struct{}{}
		}
	}

	businessErrorRegistry.mu.Lock()
	defer businessErrorRegistry.mu.Unlock()

	if existing, ok := businessErrorRegistry.byType[typ]; ok {
		if existing.id != id || !sameFieldSets(existing.known, known) || !sameFieldSets(existing.allowed, allowed) {
			panic(fmt.Sprintf("business error type %s is already registered as %q", typ, existing.id))
		}
		return &BusinessErrorType[T]{id: id}
	}
	if existing, ok := businessErrorRegistry.byID[id]; ok {
		panic(fmt.Sprintf("business error id %q is already registered for %s", id, existing.typ))
	}

	entry := &registeredBusinessError{
		id:      id,
		typ:     typ,
		known:   known,
		allowed: allowed,
		make: func(meta *businessErrorMeta) error {
			return &BusinessError[T]{meta: meta}
		},
	}

	businessErrorRegistry.byType[typ] = entry
	businessErrorRegistry.byID[id] = entry

	return &BusinessErrorType[T]{id: id}
}

// BusinessErrorType creates and matches errors for a registered type.
type BusinessErrorType[T error] struct {
	id string
}

// ID returns the stable wire identifier.
func (t *BusinessErrorType[T]) ID() string {
	return t.id
}

// New constructs a business error with exported/local fields and causes.
func (t *BusinessErrorType[T]) New(message string, fields map[string]any, causes ...error) *BusinessError[T] {
	return &BusinessError[T]{meta: &businessErrorMeta{
		id:      t.id,
		message: message,
		fields:  normalizeBusinessErrorFields(t.id, fields),
		causes:  normalizeBusinessErrorCauses(causes),
	}}
}

// Is reports whether err contains the registered business error type.
func (t *BusinessErrorType[T]) Is(err error) bool {
	var businessErr *BusinessError[T]
	return errors.As(err, &businessErr) && businessErr.meta.id == t.id
}

// As returns the typed business error when err contains the registered type.
func (t *BusinessErrorType[T]) As(err error) (*BusinessError[T], bool) {
	var businessErr *BusinessError[T]
	if errors.As(err, &businessErr) && businessErr.meta.id == t.id {
		return businessErr, true
	}
	return nil, false
}

// BusinessError is an error for a type registered by both plugin and host.
type BusinessError[T error] struct {
	meta *businessErrorMeta
}

// ID returns the stable wire identifier.
func (e *BusinessError[T]) ID() string {
	return e.meta.id
}

// Type returns the registered handle for this error.
func (e *BusinessError[T]) Type() *BusinessErrorType[T] {
	return &BusinessErrorType[T]{id: e.meta.id}
}

// Field returns a field retained on this side of the boundary.
func (e *BusinessError[T]) Field(name string) (any, bool) {
	value, ok := e.meta.fields[name]
	return value, ok
}

// Fields returns a copy of the retained fields.
func (e *BusinessError[T]) Fields() map[string]any {
	result := make(map[string]any, len(e.meta.fields))
	for name, value := range e.meta.fields {
		result[name] = value
	}
	return result
}

// Error returns the plugin-supplied message.
func (e *BusinessError[T]) Error() string {
	return renderBusinessErrorMessage(e.meta)
}

// Is matches errors carrying the same stable generic business type.
func (e *BusinessError[T]) Is(target error) bool {
	other, ok := target.(*BusinessError[T])
	return ok && other != nil && other.meta.id == e.meta.id
}

// As supports errors.As to the concrete generic business error.
func (e *BusinessError[T]) As(target any) bool {
	if typed, ok := target.(**BusinessError[T]); ok {
		*typed = e
		return true
	}

	value := reflect.ValueOf(target)
	if value.Kind() != reflect.Pointer || value.IsNil() {
		return false
	}
	elem := value.Elem()
	if !reflect.TypeOf(e).AssignableTo(elem.Type()) {
		return false
	}
	elem.Set(reflect.ValueOf(e))
	return true
}

// Unwrap returns the causes searched by errors.Is and errors.As.
func (e *BusinessError[T]) Unwrap() []error {
	return e.meta.causes
}

func (e *BusinessError[T]) encodedBusinessError() *businessErrorMeta {
	return e.meta
}

// RemoteBusinessError represents a wire type not registered by the receiver.
type RemoteBusinessError struct {
	id      string
	message string
	fields  map[string]any
	causes  []error
}

// ID returns the stable wire identifier.
func (e *RemoteBusinessError) ID() string {
	return e.id
}

// Field returns an exported field by name.
func (e *RemoteBusinessError) Field(name string) (any, bool) {
	value, ok := e.fields[name]
	return value, ok
}

// Fields returns a copy of the exported fields.
func (e *RemoteBusinessError) Fields() map[string]any {
	result := make(map[string]any, len(e.fields))
	for name, value := range e.fields {
		result[name] = value
	}
	return result
}

// Error returns the plugin-supplied message.
func (e *RemoteBusinessError) Error() string {
	if e.message != "" {
		return e.message
	}
	return "business error: " + e.id
}

// Unwrap returns the remote causes.
func (e *RemoteBusinessError) Unwrap() []error {
	return e.causes
}

type remoteBasicError struct {
	message string
	causes  []error
}

func (e *remoteBasicError) Error() string {
	return e.message
}

func (e *remoteBasicError) Unwrap() []error {
	return e.causes
}

type businessErrorRegistryType struct {
	mu     sync.RWMutex
	byType map[reflect.Type]*registeredBusinessError
	byID   map[string]*registeredBusinessError
}

var businessErrorRegistry = businessErrorRegistryType{
	byType: make(map[reflect.Type]*registeredBusinessError),
	byID:   make(map[string]*registeredBusinessError),
}

type registeredBusinessError struct {
	id      string
	typ     reflect.Type
	known   map[string]struct{}
	allowed map[string]struct{}
	make    func(*businessErrorMeta) error
}

func sameFieldSets(left, right map[string]struct{}) bool {
	if len(left) != len(right) {
		return false
	}
	for name := range left {
		if _, ok := right[name]; !ok {
			return false
		}
	}
	return true
}

func normalizeBusinessErrorFields(id string, fields map[string]any) map[string]any {
	entry := registeredBusinessErrorByID(id)
	if entry == nil {
		panic(fmt.Sprintf("business error type %q is not registered", id))
	}

	result := make(map[string]any)
	var invalid []string
	for name, value := range fields {
		if _, ok := entry.known[name]; !ok {
			continue
		}
		data, err := json.Marshal(value)
		if err != nil {
			invalid = append(invalid, name)
			continue
		}
		var decoded any
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		if err := decoder.Decode(&decoded); err != nil || decoder.More() {
			invalid = append(invalid, name)
			continue
		}
		result[name] = decoded
	}
	if len(invalid) > 0 {
		panic(fmt.Sprintf("business error type %q has non-JSON fields: %s", id, strings.Join(invalid, ", ")))
	}
	return result
}

func normalizeBusinessErrorCauses(causes []error) []error {
	result := make([]error, 0, len(causes))
	for _, cause := range causes {
		if cause != nil {
			result = append(result, cause)
		}
	}
	return result
}

func registeredBusinessErrorByID(id string) *registeredBusinessError {
	businessErrorRegistry.mu.RLock()
	defer businessErrorRegistry.mu.RUnlock()
	return businessErrorRegistry.byID[id]
}

func registeredBusinessErrorFor(err error) (*registeredBusinessError, *businessErrorMeta) {
	codec, ok := err.(businessErrorCodec)
	if !ok {
		return nil, nil
	}
	meta := codec.encodedBusinessError()
	if meta == nil {
		return nil, nil
	}
	businessErrorRegistry.mu.RLock()
	defer businessErrorRegistry.mu.RUnlock()
	return businessErrorRegistry.byID[meta.id], meta
}

func renderBusinessErrorMessage(meta *businessErrorMeta) string {
	if meta.message != "" {
		return meta.message
	}
	return "business error: " + meta.id
}
