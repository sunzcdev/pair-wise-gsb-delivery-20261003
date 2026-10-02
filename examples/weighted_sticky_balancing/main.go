// This example shows the capacity-weighted, cost-aware sticky balancer in
// action without needing a running Kafka cluster. It plays the role of both
// group members (encoding their join metadata) and the leader (decoding the
// join group response and producing the assignment), so it can print the
// estimated cost distribution and the number of partitions that migrate.
//
// It walks four situations:
//
//  1. Heterogeneous capacity: one 4x machine next to two 1x machines.
//  2. A hot partition whose cost dwarfs the others.
//  3. A member leaving, so the survivors absorb its partitions while keeping
//     every partition they already held.
//  4. Cooperative rebalancing when a member joins: the moving partitions are
//     revoked first and only handed over in the next round.
package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
)

// partitionCosts is the leader-side estimate of how expensive each partition
// is to process. In a real program this might come from bytes-in rate,
// message rate, or measured CPU; here it is a hand-built table.
type partitionCosts map[string]map[int32]float64

func (c partitionCosts) of(topic string, partition int32) float64 {
	if ps, ok := c[topic]; ok {
		if cost, ok := ps[partition]; ok {
			return cost
		}
	}
	return 1
}

type member struct {
	id     string
	weight float64
	topics []string

	assignment map[string][]int32
	generation int32
}

type cluster struct {
	topics   map[string]int32
	costs    partitionCosts
	coop     bool
	gen      int32
	members  []*member
	byID     map[string]*member
	balancer *kgo.WeightedBalancer
	// prevOwner records the owner of every partition after the previous
	// installed assignment, including owners that have since left.
	prevOwner map[string]string
	// pendingRevoke holds partitions that are currently absent from a plan
	// during a cooperative revoke round, keyed topic/partition -> prior owner.
	pendingRevoke map[string]string
}

func newCluster(topics map[string]int32, costs partitionCosts, coop bool) *cluster {
	var b *kgo.WeightedBalancer
	if coop {
		b = kgo.CooperativeWeightedStickyBalancer()
	} else {
		b = kgo.WeightedStickyBalancer()
	}
	return &cluster{
		topics:        topics,
		costs:         costs,
		coop:          coop,
		byID:          make(map[string]*member),
		prevOwner:     make(map[string]string),
		pendingRevoke: make(map[string]string),
		balancer:      b.WithPartitionCost(costs.of),
	}
}

func (c *cluster) addMember(id string, weight float64, topics ...string) {
	m := &member{
		id:         id,
		weight:     weight,
		topics:     topics,
		assignment: make(map[string][]int32),
		generation: -1,
	}
	c.members = append(c.members, m)
	c.byID[id] = m
}

// rebalance performs one leader-side balance from the current members' join
// metadata, then installs the resulting assignment. It returns the plan and
// how many partitions moved to a different member than before.
func (c *cluster) rebalance() (map[string]map[string][]int32, int) {
	c.gen++
	joinMembers := make([]kmsg.JoinGroupResponseMember, 0, len(c.members))
	for _, m := range c.members {
		// Each member builds its own join metadata using its capacity.
		var bal *kgo.WeightedBalancer
		if c.coop {
			bal = kgo.CooperativeWeightedStickyBalancer().
				WithWeight(m.weight)
		} else {
			bal = kgo.WeightedStickyBalancer().
				WithWeight(m.weight)
		}
		joinMembers = append(joinMembers, kmsg.JoinGroupResponseMember{
			MemberID:         m.id,
			ProtocolMetadata: bal.JoinGroupMetadata(m.topics, m.assignment, m.generation),
		})
	}

	mb, _, err := c.balancer.MemberBalancer(joinMembers)
	if err != nil {
		panic(err)
	}

	into, err := mb.(kgo.GroupMemberBalancerOrError).BalanceOrError(c.topics)
	if err != nil {
		panic(err)
	}

	newPlan := make(map[string]map[string][]int32)
	for _, a := range into.IntoSyncAssignment() {
		assigned, err := c.balancer.ParseSyncAssignment(a.MemberAssignment)
		if err != nil {
			panic(err)
		}
		newPlan[a.MemberID] = assigned
	}

	moves := c.install(newPlan)
	return newPlan, moves
}

// install applies a plan to members and counts genuine migrations. Under
// cooperative rebalancing a partition that moves is first absent from the
// plan (the revoke round); a free -> assigned partition in the next round is
// counted as the migration completing.
func (c *cluster) install(newPlan map[string]map[string][]int32) int {
	moves := 0
	for _, m := range c.members {
		assigned := newPlan[m.id]
		m.assignment = make(map[string][]int32, len(assigned))
		for topic, parts := range assigned {
			cp := append([]int32(nil), parts...)
			sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
			m.assignment[topic] = cp
			for _, p := range cp {
				k := key(topic, p)
				// A partition lands on a new member either directly (eager)
				// or after a cooperative revoke round (pendingRevoke).
				if before := c.prevOwner[k]; before != "" && before != m.id {
					moves++
				} else if revoker := c.pendingRevoke[k]; revoker != "" && revoker != m.id {
					moves++
				}
			}
		}
		m.generation = c.gen
	}

	if c.coop {
		// Anything planned before but absent now is being revoked; record it
		// so the assignment round counts the eventual handover as a move.
		plannedNow := make(map[string]bool)
		for _, m := range c.members {
			for topic, parts := range m.assignment {
				for _, p := range parts {
					plannedNow[key(topic, p)] = true
				}
			}
		}
		for k, owner := range c.prevOwner {
			if !plannedNow[k] {
				c.pendingRevoke[k] = owner
			}
		}
		for k := range c.pendingRevoke {
			if plannedNow[k] {
				delete(c.pendingRevoke, k)
			}
		}
	}

	newOwners := make(map[string]string)
	for _, m := range c.members {
		for topic, parts := range m.assignment {
			for _, p := range parts {
				newOwners[key(topic, p)] = m.id
			}
		}
	}
	// Partitions absent during a cooperative revoke stay owned by the
	// revoker until the assignment round.
	for k, owner := range c.pendingRevoke {
		if _, present := newOwners[k]; !present {
			newOwners[k] = owner
		}
	}
	c.prevOwner = newOwners

	return moves
}

