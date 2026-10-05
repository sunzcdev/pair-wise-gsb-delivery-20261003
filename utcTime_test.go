package ber

import (
	"testing"
	"time"
)

func TestParseUTCTime(t *testing.T) {
	for _, test := range []struct {
		name   string
		str    string
		wanted time.Time
	}{
		{
			name:   "basic with seconds, Z",
			str:    "170222190527Z",
			wanted: time.Date(2017, time.February, 22, 19, 5, 27, 0, time.UTC),
		},
		{
			name:   "without seconds, Z (BER permitted)",
			str:    "1702221905Z",
			wanted: time.Date(2017, time.February, 22, 19, 5, 0, 0, time.UTC),
		},
		{
			name:   "negative offset with seconds",
			str:    "170222190525-0100",
			wanted: time.Date(2017, time.February, 22, 19, 5, 25, 0, time.FixedZone("", -3600)),
		},
		{
			name:   "positive offset with seconds",
			str:    "170222190525+0100",
			wanted: time.Date(2017, time.February, 22, 19, 5, 25, 0, time.FixedZone("", 3600)),
		},
		{
			name:   "hour-only offset, no seconds",
			str:    "1702221905+01",
			wanted: time.Date(2017, time.February, 22, 19, 5, 0, 0, time.FixedZone("", 3600)),
		},
		{
			name:   "offset with minutes, no seconds",
			str:    "1702221905+0530",
			wanted: time.Date(2017, time.February, 22, 19, 5, 0, 0, time.FixedZone("", 5*3600+30*60)),
		},
		{
			name:   "year window lower edge 50 -> 1950",
			str:    "500101000000Z",
			wanted: time.Date(1950, time.January, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name:   "year window upper edge 49 -> 2049",
			str:    "491231235959Z",
			wanted: time.Date(2049, time.December, 31, 23, 59, 59, 0, time.UTC),
		},
		{
			name:   "window boundary pair 99/00",
			str:    "991231235959Z",
			wanted: time.Date(1999, time.December, 31, 23, 59, 59, 0, time.UTC),
		},
		{
			name:   "00 -> 2000",
			str:    "000101000000Z",
			wanted: time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name:   "leap day 2000 (century leap year)",
			str:    "000229120000Z",
			wanted: time.Date(2000, time.February, 29, 12, 0, 0, 0, time.UTC),
		},
		{
			name:   "leap day 2020",
			str:    "200229120000Z",
			wanted: time.Date(2020, time.February, 29, 12, 0, 0, 0, time.UTC),
		},
		{
			name:   "positive offset crossing midnight backwards",
			str:    "1701010030+0100",
			wanted: time.Date(2016, time.December, 31, 23, 30, 0, 0, time.UTC),
		},
		{
			name:   "negative offset crossing midnight forwards",
			str:    "1612312330-0100",
			wanted: time.Date(2017, time.January, 1, 0, 30, 0, 0, time.UTC),
		},
		{
			name:   "negative offset crossing year boundary",
			str:    "0001010030-0100",
			wanted: time.Date(2000, time.January, 1, 1, 30, 0, 0, time.UTC),
		},
		{
			name:   "offset minutes crossing day boundary",
			str:    "1701010015+0230",
			wanted: time.Date(2016, time.December, 31, 21, 45, 0, 0, time.UTC),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseUTCTime([]byte(test.str))
			if err != nil {
				t.Fatalf("ParseUTCTime(%q) returned error: %v", test.str, err)
			}
			if !got.Equal(test.wanted) {
				t.Errorf("ParseUTCTime(%q) = %v, want %v", test.str, got.UTC(), test.wanted.UTC())
			}
		})
	}
}

func TestParseUTCTimeErrors(t *testing.T) {
	for _, str := range []string{
		"",
		"Z",
		"1702221905",     // missing terminator
		"170222190527",   // missing terminator
		"170222190527ZZ", // trailing garbage
		"170222190527Z ", // trailing space
		"170222190527Zx",
		"17022190527Z",   // too short / missing fields
		"17022219052+Z",  // lone offset sign
		"170222190527+1", // truncated offset
		"170222190527+01000",
		"170222190527+2400", // offset hour out of range
		"170222190527+0060", // offset minute out of range
		"170222190527+0ab0",
		"171322190527Z", // month 13
		"170022190527Z", // month 00
		"170200190527Z", // day 00
		"170232190527Z", // day 32
		"170229120000Z", // 2017 is not a leap year
		"190229120000Z", // 2019 is not a leap year
		"010229120000Z", // 2001 is not a leap year
		"170222240527Z", // hour 24
		"170222196027Z", // minute 60
		"170222190560Z", // second 60 (leap second not accepted)
		"17a222190527Z",
		"xx0222190527Z",
	} {
		t.Run(str, func(t *testing.T) {
			if got, err := ParseUTCTime([]byte(str)); err == nil {
				t.Errorf("ParseUTCTime(%q) = %v, want error", str, got)
			}
		})
	}
}

