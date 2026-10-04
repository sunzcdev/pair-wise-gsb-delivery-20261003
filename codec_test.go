package ber

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

var errTestCodec = errors.New("test codec failure")

// upperStringCodec decodes content as an uppercase string and re-encodes it
// lowercased, making encode/decode direction observable in tests.
func upperStringCodec() Funcs {
	return Funcs{
		DecodeFunc: func(content []byte) (interface{}, error) {
			return strings.ToUpper(string(content)), nil
		},
		EncodeFunc: func(value interface{}) ([]byte, error) {
			s, ok := value.(string)
			if !ok {
				return nil, errTestCodec
			}
			return []byte(strings.ToLower(s)), nil
		},
	}
}

func mustDecoder(t *testing.T, opts ...DecoderOption) *Decoder {
	t.Helper()
	d, err := NewDecoder(opts...)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	return d
}

func TestCodecDecodeCustomType(t *testing.T) {
	d := mustDecoder(t, WithCodec(ClassApplication, TypePrimitive, 5, upperStringCodec()))
	// [APPLICATION 5] primitive, content "abc"
	p, err := d.DecodePacketErr([]byte{0x45, 0x03, 'a', 'b', 'c'})
	if err != nil {
		t.Fatalf("DecodePacketErr: %v", err)
	}
	if v, ok := p.Value.(string); !ok || v != "ABC" {
		t.Errorf("expected Value \"ABC\", got %#v", p.Value)
	}
	// Raw content is retained so the packet still round-trips via Bytes.
	if !bytes.Equal(p.ByteValue, []byte("abc")) {
		t.Errorf("expected ByteValue %q, got %q", "abc", p.ByteValue)
	}
	if !bytes.Equal(p.Bytes(), []byte{0x45, 0x03, 'a', 'b', 'c'}) {
		t.Errorf("round-trip via Bytes changed the encoding: %x", p.Bytes())
	}
}

func TestCodecUnregisteredKeepsRawData(t *testing.T) {
	d := mustDecoder(t, WithCodec(ClassApplication, TypePrimitive, 5, upperStringCodec()))
	// [APPLICATION 7] primitive: no codec registered.
	p, err := d.DecodePacketErr([]byte{0x47, 0x02, 0xDE, 0xAD})
	if err != nil {
		t.Fatalf("DecodePacketErr: %v", err)
	}
	if p.Value != nil {
		t.Errorf("expected nil Value for unregistered type, got %#v", p.Value)
	}
	if !bytes.Equal(p.Data.Bytes(), []byte{0xDE, 0xAD}) {
		t.Errorf("raw content lost: %x", p.Data.Bytes())
	}
	if !bytes.Equal(p.Bytes(), []byte{0x47, 0x02, 0xDE, 0xAD}) {
		t.Errorf("round-trip changed the encoding: %x", p.Bytes())
	}
}

func TestCodecRegistrationConflict(t *testing.T) {
	if _, err := NewDecoder(
		WithCodec(ClassApplication, TypePrimitive, 5, upperStringCodec()),
		WithCodec(ClassApplication, TypePrimitive, 5, upperStringCodec()),
	); err == nil {
		t.Fatal("expected duplicate registration to fail at build time")
	}
}

func TestCodecConstructedRejected(t *testing.T) {
	if _, err := NewDecoder(
		WithCodec(ClassUniversal, TypeConstructed, TagSequence, upperStringCodec()),
	); err == nil {
		t.Fatal("expected constructed codec registration to fail at build time")
	}
}

func TestCodecUniversalOverrideRequiresOption(t *testing.T) {
	if _, err := NewDecoder(
		WithCodec(ClassUniversal, TypePrimitive, TagInteger, upperStringCodec()),
	); err == nil {
		t.Fatal("expected universal override without AllowUniversalOverride to fail")
	}

	d := mustDecoder(t,
		AllowUniversalOverride(),
		WithCodec(ClassUniversal, TypePrimitive, TagInteger, upperStringCodec()),
	)
	// INTEGER 'a' decodes via the codec, not the built-in int64 path.
	p, err := d.DecodePacketErr([]byte{0x02, 0x01, 'a'})
	if err != nil {
		t.Fatalf("DecodePacketErr: %v", err)
	}
	if v, ok := p.Value.(string); !ok || v != "A" {
		t.Errorf("expected codec Value \"A\", got %#v", p.Value)
	}

	// The package-level decoder is unaffected by the instance's override.
	q := DecodePacket([]byte{0x02, 0x01, 'a'})
	if v, ok := q.Value.(int64); !ok || v != int64('a') {
		t.Errorf("expected built-in int64 decode, got %#v", q.Value)
	}
}

