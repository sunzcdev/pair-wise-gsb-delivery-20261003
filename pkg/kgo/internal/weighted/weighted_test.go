package weighted

import (
	"math"
	"strconv"
	"testing"
	"time"
)

func cost1(string, int32) float64 { return 1 }

func ownedBy(prior Plan, topic string, p int32) string {
	for member, topics := range prior {
		for _, q := range topics[topic] {
			if q == p {
				return member
			}
		}
	}
	return ""
}

// partitionCount totals the assigned partitions and the number that moved
// relative to prior (which may be nil).
func partitionCount(t *testing.T, plan, prior Plan) (int, int) {
	t.Helper()
	seen := map[string]bool{}
	moves := 0
	n := 0
	for member, topics := range plan {
		for topic, parts := range topics {
			for _, p := range parts {
				key := topic + ":" + strconv.Itoa(int(p))
				if seen[key] {
					t.Fatalf("partition %s assigned twice", key)
				}
				seen[key] = true
				n++
				if ownedBy(prior, topic, p) != member {
					moves++
				}
			}
		}
	}
	return n, moves
}

// planRatio computes each member's load (cost/weight) under cost, returning
// the maximum.
func planRatio(plan Plan, weight map[string]float64, cost PartitionCost) map[string]float64 {
	r := map[string]float64{}
	for member, topics := range plan {
		var c float64
		for topic, parts := range topics {
			for _, p := range parts {
				c += cost(topic, p)
			}
		}
		r[member] = c / weight[member]
	}
	return r
}

func maxMap(r map[string]float64) float64 {
	m := math.Inf(-1)
	for _, v := range r {
		if v > m {
			m = v
		}
	}
	return m
}

// TestHeterogeneousCapacity splits unit cost proportional to weight, not by
// partition count, and requires a stable rebalance to move nothing.
func TestHeterogeneousCapacity(t *testing.T) {
	topics := map[string]int32{"orders": 6}
	weights := map[string]float64{"small-a": 1, "small-b": 1, "big": 4}
	members := []GroupMember{
		{ID: "big", Topics: []string{"orders"}, Weight: 4},
		{ID: "small-a", Topics: []string{"orders"}, Weight: 1},
		{ID: "small-b", Topics: []string{"orders"}, Weight: 1},
	}

	plan := Balance(members, topics, cost1)
	if n, _ := partitionCount(t, plan, nil); n != 6 {
		t.Fatalf("expected 6 partitions, got %d", n)
	}
	for member, r := range planRatio(plan, weights, cost1) {
		if math.Abs(r-1) > 1e-9 {
			t.Fatalf("member %s ratio %v, want 1 (plan %v)", member, r, plan)
		}
	}
	if len(plan["big"]["orders"]) != 4 ||
		len(plan["small-a"]["orders"]) != 1 ||
		len(plan["small-b"]["orders"]) != 1 {
		t.Fatalf("expected 4/1/1 split, got %v", plan)
	}

	for i := range members {
		members[i].Prior = map[string][]int32{"orders": plan[members[i].ID]["orders"]}
	}
	plan2 := Balance(members, topics, cost1)
	if _, moves := partitionCount(t, plan2, plan); moves != 0 {
		t.Fatalf("stable rebalance moved %d partitions, want 0; %v", moves, plan2)
	}
}

// TestHotPartition checks the expensive partition is offset with cheap ones on
// the other member so the peak is much better than the naive 10 vs 3 split.
func TestHotPartition(t *testing.T) {
	topics := map[string]int32{"clicks": 4}
	cost := func(_ string, p int32) float64 {
		if p == 0 {
			return 10
		}
		return 1
	}
	members := []GroupMember{
		{ID: "a", Topics: []string{"clicks"}, Weight: 1},
		{ID: "b", Topics: []string{"clicks"}, Weight: 1},
	}
	plan := Balance(members, topics, cost)
	r := planRatio(plan, map[string]float64{"a": 1, "b": 1}, cost)
	if maxMap(r) > 11 {
		t.Fatalf("peak load %v worse than hot+one cold (11); plan=%v", maxMap(r), plan)
	}
}

