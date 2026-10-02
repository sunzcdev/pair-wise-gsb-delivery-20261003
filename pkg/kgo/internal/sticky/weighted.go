package sticky

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"

	"github.com/twmb/franz-go/pkg/kbin"
	"github.com/twmb/franz-go/pkg/kmsg"
)

// WeightedMember is a group member with a declared processing capacity.
type WeightedMember struct {
	ID          string
	Topics      []string
	Weight      float64
	Assignment  []kmsg.StickyMemberMetadataCurrentAssignment
	Owned       []kmsg.ConsumerMemberMetadataOwnedPartition
	Generation  int32
	Cooperative bool
}

// WeightedPartitionCost returns the estimated processing cost of a partition.
type WeightedPartitionCost func(topic string, partition int32) float64

type weightedPartition struct {
	topic     string
	partition int32
	cost      float64
	owner     int
	origOwner int
}

type weightedClaim struct {
	member     int
	generation int32
}

type weightedScore struct {
	max   float64
	sq    float64
	moves int
}

const weightedUserDataVersion int16 = 0

// EncodeWeightedUserData stores a member's capacity weight ahead of the
// standard sticky current assignment.
func EncodeWeightedUserData(weight float64, assignment []kmsg.StickyMemberMetadataCurrentAssignment, generation int32) []byte {
	stickyMeta := kmsg.NewStickyMemberMetadata()
	stickyMeta.Generation = generation
	stickyMeta.CurrentAssignment = assignment

	dst := make([]byte, 0, 10)
	dst = kbin.AppendInt16(dst, weightedUserDataVersion)
	dst = kbin.AppendFloat64(dst, weight)
	return stickyMeta.AppendTo(dst)
}

// DecodeWeightedUserData reads the member capacity and standard sticky
// assignment encoded by EncodeWeightedUserData.
func DecodeWeightedUserData(userdata []byte) (float64, kmsg.StickyMemberMetadata, error) {
	var stickyMeta kmsg.StickyMemberMetadata
	if len(userdata) < 10 {
		return 0, stickyMeta, fmt.Errorf("weighted sticky metadata is too short")
	}
	version := int16(binary.BigEndian.Uint16(userdata[:2]))
	if version != weightedUserDataVersion {
		return 0, stickyMeta, fmt.Errorf("unsupported weighted sticky metadata version %d", version)
	}
	weight := math.Float64frombits(binary.BigEndian.Uint64(userdata[2:10]))
	if !validWeight(weight) {
		return 0, stickyMeta, fmt.Errorf("invalid capacity weight %v", weight)
	}
	if err := stickyMeta.ReadFrom(userdata[10:]); err != nil {
		return 0, stickyMeta, fmt.Errorf("unable to read sticky assignment: %v", err)
	}
	return weight, stickyMeta, nil
}

