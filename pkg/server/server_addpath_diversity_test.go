package server

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/internal/pkg/table"
	"github.com/osrg/gobgp/v4/pkg/config/oc"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
)

type addPathDiversityPathSpec struct {
	source    string
	sourceAS  uint32
	localPref uint32
	nexthop   string
	asPath    []uint32
	community []uint32
}

func makeAddPathDiversityPath(t *testing.T, spec addPathDiversityPathSpec) *table.Path {
	t.Helper()
	nlri, err := bgp.NewIPAddrPrefix(netip.MustParsePrefix("192.0.2.0/32"))
	require.NoError(t, err)
	sourceAddr := netip.MustParseAddr(spec.source)
	nextHop, err := bgp.NewPathAttributeNextHop(netip.MustParseAddr(spec.nexthop))
	require.NoError(t, err)
	attrs := []bgp.PathAttributeInterface{
		bgp.NewPathAttributeOrigin(0),
		bgp.NewPathAttributeAsPath([]bgp.AsPathParamInterface{
			bgp.NewAs4PathParam(2, spec.asPath),
		}),
		nextHop,
		bgp.NewPathAttributeLocalPref(spec.localPref),
	}
	if len(spec.community) > 0 {
		attrs = append(attrs, bgp.NewPathAttributeCommunities(spec.community))
	}
	return table.NewPath(bgp.RF_IPv4_UC, &table.PeerInfo{
		AS:           spec.sourceAS,
		ID:           sourceAddr,
		Address:      sourceAddr,
		LocalAS:      65001,
		LocalID:      netip.MustParseAddr("1.1.1.1"),
		LocalAddress: netip.MustParseAddr("1.1.1.1"),
	}, bgp.PathNLRI{NLRI: nlri}, false, attrs, time.Now(), false)
}

func setupAddPathDiversityPeer(t *testing.T, diversity oc.AddPathsSelectionDiversityType, backfill oc.AddPathsSelectionBackfillType) (*BgpServer, *peer, netip.Addr) {
	t.Helper()
	s := NewBgpServer()
	go s.Serve()

	err := s.StartBgp(context.Background(), &api.StartBgpRequest{
		Global: &api.Global{
			Asn:        65001,
			RouterId:   "1.1.1.1",
			ListenPort: -1,
		},
	})
	require.NoError(t, err)

	peerAddr := netip.MustParseAddr("10.0.0.1")
	p := newPeerandInfo(t, 65001, 65002, peerAddr.String(), s.globalRib)
	p.policy = s.policy
	p.fsm.state.Store(bgp.BGP_FSM_ESTABLISHED)
	p.fsm.familyMap.Store(map[bgp.Family]bgp.BGPAddPathMode{
		bgp.RF_IPv4_UC: bgp.BGP_ADD_PATH_SEND,
	})
	p.fsm.lock.Lock()
	conf := p.fsm.pConf.ReadCopy()
	foundFamily := false
	for i := range conf.AfiSafis {
		if conf.AfiSafis[i].State.Family != bgp.RF_IPv4_UC {
			continue
		}
		conf.AfiSafis[i].AddPaths.Config.SendMax = 2
		conf.AfiSafis[i].AddPaths.State.SendMax = 2
		conf.AfiSafis[i].AddPaths.Config.SelectionDiversity = diversity
		conf.AfiSafis[i].AddPaths.Config.SelectionBackfill = backfill
		foundFamily = true
	}
	p.fsm.pConf.Update(&conf)
	p.fsm.lock.Unlock()
	require.True(t, foundFamily)

	err = s.mgmtOperation(func() error {
		s.neighborMap[peerAddr] = p
		return nil
	}, true)
	require.NoError(t, err)
	t.Cleanup(func() {
		err := s.mgmtOperation(func() error {
			delete(s.neighborMap, peerAddr)
			return nil
		}, false)
		require.NoError(t, err)
		cleanInfiniteChannel(p.fsm.outgoingCh)
		require.NoError(t, s.StopBgp(context.Background(), &api.StopBgpRequest{}))
	})
	return s, p, peerAddr
}

