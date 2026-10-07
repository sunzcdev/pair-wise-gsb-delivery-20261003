package server6

import (
	"errors"
	"fmt"
	"math"
	"time"
)

const (
	// InfiniteDuration is the planner's representation of an infinite
	// duration. It is intentionally distinct from the DHCPv6 wire value
	// math.MaxUint32.
	InfiniteDuration time.Duration = -1

	maxFiniteLeaseSeconds = int64(math.MaxUint32) - 1
	infiniteWireLease     = time.Duration(math.MaxUint32) * time.Second
)

// AdjustmentReason describes a deterministic change made to a client request.
type AdjustmentReason string

const (
	LeaseDefaulted                  AdjustmentReason = "requested lease omitted; using default lease"
	LeaseRounded                    AdjustmentReason = "requested lease rounded to a whole number of seconds"
	LeaseRaisedToMinimum            AdjustmentReason = "requested lease below minimum; using minimum lease"
	LeaseLoweredToMaximum           AdjustmentReason = "requested lease above maximum; using maximum lease"
	InfiniteLeaseLoweredToMaximum   AdjustmentReason = "infinite lease requested but not allowed; using maximum lease"
	T1Rounded                       AdjustmentReason = "requested T1 rounded to a whole number of seconds"
	T2Rounded                       AdjustmentReason = "requested T2 rounded to a whole number of seconds"
	T1SelectedFromLease             AdjustmentReason = "T1 omitted; selecting T1 from the planned lease"
	T2SelectedFromLease             AdjustmentReason = "T2 omitted; selecting T2 from the planned lease"
	FiniteT1IgnoredForInfiniteLease AdjustmentReason = "finite T1 requested for an infinite lease; using infinite T1"
	FiniteT2IgnoredForInfiniteLease AdjustmentReason = "finite T2 requested for an infinite lease; using infinite T2"
)

// LeaseConfig contains the server's lease policy.
type LeaseConfig struct {
	MinLease     time.Duration
	MaxLease     time.Duration
	DefaultLease time.Duration
}

// LeaseRequest contains the client's requested lease and optional timers. A
// nil pointer or a zero duration means that the server should select that
// value.
type LeaseRequest struct {
	Lease *time.Duration
	T1    *time.Duration
	T2    *time.Duration
}

// LeasePlan contains durations suitable for an IA option and the changes made
// to the client request. Infinite is true when Lease, T1, and T2 represent the
// DHCPv6 infinite-duration wire value.
type LeasePlan struct {
	Lease       time.Duration
	T1          time.Duration
	T2          time.Duration
	Infinite    bool
	Adjustments []AdjustmentReason
}

// PlanLease deterministically plans a finite or infinite lease and its T1 and
// T2 timers. It does not read the system clock.
func PlanLease(config LeaseConfig, request LeaseRequest) (LeasePlan, error) {
	minLease, err := finiteConfigSeconds(config.MinLease, "minimum lease")
	if err != nil {
		return LeasePlan{}, err
	}

	maxInfinite := config.MaxLease == InfiniteDuration
	var maxLease int64
	if !maxInfinite {
		maxLease, err = finiteConfigSeconds(config.MaxLease, "maximum lease")
		if err != nil {
			return LeasePlan{}, err
		}
		if maxLease < 3 {
			return LeasePlan{}, errors.New("maximum lease must be at least 3 seconds to schedule distinct T1 and T2 timers")
		}
		if maxLease < minLease {
			return LeasePlan{}, errors.New("maximum lease must be at least the minimum lease")
		}
	}

	defaultInfinite := config.DefaultLease == InfiniteDuration
	var defaultLease int64
	if !defaultInfinite {
		defaultLease, err = finiteConfigSeconds(config.DefaultLease, "default lease")
		if err != nil {
			return LeasePlan{}, err
		}
		if defaultLease < 3 {
			return LeasePlan{}, errors.New("default finite lease must be at least 3 seconds")
		}
		if defaultLease < minLease || (!maxInfinite && defaultLease > maxLease) {
			return LeasePlan{}, errors.New("default lease must be within the configured lease range")
		}
	} else if !maxInfinite {
		return LeasePlan{}, errors.New("default lease cannot be infinite when maximum lease is finite")
	}

	var adjustments []AdjustmentReason
	leaseInfinite, lease, err := plannedLeaseSeconds(request, minLease, maxLease, defaultLease, maxInfinite, defaultInfinite, &adjustments)
	if err != nil {
		return LeasePlan{}, err
	}

	if leaseInfinite {
		if err := planInfiniteTimers(request, &adjustments); err != nil {
			return LeasePlan{}, err
		}
		return LeasePlan{
			Lease:       infiniteWireLease,
			T1:          infiniteWireLease,
			T2:          infiniteWireLease,
			Infinite:    true,
			Adjustments: adjustments,
		}, nil
	}

	if lease < 3 {
		return LeasePlan{}, fmt.Errorf("lease of %d second(s) is too short; finite leases require 0 < T1 < T2 < lease", lease)
	}

	t1, t2, err := planFiniteTimers(request, lease, &adjustments)
	if err != nil {
		return LeasePlan{}, err
	}

	return LeasePlan{
		Lease:       time.Duration(lease) * time.Second,
		T1:          time.Duration(t1) * time.Second,
		T2:          time.Duration(t2) * time.Second,
		Adjustments: adjustments,
	}, nil
}

