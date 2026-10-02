package server

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/internal/pkg/table"
	"github.com/osrg/gobgp/v4/pkg/config/oc"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
)

const (
	diversityTestPrefix = "10.10.10.0/24"
	diversityTestMyAS   = 65000
)

// diversityTestCandidate builds a path for the test prefix whose priority
// (local-pref) and source (next-hop and leftmost AS) are controlled, so the
// selection result is observable. The source peer is iBGP so the local-pref
// survives ingress.
func diversityTestCandidate(t *testing.T, src *peer, localPref uint32, nexthop string, neighborAS uint32) *table.Path {
	t.Helper()
	nlri, err := bgp.NewIPAddrPrefix(netip.MustParsePrefix(diversityTestPrefix))
	require.NoError(t, err)
	nh, err := bgp.NewPathAttributeNextHop(netip.MustParseAddr(nexthop))
	require.NoError(t, err)
	attrs := []bgp.PathAttributeInterface{
		bgp.NewPathAttributeOrigin(0),
		nh,
		bgp.NewPathAttributeAsPath([]bgp.AsPathParamInterface{
			bgp.NewAs4PathParam(bgp.BGP_ASPATH_ATTR_TYPE_SEQ, []uint32{neighborAS}),
		}),
		bgp.NewPathAttributeLocalPref(localPref),
	}
	return table.NewPath(bgp.RF_IPv4_UC, src.peerInfo.Load(), bgp.PathNLRI{NLRI: nlri}, false, attrs, time.Now(), false)
}

// setupAddPathDiversityTest starts a server with one Add-Path send peer
// (send-max 2) and four iBGP source peers, and accepts everything by
// default on both policy directions.
func setupAddPathDiversityTest(t *testing.T, mode oc.AddPathsDiversityMode, fill oc.AddPathsDiversityFillMode) (*BgpServer, *peer, []*peer) {
	t.Helper()
	ctx := context.Background()
	s := NewBgpServer()
	go s.Serve()
	require.NoError(t, s.StartBgp(ctx, &api.StartBgpRequest{
		Global: &api.Global{
			Asn:        diversityTestMyAS,
			RouterId:   "1.1.1.1",
			ListenPort: -1,
		},
	}))

	// Default-accept assignment for the global table; without it the
	// implicit default is to reject.
	require.NoError(t, s.policy.Reset(&oc.RoutingPolicy{}, map[string]oc.ApplyPolicy{
		table.GLOBAL_RIB_NAME: {
			Config: oc.ApplyPolicyConfig{
				DefaultImportPolicy: oc.DEFAULT_POLICY_TYPE_ACCEPT_ROUTE,
				DefaultExportPolicy: oc.DEFAULT_POLICY_TYPE_ACCEPT_ROUTE,
			},
		},
	}))

	target := newPeerandInfo(t, diversityTestMyAS, 65001, "10.0.0.2", s.globalRib)
	target.policy = s.policy
	target.fsm.state.Store(bgp.BGP_FSM_ESTABLISHED)
	target.fsm.familyMap.Store(map[bgp.Family]bgp.BGPAddPathMode{
		bgp.RF_IPv4_UC: bgp.BGP_ADD_PATH_SEND,
	})
	conf := target.fsm.pConf.ReadOnly()
	require.NotEmpty(t, conf.AfiSafis)
	conf.AfiSafis[0].AddPaths.Config.SendMax = 2
	conf.AfiSafis[0].AddPaths.Config.DiversityMode = mode
	conf.AfiSafis[0].AddPaths.Config.DiversityFillMode = fill

	targetAddress := netip.MustParseAddr("10.0.0.2")
	require.NoError(t, s.mgmtOperation(func() error {
		s.neighborMap[targetAddress] = target
		return nil
	}, true))

	sources := make([]*peer, 0, 4)
	for _, addr := range []string{"192.0.2.1", "192.0.2.2", "192.0.2.3", "192.0.2.4"} {
		src := newPeerandInfo(t, diversityTestMyAS, diversityTestMyAS, addr, s.globalRib)
		src.policy = s.policy
		sources = append(sources, src)
	}

	t.Cleanup(func() {
		require.NoError(t, s.mgmtOperation(func() error {
			delete(s.neighborMap, targetAddress)
			return nil
		}, false))
		cleanInfiniteChannel(target.fsm.outgoingCh)
		require.NoError(t, s.StopBgp(ctx, &api.StopBgpRequest{}))
	})
	return s, target, sources
}