func requireAddPathOutgoing(t *testing.T, p *peer) []*table.Path {
	t.Helper()
	select {
	case o := <-p.fsm.outgoingCh.Out():
		msg, ok := o.(*fsmOutgoingMsg)
		require.True(t, ok)
		paths := make([]*table.Path, 0, len(msg.Paths))
		for _, path := range msg.Paths {
			if path != nil && !path.IsEOR() {
				paths = append(paths, path)
			}
		}
		return paths
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for outbound add-path paths")
		return nil
	}
}

func requireNoAddPathOutgoing(t *testing.T, p *peer) {
	t.Helper()
	select {
	case o := <-p.fsm.outgoingCh.Out():
		t.Fatalf("unexpected outbound paths: %#v", o)
	case <-time.After(100 * time.Millisecond):
	}
}

// findOutgoingPath returns the path with the given local ID and withdraw
// flag, or nil if there is no such path in the list.
func findOutgoingPath(paths []*table.Path, id uint32, withdraw bool) *table.Path {
	for _, p := range paths {
		if p.LocalID() == id && p.IsWithdraw == withdraw {
			return p
		}
	}
	return nil
}

// TestAddPathDiversityNexthop checks that with two send slots and four
// candidates, the nexthop diversity strategy prefers paths with distinct
// next-hops over the plain best-to-worst order, and that recomputing the
// selection over an unchanged RIB neither re-advertises nor withdraws paths.
func TestAddPathDiversityNexthop(t *testing.T) {
	s, p, _ := setupAddPathDiversityPeer(t, oc.ADD_PATHS_SELECTION_DIVERSITY_NEXTHOP, oc.ADD_PATHS_SELECTION_BACKFILL_BEST_PATH)

	// Four candidates for 192.0.2.0/32, best-to-worst by local-pref.
	// p1 and p2 share the same next-hop.
	p1 := makeAddPathDiversityPath(t, addPathDiversityPathSpec{source: "10.0.0.2", sourceAS: 65010, localPref: 400, nexthop: "192.0.2.1", asPath: []uint32{65010}})
	p2 := makeAddPathDiversityPath(t, addPathDiversityPathSpec{source: "10.0.0.3", sourceAS: 65010, localPref: 300, nexthop: "192.0.2.1", asPath: []uint32{65010}})
	p3 := makeAddPathDiversityPath(t, addPathDiversityPathSpec{source: "10.0.0.4", sourceAS: 65020, localPref: 200, nexthop: "192.0.2.2", asPath: []uint32{65020}})
	p4 := makeAddPathDiversityPath(t, addPathDiversityPathSpec{source: "10.0.0.5", sourceAS: 65030, localPref: 100, nexthop: "192.0.2.3", asPath: []uint32{65030}})

	s.propagateUpdate(nil, []*table.Path{p1})
	paths := requireAddPathOutgoing(t, p)
	require.Len(t, paths, 1)
	require.NotNil(t, findOutgoingPath(paths, p1.LocalID(), false))

	// The second slot cannot be filled diversely yet, so the best-path
	// backfill grants it to p2.
	s.propagateUpdate(nil, []*table.Path{p2})
	paths = requireAddPathOutgoing(t, p)
	require.Len(t, paths, 1)
	require.NotNil(t, findOutgoingPath(paths, p2.LocalID(), false))

	// p3 brings a new next-hop: it takes p2's slot, p2 is withdrawn.
	s.propagateUpdate(nil, []*table.Path{p3})
	paths = requireAddPathOutgoing(t, p)
	require.Len(t, paths, 2)
	require.NotNil(t, findOutgoingPath(paths, p3.LocalID(), false))
	require.NotNil(t, findOutgoingPath(paths, p2.LocalID(), true))

	// p4 adds nothing new: the selection stays {p1, p3}.
	s.propagateUpdate(nil, []*table.Path{p4})
	requireNoAddPathOutgoing(t, p)

	// Recomputing over an unchanged RIB (e.g. an inbound soft reset
	// replaying p1) must not churn Path IDs or generate withdrawals.
	s.propagateUpdate(nil, []*table.Path{p1})
	requireNoAddPathOutgoing(t, p)

	require.Equal(t, uint8(2), p.getRoutesCount(bgp.RF_IPv4_UC, "192.0.2.0/32"))
}

