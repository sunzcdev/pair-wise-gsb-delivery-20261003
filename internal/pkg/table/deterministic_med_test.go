package table

import (
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/osrg/gobgp/v4/pkg/config/oc"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"

	"github.com/stretchr/testify/require"
)

type deterministicMEDCandidate struct {
	name     string
	neighbor uint32
	firstAS  uint32
	med      *uint32
	address  string
}

func deterministicMEDPath(t *testing.T, candidate deterministicMEDCandidate, nlri bgp.NLRI, timestamp time.Time, isWithdraw bool) *Path {
	t.Helper()

	attrs := []bgp.PathAttributeInterface{
		bgp.NewPathAttributeOrigin(bgp.BGP_ORIGIN_ATTR_TYPE_IGP),
		bgp.NewPathAttributeAsPath([]bgp.AsPathParamInterface{
			bgp.NewAs4PathParam(bgp.BGP_ASPATH_ATTR_TYPE_SEQ, []uint32{candidate.firstAS}),
		}),
	}
	if candidate.med != nil {
		attrs = append(attrs, bgp.NewPathAttributeMultiExitDisc(*candidate.med))
	}

	source := &PeerInfo{
		AS:      candidate.neighbor,
		LocalAS: 65000,
		ID:      netip.MustParseAddr(candidate.address),
		Address: netip.MustParseAddr(candidate.address),
	}
	path := NewPath(bgp.RF_IPv4_UC, source, bgp.PathNLRI{NLRI: nlri}, isWithdraw, attrs, timestamp, false)
	require.NotNil(t, path)
	return path
}

func deterministicMEDCandidates() []deterministicMEDCandidate {
	med := func(value uint32) *uint32 { return &value }
	return []deterministicMEDCandidate{
		{name: "as100-low-med", neighbor: 65100, firstAS: 65100, med: med(10), address: "192.0.2.1"},
		{name: "as100-equal-med", neighbor: 65100, firstAS: 65100, med: med(10), address: "192.0.2.3"},
		{name: "as100-high-med", neighbor: 65100, firstAS: 65100, med: med(30), address: "192.0.2.7"},
		{name: "as200-missing-med", neighbor: 65200, firstAS: 65200, med: nil, address: "192.0.2.4"},
		{name: "as300-medium-med", neighbor: 65300, firstAS: 65300, med: med(5), address: "192.0.2.5"},
		{name: "as400-highest-med", neighbor: 65400, firstAS: 65400, med: med(40), address: "192.0.2.6"},
	}
}

func TestDeterministicMEDOrderIndependent(t *testing.T) {
	nlri, err := bgp.NewIPAddrPrefix(netip.MustParsePrefix("203.0.113.0/24"))
	require.NoError(t, err)

	candidates := deterministicMEDCandidates()
	options := oc.RouteSelectionOptionsConfig{DeterministicMed: true}

	for _, order := range [][]int{
		{0, 1, 2, 3, 4, 5},
		{5, 4, 3, 2, 1, 0},
		{3, 0, 5, 4, 1, 2},
		{2, 5, 4, 0, 3, 1},
	} {
		d := newDestination(nlri, 0)
		baseTime := time.Unix(1_000, 0)
		for index, candidateIndex := range order {
			candidate := candidates[candidateIndex]
			path := deterministicMEDPath(t, candidate, nlri, baseTime.Add(time.Duration(index)*time.Second), false)
			_, oldPath := d.Calculate(logger, path, options)
			require.Nil(t, oldPath)
		}

		require.Equal(t, "as100-low-med", candidateName(d.GetBestPath(GLOBAL_RIB_NAME, 0)))

		bulk := newDestination(nlri, 0)
		bulk.selectionOptions = options
		bulk.knownPathList = make([]*Path, len(order))
		for index, candidateIndex := range order {
			candidate := candidates[candidateIndex]
			bulk.knownPathList[index] = deterministicMEDPath(t, candidate, nlri, baseTime.Add(time.Duration(index)*time.Second), false)
		}
		sortPaths(bulk.knownPathList, options)
		require.Equal(t, "as100-low-med", candidateName(bulk.GetBestPath(GLOBAL_RIB_NAME, 0)))
	}
}

func TestDeterministicMEDWithdrawAndReAdd(t *testing.T) {
	nlri, err := bgp.NewIPAddrPrefix(netip.MustParsePrefix("203.0.113.0/24"))
	require.NoError(t, err)

	candidates := deterministicMEDCandidates()
	options := oc.RouteSelectionOptionsConfig{DeterministicMed: true}
	baseTime := time.Unix(1_000, 0)
	d := newDestination(nlri, 0)
	for index, candidate := range candidates {
		_, oldPath := d.Calculate(logger, deterministicMEDPath(t, candidate, nlri, baseTime.Add(time.Duration(index)*time.Second), false), options)
		require.Nil(t, oldPath)
	}

	winner := d.GetBestPath(GLOBAL_RIB_NAME, 0)
	withdraw := deterministicMEDPath(t, candidates[0], nlri, baseTime, true)
	_, withdrawnPath := d.Calculate(logger, withdraw, options)
	require.NotNil(t, withdrawnPath)
	require.Equal(t, "as100-equal-med", candidateName(d.GetBestPath(GLOBAL_RIB_NAME, 0)))

	_, replacedPath := d.Calculate(logger, deterministicMEDPath(t, candidates[0], nlri, baseTime.Add(10*time.Second), false), options)
	require.Nil(t, replacedPath)
	readded := d.GetBestPath(GLOBAL_RIB_NAME, 0)
	require.Equal(t, "as100-low-med", candidateName(readded))
	require.True(t, readded.EqualBySourceAndPathID(winner))
}