// WeightedBalance assigns every partition to a subscriber while balancing the
// sum of partition costs against member capacities. Legal ownership is fixed
// first: illegal historical claims are dropped. The remaining plan is balanced
// with a deterministic local search, and a move is accepted only when it lowers
// peak utilization or keeps peak and migration count equal while improving the
// full utilization vector.
func WeightedBalance(members []WeightedMember, topics map[string]int32, partitionCost WeightedPartitionCost) (Plan, error) {
	if len(members) == 0 {
		return Plan{}, nil
	}
	if partitionCost == nil {
		partitionCost = func(string, int32) float64 { return 1 }
	}

	ids := make(map[string]struct{}, len(members))
	subscriptions := make([]map[string]struct{}, len(members))
	for i := range members {
		if members[i].ID == "" {
			return nil, fmt.Errorf("weighted sticky balancer received a member with an empty ID")
		}
		if _, exists := ids[members[i].ID]; exists {
			return nil, fmt.Errorf("weighted sticky balancer received duplicate member ID %q", members[i].ID)
		}
		ids[members[i].ID] = struct{}{}
		if !validWeight(members[i].Weight) {
			return nil, fmt.Errorf("member %q reported invalid capacity weight %v", members[i].ID, members[i].Weight)
		}

		subscriptions[i] = make(map[string]struct{}, len(members[i].Topics))
		for _, topic := range members[i].Topics {
			if topic == "" {
				return nil, fmt.Errorf("member %q subscribed to an empty topic", members[i].ID)
			}
			subscriptions[i][topic] = struct{}{}
		}
	}

	topicNames := make([]string, 0, len(topics))
	for topic, partitionCount := range topics {
		if partitionCount < 0 {
			return nil, fmt.Errorf("topic %q has an invalid partition count %d", topic, partitionCount)
		}
		topicNames = append(topicNames, topic)
	}
	sort.Strings(topicNames)

	partitions := make([]weightedPartition, 0)
	for _, topic := range topicNames {
		for partition := int32(0); partition < topics[topic]; partition++ {
			cost := partitionCost(topic, partition)
			if !validWeight(cost) {
				return nil, fmt.Errorf("topic %q partition %d reported invalid processing cost %v", topic, partition, cost)
			}
			partitions = append(partitions, weightedPartition{
				topic:     topic,
				partition: partition,
				cost:      cost,
				owner:     -1,
				origOwner: -1,
			})
		}
	}

	topicSubscribers := make(map[string][]int)
	for _, topic := range topicNames {
		if topics[topic] == 0 {
			continue
		}
		for member := range members {
			if _, subscribed := subscriptions[member][topic]; subscribed {
				topicSubscribers[topic] = append(topicSubscribers[topic], member)
			}
		}
	}

	claims := make(map[int]weightedClaim)
	addClaim := func(part int, member int, generation int32) {
		if _, subscribed := subscriptions[member][partitions[part].topic]; !subscribed {
			return
		}
		if partition := partitions[part].partition; partition < 0 || partition >= topics[partitions[part].topic] {
			return
		}
		claim, exists := claims[part]
		if !exists || generation > claim.generation || generation == claim.generation && member < claim.member {
			claims[part] = weightedClaim{member: member, generation: generation}
		}
	}

	for member := range members {
		seenAssignment := make(map[string]map[int32]struct{})
		if members[member].Cooperative {
			for _, topic := range members[member].Owned {
				if seenAssignment[topic.Topic] == nil {
					seenAssignment[topic.Topic] = make(map[int32]struct{})
				}
				for _, partition := range topic.Partitions {
					if part := findPartition(partitions, topic.Topic, partition); part >= 0 {
						if _, duplicate := seenAssignment[topic.Topic][partition]; duplicate {
							continue
						}
						seenAssignment[topic.Topic][partition] = struct{}{}
						addClaim(part, member, members[member].Generation)
					}
				}
			}
			continue
		}
		for _, topic := range members[member].Assignment {
			if seenAssignment[topic.Topic] == nil {
				seenAssignment[topic.Topic] = make(map[int32]struct{})
			}
			for _, partition := range topic.Partitions {
				if part := findPartition(partitions, topic.Topic, partition); part >= 0 {
					if _, duplicate := seenAssignment[topic.Topic][partition]; duplicate {
						continue
					}
					seenAssignment[topic.Topic][partition] = struct{}{}
					addClaim(part, member, members[member].Generation)
				}
			}
		}
	}

	loads := make([]float64, len(members))
	for part, claim := range claims {
		partitions[part].owner = claim.member
		partitions[part].origOwner = claim.member
		loads[claim.member] += partitions[part].cost
	}

	unassigned := make([]int, 0)
	for part := range partitions {
		if partitions[part].owner < 0 {
			unassigned = append(unassigned, part)
		}
	}
	sort.Slice(unassigned, func(i, j int) bool {
		left, right := partitions[unassigned[i]], partitions[unassigned[j]]
		return left.cost > right.cost || left.cost == right.cost &&
			(left.topic < right.topic || left.topic == right.topic && left.partition < right.partition)
	})
	for _, part := range unassigned {
		best := -1
		bestUtilization := math.Inf(1)
		for _, candidate := range topicSubscribers[partitions[part].topic] {
			utilization := loads[candidate] / members[candidate].Weight
			if utilization < bestUtilization || utilization == bestUtilization &&
				(best < 0 || members[candidate].Weight > members[best].Weight ||
					members[candidate].Weight == members[best].Weight && candidate < best) {
				best, bestUtilization = candidate, utilization
			}
		}
		if best >= 0 {
			partitions[part].owner = best
			loads[best] += partitions[part].cost
		}
	}

	balanceWeightedAssignment(partitions, topicSubscribers, members, loads)

	plan := make(Plan, len(members))
	for member := range members {
		plan[members[member].ID] = make(map[string][]int32)
	}
	for _, partition := range partitions {
		if partition.owner < 0 {
			continue
		}
		member := members[partition.owner].ID
		plan[member][partition.topic] = append(plan[member][partition.topic], partition.partition)
	}
	return plan, nil
}

