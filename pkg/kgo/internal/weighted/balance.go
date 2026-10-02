package weighted

import (
	"math"
	"slices"
)

// loadEps is the tolerance used when comparing loads so floating-point
// rounding does not make the search oscillate.
const loadEps = 1e-9

// maxBalanceIters bounds the relieving chains one balance applies. Every
// accepted chain strictly improves the lexicographically ordered load vector,
// so the process converges on its own.
const maxBalanceIters = 100000

// maxDfsNodes bounds the number of search nodes explored while finding one
// improving chain. Finding an improving move is an NP-hard assignment problem
// in general; the bound keeps balancing fast on large groups while the search
// stays exhaustive on realistic group sizes.
const maxDfsNodes = 200000

// balance refines the sticky seed until no legal chain of partition moves can
// improve the members' load vector. The seed keeps every partition already
// legally held, so every partition a chain relocates is a necessary migration.
//
// The objective is the members' cost/weight loads sorted largest-first, made
// lexicographically as small as possible. Minimizing only the maximum (the
// first vector entry) is not enough when a partition cost cannot be split: a
// member pinned to one cost-10 partition has an irreducible load of 10, but the
// cold partitions should still flow off it to lower the next entry from 10
// toward 3.
//
// A single move only relieves an overloaded member when a peer can directly
// take one of its partitions. With heterogeneous subscriptions relief can be
// indirect:
//
//	S --t0--> B --t1--> T
//
// S (overloaded, only peers on t0) sends a t0 partition to B, which frees B
// to send one of its own t1 partitions to underloaded T, even though T does
// not subscribe to t0. This mirrors the standard sticky balancer's steal
// graph, but the score balanced is cost/weight rather than a partition count.
func (b *balancer) balance() {
	for _, m := range b.members {
		slices.Sort(m.parts) // deterministic tie breaking while searching
	}

	current := b.loadVector()
	for iter := 0; iter < maxBalanceIters; iter++ {
		chain := b.findImprovingChain(current)
		if chain == nil {
			return
		}
		for _, seg := range chain {
			cost := b.parts[seg.flat].cost
			b.members[seg.src].load -= cost
			b.members[seg.dst].load += cost
			removePartition(&b.members[seg.src].parts, seg.flat)
			b.members[seg.dst].parts = append(b.members[seg.dst].parts, seg.flat)
		}
		current = b.loadVector()
	}
}

// loadVector returns every member's cost/weight load, sorted descending.
func (b *balancer) loadVector() []float64 {
	v := make([]float64, len(b.members))
	for i, m := range b.members {
		v[i] = m.ratio()
	}
	slices.Sort(v)
	slices.Reverse(v)
	return v
}

// vectorLess reports whether descending vector a is lexicographically smaller
// than b, treating differences within loadEps as equal.
func vectorLess(a, b []float64) bool {
	for i := range a {
		switch {
		case a[i] < b[i]-loadEps:
			return true
		case a[i] > b[i]+loadEps:
			return false
		}
	}
	return false
}

func sameVector(a, b []float64) bool {
	for i := range a {
		if math.Abs(a[i]-b[i]) > loadEps {
			return false
		}
	}
	return true
}

type chainSegment struct {
	src, dst int
	flat     int32
}

// findImprovingChain searches for a legal chain yielding a lexicographically
// smaller load vector. It starts from members at the highest removable load
// level, requires every member on the chain to finish at or below that level,
// and ends at a member that finishes strictly below it. The chain with the
// smallest resulting vector wins, with hops and member indices for
// determinism.
func (b *balancer) findImprovingChain(current []float64) []chainSegment {
	level, sources := b.sourceLevel()
	if level == math.Inf(-1) {
		return nil
	}

	if chain := b.directMove(sources, level, current); chain != nil {
		return chain
	}

	s := search{
		b:          b,
		level:      level,
		visited:    make([]bool, len(b.members)),
		onPath:     make([]chainSegment, 0, len(b.members)),
		bestVector: current,
	}

	for _, src := range sources {
		s.start(src)
		if s.nodes >= maxDfsNodes {
			break
		}
	}
	return s.bestChain
}

// directMove finds a single partition move from a peak source to a subscriber
// that finishes strictly below the level, with the source also below it. This
// covers the common case where members share subscriptions and avoids the
// chain DFS entirely. It returns the move yielding the smallest vector.
func (b *balancer) directMove(sources []int, level float64, current []float64) []chainSegment {
	var best []chainSegment
	bestVector := current
	for _, src := range sources {
		srcM := b.members[src]
		for _, flat := range srcM.parts {
			c := b.parts[flat].cost
			topic := b.partOwner[flat]
			for _, dst := range b.subscribers[topic] {
				if dst == src {
					continue
				}
				dstM := b.members[dst]
				dstAfter := (dstM.load + c) / dstM.weight
				// The destination must not exceed the source level; whether
				// the source itself drops below it is captured by comparing
				// the full resulting vector (with multiple peak members one
				// shedding member can stay at the level and still improve the
				// vector).
				if dstAfter > level+loadEps {
					continue
				}
				chain := []chainSegment{{src: src, dst: dst, flat: flat}}
				cand := b.vectorAfterChain(chain)
				if !vectorLess(cand, bestVector) {
					continue
				}
				if best != nil && sameVector(cand, bestVector) && dst >= best[0].dst {
					continue
				}
				best, bestVector = chain, cand
			}
		}
	}
	return best
}