func TestDeterministicMEDWithAlwaysCompareAndECMP(t *testing.T) {
	nlri, err := bgp.NewIPAddrPrefix(netip.MustParsePrefix("203.0.113.0/24"))
	require.NoError(t, err)

	candidates := deterministicMEDCandidates()
	baseTime := time.Unix(1_000, 0)

	t.Run("always compare compares missing med as zero globally", func(t *testing.T) {
		options := oc.RouteSelectionOptionsConfig{
			AlwaysCompareMed: true,
			DeterministicMed: true,
		}
		d := newDestination(nlri, 0)
		for index, candidate := range []int{4, 3, 2, 1, 0} {
			_, oldPath := d.Calculate(logger, deterministicMEDPath(t, candidates[candidate], nlri, baseTime.Add(time.Duration(index)*time.Second), false), options)
			require.Nil(t, oldPath)
		}
		require.Equal(t, "as200-missing-med", candidateName(d.GetBestPath(GLOBAL_RIB_NAME, 0)))
	})

	t.Run("ecmp stays inside winning group", func(t *testing.T) {
		options := oc.RouteSelectionOptionsConfig{DeterministicMed: true}
		paths := []*Path{
			deterministicMEDPath(t, candidates[0], nlri, baseTime, false),
			deterministicMEDPath(t, candidates[1], nlri, baseTime.Add(time.Second), false),
			deterministicMEDPath(t, candidates[2], nlri, baseTime.Add(2*time.Second), false),
			deterministicMEDPath(t, candidates[3], nlri, baseTime.Add(3*time.Second), false),
		}
		sortPaths(paths, options)
		d := newDestination(nlri, 0, paths...)
		d.selectionOptions = options

		best := getMultiBestPath(GLOBAL_RIB_NAME, d.GetAllKnownPathList(), options)
		require.Len(t, best, 2)
		require.Equal(t, "as100-low-med", candidateName(best[0]))
		require.Equal(t, "as100-equal-med", candidateName(best[1]))
	})

	t.Run("always compare allows equal global med across groups", func(t *testing.T) {
		options := oc.RouteSelectionOptionsConfig{AlwaysCompareMed: true}
		med0 := uint32(10)
		candidate0 := deterministicMEDCandidate{name: "equal-as100", neighbor: 65100, firstAS: 65100, med: &med0, address: "192.0.2.10"}
		candidate1 := deterministicMEDCandidate{name: "equal-as200", neighbor: 65200, firstAS: 65200, med: &med0, address: "192.0.2.20"}
		paths := []*Path{
			deterministicMEDPath(t, candidate0, nlri, baseTime, false),
			deterministicMEDPath(t, candidate1, nlri, baseTime, false),
		}
		sortPaths(paths, options)

		best := getMultiBestPath(GLOBAL_RIB_NAME, paths, options)
		require.Len(t, best, 2)
		require.True(t, slices.Equal([]string{"equal-as100", "equal-as200"}, []string{candidateName(best[0]), candidateName(best[1])}))
	})
}

func TestDeterministicMEDDefaultKeepsExistingBehavior(t *testing.T) {
	nlri, err := bgp.NewIPAddrPrefix(netip.MustParsePrefix("203.0.113.0/24"))
	require.NoError(t, err)

	med := func(value uint32) *uint32 { return &value }
	candidates := []deterministicMEDCandidate{
		{name: "high-med-arrives-first", neighbor: 65200, firstAS: 65200, med: med(100), address: "198.51.100.1"},
		{name: "low-med-arrives-second", neighbor: 65100, firstAS: 65100, med: med(1), address: "198.51.100.2"},
	}
	d := newDestination(nlri, 0)
	baseTime := time.Unix(1_000, 0)
	for index, candidate := range candidates {
		_, oldPath := d.Calculate(logger, deterministicMEDPath(t, candidate, nlri, baseTime.Add(time.Duration(index)*time.Second), false), oc.RouteSelectionOptionsConfig{})
		require.Nil(t, oldPath)
	}

	require.Equal(t, "high-med-arrives-first", candidateName(d.GetBestPath(GLOBAL_RIB_NAME, 0)))

	d = newDestination(nlri, 0)
	for index, candidateIndex := range []int{1, 0} {
		_, oldPath := d.Calculate(logger, deterministicMEDPath(t, candidates[candidateIndex], nlri, baseTime.Add(time.Duration(index)*time.Second), false), oc.RouteSelectionOptionsConfig{})
		require.Nil(t, oldPath)
	}
	require.Equal(t, "low-med-arrives-second", candidateName(d.GetBestPath(GLOBAL_RIB_NAME, 0)))
}

func candidateName(path *Path) string {
	switch path.GetSource().Address.String() {
	case "192.0.2.1":
		return "as100-low-med"
	case "192.0.2.2":
		return "unused"
	case "192.0.2.3":
		return "as100-equal-med"
	case "192.0.2.7":
		return "as100-high-med"
	case "192.0.2.4":
		return "as200-missing-med"
	case "192.0.2.5":
		return "as300-medium-med"
	case "192.0.2.6":
		return "as400-highest-med"
	case "192.0.2.10":
		return "equal-as100"
	case "192.0.2.20":
		return "equal-as200"
	case "198.51.100.1":
		return "high-med-arrives-first"
	case "198.51.100.2":
		return "low-med-arrives-second"
	default:
		return path.GetSource().Address.String()
	}
}
