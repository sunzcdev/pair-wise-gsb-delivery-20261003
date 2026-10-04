package ber

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Codec decodes the content octets of a primitive packet into a Go value.
// Register a Codec on a [Decoder] with [WithCodec] to take over decoding for
// a specific (class, tag type, tag) triple.
type Codec interface {
	Decode(content []byte) (interface{}, error)
}

// EncodeCodec is a [Codec] that can also re-encode a Go value back into
// content octets. [Decoder.EncodePacket] requires registered codecs to
// implement it.
type EncodeCodec interface {
	Codec
	Encode(value interface{}) ([]byte, error)
}

// Funcs adapts a pair of functions to a [Codec]. A nil EncodeFunc makes the
// codec decode-only: [Decoder.EncodePacket] reports an error if it is asked
// to encode a value with it.
type Funcs struct {
	DecodeFunc func(content []byte) (interface{}, error)
	EncodeFunc func(value interface{}) ([]byte, error)
}

// Decode implements [Codec].
func (f Funcs) Decode(content []byte) (interface{}, error) {
	return f.DecodeFunc(content)
}

// Encode implements [EncodeCodec].
func (f Funcs) Encode(value interface{}) ([]byte, error) {
	if f.EncodeFunc == nil {
		return nil, errors.New("codec is decode-only")
	}
	return f.EncodeFunc(value)
}

// builtinUniversalPrimitiveTags are the universal primitive tags the package
// decodes into Go values on its own. Registering a codec for one of them is a
// conflict unless [AllowUniversalOverride] is set.
var builtinUniversalPrimitiveTags = map[Tag]bool{
	TagBoolean:          true,
	TagInteger:          true,
	TagOctetString:      true,
	TagObjectIdentifier: true,
	TagRealFloat:        true,
	TagEnumerated:       true,
	TagUTF8String:       true,
	TagRelativeOID:      true,
	TagPrintableString:  true,
	TagIA5String:        true,
	TagGeneralizedTime:  true,
}

// Decoder is an instance-level decoder (and encoder) with its own registry of
// custom primitive codecs, selected by the (class, tag type, tag) triple of a
// packet. Packets whose triple has no registered codec decode exactly as the
// package-level functions do: known universal tags get a decoded Value,
// everything else keeps its raw content in Data and ByteValue.
//
// A Decoder is safe for concurrent use; its registry is fixed at construction.
type Decoder struct {
	codecs map[Identifier]Codec
}

// codecRegistration is a pending WithCodec registration, validated and
// applied when NewDecoder builds the Decoder.
type codecRegistration struct {
	identifier Identifier
	codec      Codec
}

type decoderConfig struct {
	registrations          []codecRegistration
	allowUniversalOverride bool
}

// DecoderOption configures a [Decoder] under construction by [NewDecoder].
type DecoderOption func(*decoderConfig) error

// WithCodec registers codec for primitive packets with the given class, tag
// type and tag. tagType must be [TypePrimitive]; codecs do not apply to
// constructed packets, whose content is their children.
//
// Registration conflicts -- the same triple registered twice, or a built-in
// universal tag registered without [AllowUniversalOverride] -- are reported
// as an error from [NewDecoder].
func WithCodec(classType Class, tagType Type, tag Tag, codec Codec) DecoderOption {
	return func(cfg *decoderConfig) error {
		if codec == nil {
			return fmt.Errorf("ber: nil codec for %s", formatIdentifier(Identifier{classType, tagType, tag}))
		}
		if tagType != TypePrimitive {
			return fmt.Errorf("ber: codec for %s: codecs only apply to primitive packets",
				formatIdentifier(Identifier{classType, tagType, tag}))
		}
		cfg.registrations = append(cfg.registrations, codecRegistration{
			identifier: Identifier{ClassType: classType, TagType: tagType, Tag: tag},
			codec:      codec,
		})
		return nil
	}
}

// AllowUniversalOverride permits [WithCodec] to register codecs for universal
// tags the package decodes natively (e.g. INTEGER, UTF8String), replacing the
// built-in decoding for this Decoder only. Without it, such a registration is
// a build-time conflict.
func AllowUniversalOverride() DecoderOption {
	return func(cfg *decoderConfig) error {
		cfg.allowUniversalOverride = true
		return nil
	}
}

// NewDecoder builds a [Decoder] from the given options. Registration
// conflicts are reported here, at build time, not at first use.
func NewDecoder(opts ...DecoderOption) (*Decoder, error) {
	cfg := decoderConfig{}
	for _, opt := range opts {
		if err := opt(&cfg); err != nil {
			return nil, err
		}
	}

	d := &Decoder{codecs: make(map[Identifier]Codec, len(cfg.registrations))}
	for _, reg := range cfg.registrations {
		id := reg.identifier
		if _, exists := d.codecs[id]; exists {
			return nil, fmt.Errorf("ber: codec already registered for %s", formatIdentifier(id))
		}
		if !cfg.allowUniversalOverride && id.ClassType == ClassUniversal && builtinUniversalPrimitiveTags[id.Tag] {
			return nil, fmt.Errorf("ber: codec for %s overrides a built-in universal type; pass AllowUniversalOverride to permit it",
				formatIdentifier(id))
		}
		d.codecs[id] = reg.codec
	}
	return d, nil
}

