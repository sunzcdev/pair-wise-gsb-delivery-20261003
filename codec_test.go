package ber

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
)

// customInt is an application-specific Go value carried by a custom primitive
// codec: a big-endian int32.
type customInt struct {
	N int32
}

var customIntCodec = FuncCodec{
	DecodeFn: func(b []byte) (interface{}, error) {
		if len(b) != 4 {
			return nil, fmt.Errorf("customInt: expected 4 bytes, got %d", len(b))
		}
		return customInt{N: int32(binary.BigEndian.Uint32(b))}, nil
	},
	EncodeFn: func(v interface{}) ([]byte, error) {
		x, ok := v.(customInt)
		if !ok {
			return nil, fmt.Errorf("customInt: got %T, want customInt", v)
		}
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, uint32(x.N))
		return b, nil
	},
}

var appIntKey = CodecKey{ClassType: ClassApplication, TagType: TypePrimitive, Tag: 5}
var ctxSharedKey = CodecKey{ClassType: ClassContext, TagType: TypePrimitive, Tag: 7}

// TestCodecRegistryRoundTrip registers a codec for an application primitive and
// verifies a custom Go value survives encode/decode via the Decoder.
func TestCodecRegistryRoundTrip(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(appIntKey, customIntCodec); err != nil {
		t.Fatalf("register: %v", err)
	}
	d := NewDecoder(reg)

	p, err := d.NewPrimitive(appIntKey.ClassType, appIntKey.TagType, appIntKey.Tag, customInt{N: 0x01020304}, "app-int")
	if err != nil {
		t.Fatalf("NewPrimitive: %v", err)
	}
	encoded := p.Bytes()

	got, err := d.DecodePacketErr(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	ci, ok := got.Value.(customInt)
	if !ok {
		t.Fatalf("Value type = %T, want customInt", got.Value)
	}
	if ci.N != 0x01020304 {
		t.Fatalf("Value = %#x, want 0x01020304", ci.N)
	}
	if !bytes.Equal(got.ByteValue, p.ByteValue) {
		t.Fatalf("raw ByteValue not preserved")
	}
}

// TestUnregisteredPrimitiveRetainsRaw confirms a primitive with no codec keeps
// its content octets and Value stays nil, so unknown types are never dropped.
func TestUnregisteredPrimitiveRetainsRaw(t *testing.T) {
	raw := []byte{0x45, 0x04, 0xde, 0xad, 0xbe, 0xef} // APPLICATION 5, length 4

	got, err := NewDecoder(nil).DecodePacketErr(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Value != nil {
		t.Fatalf("Value = %v, want nil for unregistered type", got.Value)
	}
	if !bytes.Equal(got.ByteValue, []byte{0xde, 0xad, 0xbe, 0xef}) {
		t.Fatalf("ByteValue = %x, want raw content", got.ByteValue)
	}
	if !bytes.Equal(got.Bytes(), raw) {
		t.Fatalf("re-encoded bytes differ from source:\n got %x\nwant %x", got.Bytes(), raw)
	}
}

// TestRegistryIsolationSameTagDifferentTypes proves two independent decoders
// can map the same wire tag to different Go types without interference.
func TestRegistryIsolationSameTagDifferentTypes(t *testing.T) {
	wire := []byte{0x87, 0x01, 0x2A} // context [7], length 1, content 0x2A

	// Decoder A maps the tag to an int.
	oneByteIntCodec := FuncCodec{
		DecodeFn: func(b []byte) (interface{}, error) { return int(b[0]), nil },
		EncodeFn: func(v interface{}) ([]byte, error) { return []byte{byte(v.(int))}, nil },
	}
	regA := NewRegistry()
	if err := regA.Register(ctxSharedKey, oneByteIntCodec); err != nil {
		t.Fatalf("register A: %v", err)
	}

	// Decoder B maps the identical tag to a string.
	regB := NewRegistry()
	stringCodec := FuncCodec{
		DecodeFn: func(b []byte) (interface{}, error) { return fmt.Sprintf("val-%d", b[0]), nil },
		EncodeFn: func(v interface{}) ([]byte, error) { return []byte{v.(string)[4]}, nil },
	}
	if err := regB.Register(ctxSharedKey, stringCodec); err != nil {
		t.Fatalf("register B: %v", err)
	}

	pa, err := NewDecoder(regA).DecodePacketErr(wire)
	if err != nil {
		t.Fatalf("decode A: %v", err)
	}
	pb, err := NewDecoder(regB).DecodePacketErr(wire)
	if err != nil {
		t.Fatalf("decode B: %v", err)
	}

	if _, ok := pa.Value.(int); !ok {
		t.Fatalf("decoder A Value = %T (%v), want int", pa.Value, pa.Value)
	}
	if _, ok := pb.Value.(string); !ok {
		t.Fatalf("decoder B Value = %T (%v), want string", pb.Value, pb.Value)
	}
	if pa.Value.(int) != 42 || pb.Value.(string) != "val-42" {
		t.Fatalf("unexpected values: A=%v B=%v", pa.Value, pb.Value)
	}

	// The default (no-registry) decoder is unaffected by either.
	def, err := defaultDecoder.DecodePacketErr(wire)
	if err != nil {
		t.Fatalf("default decode: %v", err)
	}
	if def.Value != nil {
		t.Fatalf("default decoder Value = %v, want nil", def.Value)
	}
}

// TestRegistryRegisterConflicts verifies build-time conflict reporting.
func TestRegistryRegisterConflicts(t *testing.T) {
	t.Run("duplicate key", func(t *testing.T) {
		reg := NewRegistry()
		if err := reg.Register(appIntKey, customIntCodec); err != nil {
			t.Fatalf("first register: %v", err)
		}
		err := reg.Register(appIntKey, customIntCodec)
		if err == nil {
			t.Fatal("expected duplicate registration error")
		}
		if !strings.Contains(err.Error(), "codec conflict") {
			t.Fatalf("error %q does not mention conflict", err)
		}
	})

	t.Run("nil codec", func(t *testing.T) {
		if err := NewRegistry().Register(appIntKey, nil); err == nil {
			t.Fatal("expected nil codec error")
		}
	})

	t.Run("constructed key rejected", func(t *testing.T) {
		key := CodecKey{ClassType: ClassContext, TagType: TypeConstructed, Tag: 1}
		if err := NewRegistry().Register(key, customIntCodec); err == nil {
			t.Fatal("expected constructed key error")
		}
	})

	t.Run("universal override blocked by default", func(t *testing.T) {
		key := CodecKey{ClassType: ClassUniversal, TagType: TypePrimitive, Tag: TagInteger}
		err := NewRegistry().Register(key, customIntCodec)
		if err == nil {
			t.Fatal("expected built-in universal override to be rejected")
		}
		if !strings.Contains(err.Error(), "WithUniversalOverrides") {
			t.Fatalf("error %q does not mention the opt-in option", err)
		}
	})

	t.Run("universal override allowed with option", func(t *testing.T) {
		key := CodecKey{ClassType: ClassUniversal, TagType: TypePrimitive, Tag: TagInteger}
		reg := NewRegistry(WithUniversalOverrides())
		if err := reg.Register(key, customIntCodec); err != nil {
			t.Fatalf("expected override to be allowed, got %v", err)
		}
	})
}

var errBogusCodec = errors.New("bogus codec failure")

// TestCodecDecodeErrorPathAndChain decodes a sequence containing a primitive
// whose codec fails, asserting the path is reported, the original error chain
// is preserved, and no half-filled packet is delivered.
func TestCodecDecodeErrorPathAndChain(t *testing.T) {
	failingCodec := FuncCodec{
		DecodeFn: func([]byte) (interface{}, error) { return nil, errBogusCodec },
		EncodeFn: func(interface{}) ([]byte, error) { return nil, nil },
	}
	reg := NewRegistry()
	if err := reg.Register(ctxSharedKey, failingCodec); err != nil {
		t.Fatalf("register: %v", err)
	}
	d := NewDecoder(reg)

	// SEQUENCE { [7] { 0x2A } }
	child := Encode(ctxSharedKey.ClassType, ctxSharedKey.TagType, ctxSharedKey.Tag, nil, "")
	child.Data.Write([]byte{0x2A})
	seq := NewSequence("outer")
	seq.AppendChild(child)

	p, err := d.DecodePacketErr(seq.Bytes())
	if err == nil {
		t.Fatal("expected decode error")
	}
	if p != nil {
		t.Fatalf("expected nil packet on error, got %v", p)
	}
	if !errors.Is(err, errBogusCodec) {
		t.Fatalf("error chain lost original: %v", err)
	}
	for _, want := range []string{"codec decode error", "Context/Primitive/tag 7", "Universal/Constructed"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
}

// TestNewPrimitiveLengthBudget ensures a custom value whose re-encoding exceeds
// the length budget is rejected rather than emitted.
func TestNewPrimitiveLengthBudget(t *testing.T) {
	saved := MaxPacketLengthBytes
	MaxPacketLengthBytes = 3
	defer func() { MaxPacketLengthBytes = saved }()

	reg := NewRegistry()
	if err := reg.Register(appIntKey, customIntCodec); err != nil {
		t.Fatalf("register: %v", err)
	}
	d := NewDecoder(reg)

	_, err := d.NewPrimitive(appIntKey.ClassType, appIntKey.TagType, appIntKey.Tag, customInt{N: 1}, "")
	if err == nil {
		t.Fatal("expected length-budget error")
	}
	if !strings.Contains(err.Error(), "greater than maximum") {
		t.Fatalf("error %q does not mention the length maximum", err)
	}
}

// TestNewPrimitiveNoCodec reports a missing codec as an error.
func TestNewPrimitiveNoCodec(t *testing.T) {
	if _, err := NewDecoder(nil).NewPrimitive(appIntKey.ClassType, appIntKey.TagType, appIntKey.Tag, customInt{}, ""); err == nil {
		t.Fatal("expected no-codec error")
	}
}

// TestUniversalOverrideTakesEffect decodes an INTEGER with an overriding codec
// and confirms the custom value wins for that decoder while the default
// decoder still uses the built-in int64.
func TestUniversalOverrideTakesEffect(t *testing.T) {
	reg := NewRegistry(WithUniversalOverrides())
	stringIntCodec := FuncCodec{
		DecodeFn: func(b []byte) (interface{}, error) { return fmt.Sprintf("int:%x", b), nil },
		EncodeFn: func(v interface{}) ([]byte, error) { return nil, errEncodeNotSupported },
	}
	key := CodecKey{ClassType: ClassUniversal, TagType: TypePrimitive, Tag: TagInteger}
	if err := reg.Register(key, stringIntCodec); err != nil {
		t.Fatalf("register: %v", err)
	}

	wire := NewInteger(ClassUniversal, TypePrimitive, TagInteger, 7, "").Bytes()

	got, err := NewDecoder(reg).DecodePacketErr(wire)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Value != "int:07" {
		t.Fatalf("overridden Value = %v, want int:07", got.Value)
	}

	def, err := defaultDecoder.DecodePacketErr(wire)
	if err != nil {
		t.Fatalf("default decode: %v", err)
	}
	if def.Value != int64(7) {
		t.Fatalf("default Value = %T(%v), want int64(7)", def.Value, def.Value)
	}
}

// TestFuncCodecUnsupportedDirection verifies nil functions produce distinct
// unsupported errors.
func TestFuncCodecUnsupportedDirection(t *testing.T) {
	var c FuncCodec
	if _, err := c.Decode(nil); !errors.Is(err, errDecodeNotSupported) {
		t.Fatalf("decode error = %v, want errDecodeNotSupported", err)
	}
	if _, err := c.Encode(nil); !errors.Is(err, errEncodeNotSupported) {
		t.Fatalf("encode error = %v, want errEncodeNotSupported", err)
	}

	// Sanity: the length budget sentinel comparison against MaxInt32 default.
	if MaxPacketLengthBytes != math.MaxInt32 {
		t.Fatalf("unexpected default MaxPacketLengthBytes: %d", MaxPacketLengthBytes)
	}
}