// TestAddPathDiversityNeighborAS checks the neighbor-as diversity strategy:
// paths whose AS_PATH starts with a different adjacent AS are preferred,
// even when they have a different next-hop than an already selected path.
func TestAddPathDiversityNeighborAS(t *testing.T) {
	s, p, _ := setupAddPathDiversityPeer(t, oc.ADD_PATHS_SELECTION_DIVERSITY_NEIGHBOR_AS, oc.ADD_PATHS_SELECTION_BACKFILL_BEST_PATH)

	// p1 and p2 have the same adjacent AS (65010) but different next-hops.
	p1 := makeAddPathDiversityPath(t, addPathDiversityPathSpec{source: "10.0.0.2", sourceAS: 65010, localPref: 400, nexthop: "192.0.2.1", asPath: []uint32{65010}})
	p2 := makeAddPathDiversityPath(t, addPathDiversityPathSpec{source: "10.0.0.3", sourceAS: 65012, localPref: 300, nexthop: "192.0.2.2", asPath: []uint32{65010, 65012}})
	p3 := makeAddPathDiversityPath(t, addPathDiversityPathSpec{source: "10.0.0.4", sourceAS: 65020, localPref: 200, nexthop: "192.0.2.3", asPath: []uint32{65020}})
	p4 := makeAddPathDiversityPath(t, addPathDiversityPathSpec{source: "10.0.0.5", sourceAS: 65030, localPref: 100, nexthop: "192.0.2.4", asPath: []uint32{65030}})

	s.propagateUpdate(nil, []*table.Path{p1})
	paths := requireAddPathOutgoing(t, p)
	require.Len(t, paths, 1)
	require.NotNil(t, findOutgoingPath(paths, p1.LocalID(), false))

	// Backfill grants the second slot to p2 despite the duplicate
	// adjacent AS.
	s.propagateUpdate(nil, []*table.Path{p2})
	paths = requireAddPathOutgoing(t, p)
	require.Len(t, paths, 1)
	require.NotNil(t, findOutgoingPath(paths, p2.LocalID(), false))

	// p3's adjacent AS is not covered yet: it replaces p2.
	s.propagateUpdate(nil, []*table.Path{p3})
	paths = requireAddPathOutgoing(t, p)
	require.Len(t, paths, 2)
	require.NotNil(t, findOutgoingPath(paths, p3.LocalID(), false))
	require.NotNil(t, findOutgoingPath(paths, p2.LocalID(), true))

	s.propagateUpdate(nil, []*table.Path{p4})
	requireNoAddPathOutgoing(t, p)

	require.Equal(t, uint8(2), p.getRoutesCount(bgp.RF_IPv4_UC, "192.0.2.0/32"))
}

// TestAddPathDiversityBackfillNone checks that with backfill disabled, slots
// that cannot be filled diversely stay unused instead of being granted to
// same-source paths.
func TestAddPathDiversityBackfillNone(t *testing.T) {
	s, p, _ := setupAddPathDiversityPeer(t, oc.ADD_PATHS_SELECTION_DIVERSITY_NEXTHOP, oc.ADD_PATHS_SELECTION_BACKFILL_NONE)

	p1 := makeAddPathDiversityPath(t, addPathDiversityPathSpec{source: "10.0.0.2", sourceAS: 65010, localPref: 400, nexthop: "192.0.2.1", asPath: []uint32{65010}})
	p2 := makeAddPathDiversityPath(t, addPathDiversityPathSpec{source: "10.0.0.3", sourceAS: 65010, localPref: 300, nexthop: "192.0.2.1", asPath: []uint32{65010}})
	p3 := makeAddPathDiversityPath(t, addPathDiversityPathSpec{source: "10.0.0.4", sourceAS: 65020, localPref: 200, nexthop: "192.0.2.2", asPath: []uint32{65020}})

	s.propagateUpdate(nil, []*table.Path{p1})
	paths := requireAddPathOutgoing(t, p)
	require.Len(t, paths, 1)
	require.NotNil(t, findOutgoingPath(paths, p1.LocalID(), false))

	// p2 duplicates p1's next-hop and backfill is disabled: the second
	// slot stays unused.
	s.propagateUpdate(nil, []*table.Path{p2})
	requireNoAddPathOutgoing(t, p)

	// p3 covers a new next-hop and fills the slot; p2 was never
	// advertised, so no withdrawal is generated for it.
	s.propagateUpdate(nil, []*table.Path{p3})
	paths = requireAddPathOutgoing(t, p)
	require.Len(t, paths, 1)
	require.NotNil(t, findOutgoingPath(paths, p3.LocalID(), false))

	// Replaying p2 changes nothing.
	s.propagateUpdate(nil, []*table.Path{p2})
	requireNoAddPathOutgoing(t, p)

	require.Equal(t, uint8(2), p.getRoutesCount(bgp.RF_IPv4_UC, "192.0.2.0/32"))
}

