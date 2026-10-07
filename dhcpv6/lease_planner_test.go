package dhcpv6

import (
	"errors"
	"math"
	"reflect"
	"testing"
	"time"
)

func durationPtr(d time.Duration) *time.Duration {
	return &d
}

func TestPlanLeaseFiniteDefaults(t *testing.T) {
	config := LeasePlannerConfig{
		MinLease:     3 * time.Second,
		MaxLease:     100 * time.Second,
		DefaultLease: 10 * time.Second,
	}

	plan, err := PlanLease(config, LeaseRequest{})
	if err != nil {
		t.Fatalf("PlanLease returned unexpected error: %v", err)
	}

	want := LeasePlan{
		Lease: 10 * time.Second,
		T1:    5 * time.Second,
		T2:    8 * time.Second,
		Adjustments: []LeaseAdjustment{
			{Field: LeaseFieldLease, Reason: LeaseAdjustmentDefault, To: 10 * time.Second},
			{Field: LeaseFieldT1, Reason: LeaseAdjustmentDefault, To: 5 * time.Second},
			{Field: LeaseFieldT2, Reason: LeaseAdjustmentDefault, To: 8 * time.Second},
		},
	}
	if !reflect.DeepEqual(plan, want) {
		t.Fatalf("PlanLease = %#v, want %#v", plan, want)
	}
	if plan.LeaseSeconds() != 10 || plan.T1Seconds() != 5 || plan.T2Seconds() != 8 {
		t.Fatalf("wire seconds = (%d, %d, %d), want (10, 5, 8)", plan.LeaseSeconds(), plan.T1Seconds(), plan.T2Seconds())
	}
}

func TestPlanLeaseBoundarySeconds(t *testing.T) {
	config := LeasePlannerConfig{
		MinLease:     3 * time.Second,
		MaxLease:     10 * time.Second,
		DefaultLease: 10 * time.Second,
	}

	plan, err := PlanLease(config, LeaseRequest{
		Lease: durationPtr(3 * time.Second),
		T1:    durationPtr(1 * time.Second),
		T2:    durationPtr(2 * time.Second),
	})
	if err != nil {
		t.Fatalf("PlanLease returned unexpected error: %v", err)
	}
	if plan.Lease != 3*time.Second || plan.T1 != time.Second || plan.T2 != 2*time.Second {
		t.Fatalf("plan = %v/%v/%v, want 1s/2s/3s", plan.T1, plan.T2, plan.Lease)
	}
}

func TestPlanLeaseAdjustsBelowMinimum(t *testing.T) {
	config := LeasePlannerConfig{
		MinLease:     10 * time.Second,
		MaxLease:     100 * time.Second,
		DefaultLease: 20 * time.Second,
	}

	plan, err := PlanLease(config, LeaseRequest{
		Lease: durationPtr(4 * time.Second),
		T1:    durationPtr(4 * time.Second),
		T2:    durationPtr(7 * time.Second),
	})
	if err != nil {
		t.Fatalf("PlanLease returned unexpected error: %v", err)
	}
	if plan.Lease != 10*time.Second || plan.T1 != 4*time.Second || plan.T2 != 7*time.Second {
		t.Fatalf("plan = %v/%v/%v, want 4s/7s/10s", plan.T1, plan.T2, plan.Lease)
	}
	if plan.Adjustments[0] != (LeaseAdjustment{
		Field:  LeaseFieldLease,
		Reason: LeaseAdjustmentBelowMinimum,
		From:   4 * time.Second,
		To:     10 * time.Second,
	}) {
		t.Fatalf("first adjustment = %#v, want below minimum", plan.Adjustments[0])
	}
}

func TestPlanLeaseAdjustsAboveMaximum(t *testing.T) {
	maximumLease := time.Duration(math.MaxUint32-1) * time.Second
	config := LeasePlannerConfig{
		MinLease:     3 * time.Second,
		MaxLease:     maximumLease,
		DefaultLease: 10 * time.Second,
	}

	plan, err := PlanLease(config, LeaseRequest{
		Lease: durationPtr(time.Duration(math.MaxUint32) * time.Second),
	})
	if err != nil {
		t.Fatalf("PlanLease returned unexpected error: %v", err)
	}
	if plan.Infinite {
		t.Fatal("finite uint32 sentinel duration was incorrectly interpreted as infinite")
	}
	if plan.Lease != maximumLease || plan.LeaseSeconds() != math.MaxUint32-1 {
		t.Fatalf("lease = %v (%d), want maximum finite lease", plan.Lease, plan.LeaseSeconds())
	}
	if plan.Adjustments[0].Reason != LeaseAdjustmentAboveMaximum {
		t.Fatalf("adjustment reason = %v, want above maximum", plan.Adjustments[0].Reason)
	}
}

