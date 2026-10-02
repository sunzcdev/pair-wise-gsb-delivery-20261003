package table

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
)

// addPathTestCandidate builds a path for 10.0.0.0/24 whose priority
// (local-pref) and source (next-hop and neighbor AS) are controlled so the
// selection result is observable.
func addPathTestCandidate(t *testing.T, localPref uint32, nexthop string, neighborAS uint32) *Path {
	t.Helper()
	nlri, err := bgp.NewIPAddrPrefix(netip.MustParsePrefix("10.0.0.0/24"))
	assert.NoError(t, err)
	nh, err := bgp.NewPathAttributeNextHop(netip.MustParseAddr(nexthop))
	assert.NoError(t, err)
	attrs := []bgp.PathAttributeInterface{
		bgp.NewPathAttributeOrigin(0),
		nh,
		bgp.NewPathAttributeAsPath([]bgp.AsPathParamInterface{
			bgp.NewAs4PathParam(bgp.BGP_ASPATH_ATTR_TYPE_SEQ, []uint32{neighborAS, 64496}),
		}),
		bgp.NewPathAttributeLocalPref(localPref),
	}
	source := &PeerInfo{
		AS:      neighborAS,
		Address: netip.MustParseAddr(nexthop),
		ID:      netip.MustParseAddr(nexthop),
	}
	return NewPath(bgp.RF_IPv4_UC, source, bgp.PathNLRI{NLRI: nlri}, false, attrs, time.Now(), false)
}

func TestSelectAddPathsDisabled(t *testing.T) {
	candidates := []*Path{
		addPathTestCandidate(t, 400, "192.0.2.1", 65001),
		addPathTestCandidate(t, 300, "192.0.2.1", 65001),
		addPathTestCandidate(t, 200, "192.0.2.2", 65002),
		addPathTestCandidate(t, 100, "192.0.2.3", 65003),
	}
	// Without a diversity mode the first send-max candidates win, even if
	// they share the same source.
	assert.Equal(t, []int{0, 1}, SelectAddPaths(candidates, 2, AddPathDiversityNone, AddPathDiversityFillBest))
	assert.Nil(t, SelectAddPaths(candidates, 0, AddPathDiversityNone, AddPathDiversityFillBest))
	assert.Nil(t, SelectAddPaths(nil, 2, AddPathDiversityNone, AddPathDiversityFillBest))
}

func TestSelectAddPathsNexthopDiversity(t *testing.T) {
	// Four candidates, two send slots. The two best paths share a next-hop,
	// so plain send-max would spend both slots on the same source.
	candidates := []*Path{
		addPathTestCandidate(t, 400, "192.0.2.1", 65001),
		addPathTestCandidate(t, 300, "192.0.2.1", 65001),
		addPathTestCandidate(t, 200, "192.0.2.2", 65002),
		addPathTestCandidate(t, 100, "192.0.2.3", 65003),
	}
	selected := SelectAddPaths(candidates, 2, AddPathDiversityNexthop, AddPathDiversityFillBest)
	assert.Equal(t, []int{0, 2}, selected, "distinct next-hops must be covered first")

	// The selection is deterministic: recomputing yields the same set.
	assert.Equal(t, selected, SelectAddPaths(candidates, 2, AddPathDiversityNexthop, AddPathDiversityFillBest))
}

func TestSelectAddPathsNeighborASDiversity(t *testing.T) {
	// Distinct next-hops but only two distinct neighbor ASes; the two best
	// paths come from the same neighbor AS.
	candidates := []*Path{
		addPathTestCandidate(t, 400, "192.0.2.1", 65001),
		addPathTestCandidate(t, 300, "192.0.2.2", 65001),
		addPathTestCandidate(t, 200, "192.0.2.3", 65002),
		addPathTestCandidate(t, 100, "192.0.2.4", 65003),
	}
	selected := SelectAddPaths(candidates, 2, AddPathDiversityNeighborAS, AddPathDiversityFillBest)
	assert.Equal(t, []int{0, 2}, selected, "distinct neighbor ASes must be covered first")

	// Next-hop diversity sees four distinct next-hops and keeps the best two.
	selected = SelectAddPaths(candidates, 2, AddPathDiversityNexthop, AddPathDiversityFillBest)
	assert.Equal(t, []int{0, 1}, selected)
}

func TestSelectAddPathsFillModes(t *testing.T) {
	// Only one distinct source for two send slots.
	candidates := []*Path{
		addPathTestCandidate(t, 400, "192.0.2.1", 65001),
		addPathTestCandidate(t, 300, "192.0.2.1", 65001),
	}
	// Best-effort fill backfills the remaining slot with the next best path.
	assert.Equal(t, []int{0, 1}, SelectAddPaths(candidates, 2, AddPathDiversityNexthop, AddPathDiversityFillBest))
	// Strict fill leaves the slot empty.
	assert.Equal(t, []int{0}, SelectAddPaths(candidates, 2, AddPathDiversityNexthop, AddPathDiversityFillStrict))
}

func TestSelectAddPathsSendMaxCoversAll(t *testing.T) {
	candidates := []*Path{
		addPathTestCandidate(t, 400, "192.0.2.1", 65001),
		addPathTestCandidate(t, 300, "192.0.2.1", 65001),
	}
	assert.Equal(t, []int{0, 1}, SelectAddPaths(candidates, 2, AddPathDiversityNexthop, AddPathDiversityFillBest))
	assert.Equal(t, []int{0, 1}, SelectAddPaths(candidates, 5, AddPathDiversityNexthop, AddPathDiversityFillBest))
}
