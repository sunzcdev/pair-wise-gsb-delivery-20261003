// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

// Package shared defines the business error contract shared by a host and
// its plugins for the bizerror example.
package shared

import "github.com/hashicorp/go-plugin/bizerror"

// Stable type identifiers. These strings - not Go type names - are the wire
// contract: rename the structs freely, never change the strings casually.
const (
	TypeNotFound     = "store.v1.not_found"
	TypeUnavailable  = "store.v1.unavailable"
	TypeAccessDenied = "store.v1.access_denied"
	TypeHostOnly     = "store.v1.host_only"
)

// NotFoundError means the requested resource does not exist.
type NotFoundError struct {
	Resource string
	Key      string

	// Token is sensitive: it is intentionally absent from the registry's
	// ExportedFields allow-list and must never appear on the host side.
	Token string
}

func (e *NotFoundError) Error() string {
	return "resource not found: " + e.Resource
}

// UnavailableError means the resource exists but cannot currently be served.
// It is used with multiple causes to show error trees.
type UnavailableError struct {
	Resource      string
	RetryAfterSec int
	InternalTrace string // sensitive, not exported
}

func (e *UnavailableError) Error() string {
	return "resource temporarily unavailable: " + e.Resource
}

// AccessDeniedError is reconstructed through explicit Encode/Decode hooks
// instead of reflection, demonstrating field-level control.
type AccessDeniedError struct {
	Resource string
	Reason   string
}

func (e *AccessDeniedError) Error() string {
	return "access denied: " + e.Resource + " (" + e.Reason + ")"
}

// HostOnlyError is registered on the host but never on the plugin. The
// example plugin emits it with a raw Encode fallback so the host can observe
// the "unknown type on the other side" path.
type HostOnlyError struct {
	Secret string
}

func (e *HostOnlyError) Error() string { return "host-only error type" }

// HostRegistry contains every type the host agrees to understand.
func HostRegistry() *bizerror.Registry {
	reg := bizerror.NewRegistry()
	reg.MustRegister(bizerror.Registration{
		TypeID:         TypeNotFound,
		Prototype:      func() error { return &NotFoundError{} },
		ExportedFields: []string{"Resource", "Key"},
	})
	reg.MustRegister(bizerror.Registration{
		TypeID:         TypeUnavailable,
		Prototype:      func() error { return &UnavailableError{} },
		ExportedFields: []string{"Resource", "RetryAfterSec"},
	})
	reg.MustRegister(accessDeniedRegistration())
	reg.MustRegister(bizerror.Registration{
		TypeID:         TypeHostOnly,
		Prototype:      func() error { return &HostOnlyError{} },
		ExportedFields: nil, // nothing exported by default
	})
	return reg
}

// PluginRegistry contains only the types a plugin knows how to produce.
// TypeHostOnly is deliberately missing here.
func PluginRegistry() *bizerror.Registry {
	reg := bizerror.NewRegistry()
	reg.MustRegister(bizerror.Registration{
		TypeID:         TypeNotFound,
		Prototype:      func() error { return &NotFoundError{} },
		ExportedFields: []string{"Resource", "Key"},
	})
	reg.MustRegister(bizerror.Registration{
		TypeID:         TypeUnavailable,
		Prototype:      func() error { return &UnavailableError{} },
		ExportedFields: []string{"Resource", "RetryAfterSec"},
	})
	reg.MustRegister(accessDeniedRegistration())
	return reg
}

func accessDeniedRegistration() bizerror.Registration {
	return bizerror.Registration{
		TypeID:         TypeAccessDenied,
		ExportedFields: []string{"resource", "reason"},
		Encode: func(err error) (map[string]any, error) {
			e, ok := err.(*AccessDeniedError)
			if !ok {
				// Fallback hooks receive every otherwise-unmatched error
				// (including plain wrappers); nil means "not mine".
				return nil, nil
			}
			return map[string]any{
				"resource": e.Resource,
				"reason":   e.Reason,
			}, nil
		},
		Decode: func(fields map[string]any) (error, error) {
			e := &AccessDeniedError{}
			if v, ok := fields["resource"].(string); ok {
				e.Resource = v
			}
			if v, ok := fields["reason"].(string); ok {
				e.Reason = v
			}
			return e, nil
		},
	}
}