func TestPlanLeaseMaximumFiniteBoundary(t *testing.T) {
	maximumLease := MaxPlannedFiniteLease
	config := LeasePlannerConfig{
		MinLease:     3 * time.Second,
		MaxLease:     maximumLease,
		DefaultLease: 10 * time.Second,
	}

	plan, err := PlanLease(config, LeaseRequest{Lease: durationPtr(maximumLease)})
	if err != nil {
		t.Fatalf("PlanLease returned unexpected error: %v", err)
	}
	if plan.Infinite || plan.Lease != maximumLease || plan.LeaseSeconds() != math.MaxUint32-1 {
		t.Fatalf("plan = %#v, want finite maximum lease", plan)
	}
}

func TestPlanLeaseRoundsRequestedLeaseToSeconds(t *testing.T) {
	config := LeasePlannerConfig{
		MinLease:     3 * time.Second,
		MaxLease:     10 * time.Second,
		DefaultLease: 10 * time.Second,
	}

	plan, err := PlanLease(config, LeaseRequest{Lease: durationPtr(9500 * time.Millisecond)})
	if err != nil {
		t.Fatalf("PlanLease returned unexpected error: %v", err)
	}
	if plan.Lease != 10*time.Second {
		t.Fatalf("lease = %v, want 10s after rounding", plan.Lease)
	}
}

func TestPlanLeaseOnlyOneTimer(t *testing.T) {
	config := LeasePlannerConfig{
		MinLease:     3 * time.Second,
		MaxLease:     10 * time.Second,
		DefaultLease: 10 * time.Second,
	}

	t.Run("only T1", func(t *testing.T) {
		plan, err := PlanLease(config, LeaseRequest{
			Lease: durationPtr(10 * time.Second),
			T1:    durationPtr(8 * time.Second),
		})
		if err != nil {
			t.Fatalf("PlanLease returned unexpected error: %v", err)
		}
		if plan.T1 != 8*time.Second || plan.T2 != 9*time.Second {
			t.Fatalf("timers = %v/%v, want 8s/9s", plan.T1, plan.T2)
		}
	})

	t.Run("only T2", func(t *testing.T) {
		plan, err := PlanLease(config, LeaseRequest{
			Lease: durationPtr(3 * time.Second),
			T2:    durationPtr(2 * time.Second),
		})
		if err != nil {
			t.Fatalf("PlanLease returned unexpected error: %v", err)
		}
		if plan.T1 != time.Second || plan.T2 != 2*time.Second {
			t.Fatalf("timers = %v/%v, want 1s/2s", plan.T1, plan.T2)
		}
	})
}

func TestPlanLeaseInfinite(t *testing.T) {
	config := LeasePlannerConfig{
		MinLease:      3 * time.Second,
		MaxLease:      100 * time.Second,
		DefaultLease:  10 * time.Second,
		AllowInfinite: true,
	}

	plan, err := PlanLease(config, LeaseRequest{
		Infinite: true,
		T1:       durationPtr(50 * time.Second),
		T2:       durationPtr(80 * time.Second),
	})
	if err != nil {
		t.Fatalf("PlanLease returned unexpected error: %v", err)
	}
	if !plan.Infinite || plan.Lease != 0 || plan.T1 != 0 || plan.T2 != 0 {
		t.Fatalf("infinite plan = %#v, want zero durations and Infinite=true", plan)
	}
	if plan.LeaseSeconds() != math.MaxUint32 {
		t.Fatalf("lease seconds = %d, want infinite sentinel", plan.LeaseSeconds())
	}
	if len(plan.Adjustments) != 2 ||
		plan.Adjustments[0].Reason != LeaseAdjustmentInfiniteLease ||
		plan.Adjustments[1].Reason != LeaseAdjustmentInfiniteLease {
		t.Fatalf("adjustments = %#v, want two infinite lease adjustments", plan.Adjustments)
	}
}

