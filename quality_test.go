package ntp

import (
	"errors"
	"math"
	"reflect"
	"testing"
	"time"
)

func makeQualityTestResponse() Response {
	now := time.Now()
	return Response{
		ClockOffset:    50 * time.Millisecond,
		Time:           now,
		RTT:            100 * time.Millisecond,
		Precision:      500 * time.Microsecond,
		Version:        4,
		Stratum:        1,
		ReferenceID:    refID,
		ReferenceTime:  now,
		RootDelay:      1 * time.Millisecond,
		RootDispersion: 1 * time.Millisecond,
		RootDistance:   2 * time.Millisecond,
		Leap:           LeapNoWarning,
		MinError:       1 * time.Millisecond,
		Poll:           1 * time.Second,
		Flags:          0,
		Timestamps: ProtocolTimestamps{
			ClientXmit: now.Add(-100 * time.Millisecond),
			ServerRecv: now.Add(-50 * time.Millisecond),
			ServerXmit: now.Add(-50 * time.Millisecond),
			ClientRecv: now,
		},
	}
}

func findCheck(t *testing.T, report QualityReport, id QualityCheckID) QualityCheck {
	t.Helper()
	for _, c := range report.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("quality check %s not found in report", id)
	return QualityCheck{}
}

// The same response must yield different quality conclusions under two
// different policies while the protocol conclusion remains identical.
func TestOfflineQualityPolicyTwoPoliciesSameProtocol(t *testing.T) {
	r := makeQualityTestResponse()
	snapshot := r

	strict := QualityPolicy{
		MaxRTT:         10 * time.Millisecond, // r.RTT is 100ms
		MaxClockOffset: 10 * time.Millisecond, // r.ClockOffset is 50ms
		Now:            r.Timestamps.ClientRecv,
	}
	lenient := QualityPolicy{
		MaxRTT:         1 * time.Second,
		MaxClockOffset: 1 * time.Second,
		Now:            r.Timestamps.ClientRecv,
	}

	strictReport := r.ValidateQuality(strict)
	lenientReport := r.ValidateQuality(lenient)

	// Same protocol conclusion under both policies.
	if !strictReport.ProtocolValid() || !lenientReport.ProtocolValid() {
		t.Errorf("expected protocol-valid reports, got %v and %v",
			strictReport.ProtocolErr, lenientReport.ProtocolErr)
	}

	// Different quality conclusions.
	if strictReport.QualityValid() {
		t.Error("expected strict policy to fail quality checks")
	}
	if !lenientReport.QualityValid() {
		t.Error("expected lenient policy to pass quality checks")
	}
	if strictReport.Valid() {
		t.Error("expected strict report to be invalid overall")
	}
	if !lenientReport.Valid() {
		t.Error("expected lenient report to be valid overall")
	}

	// The strict report itemizes exactly the failing checks.
	failed := strictReport.FailedChecks()
	if len(failed) != 2 {
		t.Fatalf("expected 2 failed checks, got %d", len(failed))
	}
	if failed[0].ID != QualityCheckRTT || failed[1].ID != QualityCheckClockOffset {
		t.Errorf("unexpected failed checks: %v", failed)
	}

	// Validate remains compatible and unaffected by the quality policy.
	if err := r.Validate(); err != nil {
		t.Errorf("Validate() returned unexpected error: %v", err)
	}

	// Reporting must not modify the response.
	if !reflect.DeepEqual(snapshot, r) {
		t.Error("ValidateQuality modified the response")
	}
}

// Protocol failures such as authentication errors must not be re-accepted
// by relaxing quality thresholds.
func TestOfflineQualityPolicyProtocolFailureNotOverridable(t *testing.T) {
	r := makeQualityTestResponse()
	r.authErr = errors.New("authentication error")

	// An empty policy imposes no quality limits at all.
	report := r.ValidateQuality(QualityPolicy{})

	if report.ProtocolValid() {
		t.Error("expected protocol-invalid report")
	}
	if !report.QualityValid() {
		t.Error("expected quality-valid report with no limits")
	}
	if report.Valid() {
		t.Error("protocol failure must not be overridden by relaxed quality limits")
	}
	if len(report.Checks) != 0 {
		t.Errorf("expected no checks, got %d", len(report.Checks))
	}
}

// Limits are inclusive maxima: a value equal to its limit passes, a value
// one nanosecond beyond fails.
func TestOfflineQualityPolicyBoundaries(t *testing.T) {
	r := makeQualityTestResponse()
	now := r.Timestamps.ClientRecv

	atLimit := QualityPolicy{
		MaxRTT:          r.RTT,
		MaxRootDistance: r.RootDistance,
		MaxClockOffset:  r.ClockOffset,
		Now:             now,
	}
	report := r.ValidateQuality(atLimit)
	if !report.Valid() {
		t.Errorf("expected values at the limit to pass: %+v", report.FailedChecks())
	}

	beyondLimit := QualityPolicy{
		MaxRTT:          r.RTT - 1,
		MaxRootDistance: r.RootDistance - 1,
		MaxClockOffset:  r.ClockOffset - 1,
		Now:             now,
	}
	report = r.ValidateQuality(beyondLimit)
	if report.QualityValid() {
		t.Error("expected values beyond the limit to fail")
	}
	if len(report.FailedChecks()) != 3 {
		t.Errorf("expected 3 failed checks, got %d", len(report.FailedChecks()))
	}
}

