package ntp

import (
	"errors"
	"math"
	"time"
)

var (
	// ErrInvalidQualityPolicy indicates that a QualityPolicy contains
	// invalid values, such as a negative duration limit.
	ErrInvalidQualityPolicy = errors.New("invalid quality policy")

	// ErrMissingReceiveTime indicates that a response cannot be evaluated
	// for sample age because it contains no client receive timestamp.
	ErrMissingReceiveTime = errors.New("response has no client receive timestamp")
)

// A QualityPolicy describes caller-imposed quality limits that a response
// must satisfy in addition to the protocol-level checks performed by
// Validate. Each limit is an inclusive maximum: a measured value equal to
// its limit passes. A zero limit disables the corresponding check. A
// negative limit is invalid and causes the entire policy to be rejected
// with ErrInvalidQualityPolicy.
type QualityPolicy struct {
	// MaxRTT limits the response's round-trip-time delay. Zero disables
	// the check.
	MaxRTT time.Duration

	// MaxRootDistance limits the response's synchronization distance to
	// the root (stratum 1) server. Zero disables the check.
	MaxRootDistance time.Duration

	// MaxClockOffset limits the absolute value of the response's clock
	// offset. Zero disables the check.
	MaxClockOffset time.Duration

	// MaxAge limits the age of the sample, measured from the response's
	// client receive timestamp (Timestamps.ClientRecv) to Now. A sample
	// whose receive timestamp is in the future relative to Now (a negative
	// age) fails the check. Zero disables the check.
	MaxAge time.Duration

	// Now is the reference time used to compute the sample's age. If zero,
	// the current system time is used.
	Now time.Time
}

// validate checks the policy for invalid values.
func (p QualityPolicy) validate() error {
	if p.MaxRTT < 0 || p.MaxRootDistance < 0 || p.MaxClockOffset < 0 ||
		p.MaxAge < 0 {
		return ErrInvalidQualityPolicy
	}
	return nil
}

// A QualityCheckID identifies an individual quality check.
type QualityCheckID int

const (
	// QualityCheckRTT identifies the round-trip-time check.
	QualityCheckRTT QualityCheckID = iota

	// QualityCheckRootDistance identifies the root synchronization distance
	// check.
	QualityCheckRootDistance

	// QualityCheckClockOffset identifies the absolute clock offset check.
	QualityCheckClockOffset

	// QualityCheckAge identifies the sample age check.
	QualityCheckAge
)

// String returns a human-readable name for the quality check.
func (id QualityCheckID) String() string {
	switch id {
	case QualityCheckRTT:
		return "RTT"
	case QualityCheckRootDistance:
		return "RootDistance"
	case QualityCheckClockOffset:
		return "ClockOffset"
	case QualityCheckAge:
		return "Age"
	default:
		return "Unknown"
	}
}

// A QualityCheck reports the outcome of a single quality check.
type QualityCheck struct {
	// ID identifies which quality check was performed.
	ID QualityCheckID

	// Limit is the inclusive maximum value allowed by the policy.
	Limit time.Duration

	// Value is the value measured from the response.
	Value time.Duration

	// Pass reports whether the measured value satisfies the limit.
	Pass bool

	// Err is non-nil when the check could not be evaluated (for example,
	// when the response lacks a client receive timestamp). A check that
	// cannot be evaluated never passes.
	Err error
}

// A QualityReport is an itemized report describing whether a response is
// suitable for time synchronization under a caller-supplied QualityPolicy.
// It distinguishes protocol-level validity from caller-imposed quality
// requirements. The report is derived from the response without modifying
// it.
type QualityReport struct {
	// ProtocolErr is non-nil when the response failed the protocol-level
	// checks performed by Validate. Protocol failures, including
	// authentication and request-correlation failures, cannot be overridden
	// by relaxing quality limits.
	ProtocolErr error

	// PolicyErr is non-nil when the quality policy itself is invalid (for
	// example, when it contains a negative duration limit). When PolicyErr
	// is non-nil, no quality checks are performed.
	PolicyErr error

	// Checks contains one entry for each quality check enabled by the
	// policy.
	Checks []QualityCheck
}

// ProtocolValid reports whether the response passed the protocol-level
// checks performed by Validate.
func (r QualityReport) ProtocolValid() bool {
	return r.ProtocolErr == nil
}

// QualityValid reports whether the quality policy was valid and every
// enabled quality check passed. It does not consider protocol-level
// validity.
func (r QualityReport) QualityValid() bool {
	if r.PolicyErr != nil {
		return false
	}
	for _, c := range r.Checks {
		if !c.Pass {
			return false
		}
	}
	return true
}

// Valid reports whether the response is both protocol-valid and
// quality-valid.
func (r QualityReport) Valid() bool {
	return r.ProtocolValid() && r.QualityValid()
}

// FailedChecks returns the subset of quality checks that did not pass.
func (r QualityReport) FailedChecks() []QualityCheck {
	var failed []QualityCheck
	for _, c := range r.Checks {
		if !c.Pass {
			failed = append(failed, c)
		}
	}
	return failed
}

// ValidateQuality validates the response against the protocol-level rules
// applied by Validate and the caller-supplied quality policy, returning an
// itemized report. The response is not modified. Protocol-level failures
// (such as authentication or request-correlation failures) are reported in
// the report's ProtocolErr field and cannot be overridden by relaxing
// quality limits.
func (r *Response) ValidateQuality(policy QualityPolicy) QualityReport {
	report := QualityReport{ProtocolErr: r.Validate()}

	if err := policy.validate(); err != nil {
		report.PolicyErr = err
		return report
	}

	if policy.MaxRTT > 0 {
		report.Checks = append(report.Checks, QualityCheck{
			ID:    QualityCheckRTT,
			Limit: policy.MaxRTT,
			Value: r.RTT,
			Pass:  r.RTT <= policy.MaxRTT,
		})
	}

	if policy.MaxRootDistance > 0 {
		report.Checks = append(report.Checks, QualityCheck{
			ID:    QualityCheckRootDistance,
			Limit: policy.MaxRootDistance,
			Value: r.RootDistance,
			Pass:  r.RootDistance <= policy.MaxRootDistance,
		})
	}

	if policy.MaxClockOffset > 0 {
		offset := absDuration(r.ClockOffset)
		report.Checks = append(report.Checks, QualityCheck{
			ID:    QualityCheckClockOffset,
			Limit: policy.MaxClockOffset,
			Value: offset,
			Pass:  offset <= policy.MaxClockOffset,
		})
	}

	if policy.MaxAge > 0 {
		check := QualityCheck{
			ID:    QualityCheckAge,
			Limit: policy.MaxAge,
		}
		if r.Timestamps.ClientRecv.IsZero() {
			// The sample's age cannot be determined without a client
			// receive timestamp, so the check fails closed.
			check.Err = ErrMissingReceiveTime
		} else {
			now := policy.Now
			if now.IsZero() {
				now = getSystemTime()
			}
			age := now.Sub(r.Timestamps.ClientRecv)
			check.Value = age
			check.Pass = age >= 0 && age <= policy.MaxAge
		}
		report.Checks = append(report.Checks, check)
	}

	return report
}

// absDuration returns the absolute value of d. The absolute value of the
// minimum duration is not representable, so it saturates to the maximum
// duration.
func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		if d == math.MinInt64 {
			return math.MaxInt64
		}
		return -d
	}
	return d
}
