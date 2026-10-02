package kgo

import (
	"sort"

	"github.com/twmb/franz-go/pkg/kbin"
	"github.com/twmb/franz-go/pkg/kgo/internal/weighted"
	"github.com/twmb/franz-go/pkg/kmsg"
)

// PartitionCost estimates the processing cost of a single partition.
//
// The weighted sticky balancer balances the sum of these costs per unit of
// member capacity, rather than the raw number of partitions. Returning a
// value at or below zero means "unknown", which is treated as a cost of one.
// The callback runs only on the group leader while balancing, so it may read
// partition metrics (bytes-in rate, message rate, measured CPU) that
// follower members do not have.
type PartitionCost func(topic string, partition int32) float64

// weightedUserDataVersion is the first field of the balancer's UserData
// envelope. Bumping it lets future fields be added while still recognizing
// old members; an unrecognized payload degrades to weight 1 with no history.
const weightedUserDataVersion int16 = 1

// WeightedStickyBalancer returns a sticky group balancer that balances
// estimated partition processing cost against each member's declared capacity
// weight, rather than balancing partition counts. Configure the member's
// capacity with WithWeight and, on the leader, partition costs with
// WithPartitionCost.
//
// Members declare how much capacity they have (for example a 16-core machine
// uses weight 16 while a 4-core machine uses weight 4). The leader maps
// partitions to costs (for example a hot partition's measured traffic). The
// balancer minimizes the maximum of
//
//	sum(partition costs assigned to a member) / member weight
//
// while satisfying every subscription constraint and keeping the assignment
// as sticky as possible: it starts from the previous assignment and only
// moves the partitions required to reach a load that no legal chain of moves
// can improve, so steady-state rebalances move the fewest partitions.
//
// A member whose weight cannot be decoded (for example a member of a
// different balancer sharing the group) is treated as weight 1. Every member
// of a group must use a weighted protocol for the weighted plan to be
// selected.
func WeightedStickyBalancer() *WeightedBalancer {
	return newWeightedBalancer(false)
}

// CooperativeWeightedStickyBalancer is WeightedStickyBalancer with cooperative
// rebalancing (KIP-429). As with CooperativeStickyBalancer, members do not
// stop processing during a rebalance: a partition that must move is first
// revoked by its current owner and is only assigned to its new owner in the
// following rebalance. Thus a still-owned partition is never handed to a
// joining member in the same round.
func CooperativeWeightedStickyBalancer() *WeightedBalancer {
	return newWeightedBalancer(true)
}

func newWeightedBalancer(cooperative bool) *WeightedBalancer {
	return &WeightedBalancer{cooperative: cooperative, weight: 1}
}

// WeightedBalancer is a GroupBalancer that balances estimated partition
// processing cost per unit of member capacity, keeping the assignment sticky.
// Construct it with WeightedStickyBalancer or CooperativeWeightedStickyBalancer
// and configure it with WithWeight (every member) and WithPartitionCost (the
// leader only).
type WeightedBalancer struct {
	cooperative bool
	weight      float64
	costOf      PartitionCost
}

// WithWeight sets this balancer instance's capacity weight and returns the
// balancer. A member with weight 4 is expected to handle four times the cost
// of a member with weight 1. Non-finite and non-positive values clamp to 1.
func (w *WeightedBalancer) WithWeight(weight float64) *WeightedBalancer {
	if weight > 0 && weight == weight { // positive and not NaN
		w.weight = weight
	}
	return w
}

// WithPartitionCost supplies the leader-side per-partition cost estimator and
// returns the balancer. Only the leader invokes it; followers ignore it. A
// nil callback means every partition costs one.
func (w *WeightedBalancer) WithPartitionCost(costOf PartitionCost) *WeightedBalancer {
	w.costOf = costOf
	return w
}

func (w *WeightedBalancer) ProtocolName() string {
	if w.cooperative {
		return "cooperative-weighted-sticky"
	}
	return "weighted-sticky"
}

func (w *WeightedBalancer) IsCooperative() bool { return w.cooperative }