// diversityTestCandidates returns four candidates for the same prefix, best
// first: the two best share a next-hop, the two worst share another.
func diversityTestCandidates(t *testing.T, sources []*peer) []*table.Path {
	t.Helper()
	return []*table.Path{
		diversityTestCandidate(t, sources[0], 400, "192.0.2.1", 65101),
		diversityTestCandidate(t, sources[1], 300, "192.0.2.1", 65102),
		diversityTestCandidate(t, sources[2], 200, "192.0.2.2", 65103),
		diversityTestCandidate(t, sources[3], 100, "192.0.2.2", 65104),
	}
}

// drainOutgoing collects the paths of all pending outgoing messages, waiting
// up to 300ms for stragglers.
func drainOutgoing(p *peer) []*table.Path {
	paths := []*table.Path{}
	for {
		select {
		case msg := <-p.fsm.outgoingCh.Out():
			paths = append(paths, msg.(*fsmOutgoingMsg).Paths...)
		case <-time.After(300 * time.Millisecond):
			return paths
		}
	}
}

func pathIDs(paths []*table.Path) map[uint32]bool {
	ids := make(map[uint32]bool, len(paths))
	for _, p := range paths {
		ids[p.LocalID()] = true
	}
	return ids
}

func sentIDs(p *peer, dest *table.Path) map[uint32]struct{} {
	return p.getSentPathIDs(dest.GetDestLocalKey())
}

func TestAddPathDiversityNexthop(t *testing.T) {
	s, target, sources := setupAddPathDiversityTest(t,
		oc.ADD_PATHS_DIVERSITY_MODE_NEXTHOP, oc.ADD_PATHS_DIVERSITY_FILL_MODE_BEST_EFFORT)
	candidates := diversityTestCandidates(t, sources)
	p1, p2, p3, p4 := candidates[0], candidates[1], candidates[2], candidates[3]

	// Four candidates, two send slots. The two best paths share the same
	// next-hop, so plain send-max would spend both slots on 192.0.2.1.
	for i, p := range candidates {
		s.propagateUpdate(sources[i], []*table.Path{p})
	}
	sent := sentIDs(target, p1)
	require.Len(t, sent, 2, "exactly the send-max number of paths must be advertised")
	assert.Contains(t, sent, p1.LocalID())
	assert.Contains(t, sent, p3.LocalID(), "the slot of the duplicate next-hop must go to a distinct source")
	assert.NotContains(t, sent, p2.LocalID())
	assert.NotContains(t, sent, p4.LocalID())
	drainOutgoing(target)

	// Recomputation over an unchanged RIB is a no-op: no path ID flapping
	// and no spurious withdrawals.
	p1ID, p3ID := p1.LocalID(), p3.LocalID()
	s.propagateUpdate(sources[2], []*table.Path{p3})
	s.propagateUpdate(sources[3], []*table.Path{p4})
	assert.Empty(t, drainOutgoing(target), "recomputation without RIB change must not generate updates")
	assert.Equal(t, p1ID, p1.LocalID())
	assert.Equal(t, p3ID, p3.LocalID())

	// Withdrawing a selected path backfills its slot with the next best
	// candidate of the same diversity group; the other slot is untouched.
	s.propagateUpdate(sources[0], []*table.Path{p1.Clone(true)})
	sent = sentIDs(target, p1)
	require.Len(t, sent, 2)
	assert.Contains(t, sent, p2.LocalID())
	assert.Contains(t, sent, p3.LocalID())
	withdrawn := map[uint32]bool{}
	announced := map[uint32]bool{}
	for _, p := range drainOutgoing(target) {
		if p.IsWithdraw {
			withdrawn[p.LocalID()] = true
		} else {
			announced[p.LocalID()] = true
		}
	}
	assert.Equal(t, map[uint32]bool{p1ID: true}, withdrawn)
	assert.Equal(t, map[uint32]bool{p2.LocalID(): true}, announced)
	assert.Equal(t, p3ID, p3.LocalID(), "untouched slots must keep their path ID")
}

func TestAddPathDiversityNeighborAS(t *testing.T) {
	s, target, sources := setupAddPathDiversityTest(t,
		oc.ADD_PATHS_DIVERSITY_MODE_NEIGHBOR_AS, oc.ADD_PATHS_DIVERSITY_FILL_MODE_BEST_EFFORT)
	candidates := diversityTestCandidates(t, sources)
	p1, p2 := candidates[0], candidates[1]

	// Same candidates as the next-hop test, but the neighbor-as strategy
	// spreads the two slots over the two best distinct neighbor ASes.
	for i, p := range candidates {
		s.propagateUpdate(sources[i], []*table.Path{p})
	}
	sent := sentIDs(target, p1)
	require.Len(t, sent, 2)
	assert.Contains(t, sent, p1.LocalID())
	assert.Contains(t, sent, p2.LocalID(), "neighbor-as diversity prefers a distinct AS over a distinct next-hop")
}

