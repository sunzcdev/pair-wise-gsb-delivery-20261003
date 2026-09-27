package socks5

import (
	"sync"
	"time"

	"github.com/things-go/go-socks5/statute"
)

const (
	// defaultFragTimeout is the default reassembly timer for fragmented UDP
	// datagrams. RFC 1928 requires the REASSEMBLY TIMER to be no less than
	// 5 seconds.
	defaultFragTimeout = 5 * time.Second
	// defaultFragMaxGroupSize bounds the bytes cached for a single fragment
	// sequence.
	defaultFragMaxGroupSize = 1 << 20 // 1MiB
	// defaultFragMaxTotalSize bounds the bytes cached for all fragment
	// sequences of one UDP association.
	defaultFragMaxTotalSize = 4 << 20 // 4MiB
)

const (
	// fragLastMask is the high-order bit of the FRAG field, marking the end
	// of a fragment sequence.
	fragLastMask = 0x80
	// fragSeqMask extracts the fragment position from the FRAG field.
	fragSeqMask = 0x7f
)

// fragKey identifies a reassembly queue. A reassembler instance is scoped to
// a single UDP association, and queues are further split by client source
// address and target address, so fragments from different clients or
// different targets are never reassembled together.
type fragKey struct {
	src string
	dst string
}

// fragGroup holds the cached fragments of one fragment sequence.
type fragGroup struct {
	frags    map[uint8][]byte
	maxSeq   uint8 // highest FRAG sequence number processed
	lastSeq  uint8 // sequence number of the last fragment, valid if hasLast
	hasLast  bool
	size     int
	expireAt time.Time
	timer    *time.Timer
}

// fragmentReassembler reassembles fragmented SOCKS5 UDP datagrams
// (RFC 1928 section 7) for one UDP association.
type fragmentReassembler struct {
	mu           sync.Mutex
	groups       map[fragKey]*fragGroup
	totalSize    int
	timeout      time.Duration
	maxGroupSize int
	maxTotalSize int
	closed       bool
}

func newFragmentReassembler(timeout time.Duration, maxGroupSize, maxTotalSize int) *fragmentReassembler {
	if timeout <= 0 {
		timeout = defaultFragTimeout
	}
	if maxGroupSize <= 0 {
		maxGroupSize = defaultFragMaxGroupSize
	}
	if maxTotalSize <= 0 {
		maxTotalSize = defaultFragMaxTotalSize
	}
	return &fragmentReassembler{
		groups:       make(map[fragKey]*fragGroup),
		timeout:      timeout,
		maxGroupSize: maxGroupSize,
		maxTotalSize: maxTotalSize,
	}
}

// push feeds one fragmented datagram (FRAG != 0) into the reassembler. It
// returns the fully reassembled datagram and true once the fragment sequence
// is complete; otherwise it returns false and the datagram must be dropped.
func (r *fragmentReassembler) push(src string, dg statute.Datagram) (statute.Datagram, bool) {
	seq := dg.Frag & fragSeqMask
	isLast := dg.Frag&fragLastMask != 0

	if seq == 0 {
		// X'80' carries a complete message in a single fragment; any other
		// zero sequence number is malformed.
		if isLast {
			dg.Frag = 0
			return dg, true
		}
		return statute.Datagram{}, false
	}

	key := fragKey{src: src, dst: dg.DstAddr.String()}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return statute.Datagram{}, false
	}

	g, ok := r.groups[key]
	if ok && seq < g.maxSeq {
		// RFC 1928: the reassembly queue must be reinitialized and the
		// associated fragments abandoned whenever a new datagram arrives
		// carrying a FRAG field whose value is less than the highest FRAG
		// value processed for this fragment sequence.
		r.removeGroup(key, g)
		g, ok = nil, false
	}
	if !ok {
		g = &fragGroup{frags: make(map[uint8][]byte)}
		g.timer = time.AfterFunc(r.timeout, func() { r.expire(key, g) })
		r.groups[key] = g
	}
	if g.hasLast && seq > g.lastSeq {
		// fragment beyond the announced end of the sequence: malformed
		r.removeGroup(key, g)
		return statute.Datagram{}, false
	}
	if _, dup := g.frags[seq]; !dup {
		if g.size+len(dg.Data) > r.maxGroupSize || r.totalSize+len(dg.Data) > r.maxTotalSize {
			r.removeGroup(key, g)
			return statute.Datagram{}, false
		}
		data := make([]byte, len(dg.Data))
		copy(data, dg.Data)
		g.frags[seq] = data
		g.size += len(data)
		r.totalSize += len(data)
	}
	if seq > g.maxSeq {
		g.maxSeq = seq
	}
	if isLast {
		g.hasLast = true
		g.lastSeq = seq
	}
	g.expireAt = time.Now().Add(r.timeout)
	g.timer.Reset(r.timeout)

	if !g.hasLast || len(g.frags) != int(g.lastSeq) {
		return statute.Datagram{}, false
	}
	// all fragments 1..lastSeq are present, reassemble in sequence order
	data := make([]byte, 0, g.size)
	for i := uint8(1); i <= g.lastSeq; i++ {
		data = append(data, g.frags[i]...)
	}
	r.removeGroup(key, g)
	dg.Frag = 0
	dg.Data = data
	return dg, true
}

// expire drops a reassembly queue when its REASSEMBLY TIMER fires.
func (r *fragmentReassembler) expire(key fragKey, g *fragGroup) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.groups[key]
	if !ok || cur != g || time.Now().Before(g.expireAt) {
		return
	}
	r.removeGroup(key, g)
}

// removeGroup deletes a reassembly queue, the caller must hold r.mu.
func (r *fragmentReassembler) removeGroup(key fragKey, g *fragGroup) {
	g.timer.Stop()
	r.totalSize -= g.size
	delete(r.groups, key)
}

// Close releases all reassembly state of the UDP association.
func (r *fragmentReassembler) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	for key, g := range r.groups {
		r.removeGroup(key, g)
	}
}

// groupCount returns the number of pending reassembly queues.
func (r *fragmentReassembler) groupCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.groups)
}