func (w *WeightedBalancer) JoinGroupMetadata(interests []string, currentAssignment map[string][]int32, generation int32) []byte {
	meta := kmsg.NewConsumerMemberMetadata()
	meta.Version = 3 // BUMP ME WHEN NEW FIELDS ARE ADDED
	meta.Topics = interests
	meta.Generation = generation

	if w.cooperative {
		owned := make([]string, 0, len(currentAssignment))
		for topic := range currentAssignment {
			owned = append(owned, topic)
		}
		sort.Strings(owned)
		for _, topic := range owned {
			part := kmsg.NewConsumerMemberMetadataOwnedPartition()
			part.Topic = topic
			part.Partitions = currentAssignment[topic]
			meta.OwnedPartitions = append(meta.OwnedPartitions, part)
		}
	}

	meta.UserData = encodeWeightedUserData(w.weight, currentAssignment, generation)
	return meta.AppendTo(nil)
}

func (*WeightedBalancer) ParseSyncAssignment(assignment []byte) (map[string][]int32, error) {
	return ParseConsumerSyncAssignment(assignment)
}

func (w *WeightedBalancer) MemberBalancer(members []kmsg.JoinGroupResponseMember) (GroupMemberBalancer, map[string]struct{}, error) {
	b, err := NewConsumerBalancer(w, members)
	return b, b.MemberTopics(), err
}

func (w *WeightedBalancer) Balance(b *ConsumerBalancer, topics map[string]int32) IntoSyncAssignment {
	members := make([]weighted.GroupMember, 0, len(b.Members()))
	b.EachMember(func(member *kmsg.JoinGroupResponseMember, meta *kmsg.ConsumerMemberMetadata) {
		weight, prior, priorGen := decodeWeightedUserData(meta.UserData)

		// Cooperative members carry generation-tagged ownership in the
		// metadata itself; that authoritative claim wins over the copy we
		// persist in UserData. Eager members revoke before rejoining and
		// rely entirely on the persisted UserData assignment.
		gen := priorGen
		if w.cooperative && meta.Generation >= 0 {
			gen = meta.Generation
			prior = make(map[string][]int32, len(meta.OwnedPartitions))
			for _, t := range meta.OwnedPartitions {
				prior[t.Topic] = t.Partitions
			}
		}

		members = append(members, weighted.GroupMember{
			ID:          member.MemberID,
			Topics:      meta.Topics,
			Weight:      weight,
			Generation:  gen,
			Cooperative: w.cooperative,
			Prior:       prior,
		})
	})

	plan := weighted.Balance(members, topics, weighted.PartitionCost(w.costOf))
	p := &BalancePlan{plan}
	if w.cooperative {
		p.AdjustCooperative(b)
	}
	return p
}

// encodeWeightedUserData writes the member's capacity weight and current
// assignment into the balancer's UserData envelope.
func encodeWeightedUserData(weight float64, currentAssignment map[string][]int32, generation int32) []byte {
	dst := make([]byte, 0, 32)
	dst = kbin.AppendInt16(dst, weightedUserDataVersion)
	dst = kbin.AppendFloat64(dst, weight)
	dst = kbin.AppendInt32(dst, generation)

	assignmentTopics := make([]string, 0, len(currentAssignment))
	for topic := range currentAssignment {
		assignmentTopics = append(assignmentTopics, topic)
	}
	sort.Strings(assignmentTopics)

	dst = kbin.AppendArrayLen(dst, len(assignmentTopics))
	for _, topic := range assignmentTopics {
		partitions := currentAssignment[topic]
		dst = kbin.AppendString(dst, topic)
		dst = kbin.AppendArrayLen(dst, len(partitions))
		for _, partition := range partitions {
			dst = kbin.AppendInt32(dst, partition)
		}
	}
	return dst
}

// decodeWeightedUserData reads the envelope written by
// encodeWeightedUserData. Any unrecognized or corrupt payload (for example a
// member running a different balancer) yields a weight of 1 and no history,
// which is always a safe input.
func decodeWeightedUserData(userdata []byte) (weight float64, prior map[string][]int32, generation int32) {
	weight = 1
	r := kbin.Reader{Src: userdata}

	version := r.Int16()
	if r.Complete() != nil || version != weightedUserDataVersion {
		return 1, nil, -1
	}
	weight = r.Float64()
	generation = r.Int32()

	prior = make(map[string][]int32)
	for n := r.ArrayLen(); n > 0; n-- {
		topic := r.String()
		partitions := make([]int32, 0)
		for np := r.ArrayLen(); np > 0; np-- {
			partitions = append(partitions, r.Int32())
		}
		prior[topic] = partitions
	}
	if r.Complete() != nil {
		return 1, nil, -1
	}
	if !(weight > 0) || weight != weight {
		weight = 1
	}
	return weight, prior, generation
}
