package ber

import (
	"bytes"
	"errors"
	"fmt"
	"io"
)

// CodecKey selects a primitive codec by the BER identifier triple: the tag
// class, the tag type (primitive/constructed), and the tag number.
type CodecKey struct {
	ClassType Class
	TagType   Type
	Tag       Tag
}

// String renders the key for diagnostics, using the well-known universal tag
// names where possible.
func (k CodecKey) String() string {
	class, ok := ClassMap[k.ClassType]
	if !ok {
		class = fmt.Sprintf("Class(%d)", k.ClassType)
	}

	typ, ok := TypeMap[k.TagType]
	if !ok {
		typ = fmt.Sprintf("Type(%d)", k.TagType)
	}

	tag := fmt.Sprintf("tag %d", k.Tag)
	if k.ClassType == ClassUniversal {
		if name, ok := tagMap[k.Tag]; ok {
			tag = name
		}
	}

	return fmt.Sprintf("%s/%s/%s", class, typ, tag)
}

// PrimitiveCodec converts between a Go value and the raw content octets of a
// primitive BER packet. Decode maps content octets to a Go value; Encode maps a
// Go value back to content octets. Either direction may return an error, which
// the decoder preserves together with the packet path.
type PrimitiveCodec interface {
	Decode(content []byte) (value interface{}, err error)
	Encode(value interface{}) (content []byte, err error)
}

// DecodeFunc adapts a plain function to the decode half of a PrimitiveCodec.
type DecodeFunc func(content []byte) (interface{}, error)

// EncodeFunc adapts a plain function to the encode half of a PrimitiveCodec.
type EncodeFunc func(value interface{}) ([]byte, error)

var (
	errDecodeNotSupported = errors.New("ber: codec does not support decoding")
	errEncodeNotSupported = errors.New("ber: codec does not support encoding")
)

// FuncCodec is a PrimitiveCodec built from optional decode and encode
// functions. A nil function reports that the direction is unsupported.
type FuncCodec struct {
	DecodeFn DecodeFunc
	EncodeFn EncodeFunc
}

// Decode implements PrimitiveCodec.
func (c FuncCodec) Decode(content []byte) (interface{}, error) {
	if c.DecodeFn == nil {
		return nil, errDecodeNotSupported
	}
	return c.DecodeFn(content)
}

// Encode implements PrimitiveCodec.
func (c FuncCodec) Encode(value interface{}) ([]byte, error) {
	if c.EncodeFn == nil {
		return nil, errEncodeNotSupported
	}
	return c.EncodeFn(value)
}

// Registry is an instance-level table of primitive codecs, keyed by the
// (class, tag type, tag) identifier triple. A zero-value Registry is not
// usable; create one with NewRegistry.
//
// Registries are configured with Register and then treated as immutable while
// a Decoder uses them; registration is not synchronized with decoding. Two
// registries are fully independent, so the same wire tag can map to different
// Go types in different Decoder instances.
type Registry struct {
	codecs                 map[CodecKey]PrimitiveCodec
	allowUniversalOverride bool
}

// RegistryOption configures a Registry at construction time.
type RegistryOption func(*Registry)

// WithUniversalOverrides permits a Registry to replace codecs for the
// built-in universal primitive types (INTEGER, OCTET STRING, OID, ...). Without
// it, registering a key that collides with a built-in universal type fails.
func WithUniversalOverrides() RegistryOption {
	return func(r *Registry) {
		r.allowUniversalOverride = true
	}
}