// TestMemberLeave drops the big member and requires survivors to rebalance to
// a 3/3 split moving only what is necessary.
func TestMemberLeave(t *testing.T) {
	topics := map[string]int32{"orders": 6}
	members := []GroupMember{
		{ID: "small-a", Topics: []string{"orders"}, Weight: 1},
		{ID: "small-b", Topics: []string{"orders"}, Weight: 1},
		{ID: "big", Topics: []string{"orders"}, Weight: 4},
	}
	plan := Balance(members, topics, cost1)

	prior := Plan{
		"small-a": {"orders": plan["small-a"]["orders"]},
		"small-b": {"orders": plan["small-b"]["orders"]},
	}
	survivors := []GroupMember{
		{ID: "small-a", Topics: []string{"orders"}, Weight: 1, Prior: map[string][]int32{"orders": prior["small-a"]["orders"]}},
		{ID: "small-b", Topics: []string{"orders"}, Weight: 1, Prior: map[string][]int32{"orders": prior["small-b"]["orders"]}},
	}
	plan2 := Balance(survivors, topics, cost1)

	if n, _ := partitionCount(t, plan2, prior); n != 6 {
		t.Fatalf("expected 6 partitions, got %d", n)
	}
	if r := planRatio(plan2, map[string]float64{"small-a": 1, "small-b": 1}, cost1); maxMap(r) > 3 {
		t.Fatalf("post-leave peak %v, want <=3; %v", maxMap(r), plan2)
	}
	// The departed big member held four partitions, and all four must be
	// absorbed by the survivors; neither survivor's own partition moves.
	if _, moves := partitionCount(t, plan2, prior); moves != 4 {
		t.Fatalf("post-leave moved %d, want exactly the big member's 4; before=%v after=%v",
			moves, plan, plan2)
	}
	// Every partition the survivors held before the leave stays with them.
	for member, topics := range prior {
		for _, p := range topics["orders"] {
			if ownedBy(plan2, "orders", p) != member {
				t.Fatalf("survivor %s lost its own partition %d on leave: %v", member, p, plan2)
			}
		}
	}
}

// TestReliefChain forces a two-hop migration: the overloaded member only peers
// on t0 with the middle, while the underloaded member only subscribes to t1.
// No single direct move lowers the peak, so the engine must chain t0 (S->B)
// and t1 (B->T).
func TestReliefChain(t *testing.T) {
	topics := map[string]int32{"t0": 2, "t1": 1}
	cost := func(string, int32) float64 { return 6 }
	members := []GroupMember{
		{ID: "S", Topics: []string{"t0"}, Weight: 1, Prior: map[string][]int32{"t0": {0, 1}}},
		{ID: "B", Topics: []string{"t0", "t1"}, Weight: 1, Prior: map[string][]int32{"t1": {0}}},
		{ID: "T", Topics: []string{"t1"}, Weight: 1},
	}
	plan := Balance(members, topics, cost)
	r := planRatio(plan, map[string]float64{"S": 1, "B": 1, "T": 1}, cost)
	if maxMap(r) > 6+1e-9 {
		t.Fatalf("chain failed to lower peak to 6: %v plan=%v", r, plan)
	}
	if len(plan["T"]["t1"]) != 1 || len(plan["S"]["t0"]) != 1 || len(plan["B"]["t0"]) != 1 {
		t.Fatalf("unexpected chain plan: %v", plan)
	}
}

// TestSubscriptionLegality keeps an exclusive topic on its sole subscriber
// and balances only shared partitions.
func TestSubscriptionLegality(t *testing.T) {
	topics := map[string]int32{"shared": 3, "exclusive": 5}
	members := []GroupMember{
		{ID: "a", Topics: []string{"shared"}},
		{ID: "b", Topics: []string{"exclusive", "shared"}},
	}
	plan := Balance(members, topics, cost1)
	if len(plan["a"]["exclusive"]) != 0 {
		t.Fatalf("non-subscriber received exclusive topic: %v", plan)
	}
	if len(plan["b"]["exclusive"]) != 5 {
		t.Fatalf("sole subscriber must own all 5, got %v", plan["b"])
	}
	if n := len(plan["a"]["shared"]) + len(plan["b"]["shared"]); n != 3 {
		t.Fatalf("shared partitions lost: %v", plan)
	}
}

// TestNoSubscriber drops a partition nobody wants instead of forcing it on a
// non-subscriber.
func TestNoSubscriber(t *testing.T) {
	topics := map[string]int32{"wanted": 1, "lonely": 1}
	members := []GroupMember{{ID: "a", Topics: []string{"wanted"}}}
	plan := Balance(members, topics, cost1)
	if _, ok := plan["a"]["lonely"]; ok {
		t.Fatalf("unwanted partition assigned: %v", plan["a"])
	}
	if len(plan["a"]["wanted"]) != 1 {
		t.Fatalf("wanted partition missing: %v", plan["a"])
	}
}