func key(topic string, p int32) string {
	return fmt.Sprintf("%s/%d", topic, p)
}

func (c *cluster) load(id string) (cost float64, ratio float64) {
	m := c.byID[id]
	for topic, parts := range m.assignment {
		for _, p := range parts {
			cost += c.costs.of(topic, p)
		}
	}
	return cost, cost / m.weight
}

func (c *cluster) describe(id string) string {
	m := c.byID[id]
	topics := make([]string, 0, len(m.assignment))
	for topic := range m.assignment {
		topics = append(topics, topic)
	}
	sort.Strings(topics)
	var sb strings.Builder
	for i, topic := range topics {
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "%s%v", topic, m.assignment[topic])
	}
	cost, ratio := c.load(id)
	fmt.Fprintf(&sb, "  -> cost=%.0f weight=%.0f load=%.2f", cost, m.weight, ratio)
	return sb.String()
}

func (c *cluster) printPlan(title string, moves int) {
	fmt.Printf("%s (migrations: %d)\n", title, moves)
	ids := make([]string, 0, len(c.members))
	for _, m := range c.members {
		ids = append(ids, m.id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		fmt.Printf("  %-5s %s\n", id, c.describe(id))
	}
	fmt.Println()
}

func main() {
	scenarioHeterogeneousCapacity()
	scenarioHotPartition()
	scenarioMemberLeave()
	scenarioCooperativeJoin()
}

func scenarioHeterogeneousCapacity() {
	fmt.Println("=== 1. Heterogeneous capacity, uniform cost ===")
	fmt.Println("Topic orders has 6 partitions, each cost 1. big has weight 4,")
	fmt.Println("small-a and small-b have weight 1. Balancing by partition count")
	fmt.Println("would give 2/2/2 (loads 0.5/2/2); balancing cost/weight gives:")
	fmt.Println()

	c := newCluster(
		map[string]int32{"orders": 6},
		partitionCosts{"orders": {}}, // every partition defaults to cost 1
		false,
	)
	c.addMember("big", 4, "orders")
	c.addMember("small-a", 1, "orders")
	c.addMember("small-b", 1, "orders")
	_, moves := c.rebalance()
	c.printPlan("initial weighted balance", moves)
}

func scenarioHotPartition() {
	fmt.Println("=== 2. A hot partition, two equal machines ===")
	fmt.Println("Topic clicks has 4 partitions; partition 0 costs 10 (the hot one),")
	fmt.Println("the rest cost 1. Balancing by count gives 2/2 partitions but loads")
	fmt.Println("of 11 vs 3 (or 10 vs 3 with the hot one isolated), a large skew.")
	fmt.Println("Cost/weight balancing isolates the indivisible cost-10 partition on")
	fmt.Println("one member and moves every cold partition to the other, the smallest")
	fmt.Println("possible descending load vector (10, 3).")
	fmt.Println()

	c := newCluster(
		map[string]int32{"clicks": 4},
		partitionCosts{"clicks": {0: 10}},
		false,
	)
	c.addMember("a", 1, "clicks")
	c.addMember("b", 1, "clicks")
	_, moves := c.rebalance()
	c.printPlan("cost-aware balance", moves)
}

func scenarioMemberLeave() {
	fmt.Println("=== 3. The big machine leaves ===")
	fmt.Println("Starting from the 4/1/1 weighted assignment, big departs. The two")
	fmt.Println("small machines keep every partition they already held and absorb the")
	fmt.Println("four that big owned, rebalanced toward equal load.")
	fmt.Println()

	c := newCluster(
		map[string]int32{"orders": 6},
		partitionCosts{"orders": {}},
		false,
	)
	c.addMember("big", 4, "orders")
	c.addMember("small-a", 1, "orders")
	c.addMember("small-b", 1, "orders")
	_, _ = c.rebalance()

	// big leaves; keep only the surviving members, carrying their assignments.
	survivors := []*member{c.byID["small-a"], c.byID["small-b"]}
	c.members = survivors
	delete(c.byID, "big")

	_, moves := c.rebalance()
	c.printPlan("after big leaves", moves)
}

func scenarioCooperativeJoin() {
	fmt.Println("=== 4. Cooperative rebalance on join (no premature handover) ===")
	fmt.Println("owner holds all four clicks partitions. A new equal-capacity member")
	fmt.Println("joins. In cooperative mode the partitions that must move are revoked")
	fmt.Println("first and handed over only in the following round, so processing")
	fmt.Println("never pauses and two members never consume one partition at once.")
	fmt.Println()

	c := newCluster(
		map[string]int32{"clicks": 4},
		partitionCosts{"clicks": {}},
		true,
	)
	c.addMember("owner", 1, "clicks")
	_, _ = c.rebalance() // owner gets everything

	c.addMember("joiner", 1, "clicks")
	_, moves1 := c.rebalance() // revoke round
	c.printPlan("join round 1: revoke before handover", moves1)

	_, moves2 := c.rebalance() // assignment round
	c.printPlan("join round 2: handover completes", moves2)
}
