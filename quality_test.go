// Copyright © Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ntp

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// qualityTestResponse builds a protocol-valid response with known quality
// values: RTT 40ms, RootDistance 30ms, ClockOffset +25ms, received at now.
func qualityTestResponse() *Response {
	now := time.Now()
	return &Response{
		ClockOffset: 25 * time.Millisecond,
		RTT:         40 * time.Millisecond,
		Timestamps: ProtocolTimestamps{
			ClientXmit: now.Add(-40 * time.Millisecond),
			ServerRecv: now.Add(-15 * time.Millisecond),
			ServerXmit: now.Add(-10 * time.Millisecond),
			ClientRecv: now,
		},
		Version:        4,
		Stratum:        2,
		ReferenceTime:  now.Add(-time.Hour),
		Time:           now.Add(-10 * time.Millisecond),
		RootDelay:      10 * time.Millisecond,
		RootDispersion: 5 * time.Millisecond,
		RootDistance:   30 * time.Millisecond,
		Flags:          FlagSynchronized,
	}
}

func TestCheckQualityStrictVsLenient(t *testing.T) {
	r := qualityTestResponse()

	strict := r.CheckQuality(QualityPolicy{
		MaxRTT:          10 * time.Millisecond,
		MaxRootDistance: 10 * time.Millisecond,
		MaxClockOffset:  10 * time.Millisecond,
	})
	lenient := r.CheckQuality(QualityPolicy{
		MaxRTT:          100 * time.Millisecond,
		MaxRootDistance: 100 * time.Millisecond,
		MaxClockOffset:  100 * time.Millisecond,
	})

	// Same protocol conclusion under both policies.
	assert.True(t, strict.ProtocolValid())
	assert.True(t, lenient.ProtocolValid())
	assert.NoError(t, strict.ProtocolErr)
	assert.NoError(t, lenient.ProtocolErr)

	// Different quality conclusions.
	assert.False(t, strict.QualityOK())
	assert.False(t, strict.OK())
	assert.True(t, lenient.QualityOK())
	assert.True(t, lenient.OK())

	assert.Equal(t, QualityFailed, strict.Check(QualityCheckRTT).Status)
	assert.Equal(t, QualityFailed, strict.Check(QualityCheckRootDistance).Status)
	assert.Equal(t, QualityFailed, strict.Check(QualityCheckClockOffset).Status)
	assert.Equal(t, QualityPassed, lenient.Check(QualityCheckRTT).Status)
	assert.Equal(t, QualityPassed, lenient.Check(QualityCheckRootDistance).Status)
	assert.Equal(t, QualityPassed, lenient.Check(QualityCheckClockOffset).Status)
}

func TestCheckQualityProtocolFailureNotRelaxable(t *testing.T) {
	// An authentication failure is a protocol failure and must remain
	// invalid even when all quality limits are disabled or generous.
	r := qualityTestResponse()
	r.authErr = ErrAuthFailed

	report := r.CheckQuality(QualityPolicy{})
	assert.False(t, report.ProtocolValid())
	assert.ErrorIs(t, report.ProtocolErr, ErrAuthFailed)
	assert.True(t, report.QualityOK())
	assert.False(t, report.OK())

	report = r.CheckQuality(QualityPolicy{
		MaxRTT:          time.Hour,
		MaxRootDistance: time.Hour,
		MaxClockOffset:  time.Hour,
		MaxAge:          time.Hour,
	})
	assert.False(t, report.ProtocolValid())
	assert.True(t, report.QualityOK())
	assert.False(t, report.OK())

	// A kiss-of-death response is protocol-invalid regardless of limits.
	r = qualityTestResponse()
	r.Stratum = 0
	r.KissCode = "RATE"
	report = r.CheckQuality(QualityPolicy{})
	assert.False(t, report.ProtocolValid())
	assert.ErrorIs(t, report.ProtocolErr, ErrKissOfDeath)
	assert.False(t, report.OK())
}

func TestCheckQualityBoundaryInclusive(t *testing.T) {
	r := qualityTestResponse()
	report := r.CheckQuality(QualityPolicy{
		MaxRTT:          40 * time.Millisecond,
		MaxRootDistance: 30 * time.Millisecond,
		MaxClockOffset:  25 * time.Millisecond,
	})
	assert.True(t, report.OK())
	assert.Equal(t, QualityPassed, report.Check(QualityCheckRTT).Status)
	assert.Equal(t, QualityPassed, report.Check(QualityCheckRootDistance).Status)
	assert.Equal(t, QualityPassed, report.Check(QualityCheckClockOffset).Status)

	// One nanosecond below the measured value fails.
	report = r.CheckQuality(QualityPolicy{MaxRTT: 40*time.Millisecond - 1})
	assert.Equal(t, QualityFailed, report.Check(QualityCheckRTT).Status)
	assert.False(t, report.OK())
}

