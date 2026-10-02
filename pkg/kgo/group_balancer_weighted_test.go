package kgo

import (
	"reflect"
	"testing"

	"github.com/twmb/franz-go/pkg/kgo/internal/sticky"
	"github.com/twmb/franz-go/pkg/kmsg"
)

type weightedTestMember struct {
	id         string
	weight     float64
	topics     []string
	assignment map[string][]int32
	generation int32
}

func weightedMetadata(t *testing.T, cooperative bool, capacity float64, member weightedTestMember) kmsg.JoinGroupResponseMember {
	t.Helper()

	current := make(map[string][]int32, len(member.assignment))
	for topic, partitions := range member.assignment {
		current[topic] = append([]int32(nil), partitions...)
	}
	balancer, err := newWeightedStickyBalancer(capacity, nil, cooperative)
	if err != nil {
		t.Fatal(err)
	}
	return kmsg.JoinGroupResponseMember{
		MemberID:         member.id,
		ProtocolMetadata: balancer.JoinGroupMetadata(member.topics, current, member.generation),
	}
}

func balanceWeighted(t *testing.T, cooperative bool, members []weightedTestMember, topics map[string]int32, costs map[string]map[int32]float64) *BalancePlan {
	t.Helper()

	jMembers := make([]kmsg.JoinGroupResponseMember, len(members))
	for i := range members {
		jMembers[i] = weightedMetadata(t, cooperative, members[i].weight, members[i])
	}
	balancer := &weightedStickyBalancer{
		cooperative: cooperative,
		capacity:    1,
		partitionCost: func(topic string, partition int32) float64 {
			if costs[topic] != nil {
				if cost, ok := costs[topic][partition]; ok {
					return cost
				}
			}
			return 1
		},
	}
	b, err := NewConsumerBalancer(balancer, jMembers)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := b.BalanceOrError(topics)
	if err != nil {
		t.Fatal(err)
	}
	return plan.(*BalancePlan)
}

func weightedLoads(plan map[string]map[string][]int32, weights map[string]float64, costs map[string]map[int32]float64) map[string]float64 {
	loads := make(map[string]float64, len(plan))
	for member := range weights {
		loads[member] = 0
	}
	for member, topics := range plan {
		for topic, partitions := range topics {
			for _, partition := range partitions {
				cost := 1.0
				if costs[topic] != nil {
					if explicit, ok := costs[topic][partition]; ok {
						cost = explicit
					}
				}
				loads[member] += cost
			}
		}
	}
	for member, load := range loads {
		loads[member] = load / weights[member]
	}
	return loads
}

func weightedMoves(before, after map[string]map[string][]int32) int {
	owners := make(map[string]map[int32]string)
	for member, topics := range before {
		for topic, partitions := range topics {
			if owners[topic] == nil {
				owners[topic] = make(map[int32]string)
			}
			for _, partition := range partitions {
				owners[topic][partition] = member
			}
		}
	}
	moves := 0
	for member, topics := range after {
		for topic, partitions := range topics {
			for _, partition := range partitions {
				if owners[topic][partition] != member {
					moves++
				}
			}
		}
	}
	return moves
}

func TestWeightedStickyBalancerHeterogeneousAndHotPartition(t *testing.T) {
	t.Parallel()

	topics := map[string]int32{"t": 6}
	weights := map[string]float64{"big": 3, "small": 1}
	costs := map[string]map[int32]float64{
		"t": {0: 6, 1: 1, 2: 1, 3: 1, 4: 1, 5: 1},
	}
	before := map[string]map[string][]int32{
		"big":   {"t": {0, 1, 2, 3, 4, 5}},
		"small": {},
	}

	plan := balanceWeighted(t, false, []weightedTestMember{
		{id: "big", weight: 3, topics: []string{"t"}, assignment: before["big"], generation: 1},
		{id: "small", weight: 1, topics: []string{"t"}, generation: 1},
	}, topics, costs).plan

	expected := map[string]map[string][]int32{
		"big":   {"t": {0, 3, 4, 5}},
		"small": {"t": {1, 2}},
	}
	if !reflect.DeepEqual(plan, expected) {
		t.Fatalf("got %v, want %v", plan, expected)
	}
	if moves := weightedMoves(before, plan); moves != 2 {
		t.Fatalf("got %d moves, want 2", moves)
	}
	loads := weightedLoads(plan, weights, costs)
	if got := loads["big"]; got != 3 {
		t.Fatalf("big utilization got %v, want 3", got)
	}
	if got := loads["small"]; got != 2 {
		t.Fatalf("small utilization got %v, want 2", got)
	}
}

