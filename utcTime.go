package ber

import (
	"errors"
	"fmt"
	"time"
)

// ErrYearOutOfRange is returned when encoding a [time.Time] whose year cannot
// be expressed in the two-digit UTCTime year window (1950 through 2049).
var ErrYearOutOfRange = errors.New("year out of range for UTCTime")

const (
	utcTimeMinYear = 1950
	utcTimeMaxYear = 2049
)

// utcTimeBaseYear maps a two-digit UTCTime year onto a full year per
// ITU-T X.501 / X.690: values 00-49 belong to 2000-2049 and values 50-99
// belong to 1950-1999.
func utcTimeBaseYear(twoDigit int) int {
	if twoDigit < 50 {
		return 2000 + twoDigit
	}
	return 1900 + twoDigit
}

func isASCIIDigits(b []byte) bool {
	for _, c := range b {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func parseTwoDigits(b []byte) int {
	return int(b[0]-'0')*10 + int(b[1]-'0')
}

// ParseUTCTime parses a BER UTCTime value (universal tag 23) and returns the
// instant it denotes.
//
// UTCTime encodes a date and time of day in Greenwich Mean Time with a
// two-digit year. Year digits 00-49 map to 2000-2049 and digits 50-99 map to
// 1950-1999, as mandated by ITU-T X.501 (D.2.2) and used by X.690.
//
// The accepted forms are:
//
//	YYMMDDhhmmZ
//	YYMMDDhhmmssZ
//	YYMMDDhhmm+HHMM   (or -HHMM)
//	YYMMDDhhmmss+HHMM (or -HHMM)
//
// Seconds are optional for BER compatibility (DER requires them). As an
// extension, the hour-only offset form +HH / -HH is accepted as well, matching
// ParseGeneralizedTime. The time-of-day and calendar fields must describe a
// real instant (February 29 in non-leap years, hour 24, second 60, etc. are
// rejected), and the value must end exactly at the terminating 'Z' or offset;
// any trailing bytes are reported as trailing garbage.
func ParseUTCTime(v []byte) (time.Time, error) {
	// YY MM DD hh mm is mandatory: 10 digits followed by the terminator.
	if len(v) < 11 || !isASCIIDigits(v[:10]) {
		return zeroTime, ErrInvalidTimeFormat
	}

	pos := 10

	// Seconds are optional.
	hasSeconds := false
	if len(v) > pos+2 && v[pos] >= '0' && v[pos] <= '9' &&
		(v[pos+1] >= '0' && v[pos+1] <= '9') {
		// A digit pair here can only be seconds; the terminator is never a digit.
		hasSeconds = true
		pos += 2
	}

	if len(v) == pos {
		return zeroTime, ErrInvalidTimeFormat
	}

	year := utcTimeBaseYear(parseTwoDigits(v[0:2]))
	month := parseTwoDigits(v[2:4])
	day := parseTwoDigits(v[4:6])
	hour := parseTwoDigits(v[6:8])
	minute := parseTwoDigits(v[8:10])
	second := 0
	if hasSeconds {
		second = parseTwoDigits(v[10:12])
	}

	var loc *time.Location
	switch sign := v[pos]; sign {
	case 'Z':
		pos++
		loc = time.UTC
	case '+', '-':
		off := v[pos+1:]
		var offH, offM int
		switch len(off) {
		case 2:
			if !isASCIIDigits(off) {
				return zeroTime, ErrInvalidTimeFormat
			}
			offH = parseTwoDigits(off)
		case 4:
			if !isASCIIDigits(off) {
				return zeroTime, ErrInvalidTimeFormat
			}
			offH = parseTwoDigits(off[0:2])
			offM = parseTwoDigits(off[2:4])
		default:
			return zeroTime, ErrInvalidTimeFormat
		}
		if offH > 23 || offM > 59 {
			return zeroTime, ErrInvalidTimeFormat
		}
		secondsEast := offH*3600 + offM*60
		if sign == '-' {
			secondsEast = -secondsEast
		}
		loc = time.FixedZone("", secondsEast)
		pos = len(v)
	default:
		return zeroTime, ErrInvalidTimeFormat
	}

	// The terminator must be the end of the value: no trailing garbage.
	if pos != len(v) {
		return zeroTime, ErrInvalidTimeFormat
	}

	// Assemble in the stated zone and round-trip the components so that
	// impossible calendar dates (2020-02-30, hour 24, second 60, ...) and
	// leap seconds are rejected.
	t := time.Date(year, time.Month(month), day, hour, minute, second, 0, loc)
	if t.Year() != year || int(t.Month()) != month || t.Day() != day ||
		t.Hour() != hour || t.Minute() != minute || t.Second() != second {
		return zeroTime, fmt.Errorf("%w: impossible date or time of day", ErrInvalidTimeFormat)
	}

	return t, nil
}

// NewUTCTimeErr builds a canonical DER-style UTCTime packet (universal tag
// 23) from value. The value is converted to UTC (so its location is
// irrelevant), seconds are always included, and the result has the form
// YYMMDDhhmmssZ.
//
// Because UTCTime only carries a two-digit year, the UTC instant must fall in
// the expressible window 1950-01-01 00:00:00 UTC through 2049-12-31
// 23:59:59 UTC. Years outside that window produce an ErrYearOutOfRange instead
// of silently truncating to the last two digits. Sub-second precision is
// truncated rather than encoded.
func NewUTCTimeErr(classType Class, tagType Type, tag Tag, value time.Time, description string) (*Packet, error) {
	utc := value.UTC()
	year := utc.Year()
	if year < utcTimeMinYear || year > utcTimeMaxYear {
		return nil, fmt.Errorf("%w: %d", ErrYearOutOfRange, year)
	}

	p := Encode(classType, tagType, tag, nil, description)
	s := utc.Format("060102150405Z")
	p.Value = s
	p.Data.Write([]byte(s))
	return p, nil
}

// NewUTCTime builds a canonical UTCTime packet. It panics if the value cannot
// be expressed in UTCTime (see [NewUTCTimeErr]); use NewUTCTimeErr to receive
// the error instead.
func NewUTCTime(classType Class, tagType Type, tag Tag, value time.Time, description string) *Packet {
	p, err := NewUTCTimeErr(classType, tagType, tag, value, description)
	if err != nil {
		panic(err.Error())
	}
	return p
}