func TestCheckQualityNegativeLimit(t *testing.T) {
	r := qualityTestResponse()
	report := r.CheckQuality(QualityPolicy{
		MaxRTT:          -1,
		MaxRootDistance: -time.Second,
		MaxClockOffset:  -time.Second,
		MaxAge:          -time.Second,
	})
	assert.True(t, report.ProtocolValid())
	names := []string{
		QualityCheckRTT, QualityCheckRootDistance,
		QualityCheckClockOffset, QualityCheckSampleAge,
	}
	for _, name := range names {
		c := report.Check(name)
		assert.Equal(t, QualityFailed, c.Status, name)
		assert.ErrorIs(t, c.Err, ErrNegativeQualityLimit, name)
	}
	assert.False(t, report.QualityOK())
	assert.False(t, report.OK())
}

func TestCheckQualityClockOffsetAbsolute(t *testing.T) {
	r := qualityTestResponse()
	r.ClockOffset = -25 * time.Millisecond

	report := r.CheckQuality(QualityPolicy{MaxClockOffset: 10 * time.Millisecond})
	c := report.Check(QualityCheckClockOffset)
	assert.Equal(t, QualityFailed, c.Status)
	assert.Equal(t, 25*time.Millisecond, c.Value)

	report = r.CheckQuality(QualityPolicy{MaxClockOffset: 25 * time.Millisecond})
	assert.Equal(t, QualityPassed, report.Check(QualityCheckClockOffset).Status)

	// The minimum duration value must not overflow when negated; it
	// saturates to the maximum duration.
	r.ClockOffset = time.Duration(math.MinInt64)
	report = r.CheckQuality(QualityPolicy{MaxClockOffset: time.Duration(math.MaxInt64 - 1)})
	c = report.Check(QualityCheckClockOffset)
	assert.Equal(t, QualityFailed, c.Status)
	assert.Equal(t, time.Duration(math.MaxInt64), c.Value)
}

func TestCheckQualitySampleAge(t *testing.T) {
	r := qualityTestResponse()
	now := r.Timestamps.ClientRecv.Add(500 * time.Millisecond)

	// Age within limit.
	report := r.CheckQuality(QualityPolicy{MaxAge: time.Second, Now: now})
	c := report.Check(QualityCheckSampleAge)
	assert.Equal(t, QualityPassed, c.Status)
	assert.Equal(t, 500*time.Millisecond, c.Value)
	assert.True(t, report.OK())

	// Age beyond limit.
	report = r.CheckQuality(QualityPolicy{MaxAge: 100 * time.Millisecond, Now: now})
	assert.Equal(t, QualityFailed, report.Check(QualityCheckSampleAge).Status)
	assert.False(t, report.OK())

	// Receive timestamp in the future yields a negative age, which fails.
	report = r.CheckQuality(QualityPolicy{
		MaxAge: time.Hour,
		Now:    r.Timestamps.ClientRecv.Add(-time.Second),
	})
	c = report.Check(QualityCheckSampleAge)
	assert.Equal(t, QualityFailed, c.Status)
	assert.Negative(t, c.Value)

	// Zero limit skips the check.
	report = r.CheckQuality(QualityPolicy{})
	assert.Equal(t, QualitySkipped, report.Check(QualityCheckSampleAge).Status)
}

func TestCheckQualitySampleAgeMissingReceiveTime(t *testing.T) {
	r := qualityTestResponse()
	r.Timestamps.ClientRecv = time.Time{}

	report := r.CheckQuality(QualityPolicy{MaxAge: time.Second})
	c := report.Check(QualityCheckSampleAge)
	assert.Equal(t, QualityUnknown, c.Status)
	assert.ErrorIs(t, c.Err, ErrMissingReceiveTime)
	assert.True(t, report.ProtocolValid())
	assert.False(t, report.QualityOK())
	assert.False(t, report.OK())

	// Without a MaxAge limit, the missing timestamp is irrelevant.
	report = r.CheckQuality(QualityPolicy{})
	assert.Equal(t, QualitySkipped, report.Check(QualityCheckSampleAge).Status)
	assert.True(t, report.OK())
}

func TestCheckQualityDoesNotModifyResponse(t *testing.T) {
	r := qualityTestResponse()
	before := *r
	before.Timestamps = r.Timestamps

	_ = r.CheckQuality(QualityPolicy{
		MaxRTT:          time.Millisecond,
		MaxRootDistance: time.Millisecond,
		MaxClockOffset:  time.Millisecond,
		MaxAge:          time.Millisecond,
	})

	assert.Equal(t, before, *r)
}

func TestCheckQualityValidateUnchanged(t *testing.T) {
	// CheckQuality must not alter the result of Validate.
	r := qualityTestResponse()
	assert.NoError(t, r.Validate())
	_ = r.CheckQuality(QualityPolicy{MaxRTT: time.Nanosecond})
	assert.NoError(t, r.Validate())

	r.authErr = ErrAuthFailed
	assert.ErrorIs(t, r.Validate(), ErrAuthFailed)
	_ = r.CheckQuality(QualityPolicy{})
	assert.ErrorIs(t, r.Validate(), ErrAuthFailed)
}
