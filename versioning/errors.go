package versioning

import (
	"errors"
	"fmt"
)

// Sentinel errors returned by the package. Use errors.Is to test for them.
var (
	// ErrNotRegistered means a Go type was marshaled or unmarshaled without
	// being registered with the Registry, so no schema version is known.
	ErrNotRegistered = errors.New("versioning: type is not registered")

	// ErrUnknownVersion means the data carries a schema version that no Go
	// type and no conversion path was registered for.
	ErrUnknownVersion = errors.New("versioning: unknown schema version")

	// ErrNoConversion means a conversion path between the data version and the
	// current version is not fully registered.
	ErrNoConversion = errors.New("versioning: no registered conversion path")

	// ErrLossy is the root of every error reporting that a type change cannot
	// be performed without losing information. A Conversion should return an
	// error wrapping ErrLossy (or an error built with LossyError) when, for
	// example, a fractional float must be stored in an integer.
	ErrLossy = errors.New("versioning: conversion is not lossless")

	// ErrBadEnvelope means the bytes are not a versioned envelope.
	ErrBadEnvelope = errors.New("versioning: not a versioned envelope")
)

// LossyError builds an error that wraps ErrLossy and explains which field
// failed and why. Return it from a Conversion to reject a lossless-required
// type change.
func LossyError(fieldID uint64, reason string) error {
	return fmt.Errorf("%w: field id %d: %s", ErrLossy, fieldID, reason)
}
