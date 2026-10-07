package server6

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func durationPtr(value time.Duration) *time.Duration {
	return &value
}

func TestPlanLease(t *testing.T) {
	tests := []struct {
		name    string
		config  LeaseConfig
		request LeaseRequest
		want    LeasePlan
	}{
		{
			name: "default finite lease selects timers",
			config: LeaseConfig{
				MinLease:     10 * time.Second,
				MaxLease:     100 * time.Second,
				DefaultLease: 20 * time.Second,
			},
			request: LeaseRequest{},
			want: LeasePlan{
				Lease:       20 * time.Second,
				T1:          10 * time.Second,
				T2:          16 * time.Second,
				Adjustments: []AdjustmentReason{LeaseDefaulted},
			},
		},
		{
			name: "request below minimum is raised",
			config: LeaseConfig{
				MinLease:     10 * time.Second,
				MaxLease:     100 * time.Second,
				DefaultLease: 20 * time.Second,
			},
			request: LeaseRequest{
				Lease: durationPtr(5 * time.Second),
			},
			want: LeasePlan{
				Lease:       10 * time.Second,
				T1:          5 * time.Second,
				T2:          8 * time.Second,
				Adjustments: []AdjustmentReason{LeaseRaisedToMinimum},
			},
		},
		{
			name: "sub-second request rounds before being raised to minimum",
			config: LeaseConfig{
				MinLease:     10 * time.Second,
				MaxLease:     100 * time.Second,
				DefaultLease: 20 * time.Second,
			},
			request: LeaseRequest{
				Lease: durationPtr(499 * time.Millisecond),
			},
			want: LeasePlan{
				Lease:       10 * time.Second,
				T1:          5 * time.Second,
				T2:          8 * time.Second,
				Adjustments: []AdjustmentReason{LeaseRounded, LeaseRaisedToMinimum},
			},
		},
		{
			name: "request above maximum is lowered",
			config: LeaseConfig{
				MinLease:     10 * time.Second,
				MaxLease:     80 * time.Second,
				DefaultLease: 20 * time.Second,
			},
			request: LeaseRequest{
				Lease: durationPtr(100 * time.Second),
				T1:    durationPtr(25 * time.Second),
				T2:    durationPtr(50 * time.Second),
			},
			want: LeasePlan{
				Lease:       80 * time.Second,
				T1:          25 * time.Second,
				T2:          50 * time.Second,
				Adjustments: []AdjustmentReason{LeaseLoweredToMaximum},
			},
		},
		{
			name: "values at boundaries are accepted",
			config: LeaseConfig{
				MinLease:     3 * time.Second,
				MaxLease:     100 * time.Second,
				DefaultLease: 20 * time.Second,
			},
			request: LeaseRequest{
				Lease: durationPtr(3 * time.Second),
				T1:    durationPtr(time.Second),
				T2:    durationPtr(2 * time.Second),
			},
			want: LeasePlan{
				Lease: 3 * time.Second,
				T1:    time.Second,
				T2:    2 * time.Second,
			},
		},
		{
			name: "fractional values round without making timers equal",
			config: LeaseConfig{
				MinLease:     10 * time.Second,
				MaxLease:     100 * time.Second,
				DefaultLease: 20 * time.Second,
			},
			request: LeaseRequest{
				Lease: durationPtr(20*time.Second + 499*time.Millisecond),
				T1:    durationPtr(time.Second + 499*time.Millisecond),
				T2:    durationPtr(2*time.Second + 499*time.Millisecond),
			},
			want: LeasePlan{
				Lease:       20 * time.Second,
				T1:          time.Second,
				T2:          2 * time.Second,
				Adjustments: []AdjustmentReason{LeaseRounded, T1Rounded, T2Rounded},
			},
		},
		{
			name: "only T1 selects T2 from lease",
			config: LeaseConfig{
				MinLease:     3 * time.Second,
				MaxLease:     100 * time.Second,
				DefaultLease: 20 * time.Second,
			},
			request: LeaseRequest{
				Lease: durationPtr(8 * time.Second),
				T1:    durationPtr(2 * time.Second),
			},
			want: LeasePlan{
				Lease:       8 * time.Second,
				T1:          2 * time.Second,
				T2:          6 * time.Second,
				Adjustments: []AdjustmentReason{T2SelectedFromLease},
			},
		},
		{
			name: "only T2 selects T1 from lease",
			config: LeaseConfig{
				MinLease:     3 * time.Second,
				MaxLease:     100 * time.Second,
				DefaultLease: 20 * time.Second,
			},
			request: LeaseRequest{
				Lease: durationPtr(8 * time.Second),
				T2:    durationPtr(3 * time.Second),
			},
			want: LeasePlan{
				Lease:       8 * time.Second,
				T1:          time.Second,
				T2:          3 * time.Second,
				Adjustments: []AdjustmentReason{T1SelectedFromLease},
			},
		},
		{
			name: "maximum finite lease is encoded normally",
			config: LeaseConfig{
				MinLease:     3 * time.Second,
				MaxLease:     time.Duration(math.MaxUint32-1) * time.Second,
				DefaultLease: 20 * time.Second,
			},
			request: LeaseRequest{
				Lease: durationPtr(time.Duration(math.MaxUint32-1) * time.Second),
			},
			want: LeasePlan{
				Lease: time.Duration(math.MaxUint32-1) * time.Second,
				T1:    time.Duration((math.MaxUint32-1)/2) * time.Second,
				T2:    time.Duration(4*(math.MaxUint32-1)/5) * time.Second,
			},
		},
		{
			name: "infinite allowed",
			config: LeaseConfig{
				MinLease:     3 * time.Second,
				MaxLease:     InfiniteDuration,
				DefaultLease: InfiniteDuration,
			},
			request: LeaseRequest{
				Lease: durationPtr(InfiniteDuration),
				T1:    durationPtr(10 * time.Second),
				T2:    durationPtr(InfiniteDuration),
			},
			want: LeasePlan{
				Lease:       time.Duration(math.MaxUint32) * time.Second,
				T1:          time.Duration(math.MaxUint32) * time.Second,
				T2:          time.Duration(math.MaxUint32) * time.Second,
				Infinite:    true,
				Adjustments: []AdjustmentReason{FiniteT1IgnoredForInfiniteLease},
			},
		},
		{
			name: "infinite request lowered to finite maximum",
			config: LeaseConfig{
				MinLease:     3 * time.Second,
				MaxLease:     100 * time.Second,
				DefaultLease: 20 * time.Second,
			},
			request: LeaseRequest{
				Lease: durationPtr(InfiniteDuration),
			},
			want: LeasePlan{
				Lease:       100 * time.Second,
				T1:          50 * time.Second,
				T2:          80 * time.Second,
				Adjustments: []AdjustmentReason{InfiniteLeaseLoweredToMaximum},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PlanLease(tt.config, tt.request)
			if err != nil {
				t.Fatalf("PlanLease returned unexpected error: %v", err)
			}
			require.Equal(t, tt.want, got)
		})
	}
}

