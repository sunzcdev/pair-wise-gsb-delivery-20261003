package ber

import (
	"fmt"
	"time"
)

// UTCTime year window per X.690 Section 11.8: two-digit years 00-49 map to
// 2000-2049 and 50-99 map to 1950-1999.
const (
	utcTimeMinYear = 1950
	utcTimeMaxYear = 2049
)

// ParseUTCTime parses a string value and if it conforms to UTCTime[^0]
// format, will return a time.Time for that value. The accepted forms are
// "YYMMDDHHMMZ", "YYMMDDHHMMSSZ", "YYMMDDHHMM+HHMM", "YYMMDDHHMM-HHMM",
// "YYMMDDHHMMSS+HHMM" and "YYMMDDHHMMSS-HHMM". Anything else, including
// trailing garbage and invalid calendar dates, is rejected.
//
// [^0]: https://www.itu.int/rec/T-REC-X.690-201508-I/en Section 11.8
func ParseUTCTime(v []byte) (time.Time, error) {
	str := string(v)

	// Strip the timezone suffix first so the remaining prefix must be all
	// digits: "Z", or "+HHMM" / "-HHMM".
	offset := 0
	switch {
	case len(str) >= 1 && str[len(str)-1] == 'Z':
		str = str[:len(str)-1]
	case len(str) >= 5 && (str[len(str)-5] == '+' || str[len(str)-5] == '-'):
		sign := 1
		if str[len(str)-5] == '-' {
			sign = -1
		}
		offHour, ok := utcTimeDigits(str, len(str)-4, 2)
		if !ok {
			return zeroTime, ErrInvalidTimeFormat
		}
		offMin, ok := utcTimeDigits(str, len(str)-2, 2)
		if !ok {
			return zeroTime, ErrInvalidTimeFormat
		}
		if offHour > 23 || offMin > 59 {
			return zeroTime, ErrInvalidTimeFormat
		}
		offset = sign * (offHour*3600 + offMin*60)
		str = str[:len(str)-5]
	default:
		return zeroTime, ErrInvalidTimeFormat
	}

	// What remains is YYMMDDHHMM (10 digits) or YYMMDDHHMMSS (12 digits).
	var sec int
	switch len(str) {
	case 10:
	case 12:
		var ok bool
		sec, ok = utcTimeDigits(str, 10, 2)
		if !ok {
			return zeroTime, ErrInvalidTimeFormat
		}
	default:
		return zeroTime, ErrInvalidTimeFormat
	}

	fields := make([]int, 5)
	for i := range fields {
		n, ok := utcTimeDigits(str, i*2, 2)
		if !ok {
			return zeroTime, ErrInvalidTimeFormat
		}
		fields[i] = n
	}
	year, month, day, hour, min := fields[0], fields[1], fields[2], fields[3], fields[4]

	if year <= 49 {
		year += 2000
	} else {
		year += 1900
	}

	if month < 1 || month > 12 || hour > 23 || min > 59 || sec > 59 {
		return zeroTime, ErrInvalidTimeFormat
	}
	if day < 1 || day > utcTimeDaysIn(year, time.Month(month)) {
		return zeroTime, ErrInvalidTimeFormat
	}

	loc := time.FixedZone("", offset)
	return time.Date(year, time.Month(month), day, hour, min, sec, 0, loc), nil
}

// utcTimeDigits parses n decimal digits starting at index i. It reports
// failure if any character is not a digit.
func utcTimeDigits(s string, i, n int) (int, bool) {
	v := 0
	for j := i; j < i+n; j++ {
		c := s[j]
		if c < '0' || c > '9' {
			return 0, false
		}
		v = v*10 + int(c-'0')
	}
	return v, true
}

// utcTimeDaysIn returns the number of days in the given month of the given
// (proleptic Gregorian) year.
func utcTimeDaysIn(year int, month time.Month) int {
	switch month {
	case time.April, time.June, time.September, time.November:
		return 30
	case time.February:
		if year%4 == 0 && (year%100 != 0 || year%400 == 0) {
			return 29
		}
		return 28
	default:
		return 31
	}
}

// NewUTCTimeErr behaves like [NewUTCTime] but returns an error instead of
// panicking when value cannot be represented as a UTCTime.
//
// The value is normalized to UTC and always encoded with seconds, in the
// canonical form "YYMMDDHHMMSSZ". Years outside 1950-2049 cannot be
// expressed in the two-digit year window and are reported as an error
// rather than silently truncated.
func NewUTCTimeErr(classType Class, tagType Type, tag Tag, value time.Time, description string) (*Packet, error) {
	u := value.UTC()
	if u.Year() < utcTimeMinYear || u.Year() > utcTimeMaxYear {
		return nil, fmt.Errorf("utc time: year %d out of range [%d, %d]", u.Year(), utcTimeMinYear, utcTimeMaxYear)
	}

	p := Encode(classType, tagType, tag, nil, description)
	s := u.Format(`060102150405Z`)
	p.Value = s
	p.Data.Write([]byte(s))
	return p, nil
}

// NewUTCTime builds a UTCTime packet. It panics if value cannot be
// represented as a UTCTime; use [NewUTCTimeErr] to receive an error instead.
func NewUTCTime(classType Class, tagType Type, tag Tag, value time.Time, description string) *Packet {
	p, err := NewUTCTimeErr(classType, tagType, tag, value, description)
	if err != nil {
		panic(err.Error())
	}
	return p
}
