package socks5

import (
	"sync"
	"time"
)

const (
	// fragmentFlagEnd is the high bit of the FRAG field, it marks
	// the last fragment of a fragment sequence (RFC 1928, section 7).
	fragmentFlagEnd = 0x80
	// maxFragmentNumber is the highest valid fragment sequence number.
	maxFragmentNumber = 0x7F

	// defaultFragmentTimeout is how long an incomplete fragment sequence
	// is kept before it is discarded (RFC 1928 suggests 5 seconds).
	defaultFragmentTimeout = 5 * time.Second
	// defaultFragmentMaxGroupSize bounds the buffered payload bytes of a
	// single fragment sequence.
	defaultFragmentMaxGroupSize = 64 * 1024
	// defaultFragmentMaxTotalSize bounds the buffered payload bytes of all
	// fragment sequences of one UDP association.
	defaultFragmentMaxTotalSize = 1 << 20
)

// fragmentKey identifies one fragment sequence inside a UDP association.
// The sequence belongs to a specific client (source) and target pair, so
// fragments of different clients or targets are never mixed.
type fragmentKey struct {
	src string
	dst string
}

// fragmentGroup holds the reassembly state of one fragment sequence.
type fragmentGroup struct {
	frags    [][]byte
	received int
	maxFrag  byte
	lastFrag byte // 0 means the terminating fragment has not arrived yet
	size     int
	deadline time.Time
}

// fragmentReassembler reassembles fragmented UDP request datagrams
// (FRAG field != 0) according to RFC 1928, section 7. One instance
// belongs to exactly one UDP association; its state is dropped when
// the association is closed.
type fragmentReassembler struct {
	mu           sync.Mutex
	groups       map[fragmentKey]*fragmentGroup
	totalSize    int
	timeout      time.Duration
	maxGroupSize int
	maxTotalSize int
	now          func() time.Time
}

func newFragmentReassembler() *fragmentReassembler {
	return &fragmentReassembler{
		groups:       make(map[fragmentKey]*fragmentGroup),
		timeout:      defaultFragmentTimeout,
		maxGroupSize: defaultFragmentMaxGroupSize,
		maxTotalSize: defaultFragmentMaxTotalSize,
		now:          time.Now,
	}
}

// process feeds one fragment into the reassembler. It returns the
// reassembled payload and true once the sequence is complete; otherwise
// it returns false and the fragment is either buffered or discarded.
func (r *fragmentReassembler) process(src, dst string, frag byte, data []byte) ([]byte, bool) {
	fragNum := frag & maxFragmentNumber
	isLast := frag&fragmentFlagEnd != 0
	if fragNum == 0 {
		// FRAG 0 is handled elsewhere; 0x80 is invalid.
		return nil, false
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	r.purgeExpiredLocked(now)

	key := fragmentKey{src: src, dst: dst}
	g, ok := r.groups[key]
	if !ok {
		if fragNum != 1 {
			// a sequence must start with fragment number 1
			return nil, false
		}
		g = &fragmentGroup{}
		r.groups[key] = g
	} else {
		switch {
		case fragNum < g.maxFrag:
			// RFC 1928: the sequence MUST be dropped when a fragment
			// arrives with a number lower than the highest one seen.
			r.dropGroupLocked(key, g)
			return nil, false
		case fragNum == g.maxFrag:
			// duplicate fragment, ignore it
			return nil, false
		case g.lastFrag != 0 && fragNum > g.lastFrag:
			// fragment beyond the announced end of the sequence
			r.dropGroupLocked(key, g)
			return nil, false
		}
	}

	dataLen := len(data)
	if g.size+dataLen > r.maxGroupSize || r.totalSize+dataLen > r.maxTotalSize {
		r.dropGroupLocked(key, g)
		return nil, false
	}

	for len(g.frags) < int(fragNum) {
		g.frags = append(g.frags, nil)
	}
	// the data buffer is reused by the read loop, so it must be copied
	buf := make([]byte, dataLen)
	copy(buf, data)
	g.frags[fragNum-1] = buf
	g.received++
	g.size += dataLen
	r.totalSize += dataLen
	g.maxFrag = fragNum
	if isLast {
		g.lastFrag = fragNum
	}
	g.deadline = now.Add(r.timeout)

	if g.lastFrag == 0 || g.received != int(g.lastFrag) {
		return nil, false
	}

	r.dropGroupLocked(key, g)
	assembled := make([]byte, 0, g.size)
	for _, part := range g.frags {
		assembled = append(assembled, part...)
	}
	return assembled, true
}

// dropGroupLocked removes a group and releases its buffered bytes.
// The caller must hold r.mu.
func (r *fragmentReassembler) dropGroupLocked(key fragmentKey, g *fragmentGroup) {
	r.totalSize -= g.size
	delete(r.groups, key)
}

// purgeExpiredLocked discards all groups whose reassembly timer expired.
// The caller must hold r.mu.
func (r *fragmentReassembler) purgeExpiredLocked(now time.Time) {
	for key, g := range r.groups {
		if now.After(g.deadline) {
			r.dropGroupLocked(key, g)
		}
	}
}