func TestCodecDecodeErrorPathAndChain(t *testing.T) {
	d := mustDecoder(t, WithCodec(ClassContext, TypePrimitive, 3, Funcs{
		DecodeFunc: func([]byte) (interface{}, error) {
			return nil, errTestCodec
		},
	}))

	// SEQUENCE { [3] primitive 0x2A }
	p, err := d.DecodePacketErr([]byte{0x30, 0x03, 0x83, 0x01, 0x2A})
	if err == nil {
		t.Fatal("expected codec error")
	}
	if p != nil {
		t.Errorf("expected nil packet on codec error, got %#v", p)
	}
	if !errors.Is(err, errTestCodec) {
		t.Errorf("error chain lost the codec's error: %v", err)
	}
	if !strings.Contains(err.Error(), "Sequence") || !strings.Contains(err.Error(), "0x03") {
		t.Errorf("error missing packet path: %v", err)
	}
}

func TestCodecEncodePacket(t *testing.T) {
	d := mustDecoder(t, WithCodec(ClassApplication, TypePrimitive, 5, upperStringCodec()))

	seq := NewSequence("root")
	seq.AppendChild(Encode(ClassApplication, TypePrimitive, 5, "ABC", "custom"))
	seq.AppendChild(NewInteger(ClassUniversal, TypePrimitive, TagInteger, 7, "seven"))

	encoded, err := d.EncodePacket(seq)
	if err != nil {
		t.Fatalf("EncodePacket: %v", err)
	}
	// The codec lowercases on encode; the unregistered INTEGER uses its raw Data.
	expected := []byte{0x30, 0x08, 0x45, 0x03, 'a', 'b', 'c', 0x02, 0x01, 0x07}
	if !bytes.Equal(encoded, expected) {
		t.Errorf("expected %x, got %x", expected, encoded)
	}
}

func TestCodecEncodePacketError(t *testing.T) {
	d := mustDecoder(t, WithCodec(ClassApplication, TypePrimitive, 5, upperStringCodec()))

	// Value of the wrong type makes the codec's Encode fail.
	p := Encode(ClassApplication, TypePrimitive, 5, 42, "bad")
	out, err := d.EncodePacket(p)
	if err == nil {
		t.Fatal("expected codec encode error")
	}
	if out != nil {
		t.Errorf("expected no partial output, got %x", out)
	}
	if !errors.Is(err, errTestCodec) {
		t.Errorf("error chain lost the codec's error: %v", err)
	}
	if !strings.Contains(err.Error(), "0x05") {
		t.Errorf("error missing packet path: %v", err)
	}
}

func TestCodecEncodePacketDecodeOnly(t *testing.T) {
	decodeOnly := Funcs{DecodeFunc: func(content []byte) (interface{}, error) {
		return string(content), nil
	}}
	d := mustDecoder(t, WithCodec(ClassApplication, TypePrimitive, 5, decodeOnly))

	p := Encode(ClassApplication, TypePrimitive, 5, "x", "custom")
	if _, err := d.EncodePacket(p); err == nil {
		t.Fatal("expected error encoding with a decode-only codec")
	}
}

func TestCodecEncodePacketLengthBudget(t *testing.T) {
	d := mustDecoder(t, WithCodec(ClassApplication, TypePrimitive, 5, upperStringCodec()))

	defer func(prev int64) { MaxPacketLengthBytes = prev }(MaxPacketLengthBytes)
	MaxPacketLengthBytes = 4

	// The codec re-encodes "TOOLONG" to 7 content octets, over the budget.
	p := Encode(ClassApplication, TypePrimitive, 5, "TOOLONG", "custom")
	if _, err := d.EncodePacket(p); err == nil {
		t.Fatal("expected length budget error")
	} else if !strings.Contains(err.Error(), "greater than maximum") {
		t.Errorf("expected length budget error, got: %v", err)
	}
}

func TestCodecDecoderIsolation(t *testing.T) {
	stringDecoder := mustDecoder(t, WithCodec(ClassContext, TypePrimitive, 3, Funcs{
		DecodeFunc: func(content []byte) (interface{}, error) {
			return string(content), nil
		},
	}))
	intDecoder := mustDecoder(t, WithCodec(ClassContext, TypePrimitive, 3, Funcs{
		DecodeFunc: func(content []byte) (interface{}, error) {
			return ParseInt64(content)
		},
	}))

	// [3] primitive, content 0x01 0x02 -- same bytes for both decoders.
	data := []byte{0x83, 0x02, 0x01, 0x02}

	sp, err := stringDecoder.DecodePacketErr(data)
	if err != nil {
		t.Fatalf("stringDecoder: %v", err)
	}
	if v, ok := sp.Value.(string); !ok || v != "\x01\x02" {
		t.Errorf("expected string Value, got %#v", sp.Value)
	}

	ip, err := intDecoder.DecodePacketErr(data)
	if err != nil {
		t.Fatalf("intDecoder: %v", err)
	}
	if v, ok := ip.Value.(int64); !ok || v != 0x0102 {
		t.Errorf("expected int64 Value 0x102, got %#v", ip.Value)
	}

	// The package-level decoder sees neither registration and keeps raw data.
	gp := DecodePacket(data)
	if gp.Value != nil {
		t.Errorf("expected nil Value from package-level decode, got %#v", gp.Value)
	}
	if !bytes.Equal(gp.Data.Bytes(), []byte{0x01, 0x02}) {
		t.Errorf("raw content lost: %x", gp.Data.Bytes())
	}
}