type search struct {
	b       *balancer
	level   float64
	visited []bool
	onPath  []chainSegment
	nodes   int

	// bestVector is the lexicographically smallest reachable vector; the
	// chain starts empty (meaning "no move"), so only strictly improving
	// chains replace bestChain.
	bestVector []float64
	bestChain  []chainSegment
}

// start begins a chain at a peak source. Sources only shed load, so they
// always stay at or below the level.
func (s *search) start(src int) {
	s.visited[src] = true
	s.extend(src, 0)
	s.visited[src] = false
}

// extend explores sending one partition currently held by node to a different
// subscriber. inCost is the cost node receives along the path (0 at a
// source); after relaying its chosen partition, node's load is
// (load + inCost - sentCost) / weight, which must not exceed the level.
func (s *search) extend(node int, inCost float64) {
	b := s.b
	nodeM := b.members[node]

	for _, flat := range nodeM.parts {
		sent := b.parts[flat].cost
		if after := (nodeM.load + inCost - sent) / nodeM.weight; after > s.level+loadEps {
			continue
		}
		topic := b.partOwner[flat]
		for _, next := range b.subscribers[topic] {
			if next == node || s.visited[next] {
				continue
			}
			s.nodes++
			nextM := b.members[next]
			nextAfter := (nextM.load + sent) / nextM.weight
			if nextAfter > s.level+loadEps {
				continue
			}
			seg := chainSegment{src: node, dst: next, flat: flat}
			s.onPath = append(s.onPath, seg)

			// A terminal strictly below the level closes a chain that
			// drops that level. Intermediates at the level may relay on.
			if nextAfter < s.level-loadEps {
				s.consider()
			}
			if s.nodes < maxDfsNodes {
				s.visited[next] = true
				s.extend(next, sent)
				s.visited[next] = false
			}

			s.onPath = s.onPath[:len(s.onPath)-1]
		}
	}
}

// consider evaluates the current path against the best found so far.
func (s *search) consider() {
	b := s.b
	chain := s.onPath
	cand := b.vectorAfterChain(chain)
	if !vectorLess(cand, s.bestVector) {
		return
	}
	terminal := chain[len(chain)-1].dst
	if s.bestChain != nil && sameVector(cand, s.bestVector) &&
		(len(chain) > len(s.bestChain) ||
			len(chain) == len(s.bestChain) && terminal >= s.bestChain[len(s.bestChain)-1].dst) {
		return
	}
	s.bestVector = cand
	s.bestChain = slices.Clone(chain)
}

// sourceLevel returns the highest distinct member load level at which some
// member holds a partition a different subscriber could take.
func (b *balancer) sourceLevel() (float64, []int) {
	for _, level := range distinctDescending(b.loadVector()) {
		var sources []int
		for i, m := range b.members {
			if math.Abs(m.ratio()-level) <= loadEps && b.canShed(i) {
				sources = append(sources, i)
			}
		}
		if len(sources) > 0 {
			return level, sources
		}
	}
	return math.Inf(-1), nil
}

// canShed reports whether member i holds a partition a different subscriber
// could take.
func (b *balancer) canShed(i int) bool {
	for _, flat := range b.members[i].parts {
		topic := b.partOwner[flat]
		for _, v := range b.subscribers[topic] {
			if v != i {
				return true
			}
		}
	}
	return false
}

// distinctDescending returns the distinct load levels of v, largest first.
func distinctDescending(v []float64) []float64 {
	out := slices.Clone(v)
	slices.Sort(out)
	slices.Reverse(out)
	dst := out[:0]
	for _, x := range out {
		if len(dst) == 0 || math.Abs(x-dst[len(dst)-1]) > loadEps {
			dst = append(dst, x)
		}
	}
	return dst
}

// vectorAfterChain computes the descending load vector after applying a chain;
// untouched members keep their current load.
func (b *balancer) vectorAfterChain(chain []chainSegment) []float64 {
	load := make([]float64, len(b.members))
	for i, m := range b.members {
		load[i] = m.load
	}
	for _, seg := range chain {
		c := b.parts[seg.flat].cost
		load[seg.src] -= c
		load[seg.dst] += c
	}
	v := make([]float64, len(b.members))
	for i, m := range b.members {
		v[i] = load[i] / m.weight
	}
	slices.Sort(v)
	slices.Reverse(v)
	return v
}