func TestDecodeTagUTCTime(t *testing.T) {
	// A BER UTCTime value with seconds omitted must still decode structurally.
	encoded := []byte{0x17, 0x0b}
	encoded = append(encoded, []byte("1702221905Z")...)

	p, err := DecodePacketErr(encoded)
	if err != nil {
		t.Fatalf("DecodePacketErr returned error: %v", err)
	}
	if p.Tag != TagUTCTime {
		t.Fatalf("tag = %v, want TagUTCTime", p.Tag)
	}
	got, ok := p.Value.(time.Time)
	if !ok {
		t.Fatalf("Value has type %T, want time.Time", p.Value)
	}
	want := time.Date(2017, time.February, 22, 19, 5, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("decoded time = %v, want %v", got, want)
	}

	// Malformed UTCTime content makes the whole packet fail, not a packet
	// carrying a nil Value.
	bad := []byte{0x17, 0x0e}
	bad = append(bad, []byte("170222190527Zxx")...)
	if p, err := DecodePacketErr(bad); err == nil {
		t.Fatalf("malformed UTCTime decoded without error: %v", p)
	}
}

func TestNewUTCTimeCanonical(t *testing.T) {
	for _, test := range []struct {
		name        string
		value       time.Time
		want        string
		wantInstant time.Time
	}{
		{
			name:  "utc with seconds",
			value: time.Date(2017, time.February, 22, 19, 5, 27, 0, time.UTC),
			want:  "170222190527Z",
		},
		{
			name:  "seconds always emitted, even zero",
			value: time.Date(2017, time.February, 22, 19, 5, 0, 0, time.UTC),
			want:  "170222190500Z",
		},
		{
			name:  "fixed zone converted to UTC",
			value: time.Date(2017, time.February, 22, 20, 5, 27, 0, time.FixedZone("", 3600)),
			want:  "170222190527Z",
		},
		{
			name:        "nanoseconds truncated",
			value:       time.Date(2017, time.February, 22, 19, 5, 27, 987654321, time.UTC),
			want:        "170222190527Z",
			wantInstant: time.Date(2017, time.February, 22, 19, 5, 27, 0, time.UTC),
		},
		{
			name:  "lower window edge",
			value: time.Date(1950, time.January, 1, 0, 0, 0, 0, time.UTC),
			want:  "500101000000Z",
		},
		{
			name:  "upper window edge",
			value: time.Date(2049, time.December, 31, 23, 59, 59, 0, time.UTC),
			want:  "491231235959Z",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, err := NewUTCTimeErr(ClassUniversal, TypePrimitive, TagUTCTime, test.value, "")
			if err != nil {
				t.Fatalf("NewUTCTimeErr returned error: %v", err)
			}
			if got := p.Data.String(); got != test.want {
				t.Errorf("content = %q, want %q", got, test.want)
			}
			if got, ok := p.Value.(string); !ok || got != test.want {
				t.Errorf("Value = %v, want %q", p.Value, test.want)
			}

			// Canonical output must parse back to the same UTC instant.
			decoded, err := DecodePacketErr(p.Bytes())
			if err != nil {
				t.Fatalf("round-trip decode failed: %v", err)
			}
			gotTime, ok := decoded.Value.(time.Time)
			if !ok {
				t.Fatalf("decoded Value has type %T, want time.Time", decoded.Value)
			}
			wantInstant := test.wantInstant
			if wantInstant.IsZero() {
				wantInstant = test.value
			}
			if !gotTime.Equal(wantInstant) {
				t.Errorf("round-trip instant = %v, want %v", gotTime.UTC(), wantInstant.UTC())
			}
		})
	}
}

func TestNewUTCTimeYearOutOfRange(t *testing.T) {
	for _, value := range []time.Time{
		time.Date(1949, time.December, 31, 23, 59, 59, 0, time.UTC),
		time.Date(2050, time.January, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2100, time.January, 1, 0, 0, 0, 0, time.UTC),
		time.Date(1900, time.January, 1, 0, 0, 0, 0, time.UTC),
	} {
		t.Run(value.Format("2006"), func(t *testing.T) {
			p, err := NewUTCTimeErr(ClassUniversal, TypePrimitive, TagUTCTime, value, "")
			if err == nil {
				t.Fatalf("expected error, got packet %q", p.Data.String())
			}

			func() {
				defer func() {
					if r := recover(); r == nil {
						t.Fatal("NewUTCTime did not panic")
					}
				}()
				NewUTCTime(ClassUniversal, TypePrimitive, TagUTCTime, value, "")
			}()
		})
	}
}