func balanceWeightedAssignment(
	partitions []weightedPartition,
	topicSubscribers map[string][]int,
	members []WeightedMember,
	loads []float64,
) {
	scoreWith := func(loads []float64, moves int) weightedScore {
		result := weightedScore{moves: moves, max: math.Inf(-1)}
		for member := range members {
			utilization := loads[member] / members[member].Weight
			if utilization > result.max {
				result.max = utilization
			}
			result.sq += utilization * utilization
		}
		return result
	}
	currentMoves := func() int {
		moves := 0
		for part := range partitions {
			if partitions[part].owner != partitions[part].origOwner {
				moves++
			}
		}
		return moves
	}
	moveCount := func(part, owner int) int {
		if owner == partitions[part].origOwner {
			return 0
		}
		return 1
	}

	for range len(partitions) {
		bestScore := scoreWith(loads, currentMoves())
		bestPart, bestDestination := -1, -1

		for part := range partitions {
			source := partitions[part].owner
			if source < 0 {
				continue
			}
			for _, destination := range topicSubscribers[partitions[part].topic] {
				if destination == source {
					continue
				}
				loads[source] -= partitions[part].cost
				loads[destination] += partitions[part].cost
				moves := bestScore.moves -
					moveCount(part, source) + moveCount(part, destination)
				candidate := scoreWith(loads, moves)
				loads[source] += partitions[part].cost
				loads[destination] -= partitions[part].cost

				if betterWeightedScore(candidate, bestScore) {
					bestScore, bestPart, bestDestination = candidate, part, destination
				}
			}
		}
		if bestPart < 0 {
			return
		}

		source := partitions[bestPart].owner
		loads[source] -= partitions[bestPart].cost
		loads[bestDestination] += partitions[bestPart].cost
		partitions[bestPart].owner = bestDestination
	}
}

func betterWeightedScore(candidate, current weightedScore) bool {
	if lessWeightedFloat(candidate.max, current.max) {
		return true
	}
	if equalWeightedFloat(candidate.max, current.max) {
		if candidate.moves < current.moves {
			return true
		}
		if candidate.moves == current.moves && lessWeightedFloat(candidate.sq, current.sq) {
			return true
		}
	}
	return false
}

func lessWeightedFloat(candidate, current float64) bool {
	epsilon := 1e-12 * max(1, math.Abs(candidate), math.Abs(current))
	return candidate < current-epsilon
}

func equalWeightedFloat(candidate, current float64) bool {
	return !lessWeightedFloat(candidate, current) && !lessWeightedFloat(current, candidate)
}

func findPartition(partitions []weightedPartition, topic string, partition int32) int {
	for i := range partitions {
		if partitions[i].topic == topic && partitions[i].partition == partition {
			return i
		}
	}
	return -1
}

func validWeight(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}
