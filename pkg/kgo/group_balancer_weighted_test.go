package kgo

import (
	"testing"

	"github.com/twmb/franz-go/pkg/kmsg"
)

func TestWeightedBalancerProtocol(t *testing.T) {
	t.Parallel()
	if got := WeightedStickyBalancer().ProtocolName(); got != "weighted-sticky" {
		t.Fatalf("eager protocol name %q", got)
	}
	if WeightedStickyBalancer().IsCooperative() {
		t.Fatal("weighted-sticky should not be cooperative")
	}
	if got := CooperativeWeightedStickyBalancer().ProtocolName(); got != "cooperative-weighted-sticky" {
		t.Fatalf("cooperative protocol name %q", got)
	}
	if !CooperativeWeightedStickyBalancer().IsCooperative() {
		t.Fatal("cooperative-weighted-sticky should be cooperative")
	}
}

func TestWeightedUserDataRoundTrip(t *testing.T) {
	t.Parallel()
	w, prior, gen := decodeWeightedUserData(
		encodeWeightedUserData(3.5, map[string][]int32{"t": {0, 2}}, 7),
	)
	if w != 3.5 || gen != 7 || len(prior["t"]) != 2 || prior["t"][1] != 2 {
		t.Fatalf("round trip mismatch: w=%v gen=%d prior=%v", w, gen, prior)
	}

	for _, bad := range [][]byte{nil, {}, {0, 2}, {0, 1, 0, 0, 0, 0}} {
		if w, prior, gen := decodeWeightedUserData(bad); w != 1 || prior != nil || gen != -1 {
			t.Fatalf("bad payload %v decoded as w=%v prior=%v gen=%d", bad, w, prior, gen)
		}
	}
}

// Weighted balancing through the public API must split cost by capacity: with
// weights 1 and 4 and five unit-cost partitions, the heavy member takes four.
func TestWeightedBalancerSplitsByCapacity(t *testing.T) {
	t.Parallel()

	small := WeightedStickyBalancer().WithWeight(1)
	big := WeightedStickyBalancer().WithWeight(4)

	members := []kmsg.JoinGroupResponseMember{
		{MemberID: "big", ProtocolMetadata: big.JoinGroupMetadata([]string{"t"}, nil, 0)},
		{MemberID: "small", ProtocolMetadata: small.JoinGroupMetadata([]string{"t"}, nil, 0)},
	}

	gb, _, err := big.MemberBalancer(members)
	if err != nil {
		t.Fatal(err)
	}
	into, err := gb.(GroupMemberBalancerOrError).BalanceOrError(map[string]int32{"t": 5})
	if err != nil {
		t.Fatal(err)
	}
	plan := into.(*BalancePlan).AsMemberIDMap()
	if got := len(plan["big"]["t"]); got != 4 {
		t.Fatalf("big member takes 4/5, got %d (%v)", got, plan)
	}
	if got := len(plan["small"]["t"]); got != 1 {
		t.Fatalf("small member takes 1/5, got %d (%v)", got, plan)
	}
}

// A member whose UserData we do not understand must be treated as weight 1
// rather than failing the balance.
func TestWeightedBalancerUnknownMemberDefaultsToOne(t *testing.T) {
	t.Parallel()

	big := WeightedStickyBalancer().WithWeight(4)
	members := []kmsg.JoinGroupResponseMember{
		{MemberID: "big", ProtocolMetadata: big.JoinGroupMetadata([]string{"t"}, nil, 0)},
		// Plain consumer metadata, no weighted UserData envelope.
		{MemberID: "peer", ProtocolMetadata: simpleMemberMetadata([]string{"t"}, 0)},
	}
	gb, _, err := big.MemberBalancer(members)
	if err != nil {
		t.Fatal(err)
	}
	into, err := gb.(GroupMemberBalancerOrError).BalanceOrError(map[string]int32{"t": 5})
	if err != nil {
		t.Fatal(err)
	}
	plan := into.(*BalancePlan).AsMemberIDMap()
	if n := len(plan["big"]["t"]) + len(plan["peer"]["t"]); n != 5 {
		t.Fatalf("expected 5 partitions assigned, got %d (%v)", n, plan)
	}
}

// Cooperative weighted balancing must not hand a still-owned partition to a
// newly joining member in the same round: it is dropped from the first plan so
// the current owner revokes it, and it is assigned in the following rebalance.
func TestCooperativeWeightedNoImmediateHandover(t *testing.T) {
	t.Parallel()

	owner := CooperativeWeightedStickyBalancer().WithWeight(1)
	joiner := CooperativeWeightedStickyBalancer().WithWeight(1)

	// The owner already holds all four partitions from generation 5.
	ownerMeta := owner.JoinGroupMetadata(
		[]string{"t"},
		map[string][]int32{"t": {0, 1, 2, 3}},
		5,
	)
	joinerMeta := joiner.JoinGroupMetadata([]string{"t"}, nil, -1)

	members := []kmsg.JoinGroupResponseMember{
		{MemberID: "joiner", ProtocolMetadata: joinerMeta},
		{MemberID: "owner", ProtocolMetadata: ownerMeta},
	}

	gb, _, err := owner.MemberBalancer(members)
	if err != nil {
		t.Fatal(err)
	}
	into, err := gb.(GroupMemberBalancerOrError).BalanceOrError(map[string]int32{"t": 4})
	if err != nil {
		t.Fatal(err)
	}
	plan := into.(*BalancePlan).AsMemberIDMap()

	if got := plan["joiner"]["t"]; len(got) != 0 {
		t.Fatalf("joiner received not-yet-revoked partitions %v", got)
	}
	if got := len(plan["owner"]["t"]); got > 4 {
		t.Fatalf("owner assigned unexpected partitions: %v", plan["owner"])
	}

	// Second round: the owner now reports only what it kept. Whatever was
	// revoked is free and must be assigned to the joiner.
	kept := map[string][]int32{"t": plan["owner"]["t"]}
	ownerMeta2 := owner.JoinGroupMetadata([]string{"t"}, kept, 6)
	joinerMeta2 := joiner.JoinGroupMetadata([]string{"t"}, nil, 6)
	members2 := []kmsg.JoinGroupResponseMember{
		{MemberID: "joiner", ProtocolMetadata: joinerMeta2},
		{MemberID: "owner", ProtocolMetadata: ownerMeta2},
	}
	gb2, _, err := owner.MemberBalancer(members2)
	if err != nil {
		t.Fatal(err)
	}
	into2, err := gb2.(GroupMemberBalancerOrError).BalanceOrError(map[string]int32{"t": 4})
	if err != nil {
		t.Fatal(err)
	}
	plan2 := into2.(*BalancePlan).AsMemberIDMap()

	var assigned int
	for _, topics := range plan2 {
		assigned += len(topics["t"])
	}
	if assigned != 4 {
		t.Fatalf("after revoke round expected all 4 assigned, got %d (%v)", assigned, plan2)
	}
	if got := len(plan2["joiner"]["t"]); got == 0 {
		t.Fatalf("joiner should receive revoked partitions in round two, got %v", plan2)
	}
}
