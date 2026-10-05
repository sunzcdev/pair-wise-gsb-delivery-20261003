// Copyright © Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ntp

import (
	"errors"
	"math"
	"time"
)

var (
	// ErrNegativeQualityLimit indicates that a quality policy limit was
	// negative. Negative limits are invalid and are never treated as
	// "no limit"; the corresponding check always fails.
	ErrNegativeQualityLimit = errors.New("quality limit must not be negative")

	// ErrMissingReceiveTime indicates that a sample age check was requested
	// but the response contains no client receive timestamp, so the age of
	// the sample cannot be determined.
	ErrMissingReceiveTime = errors.New("response has no client receive timestamp")
)

// Names of the individual checks included in a QualityReport.
const (
	QualityCheckRTT          = "RTT"
	QualityCheckRootDistance = "RootDistance"
	QualityCheckClockOffset  = "ClockOffset"
	QualityCheckSampleAge    = "SampleAge"
)

// A QualityPolicy describes caller-configurable quality limits applied to an
// NTP response in addition to the protocol validation performed by
// Response.Validate.
//
// Each limit is a time.Duration with the following semantics:
//   - A zero limit disables the corresponding check (reported as skipped).
//   - A negative limit is invalid; the corresponding check fails with
//     ErrNegativeQualityLimit.
//   - A measured value passes when it is less than or equal to the limit
//     (the boundary is inclusive).
type QualityPolicy struct {
	// MaxRTT limits the response's round-trip-time delay (Response.RTT).
	MaxRTT time.Duration

	// MaxRootDistance limits the estimated synchronization distance to the
	// stratum 1 server (Response.RootDistance).
	MaxRootDistance time.Duration

	// MaxClockOffset limits the absolute value of the estimated clock
	// offset (Response.ClockOffset). The sign of the offset is ignored.
	MaxClockOffset time.Duration

	// MaxAge limits the age of the sample, measured from the response's
	// client receive timestamp (Response.Timestamps.ClientRecv) to the
	// reference time (see Now). A response without a client receive
	// timestamp cannot be evaluated and fails with ErrMissingReceiveTime.
	// A receive timestamp later than the reference time yields a negative
	// age, which always fails the check.
	MaxAge time.Duration

	// Now is the reference time used to compute the sample age for the
	// MaxAge check. If zero, the current system time is used.
	Now time.Time
}

// QualityStatus describes the outcome of a single quality check.
type QualityStatus int

const (
	// QualitySkipped indicates the check was not configured (zero limit).
	QualitySkipped QualityStatus = iota

	// QualityPassed indicates the measured value is within the limit.
	QualityPassed

	// QualityFailed indicates the measured value exceeds the limit, the
	// limit itself is invalid, or the measured value is not acceptable
	// (e.g. a negative sample age).
	QualityFailed

	// QualityUnknown indicates the check could not be evaluated because
	// the response lacks the required data (e.g. no receive timestamp).
	QualityUnknown
)

func (s QualityStatus) String() string {
	switch s {
	case QualitySkipped:
		return "skipped"
	case QualityPassed:
		return "passed"
	case QualityFailed:
		return "failed"
	case QualityUnknown:
		return "unknown"
	default:
		return "invalid"
	}
}

// A QualityCheck reports the outcome of a single quality limit check.
type QualityCheck struct {
	// Name identifies the check (e.g. QualityCheckRTT).
	Name string

	// Limit is the configured limit. Zero means the check was disabled.
	Limit time.Duration

	// Value is the measured value compared against the limit. For the
	// ClockOffset check it is the absolute value of the offset. It is
	// zero when the check was skipped or could not be evaluated.
	Value time.Duration

	// Status is the outcome of the check.
	Status QualityStatus

	// Err provides additional detail when the check failed or could not
	// be evaluated (e.g. ErrNegativeQualityLimit, ErrMissingReceiveTime).
	Err error
}

