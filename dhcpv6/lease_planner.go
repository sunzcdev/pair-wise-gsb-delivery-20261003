package dhcpv6

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// InfiniteLeaseSeconds is the wire value that represents an infinite DHCPv6
// lifetime.
const InfiniteLeaseSeconds uint32 = math.MaxUint32

const (
	// MinPlannedFiniteLease is the shortest finite lease that can contain
	// distinct renewal and rebinding times.
	MinPlannedFiniteLease = 3 * time.Second

	// MaxPlannedFiniteLease is the longest finite lease that can be encoded
	// without using DHCPv6's infinite-lifetime sentinel.
	MaxPlannedFiniteLease = time.Duration(InfiniteLeaseSeconds-1) * time.Second
)

var (
	// ErrInvalidLeaseConfiguration indicates that the planner configuration is
	// invalid.
	ErrInvalidLeaseConfiguration = errors.New("invalid lease configuration")

	// ErrInvalidLeaseRequest indicates that a requested lease or timer cannot be
	// planned.
	ErrInvalidLeaseRequest = errors.New("invalid lease request")
)

// LeaseField identifies a field planned by PlanLease.
type LeaseField uint8

const (
	// LeaseFieldLease is the lease duration.
	LeaseFieldLease LeaseField = iota

	// LeaseFieldT1 is the renewal time.
	LeaseFieldT1

	// LeaseFieldT2 is the rebinding time.
	LeaseFieldT2
)

// LeaseAdjustmentReason explains why a planned field differs from the request.
type LeaseAdjustmentReason uint8

const (
	// LeaseAdjustmentDefault means that the field was selected by the server.
	LeaseAdjustmentDefault LeaseAdjustmentReason = iota

	// LeaseAdjustmentBelowMinimum means that the request was increased to the
	// configured minimum.
	LeaseAdjustmentBelowMinimum

	// LeaseAdjustmentAboveMaximum means that the request was decreased to the
	// configured maximum.
	LeaseAdjustmentAboveMaximum

	// LeaseAdjustmentInfiniteLease means that a finite timer was ignored because
	// DHCPv6 uses zero renewal and rebinding times with an infinite lease.
	LeaseAdjustmentInfiniteLease
)

// LeaseAdjustment records one deterministic change made by the planner.
type LeaseAdjustment struct {
	Field  LeaseField
	Reason LeaseAdjustmentReason
	From   time.Duration
	To     time.Duration
}

// LeasePlannerConfig contains the server policy used to plan a lease.
type LeasePlannerConfig struct {
	MinLease        time.Duration
	MaxLease        time.Duration
	DefaultLease    time.Duration
	DefaultInfinite bool
	AllowInfinite   bool
}

// LeaseRequest contains the client's requested lease and optional timers.
type LeaseRequest struct {
	Lease    *time.Duration
	Infinite bool
	T1       *time.Duration
	T2       *time.Duration
}

// LeasePlan contains lease parameters suitable for a DHCPv6 reply.
type LeasePlan struct {
	Lease       time.Duration
	Infinite    bool
	T1          time.Duration
	T2          time.Duration
	Adjustments []LeaseAdjustment
}

// LeaseSeconds returns the uint32 lifetime used by DHCPv6 options.
func (p LeasePlan) LeaseSeconds() uint32 {
	if p.Infinite {
		return InfiniteLeaseSeconds
	}
	return uint32(p.Lease / time.Second)
}

// T1Seconds returns the uint32 renewal time used by DHCPv6 options.
func (p LeasePlan) T1Seconds() uint32 {
	return uint32(p.T1 / time.Second)
}

// T2Seconds returns the uint32 rebinding time used by DHCPv6 options.
func (p LeasePlan) T2Seconds() uint32 {
	return uint32(p.T2 / time.Second)
}