func TestPlanLeaseDefaultInfinite(t *testing.T) {
	config := LeasePlannerConfig{
		MinLease:        3 * time.Second,
		MaxLease:        100 * time.Second,
		DefaultLease:    10 * time.Second,
		DefaultInfinite: true,
		AllowInfinite:   true,
	}

	plan, err := PlanLease(config, LeaseRequest{})
	if err != nil {
		t.Fatalf("PlanLease returned unexpected error: %v", err)
	}
	if !plan.Infinite || plan.LeaseSeconds() != math.MaxUint32 {
		t.Fatalf("plan = %#v, want infinite default", plan)
	}
}

func TestPlanLeaseErrors(t *testing.T) {
	validConfig := LeasePlannerConfig{
		MinLease:     3 * time.Second,
		MaxLease:     10 * time.Second,
		DefaultLease: 10 * time.Second,
	}

	tests := []struct {
		name    string
		config  LeasePlannerConfig
		request LeaseRequest
		wantErr error
	}{
		{
			name: "minimum lease too short",
			config: LeasePlannerConfig{
				MinLease:     2 * time.Second,
				MaxLease:     10 * time.Second,
				DefaultLease: 10 * time.Second,
			},
			wantErr: ErrInvalidLeaseConfiguration,
		},
		{
			name: "maximum finite lease uses infinite wire value",
			config: LeasePlannerConfig{
				MinLease:     3 * time.Second,
				MaxLease:     time.Duration(math.MaxUint32) * time.Second,
				DefaultLease: 10 * time.Second,
			},
			wantErr: ErrInvalidLeaseConfiguration,
		},
		{
			name: "default outside range",
			config: LeasePlannerConfig{
				MinLease:     10 * time.Second,
				MaxLease:     20 * time.Second,
				DefaultLease: 30 * time.Second,
			},
			wantErr: ErrInvalidLeaseConfiguration,
		},
		{
			name:   "infinite lease disallowed",
			config: validConfig,
			request: LeaseRequest{
				Infinite: true,
			},
			wantErr: ErrInvalidLeaseRequest,
		},
		{
			name:    "explicit zero lease",
			config:  validConfig,
			request: LeaseRequest{Lease: durationPtr(0)},
			wantErr: ErrInvalidLeaseRequest,
		},
		{
			name:    "negative lease",
			config:  validConfig,
			request: LeaseRequest{Lease: durationPtr(-time.Second)},
			wantErr: ErrInvalidLeaseRequest,
		},
		{
			name:   "rounded timers become equal",
			config: validConfig,
			request: LeaseRequest{
				Lease: durationPtr(10 * time.Second),
				T1:    durationPtr(1500 * time.Millisecond),
				T2:    durationPtr(1500 * time.Millisecond),
			},
			wantErr: ErrInvalidLeaseRequest,
		},
		{
			name:   "T1 rounds to zero",
			config: validConfig,
			request: LeaseRequest{
				Lease: durationPtr(10 * time.Second),
				T1:    durationPtr(499 * time.Millisecond),
				T2:    durationPtr(2 * time.Second),
			},
			wantErr: ErrInvalidLeaseRequest,
		},
		{
			name:   "only T1 leaves no room for T2",
			config: validConfig,
			request: LeaseRequest{
				Lease: durationPtr(10 * time.Second),
				T1:    durationPtr(9 * time.Second),
			},
			wantErr: ErrInvalidLeaseRequest,
		},
		{
			name:   "only T2 leaves no room for T1",
			config: validConfig,
			request: LeaseRequest{
				Lease: durationPtr(3 * time.Second),
				T2:    durationPtr(time.Second),
			},
			wantErr: ErrInvalidLeaseRequest,
		},
		{
			name:   "T2 equals lease",
			config: validConfig,
			request: LeaseRequest{
				Lease: durationPtr(10 * time.Second),
				T1:    durationPtr(5 * time.Second),
				T2:    durationPtr(10 * time.Second),
			},
			wantErr: ErrInvalidLeaseRequest,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := PlanLease(test.config, test.request)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("PlanLease error = %v, want error containing %v", err, test.wantErr)
			}
		})
	}
}
