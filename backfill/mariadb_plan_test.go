package backfill

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPlanMariaDBUnknownRetentionIsIndeterminate(t *testing.T) {
	plan, err := PlanMariaDBFromStrings(
		"0-100-10",
		"0-100-30",
		"",
		false,
		nil,
	)
	require.NoError(t, err)

	require.Equal(t, ActionInspect, plan.Action)
	require.Len(t, plan.Domains, 1)
	domain := plan.Domains[0]
	require.Equal(t, uint32(0), domain.DomainID)
	require.Equal(t, uint64(10), domain.LocalHigh)
	require.Equal(t, uint64(30), domain.ExecutedHigh)
	require.Equal(t, []SequenceRange{{From: 11, To: 30}}, domain.Indeterminate)
	require.Empty(t, domain.Fetchable)
	require.Empty(t, domain.Purged)
	require.False(t, domain.PurgedKnown)
}

func TestPlanMariaDBPurgedFrontierSplitsRange(t *testing.T) {
	plan, err := PlanMariaDBFromStrings(
		"0-100-10",
		"0-100-30",
		"0-100-15",
		true,
		nil,
	)
	require.NoError(t, err)

	require.Equal(t, ActionConnect, plan.Action)
	domain := plan.Domains[0]
	require.Equal(t, []SequenceRange{{From: 11, To: 15}}, domain.Purged)
	require.Equal(t, []SequenceRange{{From: 16, To: 30}}, domain.Fetchable)
	require.Empty(t, domain.Indeterminate)
}

func TestPlanMariaDBMultiWriterFailoverAttributesHistory(t *testing.T) {
	// Sequences 1..15 written by server 1, then failover; sequences 16..30
	// written by server 2. The executed state preserves both writers.
	plan, err := PlanMariaDBFromStrings(
		"0-2-5",
		"0-1-15,0-2-30",
		"0-1-15",
		true,
		nil,
	)
	require.NoError(t, err)

	require.Equal(t, ActionConnect, plan.Action)
	domain := plan.Domains[0]
	require.Equal(t, uint64(30), domain.ExecutedHigh)
	require.Len(t, domain.Executed, 2)
	require.Equal(t, uint32(1), domain.Executed[0].ServerID)
	require.Equal(t, uint64(15), domain.Executed[0].Sequence)
	require.Equal(t, uint32(2), domain.Executed[1].ServerID)
	require.Equal(t, uint64(30), domain.Executed[1].Sequence)

	// Local 6..15 belonged to the old primary and is purged; 16..30 is
	// served by the new primary.
	require.Equal(t, []SequenceRange{{From: 6, To: 15}}, domain.Purged)
	require.Equal(t, []SequenceRange{{From: 16, To: 30}}, domain.Fetchable)
}

func TestPlanMariaDBSourceNeverExecutedDomain(t *testing.T) {
	plan, err := PlanMariaDBFromStrings(
		"5-100-40",
		"",
		"",
		true,
		nil,
	)
	require.NoError(t, err)

	require.Equal(t, ActionSkip, plan.Action)
	domain := plan.Domains[0]
	require.Equal(t, uint32(5), domain.DomainID)
	require.Equal(t, uint64(40), domain.LocalHigh)
	require.Equal(t, uint64(0), domain.ExecutedHigh)
	require.Equal(t, []SequenceRange{{From: 1, To: 40}}, domain.LocalAhead)
	require.Empty(t, domain.Fetchable)
	require.Empty(t, domain.Indeterminate)
}

func TestPlanMariaDBScopeBeyondSourceIsMissing(t *testing.T) {
	plan, err := PlanMariaDBFromStrings(
		"0-100-10",
		"0-100-20",
		"0-100-10",
		true,
		MariaDBScope{0: 50},
	)
	require.NoError(t, err)

	domain := plan.Domains[0]
	require.Equal(t, uint64(50), domain.Target)
	require.Equal(t, []SequenceRange{{From: 11, To: 20}}, domain.Fetchable)
	require.Equal(t, []SequenceRange{{From: 21, To: 50}}, domain.SourceMissing)
	require.Empty(t, domain.Purged)
	require.Equal(t, ActionConnect, plan.Action)
}

func TestPlanMariaDBLocalAheadOfSource(t *testing.T) {
	plan, err := PlanMariaDBFromStrings(
		"0-100-40",
		"0-100-20",
		"",
		true,
		nil,
	)
	require.NoError(t, err)

	domain := plan.Domains[0]
	require.Equal(t, []SequenceRange{{From: 21, To: 40}}, domain.LocalAhead)
	require.Empty(t, domain.Fetchable)
	require.Empty(t, domain.SourceMissing)
	require.Equal(t, ActionSkip, plan.Action)
}

func TestPlanMariaDBRejectsPurgedBeyondExecuted(t *testing.T) {
	_, err := PlanMariaDBFromStrings(
		"0-100-1",
		"0-100-10",
		"0-100-40",
		true,
		nil,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds source executed")
}

func TestPlanMariaDBMultipleDomainsSorted(t *testing.T) {
	plan, err := PlanMariaDBFromStrings(
		"2-7-3,0-7-5",
		"2-7-9,0-7-8",
		"",
		false,
		nil,
	)
	require.NoError(t, err)

	require.Equal(t, ActionInspect, plan.Action)
	require.Len(t, plan.Domains, 2)
	require.Equal(t, uint32(0), plan.Domains[0].DomainID)
	require.Equal(t, uint32(2), plan.Domains[1].DomainID)
	require.Equal(t, []SequenceRange{{From: 6, To: 8}}, plan.Domains[0].Indeterminate)
	require.Equal(t, []SequenceRange{{From: 4, To: 9}}, plan.Domains[1].Indeterminate)
}

func TestPlanMariaDBConnectWinsWhenSomeDomainsCaughtUp(t *testing.T) {
	// Domain 0 is provably fetchable while domain 1 is already caught up.
	// The overall action must still be Connect.
	plan, err := PlanMariaDBFromStrings(
		"0-7-10,1-7-3",
		"0-7-20,1-7-3",
		"0-7-10,1-7-3",
		true,
		nil,
	)
	require.NoError(t, err)

	require.Equal(t, ActionConnect, plan.Action)
	require.Equal(t, []SequenceRange{{From: 11, To: 20}}, plan.Domains[0].Fetchable)
	require.Empty(t, plan.Domains[1].Fetchable)
}
