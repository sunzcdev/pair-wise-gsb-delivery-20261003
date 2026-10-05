package ws

import (
	"errors"
	"fmt"

	"github.com/gobwas/httphead"
)

// RSV bit masks used to declare which reserved bits of a frame header an
// extension negotiates.
//
// The Rsv field of a Header (see Rsv()) stores bits shifted into its lower
// three bits, so these masks are suitable to be compared with it directly.
const (
	RsvBit1 byte = bit5
	RsvBit2 byte = bit6
	RsvBit3 byte = bit7

	// rsvBitMask masks all three RSV bits of a Header's Rsv field.
	rsvBitMask byte = bit5 | bit6 | bit7
)

// NegotiatedExtension describes an extension accepted during the WebSocket
// handshake together with the frame header RSV bits it claims ownership of.
//
// When multiple extensions are negotiated each accepted extension must claim
// a disjoint set of RSV bits: two extensions can not share an RSV bit, and an
// extension may only claim RSV1, RSV2 or RSV3.
type NegotiatedExtension struct {
	// Option is the accepted extension as it appears in the
	// Sec-WebSocket-Extensions header.
	Option httphead.Option

	// Rsv is the bitmask of RSV bits the extension claims (see RsvBit*
	// constants). Zero value means that the extension does not use RSV bits.
	Rsv byte
}

// ErrExtensionConflict is returned when two or more negotiated extensions
// claim the same RSV bit, or when an extension declares RSV bits that are not
// part of the protocol.
var ErrExtensionConflict = errors.New("websocket extension rsv bit conflict")

// ExtensionConflictError describes a collision of RSV bit declarations
// between negotiated extensions.
//
// It wraps ErrExtensionConflict and could be inspected with errors.Is().
type ExtensionConflictError struct {
	// Name is the name of the extension that declares conflicting bits.
	Name string

	// Claimed is the extension that already owns the conflicting bits.
	Claimed string

	// Bits is the RSV bit mask that conflicts.
	Bits byte
}

// Error implements error.
func (e *ExtensionConflictError) Error() string {
	if e.Claimed == "" {
		return fmt.Sprintf(
			"websocket extension %q declares invalid rsv bits mask %#x",
			e.Name, e.Bits,
		)
	}
	return fmt.Sprintf(
		"websocket extension %q claims rsv bits %s already claimed by %q",
		e.Name, rsvBitsName(e.Bits), e.Claimed,
	)
}

// Unwrap implements errors wrapper.
func (e *ExtensionConflictError) Unwrap() error {
	return ErrExtensionConflict
}

// CheckExtensionConflicts verifies that every negotiated extension claims a
// valid and disjoint set of RSV bits. It returns *ExtensionConflictError
// describing the first extension and bits that conflict.
func CheckExtensionConflicts(exts []NegotiatedExtension) error {
	var claimed [3]string
	for _, ext := range exts {
		if ext.Rsv&^rsvBitMask != 0 {
			return &ExtensionConflictError{
				Name: string(ext.Option.Name),
				Bits: ext.Rsv &^ rsvBitMask,
			}
		}
		name := string(ext.Option.Name)
		for i, bit := range [3]byte{RsvBit1, RsvBit2, RsvBit3} {
			if ext.Rsv&bit == 0 {
				continue
			}
			if claimed[i] != "" {
				return &ExtensionConflictError{
					Name:    name,
					Claimed: claimed[i],
					Bits:    bit,
				}
			}
			claimed[i] = name
		}
	}
	return nil
}

// rsvBitsName returns human readable representation of RSV bits in mask, such
// as "RSV1|RSV3".
func rsvBitsName(mask byte) string {
	const sep = "|"
	var (
		bts [len("RSV1")*3 + 2]byte
		buf = bts[:0]
	)
	for _, bit := range [3]struct {
		mask byte
		name string
	}{
		{RsvBit1, "RSV1"},
		{RsvBit2, "RSV2"},
		{RsvBit3, "RSV3"},
	} {
		if mask&bit.mask == 0 {
			continue
		}
		if len(buf) > 0 {
			buf = append(buf, sep...)
		}
		buf = append(buf, bit.name...)
	}
	return string(buf)
}