func TestWeightedStickyBalancerMemberLeaves(t *testing.T) {
	t.Parallel()

	topics := map[string]int32{"t": 6}
	weights := map[string]float64{"big": 3, "small-a": 1, "small-b": 1}
	costs := map[string]map[int32]float64{
		"t": {0: 6, 1: 1, 2: 1, 3: 1, 4: 1, 5: 1},
	}
	before := map[string]map[string][]int32{
		"big":     {"t": {0, 2, 3, 4, 5}},
		"small-a": {"t": {1}},
	}

	plan := balanceWeighted(t, false, []weightedTestMember{
		{id: "big", weight: 3, topics: []string{"t"}, assignment: before["big"], generation: 2},
		{id: "small-b", weight: 1, topics: []string{"t"}, generation: 2},
	}, topics, costs).plan

	expected := map[string]map[string][]int32{
		"big":     {"t": {0, 3, 4, 5}},
		"small-b": {"t": {1, 2}},
	}
	if !reflect.DeepEqual(plan, expected) {
		t.Fatalf("got %v, want %v", plan, expected)
	}
	if moves := weightedMoves(before, plan); moves != 2 {
		t.Fatalf("got %d moved or newly available partitions, want 2", moves)
	}
	loads := weightedLoads(plan, weights, costs)
	if got := loads["big"]; got != 3 {
		t.Fatalf("big utilization got %v, want 3", got)
	}
	if got := loads["small-b"]; got != 2 {
		t.Fatalf("small-b utilization got %v, want 2", got)
	}
}

func TestWeightedStickyBalancerDropsPartitionsOutsideSubscription(t *testing.T) {
	t.Parallel()

	plan := balanceWeighted(t, false, []weightedTestMember{
		{
			id:         "a",
			weight:     1,
			topics:     []string{"keep"},
			assignment: map[string][]int32{"dropped": {0}, "keep": {0}},
			generation: 1,
		},
		{id: "b", weight: 1, topics: []string{"dropped"}, generation: 1},
	}, map[string]int32{"keep": 2, "dropped": 1}, nil).plan

	expected := map[string]map[string][]int32{
		"a": {"keep": {0, 1}},
		"b": {"dropped": {0}},
	}
	if !reflect.DeepEqual(plan, expected) {
		t.Fatalf("got %v, want %v", plan, expected)
	}
}

func TestCooperativeWeightedStickyBalancerWaitsForRevocation(t *testing.T) {
	t.Parallel()

	topics := map[string]int32{"t": 6}
	costs := map[string]map[int32]float64{
		"t": {0: 6, 1: 1, 2: 1, 3: 1, 4: 1, 5: 1},
	}
	initialMembers := []weightedTestMember{
		{id: "big", weight: 3, topics: []string{"t"}, assignment: map[string][]int32{"t": {0, 2, 3, 4, 5}}, generation: 2},
		{id: "small-a", weight: 1, topics: []string{"t"}, assignment: map[string][]int32{"t": {1}}, generation: 2},
	}

	first := balanceWeighted(t, true, append(initialMembers, weightedTestMember{
		id:     "small-b",
		weight: 1,
		topics: []string{"t"},
	}), topics, costs).plan
	expectedFirst := map[string]map[string][]int32{
		"big":     {"t": {0, 5}},
		"small-a": {"t": {1}},
		"small-b": {},
	}
	if !reflect.DeepEqual(first, expectedFirst) {
		t.Fatalf("first plan got %v, want %v", first, expectedFirst)
	}

	secondMembers := []weightedTestMember{
		{id: "big", weight: 3, topics: []string{"t"}, assignment: first["big"], generation: 3},
		{id: "small-a", weight: 1, topics: []string{"t"}, assignment: first["small-a"], generation: 3},
		{id: "small-b", weight: 1, topics: []string{"t"}, generation: 3},
	}
	second := balanceWeighted(t, true, secondMembers, topics, costs).plan
	expectedSecond := map[string]map[string][]int32{
		"big":     {"t": {0, 5}},
		"small-a": {"t": {1, 3}},
		"small-b": {"t": {2, 4}},
	}
	if !reflect.DeepEqual(second, expectedSecond) {
		t.Fatalf("second plan got %v, want %v", second, expectedSecond)
	}
}

func TestWeightedStickyBalancerRejectsInvalidMetadataAndCost(t *testing.T) {
	t.Parallel()

	if _, err := WeightedStickyBalancer(0, nil); err == nil {
		t.Fatal("expected invalid capacity to fail")
	}

	members := []kmsg.JoinGroupResponseMember{
		{MemberID: "a", ProtocolMetadata: simpleMemberMetadata([]string{"t"}, 0)},
	}
	if _, _, err := (&weightedStickyBalancer{}).MemberBalancer(members); err == nil {
		t.Fatal("expected non-weighted metadata to fail")
	}

	members = []kmsg.JoinGroupResponseMember{
		weightedMetadata(t, false, 1, weightedTestMember{id: "a", weight: 1, topics: []string{"t"}}),
	}
	balancer := &weightedStickyBalancer{
		capacity: 1,
		partitionCost: func(string, int32) float64 {
			return 0
		},
	}
	b, err := NewConsumerBalancer(balancer, members)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.BalanceOrError(map[string]int32{"t": 1}); err == nil {
		t.Fatal("expected invalid partition cost to fail")
	}
}

func TestWeightedUserDataRoundTrip(t *testing.T) {
	t.Parallel()

	assignment := []kmsg.StickyMemberMetadataCurrentAssignment{{
		Topic:      "t",
		Partitions: []int32{2, 5},
	}}
	encoded := sticky.EncodeWeightedUserData(2.5, assignment, 7)
	weight, decoded, err := sticky.DecodeWeightedUserData(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if weight != 2.5 || decoded.Generation != 7 || !reflect.DeepEqual(decoded.CurrentAssignment, assignment) {
		t.Fatalf("got weight %v generation %d assignment %v", weight, decoded.Generation, decoded.CurrentAssignment)
	}
}