// A QualityReport contains the per-check results of evaluating a
// QualityPolicy against a Response. The protocol verdict is reported
// separately from the quality verdicts: a protocol-invalid response (for
// example, one that fails authentication or request correlation) remains
// invalid no matter how lenient the quality limits are.
type QualityReport struct {
	// ProtocolErr is the result of Response.Validate. It is nil when the
	// response is protocol-valid.
	ProtocolErr error

	// Checks contains one entry per quality check, in the order RTT,
	// RootDistance, ClockOffset, SampleAge.
	Checks []QualityCheck
}

// ProtocolValid reports whether the response passed protocol validation.
func (q QualityReport) ProtocolValid() bool {
	return q.ProtocolErr == nil
}

// QualityOK reports whether every configured quality check passed. Checks
// that were skipped do not affect the result; checks that failed or could
// not be evaluated cause QualityOK to return false.
func (q QualityReport) QualityOK() bool {
	for _, c := range q.Checks {
		if c.Status == QualityFailed || c.Status == QualityUnknown {
			return false
		}
	}
	return true
}

// OK reports whether the response is both protocol-valid and within all
// configured quality limits.
func (q QualityReport) OK() bool {
	return q.ProtocolValid() && q.QualityOK()
}

// Check returns the result of the named quality check, or nil if no such
// check is present in the report.
func (q QualityReport) Check(name string) *QualityCheck {
	for i := range q.Checks {
		if q.Checks[i].Name == name {
			return &q.Checks[i]
		}
	}
	return nil
}

// CheckQuality evaluates the response against the provided quality policy
// and returns a per-check report. It does not modify the response, and it
// does not change the behavior of Response.Validate.
//
// The report's protocol verdict is produced by Response.Validate, so
// protocol failures such as authentication errors or request/response
// mismatches are always reported as protocol-invalid and can never be
// re-accepted by relaxing quality limits.
func (r *Response) CheckQuality(p QualityPolicy) QualityReport {
	report := QualityReport{ProtocolErr: r.Validate()}

	report.Checks = append(report.Checks,
		checkLimit(QualityCheckRTT, p.MaxRTT, r.RTT),
		checkLimit(QualityCheckRootDistance, p.MaxRootDistance, r.RootDistance),
		checkLimit(QualityCheckClockOffset, p.MaxClockOffset, absDuration(r.ClockOffset)),
		r.checkSampleAge(p),
	)

	return report
}

// checkLimit compares a measured value against a configured limit. A zero
// limit skips the check, a negative limit fails it, and otherwise the value
// passes when it does not exceed the limit.
func checkLimit(name string, limit, value time.Duration) QualityCheck {
	c := QualityCheck{Name: name, Limit: limit}
	switch {
	case limit < 0:
		c.Status = QualityFailed
		c.Err = ErrNegativeQualityLimit
	case limit == 0:
		c.Status = QualitySkipped
	case value <= limit:
		c.Value = value
		c.Status = QualityPassed
	default:
		c.Value = value
		c.Status = QualityFailed
	}
	return c
}

// checkSampleAge evaluates the MaxAge limit against the age of the sample,
// measured from the client receive timestamp to the policy's reference time.
func (r *Response) checkSampleAge(p QualityPolicy) QualityCheck {
	c := QualityCheck{Name: QualityCheckSampleAge, Limit: p.MaxAge}
	switch {
	case p.MaxAge < 0:
		c.Status = QualityFailed
		c.Err = ErrNegativeQualityLimit
	case p.MaxAge == 0:
		c.Status = QualitySkipped
	default:
		recv := r.Timestamps.ClientRecv
		if recv.IsZero() {
			c.Status = QualityUnknown
			c.Err = ErrMissingReceiveTime
			break
		}
		now := p.Now
		if now.IsZero() {
			now = time.Now()
		}
		// time.Time.Sub saturates on overflow, so the age cannot wrap.
		age := now.Sub(recv)
		c.Value = age
		if age >= 0 && age <= p.MaxAge {
			c.Status = QualityPassed
		} else {
			c.Status = QualityFailed
		}
	}
	return c
}

// absDuration returns the absolute value of d. The minimum duration value
// has no positive representation, so it saturates to the maximum duration.
func absDuration(d time.Duration) time.Duration {
	if d == time.Duration(math.MinInt64) {
		return time.Duration(math.MaxInt64)
	}
	if d < 0 {
		return -d
	}
	return d
}