func TestAddPathDiversityFillModes(t *testing.T) {
	// Strict: with a single distinct source, the second slot stays empty.
	s, target, sources := setupAddPathDiversityTest(t,
		oc.ADD_PATHS_DIVERSITY_MODE_NEXTHOP, oc.ADD_PATHS_DIVERSITY_FILL_MODE_STRICT)
	candidates := diversityTestCandidates(t, sources)
	p1, p2 := candidates[0], candidates[1]

	s.propagateUpdate(sources[0], []*table.Path{p1})
	s.propagateUpdate(sources[1], []*table.Path{p2})
	sent := sentIDs(target, p1)
	require.Len(t, sent, 1, "strict fill must leave the slot of the duplicate source empty")
	assert.Contains(t, sent, p1.LocalID())

	// Best-effort: the remaining slot is backfilled with the next best path.
	s2, target2, sources2 := setupAddPathDiversityTest(t,
		oc.ADD_PATHS_DIVERSITY_MODE_NEXTHOP, oc.ADD_PATHS_DIVERSITY_FILL_MODE_BEST_EFFORT)
	candidates2 := diversityTestCandidates(t, sources2)
	q1, q2 := candidates2[0], candidates2[1]

	s2.propagateUpdate(sources2[0], []*table.Path{q1})
	s2.propagateUpdate(sources2[1], []*table.Path{q2})
	sent = sentIDs(target2, q1)
	require.Len(t, sent, 2, "best-effort fill must backfill the slot with the best remaining candidate")
	assert.Contains(t, sent, q1.LocalID())
	assert.Contains(t, sent, q2.LocalID())
}

func TestAddPathDiversityRespectsExportPolicy(t *testing.T) {
	s, target, sources := setupAddPathDiversityTest(t,
		oc.ADD_PATHS_DIVERSITY_MODE_NEXTHOP, oc.ADD_PATHS_DIVERSITY_FILL_MODE_BEST_EFFORT)
	candidates := diversityTestCandidates(t, sources)
	p1, p2, p3, p4 := candidates[0], candidates[1], candidates[2], candidates[3]

	for i, p := range candidates {
		s.propagateUpdate(sources[i], []*table.Path{p})
	}
	sent := sentIDs(target, p1)
	require.Len(t, sent, 2)
	require.Contains(t, sent, p3.LocalID())
	drainOutgoing(target)

	// Explicitly deny the selected path from AS 65103 in the export policy.
	require.NoError(t, s.policy.Reset(&oc.RoutingPolicy{
		DefinedSets: oc.DefinedSets{
			BgpDefinedSets: oc.BgpDefinedSets{
				AsPathSets: []oc.AsPathSet{
					{AsPathSetName: "deny65103", AsPathList: []string{"65103"}},
				},
			},
		},
		PolicyDefinitions: []oc.PolicyDefinition{
			{
				Name: "deny65103",
				Statements: []oc.Statement{
					{
						Name: "deny",
						Conditions: oc.Conditions{
							BgpConditions: oc.BgpConditions{
								MatchAsPathSet: oc.MatchAsPathSet{
									AsPathSet:       "deny65103",
									MatchSetOptions: oc.MATCH_SET_OPTIONS_TYPE_ANY,
								},
							},
						},
						Actions: oc.Actions{
							RouteDisposition: oc.ROUTE_DISPOSITION_REJECT_ROUTE,
						},
					},
				},
			},
		},
	}, map[string]oc.ApplyPolicy{
		table.GLOBAL_RIB_NAME: {
			Config: oc.ApplyPolicyConfig{
				DefaultImportPolicy: oc.DEFAULT_POLICY_TYPE_ACCEPT_ROUTE,
				DefaultExportPolicy: oc.DEFAULT_POLICY_TYPE_ACCEPT_ROUTE,
				ExportPolicyList:    []string{"deny65103"},
			},
		},
	}))

	// Trigger a recomputation; the denied path must be withdrawn and must
	// not be selected again, however diverse it is.
	s.propagateUpdate(sources[3], []*table.Path{p4})
	sent = sentIDs(target, p1)
	require.Len(t, sent, 2)
	assert.NotContains(t, sent, p3.LocalID(), "diversity must not override an explicit export deny")
	assert.Contains(t, sent, p1.LocalID(), "the still-eligible path must keep its slot")
	assert.NotContains(t, sent, p2.LocalID())
	assert.Contains(t, sent, p4.LocalID(), "the freed slot must be backfilled by a policy-compliant candidate")
}