// TestAddPathDiversityRespectsExportPolicy checks that diversity selection
// never overrides an explicit export policy denial: a policy-rejected path
// is not used to fill a free slot, even when it would add diversity.
func TestAddPathDiversityRespectsExportPolicy(t *testing.T) {
	s, p, _ := setupAddPathDiversityPeer(t, oc.ADD_PATHS_SELECTION_DIVERSITY_NEXTHOP, oc.ADD_PATHS_SELECTION_BACKFILL_NONE)

	comSet, err := table.NewCommunitySet(oc.CommunitySet{
		CommunitySetName: "comset1",
		CommunityList:    []string{"65001:100"},
	})
	require.NoError(t, err)
	require.NoError(t, s.policy.AddDefinedSet(comSet, false))
	pol, err := table.NewPolicy(oc.PolicyDefinition{
		Name: "policy1",
		Statements: []oc.Statement{
			{
				Name: "stmt1",
				Conditions: oc.Conditions{
					BgpConditions: oc.BgpConditions{
						MatchCommunitySet: oc.MatchCommunitySet{
							CommunitySet: "comset1",
						},
					},
				},
				Actions: oc.Actions{
					RouteDisposition: oc.ROUTE_DISPOSITION_REJECT_ROUTE,
				},
			},
		},
	})
	require.NoError(t, err)
	require.NoError(t, s.policy.AddPolicy(pol, false))
	require.NoError(t, s.policy.AddPolicyAssignment(table.GLOBAL_RIB_NAME, table.POLICY_DIRECTION_EXPORT, []*oc.PolicyDefinition{{Name: "policy1"}}, table.ROUTE_TYPE_ACCEPT))

	p1 := makeAddPathDiversityPath(t, addPathDiversityPathSpec{source: "10.0.0.2", sourceAS: 65010, localPref: 400, nexthop: "192.0.2.1", asPath: []uint32{65010}})
	// p2 has a distinct next-hop but is rejected by the export policy.
	p2 := makeAddPathDiversityPath(t, addPathDiversityPathSpec{source: "10.0.0.3", sourceAS: 65020, localPref: 300, nexthop: "192.0.2.2", asPath: []uint32{65020}, community: []uint32{65001<<16 | 100}})
	p3 := makeAddPathDiversityPath(t, addPathDiversityPathSpec{source: "10.0.0.4", sourceAS: 65030, localPref: 200, nexthop: "192.0.2.3", asPath: []uint32{65030}})

	s.propagateUpdate(nil, []*table.Path{p1})
	paths := requireAddPathOutgoing(t, p)
	require.Len(t, paths, 1)
	require.NotNil(t, findOutgoingPath(paths, p1.LocalID(), false))

	// The free slot must not be granted to the policy-rejected p2, even
	// though its next-hop is not covered yet.
	s.propagateUpdate(nil, []*table.Path{p2})
	requireNoAddPathOutgoing(t, p)

	s.propagateUpdate(nil, []*table.Path{p3})
	paths = requireAddPathOutgoing(t, p)
	require.Len(t, paths, 1)
	require.NotNil(t, findOutgoingPath(paths, p3.LocalID(), false))

	require.Equal(t, uint8(2), p.getRoutesCount(bgp.RF_IPv4_UC, "192.0.2.0/32"))
}