// Negative duration limits make the policy invalid.
func TestOfflineQualityPolicyNegativeLimits(t *testing.T) {
	r := makeQualityTestResponse()

	policies := []QualityPolicy{
		{MaxRTT: -1},
		{MaxRootDistance: -1},
		{MaxClockOffset: -1},
		{MaxAge: -1},
	}
	for _, p := range policies {
		report := r.ValidateQuality(p)
		if !errors.Is(report.PolicyErr, ErrInvalidQualityPolicy) {
			t.Errorf("expected ErrInvalidQualityPolicy, got %v", report.PolicyErr)
		}
		if report.Valid() {
			t.Error("invalid policy must not produce a valid report")
		}
		if len(report.Checks) != 0 {
			t.Errorf("expected no checks for invalid policy, got %d", len(report.Checks))
		}
		// The protocol conclusion is still reported.
		if !report.ProtocolValid() {
			t.Error("expected protocol-valid report")
		}
	}
}

// The absolute value of the minimum duration saturates to the maximum
// duration instead of overflowing.
func TestOfflineQualityPolicyClockOffsetOverflow(t *testing.T) {
	r := makeQualityTestResponse()
	r.ClockOffset = math.MinInt64

	report := r.ValidateQuality(QualityPolicy{MaxClockOffset: math.MaxInt64})
	check := findCheck(t, report, QualityCheckClockOffset)
	if check.Value != math.MaxInt64 {
		t.Errorf("expected saturated absolute offset, got %d", check.Value)
	}
	if !check.Pass {
		t.Error("expected saturated offset to pass a MaxInt64 limit")
	}

	report = r.ValidateQuality(QualityPolicy{MaxClockOffset: math.MaxInt64 - 1})
	check = findCheck(t, report, QualityCheckClockOffset)
	if check.Pass {
		t.Error("expected saturated offset to fail a smaller limit")
	}
}

// A sample without a client receive timestamp cannot be evaluated for age
// and fails closed.
func TestOfflineQualityPolicyMissingReceiveTime(t *testing.T) {
	r := makeQualityTestResponse()
	r.Timestamps.ClientRecv = time.Time{}

	report := r.ValidateQuality(QualityPolicy{MaxAge: time.Hour})
	check := findCheck(t, report, QualityCheckAge)
	if check.Pass {
		t.Error("expected age check to fail without a receive timestamp")
	}
	if !errors.Is(check.Err, ErrMissingReceiveTime) {
		t.Errorf("expected ErrMissingReceiveTime, got %v", check.Err)
	}
	if report.Valid() {
		t.Error("expected report to be invalid")
	}

	// Without an age limit, the missing receive time is irrelevant.
	report = r.ValidateQuality(QualityPolicy{})
	if !report.Valid() {
		t.Errorf("expected valid report without age limit, got %+v", report)
	}
}

// Sample age is measured from the client receive timestamp to the policy's
// reference time. A future-dated sample (negative age) fails.
func TestOfflineQualityPolicyAge(t *testing.T) {
	r := makeQualityTestResponse()
	recv := r.Timestamps.ClientRecv

	// Fresh sample passes.
	report := r.ValidateQuality(QualityPolicy{
		MaxAge: time.Minute,
		Now:    recv.Add(30 * time.Second),
	})
	check := findCheck(t, report, QualityCheckAge)
	if !check.Pass || check.Value != 30*time.Second {
		t.Errorf("expected fresh sample to pass, got %+v", check)
	}

	// Stale sample fails.
	report = r.ValidateQuality(QualityPolicy{
		MaxAge: time.Minute,
		Now:    recv.Add(2 * time.Minute),
	})
	check = findCheck(t, report, QualityCheckAge)
	if check.Pass {
		t.Errorf("expected stale sample to fail, got %+v", check)
	}

	// Future-dated sample fails.
	report = r.ValidateQuality(QualityPolicy{
		MaxAge: time.Minute,
		Now:    recv.Add(-time.Second),
	})
	check = findCheck(t, report, QualityCheckAge)
	if check.Pass {
		t.Errorf("expected future-dated sample to fail, got %+v", check)
	}
}

// A zero-valued policy disables all quality checks.
func TestOfflineQualityPolicyZeroDisablesChecks(t *testing.T) {
	r := makeQualityTestResponse()
	r.RTT = time.Hour
	r.ClockOffset = time.Hour
	r.RootDistance = time.Hour

	report := r.ValidateQuality(QualityPolicy{})
	if len(report.Checks) != 0 {
		t.Errorf("expected no checks, got %d", len(report.Checks))
	}
	if !report.Valid() {
		t.Error("expected valid report with all checks disabled")
	}
}
