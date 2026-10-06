package reedsolomon

import "fmt"

// LRC is an independent locally repairable codec described by
// [LRCLayoutParams]. It produces one XOR local parity per group and
// GlobalParityShards Vandermonde Reed-Solomon global parities.
//
// LRC is intentionally not an [Encoder]: it is a different layout with a
// weaker, non-MDS fault-tolerance profile. Use [NewLRC] and [NewLRCCodec]
// to construct one and [LRC.PlanRepair] to find out which shards must be
// read before touching the object store.
type LRC struct {
	params LRCLayoutParams
	// global encodes the k data shards into the g global parity shards.
	global Encoder
	// globalRows are the g coding rows (length k) used for global parity.
	globalRows [][]byte
	o          *options
}

// NewLRCCodec constructs an LRC codec from validated parameters.
func NewLRCCodec(params LRCLayoutParams, opts ...Option) (*LRC, error) {
	if err := params.Validate(); err != nil {
		return nil, err
	}
	// The global parity is a flat (k, g) Reed-Solomon stripe over the data
	// shards. Its identity/parity layout is exactly what we want: the
	// global rows are expressed directly in the k data variables.
	global, err := New(params.DataShards, params.GlobalParityShards, opts...)
	if err != nil {
		return nil, err
	}
	rs, ok := global.(*reedSolomon)
	if !ok {
		return nil, fmt.Errorf("%w: global parity codec has unexpected backend %T",
			ErrInvalidLRCParams, global)
	}
	lrc := &LRC{
		params:     params,
		global:     global,
		globalRows: rs.parity,
		o:          &rs.o,
	}
	return lrc, nil
}

// Params returns a copy of the codec's layout parameters.
func (l *LRC) Params() LRCLayoutParams {
	p := l.params
	p.GroupSizes = append([]int(nil), l.params.GroupSizes...)
	return p
}

// DataShards returns the number of logical data shards (k).
func (l *LRC) DataShards() int { return l.params.DataShards }

// LocalParityShards returns the number of local parity shards (l).
func (l *LRC) LocalParityShards() int { return l.params.LocalGroups() }

// GlobalParityShards returns the number of global parity shards (g).
func (l *LRC) TotalShards() int { return l.params.TotalShards() }

// AllocShards returns a zeroed physical shard vector of length
// TotalShards, each shard sized shardSize bytes.
func (l *LRC) AllocShards(shardSize int) [][]byte {
	shards := make([][]byte, l.TotalShards())
	for i := range shards {
		shards[i] = make([]byte, shardSize)
	}
	return shards
}

// Encode computes all local and global parity shards from the data.
// shards must have TotalShards entries of equal length, with the first
// DataShards entries populated. Parity shards are overwritten.
func (l *LRC) Encode(shards [][]byte) error {
	if len(shards) != l.TotalShards() {
		return ErrTooFewShards
	}
	if err := checkShards(shards, false); err != nil {
		return err
	}
	k, ng := l.params.DataShards, l.params.LocalGroups()
	size := len(shards[0])

	// Global parity: reuse the flat RS encoder on [data | global parity].
	rsShards := make([][]byte, k+l.params.GlobalParityShards)
	copy(rsShards, shards[:k])
	for j := 0; j < l.params.GlobalParityShards; j++ {
		idx, _ := l.params.GlobalParityIndex(j)
		rsShards[k+j] = shards[idx][:size]
	}
	if err := l.global.Encode(rsShards); err != nil {
		return err
	}

	// Local parity: XOR of each group's members.
	for g := 0; g < ng; g++ {
		members, _ := l.params.GroupMembers(g)
		localIdx, _ := l.params.LocalParityIndex(g)
		out := shards[localIdx][:size]
		for i := range out {
			out[i] = 0
		}
		for _, d := range members {
			sliceXor(shards[d][:size], out, l.o)
		}
	}
	return nil
}

// Verify checks that all parity shards agree with the data shards.
// Missing (nil/zero-length) shards make the check return an error so that
// callers do not mistake a partial stripe for a verified one.
func (l *LRC) Verify(shards [][]byte) (bool, error) {
	if len(shards) != l.TotalShards() {
		return false, ErrTooFewShards
	}
	for _, s := range shards {
		if len(s) == 0 {
			return false, ErrShardNoData
		}
	}
	size := len(shards[0])

	// Global parity.
	tmp := make([][]byte, l.params.GlobalParityShards)
	for j := range tmp {
		tmp[j] = make([]byte, size)
	}
	k := l.params.DataShards
	inputs := shards[:k]
	if rs, ok := l.global.(*reedSolomon); ok {
		rs.codeSomeShards(l.globalRows, inputs, tmp, size, true)
	}
	for j := 0; j < l.params.GlobalParityShards; j++ {
		idx, _ := l.params.GlobalParityIndex(j)
		if !bytesEqual(tmp[j], shards[idx][:size]) {
			return false, nil
		}
	}

	// Local parity.
	for g := 0; g < l.params.LocalGroups(); g++ {
		members, _ := l.params.GroupMembers(g)
		localIdx, _ := l.params.LocalParityIndex(g)
		acc := make([]byte, size)
		for _, d := range members {
			sliceXor(shards[d][:size], acc, l.o)
		}
		if !bytesEqual(acc, shards[localIdx][:size]) {
			return false, nil
		}
	}
	return true, nil
}

// xorLocalInto recomputes the XOR of the supplied members into out,
// which is zeroed first.
func (l *LRC) xorLocalInto(out []byte, members []int, shards [][]byte) {
	for i := range out {
		out[i] = 0
	}
	for _, d := range members {
		sliceXor(shards[d], out, l.o)
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