// NewRegistry returns an empty Registry. By default, registration of keys that
// collide with the built-in universal primitive types is rejected; pass
// WithUniversalOverrides to opt in to replacing them.
func NewRegistry(opts ...RegistryOption) *Registry {
	r := &Registry{codecs: make(map[CodecKey]PrimitiveCodec)}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Register associates codec with key. Conflicts are reported at registration
// ("build") time: a duplicate key, a constructed or otherwise invalid key, a
// nil codec, or -- unless WithUniversalOverrides was set -- a key that
// overrides a built-in universal primitive type all return an error and leave
// the registry unchanged.
func (r *Registry) Register(key CodecKey, codec PrimitiveCodec) error {
	if codec == nil {
		return errors.New("ber: cannot register nil codec")
	}
	if err := validateCodecKey(key); err != nil {
		return err
	}
	if _, exists := r.codecs[key]; exists {
		return fmt.Errorf("ber: codec conflict: a codec is already registered for %s", key)
	}
	if !r.allowUniversalOverride && isBuiltinUniversalTag(key.Tag) {
		return fmt.Errorf(
			"ber: refusing to override the built-in universal codec for %s; pass WithUniversalOverrides to allow it",
			key,
		)
	}
	r.codecs[key] = codec
	return nil
}

// MustRegister behaves like Register but panics on a conflict, for use when
// building a registry from static, known-good definitions.
func (r *Registry) MustRegister(key CodecKey, codec PrimitiveCodec) {
	if err := r.Register(key, codec); err != nil {
		panic(err.Error())
	}
}

func (r *Registry) lookup(id Identifier) PrimitiveCodec {
	return r.codecs[CodecKey{ClassType: id.ClassType, TagType: id.TagType, Tag: id.Tag}]
}

// validateCodecKey rejects keys that cannot address a primitive codec: any
// reserved bits in the class, any tag type other than primitive.
func validateCodecKey(key CodecKey) error {
	if key.ClassType&^ClassBitmask != 0 {
		return fmt.Errorf("ber: invalid class %#x in codec key %s", uint8(key.ClassType), key)
	}
	switch key.TagType {
	case TypePrimitive:
		return nil
	case TypeConstructed:
		return fmt.Errorf("ber: cannot register codec for constructed %s; custom codecs apply only to primitive packets", key)
	default:
		return fmt.Errorf("ber: invalid tag type %#x in codec key %s", uint8(key.TagType), key)
	}
}

// builtinUniversalPrimitiveTags lists the universal tags the package decodes
// into a Go Value with its own rules. Overriding these is opt-in.
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

func isBuiltinUniversalTag(tag Tag) bool {
	return builtinUniversalPrimitiveTags[tag]
}

// Decoder decodes BER packets using a specific Registry. Instances are safe
// for concurrent use as long as the underlying registry is not modified. The
// zero value is not usable; use NewDecoder.
type Decoder struct {
	registry *Registry
}

// NewDecoder returns a Decoder backed by registry. If registry is nil, an empty
// registry is used, yielding the same behavior as the package-level decode
// functions: only built-in universal types produce a Value, while every other
// primitive retains its raw bytes.
func NewDecoder(registry *Registry) *Decoder {
	if registry == nil {
		registry = NewRegistry()
	}
	return &Decoder{registry: registry}
}

// defaultDecoder backs the package-level decode functions and carries no
// user-registered codecs.
var defaultDecoder = NewDecoder(nil)

// ReadPacket reads a single Packet from reader using the decoder's registry.
func (d *Decoder) ReadPacket(reader io.Reader) (*Packet, error) {
	p, _, err := d.readPacket(reader, 0, "")
	return p, err
}

// DecodePacketErr decodes data into a single Packet, returning any error.
func (d *Decoder) DecodePacketErr(data []byte) (*Packet, error) {
	return d.ReadPacket(bytes.NewBuffer(data))
}

// DecodePacket decodes data into a single Packet, returning nil on error.
func (d *Decoder) DecodePacket(data []byte) *Packet {
	p, err := d.DecodePacketErr(data)
	if err != nil {
		return nil
	}
	return p
}

// NewPrimitive builds a primitive packet by encoding value with the codec
// registered for the (classType, tagType, tag) triple. It returns an error if
// the key is invalid, no codec is registered, the codec fails, or the encoded
// content exceeds MaxPacketLengthBytes.
func (d *Decoder) NewPrimitive(classType Class, tagType Type, tag Tag, value interface{}, description string) (*Packet, error) {
	key := CodecKey{ClassType: classType, TagType: tagType, Tag: tag}
	if err := validateCodecKey(key); err != nil {
		return nil, err
	}

	codec := d.registry.lookup(Identifier(key))
	if codec == nil {
		if classType == ClassUniversal && isBuiltinUniversalTag(tag) {
			return nil, fmt.Errorf(
				"ber: no codec registered for %s; built-in universal values use the New* constructors",
				key,
			)
		}
		return nil, fmt.Errorf("ber: no codec registered for %s", key)
	}

	content, err := codec.Encode(value)
	if err != nil {
		return nil, fmt.Errorf("ber: codec encode error for %s: %w", key, err)
	}
	if MaxPacketLengthBytes > 0 && int64(len(content)) > MaxPacketLengthBytes {
		return nil, fmt.Errorf(
			"ber: encoded content length %d for %s greater than maximum %d",
			len(content), key, MaxPacketLengthBytes,
		)
	}

	p := Encode(classType, tagType, tag, value, description)
	p.Data = bytes.NewBuffer(content)
	p.ByteValue = content
	return p, nil
}

// joinPath appends id's segment to parent, yielding the slash-separated packet
// path used in codec error messages.
func joinPath(parent string, id Identifier) string {
	segment := CodecKey{ClassType: id.ClassType, TagType: id.TagType, Tag: id.Tag}.String()
	if parent == "" {
		return segment
	}
	return parent + "/" + segment
}

// wrapCodecError annotates a codec failure with its packet path while retaining
// the original error chain (use errors.Is/errors.As to inspect it).
func wrapCodecError(key CodecKey, path string, err error) error {
	location := path
	if location == "" {
		location = key.String()
	}
	return fmt.Errorf("ber: codec decode error at %s: %w", location, err)
}