// codecFor returns the registered codec for identifier, or nil.
func (d *Decoder) codecFor(identifier Identifier) Codec {
	if d == nil {
		return nil
	}
	return d.codecs[identifier]
}

// ReadPacket reads a single Packet from the reader, applying the Decoder's
// registered codecs.
func (d *Decoder) ReadPacket(reader io.Reader) (*Packet, error) {
	p, _, err := d.readPacket(reader, 0, nil)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// DecodePacket decodes the given bytes into a single Packet, applying the
// Decoder's registered codecs. If a decode error is encountered, nil is
// returned.
func (d *Decoder) DecodePacket(data []byte) *Packet {
	p, _, err := d.readPacket(bytes.NewBuffer(data), 0, nil)
	if err != nil {
		return nil
	}
	return p
}

// DecodePacketErr decodes the given bytes into a single Packet, applying the
// Decoder's registered codecs. If a decode error is encountered, nil is
// returned alongside the error.
func (d *Decoder) DecodePacketErr(data []byte) (*Packet, error) {
	p, _, err := d.readPacket(bytes.NewBuffer(data), 0, nil)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// EncodePacket serializes p and its children like [Packet.Bytes], except that
// packets with a registered codec have their content re-encoded from Value by
// the codec. Codec encode errors are wrapped with the packet's path and
// returned; no partial output is produced. Codec-produced content remains
// subject to the [MaxPacketLengthBytes] budget, as does the aggregate content
// of constructed packets.
func (d *Decoder) EncodePacket(p *Packet) ([]byte, error) {
	return d.encodePacket(p, nil, 0)
}

func (d *Decoder) encodePacket(p *Packet, path []Identifier, depth int) ([]byte, error) {
	if MaxNestingDepth > 0 && depth >= MaxNestingDepth {
		return nil, fmt.Errorf("nesting depth %d exceeds maximum %d", depth, MaxNestingDepth)
	}
	path = append(path, p.Identifier)

	if codec := d.codecFor(p.Identifier); codec != nil && len(p.Children) == 0 {
		encodeCodec, ok := codec.(EncodeCodec)
		if !ok {
			return nil, fmt.Errorf("ber: codec at %s cannot encode", formatIdentifierPath(path))
		}
		content, err := encodeCodec.Encode(p.Value)
		if err != nil {
			return nil, fmt.Errorf("ber: codec encode failed at %s: %w", formatIdentifierPath(path), err)
		}
		if MaxPacketLengthBytes > 0 && int64(len(content)) > MaxPacketLengthBytes {
			return nil, fmt.Errorf("ber: encoded content length %d at %s greater than maximum %d",
				len(content), formatIdentifierPath(path), MaxPacketLengthBytes)
		}
		out := encodeIdentifier(p.Identifier)
		out = append(out, encodeLength(len(content))...)
		return append(out, content...), nil
	}

	var content []byte
	if len(p.Children) == 0 {
		if p.Data != nil {
			content = p.Data.Bytes()
		}
	} else {
		for _, child := range p.Children {
			childBytes, err := d.encodePacket(child, path, depth+1)
			if err != nil {
				return nil, err
			}
			content = append(content, childBytes...)
			if MaxPacketLengthBytes > 0 && int64(len(content)) > MaxPacketLengthBytes {
				return nil, fmt.Errorf("ber: encoded content length %d at %s greater than maximum %d",
					len(content), formatIdentifierPath(path), MaxPacketLengthBytes)
			}
		}
	}

	out := encodeIdentifier(p.Identifier)
	out = append(out, encodeLength(len(content))...)
	return append(out, content...), nil
}

// formatIdentifier renders an identifier triple for error messages.
func formatIdentifier(identifier Identifier) string {
	tag := fmt.Sprintf("0x%02X", uint64(identifier.Tag))
	if identifier.ClassType == ClassUniversal {
		if name, ok := tagMap[identifier.Tag]; ok {
			tag = name
		}
	}
	return fmt.Sprintf("(%s, %s, %s)", ClassMap[identifier.ClassType], TypeMap[identifier.TagType], tag)
}

// formatIdentifierPath renders the chain of identifiers from the root packet
// to the packet being processed, for error messages.
func formatIdentifierPath(path []Identifier) string {
	parts := make([]string, len(path))
	for i, identifier := range path {
		parts[i] = formatIdentifier(identifier)
	}
	return strings.Join(parts, " > ")
}
