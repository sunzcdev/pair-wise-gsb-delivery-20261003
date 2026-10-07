package mysql

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIntervalSliceSubtract(t *testing.T) {
	cases := []struct {
		a, b, want IntervalSlice
	}{
		{IntervalSlice{{1, 10}}, IntervalSlice{{3, 5}}, IntervalSlice{{1, 3}, {5, 10}}},
		{IntervalSlice{{1, 10}}, IntervalSlice{{1, 10}}, nil},
		{IntervalSlice{{1, 10}}, nil, IntervalSlice{{1, 10}}},
		{nil, IntervalSlice{{1, 10}}, nil},
		{IntervalSlice{{1, 3}, {5, 8}}, IntervalSlice{{2, 6}}, IntervalSlice{{1, 2}, {6, 8}}},
		{IntervalSlice{{1, 5}}, IntervalSlice{{10, 20}}, IntervalSlice{{1, 5}}},
		{IntervalSlice{{1, 5}}, IntervalSlice{{4, 20}}, IntervalSlice{{1, 4}}},
		{IntervalSlice{{5, 10}}, IntervalSlice{{1, 3}, {6, 8}}, IntervalSlice{{5, 6}, {8, 10}}},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, tc.a.Subtract(tc.b), "%v - %v", tc.a, tc.b)
	}
}

func TestIntervalSliceIntersect(t *testing.T) {
	cases := []struct {
		a, b, want IntervalSlice
	}{
		{IntervalSlice{{1, 10}}, IntervalSlice{{3, 5}}, IntervalSlice{{3, 5}}},
		{IntervalSlice{{1, 10}}, IntervalSlice{{10, 20}}, nil},
		{IntervalSlice{{1, 3}, {5, 8}}, IntervalSlice{{2, 6}}, IntervalSlice{{2, 3}, {5, 6}}},
		{nil, IntervalSlice{{1, 10}}, nil},
		{IntervalSlice{{1, 10}}, IntervalSlice{{1, 10}}, IntervalSlice{{1, 10}}},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, tc.a.Intersect(tc.b), "%v and %v", tc.a, tc.b)
	}
}

func mustMysqlSet(t *testing.T, s string) *MysqlGTIDSet {
	t.Helper()
	set, err := ParseMysqlGTIDSet(s)
	require.NoError(t, err)
	return set.(*MysqlGTIDSet)
}

func mustMariadbSet(t *testing.T, s string) *MariadbGTIDSet {
	t.Helper()
	set, err := ParseMariadbGTIDSet(s)
	require.NoError(t, err)
	return set.(*MariadbGTIDSet)
}

const planSID = "3e11fa47-71ca-11e1-9e33-c80aa9429562"

func TestPlanMysqlBackfill(t *testing.T) {
	local := mustMysqlSet(t, planSID+":1-5")
	executed := mustMysqlSet(t, planSID+":1-10")
	purged := mustMysqlSet(t, planSID+":1-6")

	plan := PlanMysqlBackfill(local, executed, purged, nil)

	require.Equal(t, planSID+":7-10", plan.Pending.String())
	require.Equal(t, planSID+":6", plan.Purged.String())
	require.Equal(t, "", plan.Missing.String())
	require.True(t, plan.NeedFetch())
	require.False(t, plan.Complete())
}

func TestPlanMysqlBackfillKeepsTagsSeparate(t *testing.T) {
	// Same SID, different tags: intervals must never be merged across tags.
	local := mustMysqlSet(t, planSID+":1-5,"+planSID+":t1:1-3")
	executed := mustMysqlSet(t, planSID+":1-10,"+planSID+":t1:1-8")
	purged := mustMysqlSet(t, planSID+":1-6")

	plan := PlanMysqlBackfill(local, executed, purged, nil)

	require.Equal(t, planSID+":7-10:t1:4-8", plan.Pending.String())
	// The purged set has no t1 entry, so the t1 gap must not be reported
	// as purged even though the untagged intervals overlap numerically.
	require.Equal(t, planSID+":6", plan.Purged.String())
}

func TestPlanMysqlBackfillMissing(t *testing.T) {
	other := "5f7f0e4c-8b2d-4c1a-9f3e-1a2b3c4d5e6f"
	local := mustMysqlSet(t, planSID+":1-10,"+other+":1-3")
	executed := mustMysqlSet(t, planSID+":1-10")

	plan := PlanMysqlBackfill(local, executed, nil, nil)

	require.Equal(t, "", plan.Pending.String())
	require.Equal(t, other+":1-3", plan.Missing.String())
	require.False(t, plan.NeedFetch())
	require.False(t, plan.Complete())
}

func TestPlanMysqlBackfillScope(t *testing.T) {
	local := mustMysqlSet(t, planSID+":1-5")
	executed := mustMysqlSet(t, planSID+":1-100")
	purged := mustMysqlSet(t, planSID+":1-20")
	scope := mustMysqlSet(t, planSID+":15-30")

	plan := PlanMysqlBackfill(local, executed, purged, scope)

	require.Equal(t, planSID+":21-30", plan.Pending.String())
	require.Equal(t, planSID+":15-20", plan.Purged.String())
	require.Equal(t, "", plan.Missing.String())
}

