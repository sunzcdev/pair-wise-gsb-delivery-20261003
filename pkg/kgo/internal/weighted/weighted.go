// Package weighted implements a capacity-weighted, cost-aware sticky
// partitioning strategy.
//
// Unlike the standard sticky strategy, which balances the *count* of
// partitions per member, this package balances estimated processing *cost*
// per unit of member capacity. Each member declares a capacity weight
// (bigger machines declare more), and each partition carries an estimated
// processing cost (hot partitions cost more). A member's load is
//
//	sum(cost of its partitions) / its capacity weight
//
// and the plan minimizes the maximum of these loads while keeping the
// assignment legal (a member can only consume topics it subscribes to) and
// sticky (partitions that do not need to move do not move).
package weighted

import (
	"math"
	"slices"
)

// GroupMember is a Kafka group member participating in a weighted balance.
type GroupMember struct {
	ID         string
	Topics     []string
	Weight     float64
	Generation int32

	// Cooperative switches competing prior-claim resolution to
	// generation-tagged ownership. Both modes supply their previous
	// assignment through Prior.
	Cooperative bool
	Prior       map[string][]int32
}

// Plan is the determined plan: member => topic => partitions.
type Plan map[string]map[string][]int32

// PartitionCost returns the estimated processing cost of a partition. A nil
// function, or a returned value at or below zero, is treated as a cost of
// one.
type PartitionCost func(topic string, partition int32) float64

type topicInfo struct {
	name       string
	base       int32 // flat part number of partition 0
	partitions int32
}

type member struct {
	id     string
	weight float64

	subs map[int]struct{} // topic indices the member subscribes to

	parts []int32 // flat part numbers currently planned for the member
	load  float64 // sum of the costs of parts
}

type partition struct {
	topic int32 // topic index
	cost  float64
}

type balancer struct {
	members []*member
	topics  []topicInfo

	partOwner []int32     // flat part number => topic index
	parts     []partition // flat part number => partition

	// subscribers[t] is the members that subscribe to topic t, ordered by
	// member index (members arrive sorted by the kgo package).
	subscribers [][]int
}

// Balance performs a capacity-weighted, cost-aware sticky balance.
//
// Partitions with no subscriber are assigned to nobody (matching the other
// balancers). The returned plan always has an entry for every member, even
// empty ones.
func Balance(members []GroupMember, topicPartitions map[string]int32, costOf PartitionCost) Plan {
	if len(members) == 0 {
		return Plan{}
	}
	b := newBalancer(members, topicPartitions, costOf)
	if len(b.parts) > 0 {
		b.balance()
	}
	return b.into()
}

