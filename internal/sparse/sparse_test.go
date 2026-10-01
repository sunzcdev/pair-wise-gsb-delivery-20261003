package sparse

import (
	"testing"
)

func TestExtensionDataRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps Capability
	}{
		{"none", 0},
		{"extents", CapExtents},
		{"punch", CapPunch},
		{"both", CapAll},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseExtensionData(ExtensionData(tc.caps)); got != tc.caps {
				t.Fatalf("round trip = %d, want %d", got, tc.caps)
			}
		})
	}
}

func TestParseExtensionDataIgnoresUnknownAndOrder(t *testing.T) {
	caps := ParseExtensionData(" punch ,future-thing,extents,")
	if caps != CapAll {
		t.Fatalf("caps = %d, want %d", caps, CapAll)
	}
}

func TestExtentsRequestRoundTrip(t *testing.T) {
	orig := &ExtentsRequestPacket{ID: 42, Handle: "h1", Offset: 1 << 32, Length: 4096}
	wire, err := orig.MarshalPacket()
	if err != nil {
		t.Fatal(err)
	}

	var got ExtentsRequestPacket
	if err := got.UnmarshalBinary(wire); err != nil {
		t.Fatal(err)
	}
	if got != *orig {
		t.Fatalf("got %+v, want %+v", got, *orig)
	}
}

func TestPunchHolePacketRoundTrip(t *testing.T) {
	orig := &PunchHolePacket{ID: 7, Handle: "h2", Offset: 0, Length: 1 << 40}
	wire, err := orig.MarshalPacket()
	if err != nil {
		t.Fatal(err)
	}

	var got PunchHolePacket
	if err := got.UnmarshalBinary(wire); err != nil {
		t.Fatal(err)
	}
	if got != *orig {
		t.Fatalf("got %+v, want %+v", got, *orig)
	}

	// Request-body-only decode must reject the full packet.
	if err := got.UnmarshalRequestBody(wire); err == nil {
		t.Fatal("expected error decoding full packet as body")
	}
}

func TestExtentsReplyRoundTrip(t *testing.T) {
	for _, n := range []int{0, 1, 3} {
		orig := &ExtentsReplyPacket{ID: 9}
		for i := 0; i < n; i++ {
			orig.Extents = append(orig.Extents, Extent{
				Offset: uint64(i) * 8192,
				Length: 4096,
			})
		}

		wire, err := orig.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}

		var got ExtentsReplyPacket
		if err := got.UnmarshalBinary(wire); err != nil {
			t.Fatal(err)
		}
		if got.ID != orig.ID || len(got.Extents) != len(orig.Extents) {
			t.Fatalf("got %+v, want %+v", got, orig)
		}
		for i := range orig.Extents {
			if got.Extents[i] != orig.Extents[i] {
				t.Fatalf("extent %d = %+v, want %+v", i, got.Extents[i], orig.Extents[i])
			}
		}
	}
}

func TestDecodeErrors(t *testing.T) {
	if err := new(ExtentsRequestPacket).UnmarshalBinary(nil); err == nil {
		t.Fatal("expected error on nil packet")
	}
	if err := new(PunchHolePacket).UnmarshalBinary([]byte{packetTypeExtended, 0, 0, 0, 1}); err == nil {
		t.Fatal("expected error on truncated packet")
	}
	if err := new(ExtentsReplyPacket).UnmarshalBinary(nil); err == nil {
		t.Fatal("expected error on nil reply")
	}

	// A punch packet fed to the extents decoder must be rejected.
	punch, _ := (&PunchHolePacket{ID: 1, Handle: "h"}).MarshalPacket()
	if err := new(ExtentsRequestPacket).UnmarshalBinary(punch); err == nil {
		t.Fatal("expected request-name mismatch error")
	}
}