func TestPlanLeaseErrors(t *testing.T) {
	maxFiniteWireMinusOne := time.Duration(math.MaxUint32-1) * time.Second
	maxFiniteWire := time.Duration(math.MaxUint32) * time.Second

	tests := []struct {
		name    string
		config  LeaseConfig
		request LeaseRequest
	}{
		{
			name: "minimum lease too short for timers",
			config: LeaseConfig{
				MinLease:     time.Second,
				MaxLease:     100 * time.Second,
				DefaultLease: 20 * time.Second,
			},
			request: LeaseRequest{
				Lease: durationPtr(2 * time.Second),
			},
		},
		{
			name: "two-second lease cannot fit distinct timers",
			config: LeaseConfig{
				MinLease:     2 * time.Second,
				MaxLease:     100 * time.Second,
				DefaultLease: 2 * time.Second,
			},
			request: LeaseRequest{},
		},
		{
			name: "timers are equal after rounding",
			config: LeaseConfig{
				MinLease:     3 * time.Second,
				MaxLease:     100 * time.Second,
				DefaultLease: 20 * time.Second,
			},
			request: LeaseRequest{
				Lease: durationPtr(10 * time.Second),
				T1:    durationPtr(time.Second + 499*time.Millisecond),
				T2:    durationPtr(time.Second + 499*time.Millisecond),
			},
		},
		{
			name: "T2 is not before lease after rounding",
			config: LeaseConfig{
				MinLease:     3 * time.Second,
				MaxLease:     100 * time.Second,
				DefaultLease: 20 * time.Second,
			},
			request: LeaseRequest{
				Lease: durationPtr(10 * time.Second),
				T1:    durationPtr(time.Second),
				T2:    durationPtr(10*time.Second - time.Nanosecond),
			},
		},
		{
			name: "T1 rounds to zero",
			config: LeaseConfig{
				MinLease:     3 * time.Second,
				MaxLease:     100 * time.Second,
				DefaultLease: 20 * time.Second,
			},
			request: LeaseRequest{
				Lease: durationPtr(10 * time.Second),
				T1:    durationPtr(499 * time.Millisecond),
				T2:    durationPtr(5 * time.Second),
			},
		},
		{
			name: "only T1 leaves no second for T2",
			config: LeaseConfig{
				MinLease:     3 * time.Second,
				MaxLease:     100 * time.Second,
				DefaultLease: 20 * time.Second,
			},
			request: LeaseRequest{
				Lease: durationPtr(3 * time.Second),
				T1:    durationPtr(2 * time.Second),
			},
		},
		{
			name: "only T2 leaves no positive T1",
			config: LeaseConfig{
				MinLease:     3 * time.Second,
				MaxLease:     100 * time.Second,
				DefaultLease: 20 * time.Second,
			},
			request: LeaseRequest{
				Lease: durationPtr(3 * time.Second),
				T2:    durationPtr(time.Second),
			},
		},
		{
			name: "finite lease rejects infinite T1",
			config: LeaseConfig{
				MinLease:     3 * time.Second,
				MaxLease:     InfiniteDuration,
				DefaultLease: 20 * time.Second,
			},
			request: LeaseRequest{
				Lease: durationPtr(10 * time.Second),
				T1:    durationPtr(InfiniteDuration),
				T2:    durationPtr(5 * time.Second),
			},
		},
		{
			name: "finite lease requests must not use wire infinity",
			config: LeaseConfig{
				MinLease:     3 * time.Second,
				MaxLease:     InfiniteDuration,
				DefaultLease: 20 * time.Second,
			},
			request: LeaseRequest{
				Lease: durationPtr(maxFiniteWire),
			},
		},
		{
			name: "finite configuration must not use wire infinity",
			config: LeaseConfig{
				MinLease:     3 * time.Second,
				MaxLease:     maxFiniteWire,
				DefaultLease: 20 * time.Second,
			},
		},
		{
			name: "default below minimum",
			config: LeaseConfig{
				MinLease:     10 * time.Second,
				MaxLease:     100 * time.Second,
				DefaultLease: 5 * time.Second,
			},
		},
		{
			name: "maximum below minimum",
			config: LeaseConfig{
				MinLease:     100 * time.Second,
				MaxLease:     10 * time.Second,
				DefaultLease: 20 * time.Second,
			},
		},
		{
			name: "fractional server configuration is invalid",
			config: LeaseConfig{
				MinLease:     3*time.Second + time.Nanosecond,
				MaxLease:     100 * time.Second,
				DefaultLease: 20 * time.Second,
			},
		},
		{
			name: "configuration larger than finite wire range",
			config: LeaseConfig{
				MinLease:     3 * time.Second,
				MaxLease:     maxFiniteWireMinusOne + time.Second,
				DefaultLease: 20 * time.Second,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := PlanLease(tt.config, tt.request); err == nil {
				t.Fatal("PlanLease returned nil error, want non-nil")
			}
		})
	}
}