func TestPlanMysqlBackfillHugeRanges(t *testing.T) {
	// Intervals spanning huge sequence ranges must be handled without
	// expanding individual transaction numbers.
	local := mustMysqlSet(t, planSID+":1-5")
	executed := mustMysqlSet(t, planSID+":1-4611686018427387904") // 1<<62
	purged := mustMysqlSet(t, planSID+":1-2305843009213693952")   // 1<<61

	plan := PlanMysqlBackfill(local, executed, purged, nil)

	require.Equal(t, planSID+":2305843009213693953-4611686018427387904", plan.Pending.String())
	require.Equal(t, planSID+":6-2305843009213693952", plan.Purged.String())
}

func TestPlanMysqlBackfillComplete(t *testing.T) {
	local := mustMysqlSet(t, planSID+":1-10")
	executed := mustMysqlSet(t, planSID+":1-10")

	plan := PlanMysqlBackfill(local, executed, nil, nil)

	require.True(t, plan.Complete())
	require.False(t, plan.NeedFetch())
}

func TestPlanMysqlBackfillDoesNotMutateInputs(t *testing.T) {
	local := mustMysqlSet(t, planSID+":1-5")
	executed := mustMysqlSet(t, planSID+":1-10")
	purged := mustMysqlSet(t, planSID+":1-6")

	_ = PlanMysqlBackfill(local, executed, purged, nil)

	require.Equal(t, planSID+":1-5", local.String())
	require.Equal(t, planSID+":1-10", executed.String())
	require.Equal(t, planSID+":1-6", purged.String())
}

func TestPlanMariadbBackfillPendingAndPurged(t *testing.T) {
	local := mustMariadbSet(t, "0-1-100")
	executed := mustMariadbSet(t, "0-1-150")
	purged := mustMariadbSet(t, "0-1-120")

	plan := PlanMariadbBackfill(local, executed, purged, nil)
	require.Len(t, plan.Domains, 1)

	d := plan.Domains[0]
	require.Equal(t, uint32(0), d.DomainID)
	require.Equal(t, &MariadbSeqRange{Start: 121, Stop: 151}, d.Pending)
	require.Equal(t, &MariadbSeqRange{Start: 101, Stop: 121}, d.Purged)
	require.Nil(t, d.Unknown)
	require.True(t, plan.NeedFetch())
}

func TestPlanMariadbBackfillIndeterminate(t *testing.T) {
	// Local watermark beyond the source's executed watermark: the source
	// cannot prove this history, so it must be reported as indeterminate.
	local := mustMariadbSet(t, "0-1-200")
	executed := mustMariadbSet(t, "0-1-150")

	plan := PlanMariadbBackfill(local, executed, nil, nil)
	require.Len(t, plan.Domains, 1)

	d := plan.Domains[0]
	require.Nil(t, d.Pending)
	require.Nil(t, d.Purged)
	require.Equal(t, &MariadbSeqRange{Start: 151, Stop: 201}, d.Unknown)
	require.Contains(t, d.Reason, "indeterminate")
	require.False(t, plan.NeedFetch())
}

func TestPlanMariadbBackfillScope(t *testing.T) {
	local := mustMariadbSet(t, "0-1-100,1-1-50")
	executed := mustMariadbSet(t, "0-1-150,1-1-80")
	// Scope restricts planning to domain 0 and caps it at sequence 130.
	scope := mustMariadbSet(t, "0-1-130")

	plan := PlanMariadbBackfill(local, executed, nil, scope)
	require.Len(t, plan.Domains, 1)

	d := plan.Domains[0]
	require.Equal(t, uint32(0), d.DomainID)
	require.Equal(t, &MariadbSeqRange{Start: 101, Stop: 131}, d.Pending)
}

func TestPlanMariadbBackfillServerChange(t *testing.T) {
	// Failover: server_id changed within the same domain. The plan is
	// still computed, but the conclusion is annotated.
	local := mustMariadbSet(t, "0-1-100")
	executed := mustMariadbSet(t, "0-2-150")

	plan := PlanMariadbBackfill(local, executed, nil, nil)
	require.Len(t, plan.Domains, 1)

	d := plan.Domains[0]
	require.Equal(t, &MariadbSeqRange{Start: 101, Stop: 151}, d.Pending)
	require.Contains(t, d.Reason, "server_id changed")
}

func TestPlanMariadbBackfillUpToDate(t *testing.T) {
	local := mustMariadbSet(t, "0-1-150")
	executed := mustMariadbSet(t, "0-1-150")

	plan := PlanMariadbBackfill(local, executed, nil, nil)
	require.Len(t, plan.Domains, 1)

	d := plan.Domains[0]
	require.Nil(t, d.Pending)
	require.Nil(t, d.Purged)
	require.Nil(t, d.Unknown)
	require.False(t, plan.NeedFetch())
}

func TestPlanMariadbBackfillMissingLocalDomain(t *testing.T) {
	// Domain never seen locally: everything above the purged watermark is
	// fetchable, everything below it is a purged gap.
	executed := mustMariadbSet(t, "0-1-150")
	purged := mustMariadbSet(t, "0-1-120")

	plan := PlanMariadbBackfill(nil, executed, purged, nil)
	require.Len(t, plan.Domains, 1)

	d := plan.Domains[0]
	require.Equal(t, &MariadbSeqRange{Start: 121, Stop: 151}, d.Pending)
	require.Equal(t, &MariadbSeqRange{Start: 1, Stop: 121}, d.Purged)
}