func plannedLeaseSeconds(request LeaseRequest, minLease, maxLease, defaultLease int64, maxInfinite, defaultInfinite bool, adjustments *[]AdjustmentReason) (bool, int64, error) {
	if request.Lease == nil || *request.Lease == 0 {
		*adjustments = append(*adjustments, LeaseDefaulted)
		return defaultInfinite, defaultLease, nil
	}

	if *request.Lease == InfiniteDuration {
		if maxInfinite {
			return true, 0, nil
		}
		*adjustments = append(*adjustments, InfiniteLeaseLoweredToMaximum)
		return false, maxLease, nil
	}

	requestedLease, rounded, err := finiteRequestSeconds(*request.Lease, "requested lease")
	if err != nil {
		return false, 0, err
	}
	if rounded {
		*adjustments = append(*adjustments, LeaseRounded)
	}

	if requestedLease < minLease {
		*adjustments = append(*adjustments, LeaseRaisedToMinimum)
		return false, minLease, nil
	}
	if !maxInfinite && requestedLease > maxLease {
		*adjustments = append(*adjustments, LeaseLoweredToMaximum)
		return false, maxLease, nil
	}
	return false, requestedLease, nil
}

func planFiniteTimers(request LeaseRequest, lease int64, adjustments *[]AdjustmentReason) (int64, int64, error) {
	t1Present, t1, t1Rounded, err := optionalTimer(request.T1, "T1")
	if err != nil {
		return 0, 0, err
	}
	t2Present, t2, t2Rounded, err := optionalTimer(request.T2, "T2")
	if err != nil {
		return 0, 0, err
	}
	if t1Rounded {
		*adjustments = append(*adjustments, T1Rounded)
	}
	if t2Rounded {
		*adjustments = append(*adjustments, T2Rounded)
	}

	if t1Present && t2Present {
		if t1 >= t2 || t2 >= lease {
			return 0, 0, fmt.Errorf("finite timers require 0 < T1 < T2 < lease; got T1=%ds T2=%ds lease=%ds", t1, t2, lease)
		}
		return t1, t2, nil
	}

	if t1Present {
		if t1 >= lease-1 {
			return 0, 0, fmt.Errorf("T1=%ds leaves no whole second for T2 before a lease of %ds", t1, lease)
		}
		t2 = defaultT2(lease)
		if t2 <= t1 {
			t2 = lease - 1
		}
		*adjustments = append(*adjustments, T2SelectedFromLease)
		return t1, t2, nil
	}

	if t2Present {
		if t2 >= lease || t2 < 2 {
			return 0, 0, fmt.Errorf("T2=%ds leaves no positive whole second for T1 in a lease of %ds", t2, lease)
		}
		t1 = defaultT1(lease)
		if t1 >= t2 {
			t1 = t2 / 2
		}
		*adjustments = append(*adjustments, T1SelectedFromLease)
		return t1, t2, nil
	}

	return defaultT1(lease), defaultT2(lease), nil
}

func planInfiniteTimers(request LeaseRequest, adjustments *[]AdjustmentReason) error {
	if request.T1 != nil && *request.T1 != 0 {
		if *request.T1 != InfiniteDuration {
			if _, _, err := finiteRequestSeconds(*request.T1, "requested T1"); err != nil {
				return err
			}
			*adjustments = append(*adjustments, FiniteT1IgnoredForInfiniteLease)
		}
	}
	if request.T2 != nil && *request.T2 != 0 {
		if *request.T2 != InfiniteDuration {
			if _, _, err := finiteRequestSeconds(*request.T2, "requested T2"); err != nil {
				return err
			}
			*adjustments = append(*adjustments, FiniteT2IgnoredForInfiniteLease)
		}
	}
	return nil
}

func optionalTimer(value *time.Duration, name string) (bool, int64, bool, error) {
	if value == nil || *value == 0 {
		return false, 0, false, nil
	}
	if *value == InfiniteDuration {
		return false, 0, false, fmt.Errorf("%s cannot be infinite for a finite lease", name)
	}
	seconds, rounded, err := finiteRequestSeconds(*value, name)
	if err != nil {
		return false, 0, false, err
	}
	if seconds == 0 {
		return false, 0, false, fmt.Errorf("%s must be positive after rounding to whole seconds", name)
	}
	return true, seconds, rounded, nil
}

func defaultT1(lease int64) int64 {
	// RFC 8415 suggests a renewal time of one half of the IA's lifetime.
	return lease / 2
}

func defaultT2(lease int64) int64 {
	// Four fifths is a commonly used rebinding time and always leaves room
	// after T1 for the minimum finite lease of three seconds.
	return 4 * lease / 5
}

func finiteConfigSeconds(value time.Duration, name string) (int64, error) {
	if value <= 0 {
		return 0, fmt.Errorf("%s must be positive", name)
	}
	if value%time.Second != 0 {
		return 0, fmt.Errorf("%s must be a whole number of seconds", name)
	}
	seconds := int64(value / time.Second)
	if seconds > maxFiniteLeaseSeconds {
		return 0, fmt.Errorf("%s exceeds the maximum finite DHCPv6 duration; use InfiniteDuration for infinity", name)
	}
	return seconds, nil
}

func finiteRequestSeconds(value time.Duration, name string) (int64, bool, error) {
	if value < 0 {
		return 0, false, fmt.Errorf("%s must be positive or InfiniteDuration", name)
	}
	rounded := value.Round(time.Second)
	if rounded <= 0 {
		return 0, value != rounded, nil
	}
	seconds := int64(rounded / time.Second)
	if seconds > maxFiniteLeaseSeconds {
		return 0, false, fmt.Errorf("%s exceeds the maximum finite DHCPv6 duration; use InfiniteDuration for infinity", name)
	}
	return seconds, value != rounded, nil
}