func newBalancer(gms []GroupMember, topicPartitions map[string]int32, costOf PartitionCost) *balancer {
	// A deterministic topic ordering keeps the plan independent of map
	// iteration: leaders with identical inputs then agree byte for byte.
	topicNames := make([]string, 0, len(topicPartitions))
	for topic := range topicPartitions {
		topicNames = append(topicNames, topic)
	}
	slices.Sort(topicNames)

	b := &balancer{
		members:     make([]*member, 0, len(gms)),
		topics:      make([]topicInfo, 0, len(topicNames)),
		subscribers: make([][]int, len(topicNames)),
	}

	topicIndex := make(map[string]int, len(topicNames))
	var nparts int
	for _, name := range topicNames {
		idx := len(b.topics)
		topicIndex[name] = idx
		b.topics = append(b.topics, topicInfo{
			name:       name,
			base:       int32(nparts),
			partitions: topicPartitions[name],
		})
		nparts += int(topicPartitions[name])
	}

	b.partOwner = make([]int32, 0, nparts)
	b.parts = make([]partition, 0, nparts)
	for ti, info := range b.topics {
		for p := int32(0); p < info.partitions; p++ {
			cost := 1.0
			if costOf != nil {
				if c := costOf(info.name, p); c > 0 {
					cost = c
				}
			}
			b.partOwner = append(b.partOwner, int32(ti))
			b.parts = append(b.parts, partition{topic: int32(ti), cost: cost})
		}
	}

	for mi, gm := range gms {
		weight := gm.Weight
		if !(weight > 0) || math.IsNaN(weight) || math.IsInf(weight, 0) {
			weight = 1
		}
		m := &member{
			id:     gm.ID,
			weight: weight,
			subs:   make(map[int]struct{}),
		}
		// De-duplicate subscriptions: other members' join metadata is
		// arbitrary input, and a repeated topic must not double-count.
		seenTopic := make(map[string]struct{}, len(gm.Topics))
		for _, topic := range gm.Topics {
			if _, dup := seenTopic[topic]; dup {
				continue
			}
			seenTopic[topic] = struct{}{}
			ti, ok := topicIndex[topic]
			if !ok {
				continue
			}
			m.subs[ti] = struct{}{}
			b.subscribers[ti] = append(b.subscribers[ti], mi)
		}
		b.members = append(b.members, m)
	}

	// Seed from prior assignments, dropping anything illegal: a topic the
	// member no longer subscribes to, a partition that no longer exists, or
	// a competing claim lost to a member at a strictly higher generation.
	owner := make([]int, len(b.parts))
	ownerGen := make([]int32, len(b.parts))
	for i := range owner {
		owner[i] = -1
		ownerGen[i] = math.MinInt32
	}
	// Eager members revoke every partition before rejoining, so their prior
	// assignments never overlap a live owner: the first (deterministically
	// ordered) claim wins. Cooperative members can rejoin while still
	// consuming a partition another member claims; the highest generation
	// wins (KIP-792).
	for mi, gm := range gms {
		m := b.members[mi]
		for topic, ps := range gm.Prior {
			ti, ok := topicIndex[topic]
			if !ok {
				continue
			}
			if _, sub := m.subs[ti]; !sub {
				continue
			}
			info := b.topics[ti]
			for _, p := range ps {
				if p < 0 || p >= info.partitions {
					continue
				}
				flat := int(info.base + p)
				switch {
				case owner[flat] == -1:
				case gm.Cooperative && gm.Generation > ownerGen[flat]:
				default:
					continue
				}
				owner[flat] = mi
				ownerGen[flat] = gm.Generation
			}
		}
	}

	for flat, mi := range owner {
		if mi == -1 {
			continue
		}
		m := b.members[mi]
		m.parts = append(m.parts, int32(flat))
		m.load += b.parts[flat].cost
	}

	b.assignUnassigned(owner)
	return b
}

// assignUnassigned greedily hands every unassigned partition to the
// subscribing member currently least loaded (cost/weight), breaking ties
// toward the first (deterministically ordered) member.
func (b *balancer) assignUnassigned(owner []int) {
	for flat := range b.parts {
		if owner[flat] != -1 {
			continue
		}
		cands := b.subscribers[b.partOwner[flat]]
		if len(cands) == 0 {
			continue // nobody subscribes: leave the partition unassigned
		}
		best := cands[0]
		bestLoad := b.members[best].load / b.members[best].weight
		for _, mi := range cands[1:] {
			if load := b.members[mi].load / b.members[mi].weight; load < bestLoad {
				best, bestLoad = mi, load
			}
		}
		m := b.members[best]
		m.parts = append(m.parts, int32(flat))
		m.load += b.parts[flat].cost
		owner[flat] = best
	}
}

func (m *member) ratio() float64 { return m.load / m.weight }

// removePartition removes flat from parts; order is not preserved (the plan
// is re-sorted when emitted).
func removePartition(parts *[]int32, flat int32) {
	s := *parts
	for i, v := range s {
		if v == flat {
			s[i] = s[len(s)-1]
			*parts = s[:len(s)-1]
			return
		}
	}
}

func (b *balancer) into() Plan {
	plan := make(Plan, len(b.members))
	for _, m := range b.members {
		topics := make(map[string][]int32)
		slices.Sort(m.parts)
		for _, flat := range m.parts {
			ti := b.partOwner[flat]
			info := b.topics[ti]
			topics[info.name] = append(topics[info.name], flat-info.base)
		}
		plan[m.id] = topics
	}
	return plan
}
