package backfill

import (
	"testing"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const (
	sidA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	sidB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
)

func mustMysqlSet(t *testing.T, s string) *mysql.MysqlGTIDSet {
	t.Helper()
	parsed, err := mysql.ParseMysqlGTIDSet(s)
	require.NoError(t, err)
	return parsed.(*mysql.MysqlGTIDSet)
}

func TestPlanMySQLFetchPurgedAndGap(t *testing.T) {
	plan, err := PlanMySQLFromStrings(
		sidA+":1-10",
		sidA+":1-20",
		sidA+":1-5",
		"",
	)
	require.NoError(t, err)

	require.Equal(t, ActionConnect, plan.Action)
	require.Equal(t, sidA+":11-20", plan.Fetchable.String())
	require.Equal(t, sidA+":6-10", plan.PurgedGap.String())
	require.True(t, plan.SourceMissing.IsEmpty())
	require.True(t, plan.LocalAhead.IsEmpty())
}

func TestPlanMySQLTagsAreNeverMerged(t *testing.T) {
	// Same SID, two independent tag streams. The empty tag is fully caught
	// up; the tagged stream is entirely missing and must be planned on its
	// own without any interval bleeding across tags.
	plan, err := PlanMySQLFromStrings(
		sidA+":1-20",
		sidA+":1-20:"+"shard:1-30",
		"",
		"",
	)
	require.NoError(t, err)

	require.Equal(t, ActionConnect, plan.Action)
	require.Equal(t, sidA+":shard:1-30", plan.Fetchable.String())
	require.True(t, plan.PurgedGap.IsEmpty())
	require.True(t, plan.SourceMissing.IsEmpty())

	// The classified fetchable set must keep tags distinct: containment
	// must fail against a set carrying a different tag.
	otherTag := mustMysqlSet(t, sidA+":other:1-30")
	require.False(t, plan.Fetchable.Contain(otherTag))
}

func TestPlanMySQLScopeLimitsAndRevealsSourceMissing(t *testing.T) {
	plan, err := PlanMySQLFromStrings(
		sidA+":1-5",
		sidA+":1-20",
		"",
		sidA+":1-100",
	)
	require.NoError(t, err)

	require.Equal(t, sidA+":6-20", plan.Fetchable.String())
	require.Equal(t, sidA+":21-100", plan.SourceMissing.String())
	require.Equal(t, ActionConnect, plan.Action)
}

func TestPlanMySQLHugeRangesStayIntervalBounded(t *testing.T) {
	// A gap spanning ~9 quintillion transaction numbers must remain a
	// single interval; the planner must never expand it.
	plan, err := PlanMySQLFromStrings(
		sidA+":1",
		sidA+":1-9000000000000000000",
		"",
		"",
	)
	require.NoError(t, err)

	require.Equal(t, ActionConnect, plan.Action)
	require.Equal(t, sidA+":2-9000000000000000000", plan.Fetchable.String())
	id := uuid.MustParse(sidA)
	require.Len(t, (*plan.Fetchable)[id][mysql.NewTag("")], 1)
}

func TestPlanMySQLMultipleSIDsAndLocalAhead(t *testing.T) {
	plan, err := PlanMySQLFromStrings(
		sidA+":1-110",
		sidA+":1-90,"+sidB+":1-5",
		"",
		"",
	)
	require.NoError(t, err)

	// sidA: source serves 91-90? no: local is ahead on 91-110 while source
	// has nothing new for sidA. sidB is entirely missing locally.
	require.Equal(t, sidB+":1-5", plan.Fetchable.String())
	require.Equal(t, sidA+":91-110", plan.LocalAhead.String())
	require.True(t, plan.SourceMissing.IsEmpty())
	require.Equal(t, ActionConnect, plan.Action)
}

func TestPlanMySQLNothingToDo(t *testing.T) {
	plan, err := PlanMySQLFromStrings(
		sidA+":1-100",
		sidA+":1-100",
		sidA+":1-20",
		"",
	)
	require.NoError(t, err)

	require.Equal(t, ActionSkip, plan.Action)
	require.True(t, plan.Fetchable.IsEmpty())
	require.True(t, plan.PurgedGap.IsEmpty())
	require.True(t, plan.SourceMissing.IsEmpty())
	require.NotEmpty(t, plan.Reasons)
}

func TestPlanMySQLRejectsPurgedOutsideExecuted(t *testing.T) {
	_, err := PlanMySQLFromStrings(
		sidA+":1-5",
		sidA+":1-10",
		sidA+":1-20",
		"",
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a subset")
}

func TestPlanMySQLPartialPurgedMiddleGap(t *testing.T) {
	// Missing 11-30; source purged through 15 and executed through 25,
	// while the request runs to 40: purged gap, fetchable tail, and source
	// missing all coexist on one stream.
	plan, err := PlanMySQLFromStrings(
		sidA+":1-10",
		sidA+":1-25",
		sidA+":1-15",
		sidA+":1-40",
	)
	require.NoError(t, err)

	require.Equal(t, sidA+":16-25", plan.Fetchable.String())
	require.Equal(t, sidA+":11-15", plan.PurgedGap.String())
	require.Equal(t, sidA+":26-40", plan.SourceMissing.String())
	require.Equal(t, ActionConnect, plan.Action)
}