// PlanLease deterministically plans a lease, renewal time, and rebinding time.
//
// Finite plans satisfy 0 < T1 < T2 < lease. Infinite plans use Infinite and
// leave T1 and T2 at zero; the uint32 infinite sentinel is exposed only through
// LeaseSeconds. The function reads only its arguments and never reads a clock.
func PlanLease(config LeasePlannerConfig, request LeaseRequest) (LeasePlan, error) {
	minLease := roundSeconds(config.MinLease)
	maxLease := roundSeconds(config.MaxLease)
	defaultLease := roundSeconds(config.DefaultLease)

	if minLease < MinPlannedFiniteLease {
		return LeasePlan{}, invalidConfig("minimum lease must be at least 3 seconds")
	}
	if maxLease > MaxPlannedFiniteLease {
		return LeasePlan{}, invalidConfig("maximum finite lease cannot use the infinite lease value")
	}
	if maxLease < minLease {
		return LeasePlan{}, invalidConfig("maximum lease cannot be less than minimum lease")
	}
	if config.DefaultInfinite {
		if !config.AllowInfinite {
			return LeasePlan{}, invalidConfig("default infinite lease requires infinite leases to be allowed")
		}
	} else if defaultLease < minLease || defaultLease > maxLease {
		return LeasePlan{}, invalidConfig("default lease must be within the configured range")
	}
	if request.Infinite && request.Lease != nil {
		return LeasePlan{}, invalidRequest("infinite and finite lease cannot both be requested")
	}
	if request.Infinite {
		if !config.AllowInfinite {
			return LeasePlan{}, invalidRequest("infinite lease is not allowed")
		}
		return planInfiniteLease(request)
	}
	if request.Lease == nil && config.DefaultInfinite {
		return planInfiniteLease(request)
	}

	lease := defaultLease
	adjustments := []LeaseAdjustment{}
	if request.Lease != nil {
		requestedLease := roundSeconds(*request.Lease)
		if requestedLease <= 0 {
			return LeasePlan{}, invalidRequest("requested lease must be positive")
		}
		if requestedLease < minLease {
			lease = minLease
			adjustments = append(adjustments, newAdjustment(LeaseFieldLease, LeaseAdjustmentBelowMinimum, requestedLease, lease))
		} else if requestedLease > maxLease {
			lease = maxLease
			adjustments = append(adjustments, newAdjustment(LeaseFieldLease, LeaseAdjustmentAboveMaximum, requestedLease, lease))
		} else {
			lease = requestedLease
		}
	} else {
		adjustments = append(adjustments, newAdjustment(LeaseFieldLease, LeaseAdjustmentDefault, 0, lease))
	}

	t1, t2, timerAdjustments, err := planTimers(lease, request)
	if err != nil {
		return LeasePlan{}, err
	}
	adjustments = append(adjustments, timerAdjustments...)

	return LeasePlan{
		Lease:       lease,
		T1:          t1,
		T2:          t2,
		Adjustments: adjustments,
	}, nil
}

func planInfiniteLease(request LeaseRequest) (LeasePlan, error) {
	plan := LeasePlan{Infinite: true}
	if request.T1 != nil {
		t1 := roundSeconds(*request.T1)
		if t1 < 0 {
			return LeasePlan{}, invalidRequest("T1 cannot be negative")
		}
		if t1 > 0 {
			plan.Adjustments = append(plan.Adjustments, newAdjustment(LeaseFieldT1, LeaseAdjustmentInfiniteLease, t1, 0))
		}
	}
	if request.T2 != nil {
		t2 := roundSeconds(*request.T2)
		if t2 < 0 {
			return LeasePlan{}, invalidRequest("T2 cannot be negative")
		}
		if t2 > 0 {
			plan.Adjustments = append(plan.Adjustments, newAdjustment(LeaseFieldT2, LeaseAdjustmentInfiniteLease, t2, 0))
		}
	}
	return plan, nil
}

func planTimers(lease time.Duration, request LeaseRequest) (time.Duration, time.Duration, []LeaseAdjustment, error) {
	var t1, t2 time.Duration
	adjustments := []LeaseAdjustment{}

	if request.T1 != nil {
		t1 = roundSeconds(*request.T1)
		if t1 <= 0 {
			return 0, 0, nil, invalidRequest("T1 must be positive")
		}
	} else {
		t1 = defaultT1(lease)
		if request.T2 != nil {
			requestedT2 := roundSeconds(*request.T2)
			if requestedT2 < 2*time.Second {
				return 0, 0, nil, invalidRequest("T2 must leave room for T1")
			}
			if t1 >= requestedT2 {
				t1 = requestedT2 - time.Second
			}
		}
		adjustments = append(adjustments, newAdjustment(LeaseFieldT1, LeaseAdjustmentDefault, 0, t1))
	}

	if request.T2 != nil {
		t2 = roundSeconds(*request.T2)
		if t2 <= 0 {
			return 0, 0, nil, invalidRequest("T2 must be positive")
		}
	} else {
		t2 = defaultT2(lease)
		if t2 <= t1 {
			t2 = t1 + time.Second
		}
		adjustments = append(adjustments, newAdjustment(LeaseFieldT2, LeaseAdjustmentDefault, 0, t2))
	}

	if t1 <= 0 || t2 <= t1 || t2 >= lease {
		return 0, 0, nil, invalidRequest("finite lease requires 0 < T1 < T2 < lease")
	}
	return t1, t2, adjustments, nil
}

func defaultT1(lease time.Duration) time.Duration {
	seconds := lease / time.Second
	return seconds / 2 * time.Second
}

func defaultT2(lease time.Duration) time.Duration {
	seconds := lease / time.Second
	return seconds * 4 / 5 * time.Second
}

func newAdjustment(field LeaseField, reason LeaseAdjustmentReason, from, to time.Duration) LeaseAdjustment {
	return LeaseAdjustment{Field: field, Reason: reason, From: from, To: to}
}

func roundSeconds(d time.Duration) time.Duration {
	return d.Round(time.Second)
}

func invalidConfig(message string) error {
	return fmt.Errorf("%w: %s", ErrInvalidLeaseConfiguration, message)
}

func invalidRequest(message string) error {
	return fmt.Errorf("%w: %s", ErrInvalidLeaseRequest, message)
}
