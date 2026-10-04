package ber

import (
	"testing"
	"time"
)

// TestParseUTCTime exercises BER-compatible reading of UTCTime values. All
// expected instants are expressed against fixed zones or UTC so the tests do
// not depend on the machine's local timezone.
func TestParseUTCTime(t *testing.T) {
	for _, test := range []struct {
		str    string
		wanted time.Time
		err    bool
	}{
		// Window boundaries: 50 maps to 1950, 49 maps to 2049.
		{"500101000000Z", time.Date(1950, 1, 1, 0, 0, 0, 0, time.UTC), false},
		{"491231235959Z", time.Date(2049, 12, 31, 23, 59, 59, 0, time.UTC), false},
		{"000229120000Z", time.Date(2000, 2, 29, 12, 0, 0, 0, time.UTC), false}, // 2000 is a leap year
		{"990101000000Z", time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC), false},

		// With and without seconds, with and without offset.
		{"060101120000Z", time.Date(2006, 1, 1, 12, 0, 0, 0, time.UTC), false},
		{"0601011200Z", time.Date(2006, 1, 1, 12, 0, 0, 0, time.UTC), false},
		{"060101120030+0530", time.Date(2006, 1, 1, 12, 0, 30, 0, time.FixedZone("", 5*3600+30*60)), false},
		{"0601011200-0800", time.Date(2006, 1, 1, 12, 0, 0, 0, time.FixedZone("", -8*3600)), false},

		// Offsets crossing a day boundary, compared as instants in UTC.
		{"0601010030+0100", time.Date(2005, 12, 31, 23, 30, 0, 0, time.UTC), false},
		{"0601012330-0200", time.Date(2006, 1, 2, 1, 30, 0, 0, time.UTC), false},
		{"0603010000+0200", time.Date(2006, 2, 28, 22, 0, 0, 0, time.UTC), false}, // month boundary

		// Leap year handling.
		{"040229235960Z", time.Time{}, true}, // second 60 invalid
		{"040229000000Z", time.Date(2004, 2, 29, 0, 0, 0, 0, time.UTC), false},
		{"990229000000Z", time.Time{}, true}, // 1999 is not a leap year
		{"000230000000Z", time.Time{}, true}, // February 30th never exists

		// Invalid fields.
		{"0613011200Z", time.Time{}, true}, // month 13
		{"0601001200Z", time.Time{}, true}, // day 0
		{"0601321200Z", time.Time{}, true}, // day 32
		{"0601012400Z", time.Time{}, true}, // hour 24
		{"0601011260Z", time.Time{}, true}, // minute 60
		{"0601011200+2400", time.Time{}, true},
		{"0601011200+0060", time.Time{}, true},

		// Trailing garbage and malformed shapes.
		{"0601011200Z00", time.Time{}, true},
		{"0601011200+01000", time.Time{}, true},
		{"0601011200X", time.Time{}, true},
		{"060101120", time.Time{}, true},
		{"060101120000", time.Time{}, true}, // no timezone suffix
		{"0601011200+01", time.Time{}, true},
		{"0601011200+0Z00", time.Time{}, true},
		{"0601011a00Z", time.Time{}, true},
		{"0601011200+0a00", time.Time{}, true},
		{"", time.Time{}, true},
	} {
		utcTime, err := ParseUTCTime([]byte(test.str))
		if test.err {
			if err == nil {
				t.Errorf("test %s: expected error, got %s", test.str, utcTime)
			}
			continue
		}
		if err != nil {
			t.Errorf("test %s failed with error: %s", test.str, err)
		} else if !utcTime.Equal(test.wanted) {
			t.Errorf("test %s got unexpected result: wanted=%s, got=%s", test.str, test.wanted, utcTime)
		}
	}
}

// TestDecodePacketUTCTime verifies that BER decoding of a UTCTime packet
// produces a time.Time value.
func TestDecodePacketUTCTime(t *testing.T) {
	raw := []byte{byte(TagUTCTime), 0x0D}
	raw = append(raw, []byte("060101120030Z")...)

	p, err := DecodePacketErr(raw)
	if err != nil {
		t.Fatalf("failed to decode UTCTime packet: %s", err)
	}
	got, ok := p.Value.(time.Time)
	if !ok {
		t.Fatalf("expected time.Time value, got %T (%v)", p.Value, p.Value)
	}
	want := time.Date(2006, 1, 1, 12, 0, 30, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("unexpected value: wanted=%s, got=%s", want, got)
	}
}

// TestNewUTCTimeErr exercises the canonical output: values are normalized to
// UTC, always carry seconds, and years outside 1950-2049 are rejected instead
// of being truncated to two digits.
func TestNewUTCTimeErr(t *testing.T) {
	for _, test := range []struct {
		value  time.Time
		wanted string
		err    bool
	}{
		{time.Date(2006, 1, 1, 12, 0, 30, 0, time.UTC), "060101120030Z", false},
		// Seconds are always emitted, even when zero.
		{time.Date(2006, 1, 1, 12, 0, 0, 0, time.UTC), "060101120000Z", false},
		// Non-UTC input is normalized to UTC.
		{time.Date(2006, 1, 1, 12, 0, 0, 0, time.FixedZone("", 3600)), "060101110000Z", false},
		{time.Date(2006, 1, 1, 0, 30, 0, 0, time.FixedZone("", 3600)), "051231233000Z", false},
		// Window boundaries are expressible.
		{time.Date(1950, 1, 1, 0, 0, 0, 0, time.UTC), "500101000000Z", false},
		{time.Date(2049, 12, 31, 23, 59, 59, 0, time.UTC), "491231235959Z", false},
		// Leap day is expressible.
		{time.Date(2000, 2, 29, 12, 0, 0, 0, time.UTC), "000229120000Z", false},
		// Years outside the window are errors, not truncations.
		{time.Date(1949, 12, 31, 23, 59, 59, 0, time.UTC), "", true},
		{time.Date(2050, 1, 1, 0, 0, 0, 0, time.UTC), "", true},
		{time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), "", true},
	} {
		p, err := NewUTCTimeErr(ClassUniversal, TypePrimitive, TagUTCTime, test.value, "UTC Time")
		if test.err {
			if err == nil {
				t.Errorf("test %s: expected error, got packet %q", test.value, p.Data.String())
			}
			continue
		}
		if err != nil {
			t.Errorf("test %s failed with error: %s", test.value, err)
			continue
		}
		if got := p.Data.String(); got != test.wanted {
			t.Errorf("test %s got unexpected encoding: wanted=%q, got=%q", test.value, test.wanted, got)
		}

		// The canonical output must round-trip through the BER decoder.
		decoded, err := DecodePacketErr(p.Bytes())
		if err != nil {
			t.Errorf("test %s: failed to re-decode packet: %s", test.value, err)
		} else if got, ok := decoded.Value.(time.Time); !ok {
			t.Errorf("test %s: re-decoded value is %T, expected time.Time", test.value, decoded.Value)
		} else if !got.Equal(test.value) {
			t.Errorf("test %s: round-trip mismatch, got=%s", test.value, got)
		}
	}
}

func TestNewUTCTimePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected NewUTCTime to panic on out-of-range year")
		}
	}()
	NewUTCTime(ClassUniversal, TypePrimitive, TagUTCTime, time.Date(2050, 1, 1, 0, 0, 0, 0, time.UTC), "UTC Time")
}