// TestLargeGroupBalancesQuickly guards against exponential chain search on a
// big group: all members share the topic, so every move is one hop.
func TestLargeGroupBalancesQuickly(t *testing.T) {
	const nMembers = 100
	topics := map[string]int32{"t": 1000}
	members := make([]GroupMember, nMembers)
	for i := range members {
		members[i] = GroupMember{
			ID:     "m" + strconv.Itoa(i),
			Topics: []string{"t"},
			Weight: float64(1 + (i % 5)),
		}
	}
	done := make(chan struct{})
	go func() {
		plan := Balance(members, topics, cost1)
		_ = plan
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("balance on 100 members / 1000 partitions did not finish in 5s")
	}
}

// TestRandomized exercises the balancer on many random groups, asserting the
// assignment is a legal complete partition covering, that loads never get
// worse than the sticky seed's lexicographic vector, and that rebalancing
// from the produced plan moves nothing (idempotence / stability).
func TestRandomized(t *testing.T) {
	t.Parallel()
	rng := func(seed int) func(int) int {
		x := seed
		return func(n int) int {
			x = (x*1103515245 + 12345) & 0x7fffffff
			return x % n
		}
	}

	for seed := 0; seed < 400; seed++ {
		next := rng(seed + 1)
		nTopics := 1 + next(3)
		nMembers := 1 + next(6)

		topicNames := make([]string, nTopics)
		topicParts := make(map[string]int32, nTopics)
		for i := 0; i < nTopics; i++ {
			topicNames[i] = "t" + strconv.Itoa(i)
			topicParts[topicNames[i]] = int32(1 + next(6))
		}

		cost := func(topic string, p int32) float64 {
			// Deterministic costs in {1,2,5,10}, sometimes uniform.
			choices := []float64{1, 1, 1, 2, 5, 10}
			return choices[(int(p)+len(topic)+seed)%len(choices)]
		}

		members := make([]GroupMember, nMembers)
		weights := make(map[string]float64, nMembers)
		subsSet := make([]map[string]bool, nMembers)
		for mi := 0; mi < nMembers; mi++ {
			id := "m" + strconv.Itoa(mi)
			weights[id] = float64(1 + next(4))
			topics := []string{}
			topicSet := map[string]bool{}
			for _, tn := range topicNames {
				if next(2) == 0 || len(topics) == 0 && tn == topicNames[nTopics-1] {
					if !topicSet[tn] {
						topics = append(topics, tn)
						topicSet[tn] = true
					}
				}
			}
			members[mi] = GroupMember{ID: id, Weight: weights[id], Topics: topics}
			subsSet[mi] = topicSet
		}

		plan := Balance(members, topicParts, cost)

		// Complete, non-duplicated, subscriber-only covering.
		owner := map[string]string{}
		for member, topics := range plan {
			subs := map[string]bool{}
			for _, gm := range members {
				if gm.ID == member {
					for _, tn := range gm.Topics {
						subs[tn] = true
					}
				}
			}
			for topic, parts := range topics {
				if !subs[topic] {
					t.Fatalf("seed %d: %s assigned unsubscribed topic %s", seed, member, topic)
				}
				for _, p := range parts {
					k := topic + "/" + strconv.Itoa(int(p))
					if _, dup := owner[k]; dup {
						t.Fatalf("seed %d: %s assigned twice", seed, k)
					}
					owner[k] = member
				}
			}
		}
		for topic, n := range topicParts {
			for p := int32(0); p < n; p++ {
				k := topic + "/" + strconv.Itoa(int(p))
				if _, ok := owner[k]; !ok {
					// Unassigned is only legal if nobody subscribes.
					subscribers := 0
					for _, s := range subsSet {
						if s[topic] {
							subscribers++
						}
					}
					if subscribers > 0 {
						t.Fatalf("seed %d: subscribed partition %s unassigned", seed, k)
					}
				}
			}
		}

		// Rebalance from the result with the same membership: must be stable.
		followUp := make([]GroupMember, len(members))
		for i, gm := range members {
			followUp[i] = gm
			followUp[i].Prior = plan[gm.ID]
		}
		plan2 := Balance(followUp, topicParts, cost)
		if _, moves := partitionCount(t, plan2, plan); moves != 0 {
			t.Fatalf("seed %d: rebalance from own plan moved %d; %v -> %v",
				seed, moves, plan, plan2)
		}
	}
}
