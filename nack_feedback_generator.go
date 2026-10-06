// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package rtcp

import (
	"errors"
	"sort"
	"time"
)

var (
	errNackInvalidReorderWindow = errors.New("rtcp: nack reorder window must be >= 0")
	errNackInvalidRetryInterval = errors.New("rtcp: nack retry interval must be > 0")
	errNackInvalidTTL           = errors.New("rtcp: nack max ttl must be > 0")
	errNackInvalidMaxTracked    = errors.New("rtcp: nack max tracked gaps must be > 0")
)

const (
	// nackPairCapacity is the largest number of NackPairs a single
	// TransportLayerNack can carry (RFC 4585 uses an 8 bit length field).
	nackPairCapacity = 255 - tlnLength
)

// Default tuning parameters for NackFeedbackGenerator.
const (
	// DefaultNackReorderWindow is how long a freshly observed gap is held
	// back before it is reported, tolerating mild network reordering.
	DefaultNackReorderWindow = 10 * time.Millisecond
	// DefaultNackRetryInterval separates two consecutive requests for the
	// same still-missing sequence number.
	DefaultNackRetryInterval = 20 * time.Millisecond
	// DefaultNackMaxTTL bounds how long a sequence number is re-requested
	// before it is given up on.
	DefaultNackMaxTTL = 250 * time.Millisecond
	// DefaultNackMaxTracked bounds the number of simultaneously tracked gaps,
	// keeping the generator state finite under pathological loss.
	DefaultNackMaxTracked = 1024
)

// nackGap is the per-missing-sequence state.
type nackGap struct {
	seq          uint16
	firstMissing time.Time // when the gap was first observed
	lastReported time.Time // zero until the first report has been produced
}

// NackFeedbackConfig configures a NackFeedbackGenerator.
type NackFeedbackConfig struct {
	// ReorderWindow delays the first report of a new gap by this amount so
	// that harmless reordering does not trigger retransmission requests.
	ReorderWindow time.Duration
	// RetryInterval is the minimum time between two requests for the same
	// still-missing sequence number.
	RetryInterval time.Duration
	// MaxTTL is the maximum lifetime of a gap. Once a gap is older than this
	// it is dropped and no longer requested.
	MaxTTL time.Duration
	// MaxTracked bounds the number of gaps kept at the same time. If more
	// gaps exist, the oldest ones are discarded first.
	MaxTracked int
}

// NackFeedbackGenerator is a stateful receiver-side generator of
// TransportLayerNack feedback.
//
// The caller feeds every received RTP sequence number together with its
// reception time via Received, and periodically calls BuildTransportLayerNack
// (or BuildNackPairs) with the current virtual time. Sequence numbers that are
// missing are first held back for a configurable reorder window, then requested
// repeatedly until they arrive late, exceed their maximum lifetime, or are
// evicted by the bounded state. Late and duplicated packets, as well as the
// 16 bit sequence number wrap-around, all update the same loss state.
//
// All timing is supplied explicitly by the caller, which makes the generator
// deterministic and suitable for tests driven by a virtual clock.
type NackFeedbackGenerator struct {
	mediaSSRC  uint32
	reorder    time.Duration
	retry      time.Duration
	ttl        time.Duration
	maxTracked int
	started    bool
	highestSeq uint16
	gaps       map[uint16]*nackGap
}

// NewNackFeedbackGenerator creates a generator for a single media stream.
// A stream that is obviously restarted by the sender must be Reset by the
// caller before reuse.
func NewNackFeedbackGenerator(mediaSSRC uint32, config NackFeedbackConfig) (*NackFeedbackGenerator, error) {
	if config.ReorderWindow < 0 {
		return nil, errNackInvalidReorderWindow
	}

	if config.RetryInterval <= 0 {
		return nil, errNackInvalidRetryInterval
	}

	if config.MaxTTL <= 0 {
		return nil, errNackInvalidTTL
	}

	if config.MaxTracked <= 0 {
		return nil, errNackInvalidMaxTracked
	}

	return &NackFeedbackGenerator{
		mediaSSRC:  mediaSSRC,
		reorder:    config.ReorderWindow,
		retry:      config.RetryInterval,
		ttl:        config.MaxTTL,
		maxTracked: config.MaxTracked,
		gaps:       make(map[uint16]*nackGap),
	}, nil
}

// MediaSSRC returns the SSRC of the media stream the generator tracks.
func (g *NackFeedbackGenerator) MediaSSRC() uint32 {
	return g.mediaSSRC
}

// seqDistance returns a-b interpreted in forward (mod 65536) order. A positive
// result means a is later than b.
func seqDistance(a, b uint16) int {
	return int(int16(a - b))
}

// Received records that the RTP packet with the given sequence number arrived at
// the supplied time. It cancels any outstanding request for a late packet,
// ignores duplicates (keeping their loss state untouched), and creates gaps for
// the sequence numbers that were skipped, handling 16 bit wrap-around.
func (g *NackFeedbackGenerator) Received(seq uint16, at time.Time) {
	if !g.started {
		g.started = true
		g.highestSeq = seq

		return
	}

	if seq == g.highestSeq {
		// Duplicate of the newest packet: there is no gap to cancel.
		return
	}

	d := seqDistance(seq, g.highestSeq)
	if d < 0 {
		// Old or duplicate packet. A pending gap means the packet arrived
		// late; a duplicate simply has no gap entry and is ignored.
		delete(g.gaps, seq)

		return
	}
	if d > 1 {
		// One or more sequence numbers were skipped: register them as gaps.
		for missing := g.highestSeq + 1; missing != seq; missing++ {
			if _, exists := g.gaps[missing]; !exists {
				g.gaps[missing] = &nackGap{seq: missing, firstMissing: at}
			}
		}
	}

	g.highestSeq = seq
	g.evictOldest()
}

// evictOldest drops the oldest tracked gaps until the bounded capacity is
// respected.
func (g *NackFeedbackGenerator) evictOldest() {
	if len(g.gaps) <= g.maxTracked {
		return
	}

	ordered := make([]*nackGap, 0, len(g.gaps))
	for _, gap := range g.gaps {
		ordered = append(ordered, gap)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if !ordered[i].firstMissing.Equal(ordered[j].firstMissing) {
			return ordered[i].firstMissing.Before(ordered[j].firstMissing)
		}
		// Same observation time (one burst): older sequence number first,
		// evaluated in forward order across 16 bit wrap-around.
		return seqDistance(ordered[j].seq, ordered[i].seq) > 0
	})

	drop := len(ordered) - g.maxTracked
	for i := 0; i < drop; i++ {
		delete(g.gaps, ordered[i].seq)
	}
}

// pruneExpired removes gaps that outlived their maximum lifetime.
func (g *NackFeedbackGenerator) pruneExpired(at time.Time) {
	for seq, gap := range g.gaps {
		if at.Sub(gap.firstMissing) >= g.ttl {
			delete(g.gaps, seq)
		}
	}
}

// BuildNackPairs returns the NackPairs that should be sent at the given time.
//
// A gap is eligible when its reorder window has elapsed and either it has never
// been reported or the retry interval has elapsed since the last report.
// Reporting updates each gap's last-request time; expired gaps are removed.
// Repeated calls without advancing time return an empty result.
func (g *NackFeedbackGenerator) BuildNackPairs(at time.Time) []NackPair {
	g.pruneExpired(at)

	eligible := make([]uint16, 0, len(g.gaps))
	for seq, gap := range g.gaps {
		if at.Sub(gap.firstMissing) < g.reorder {
			continue
		}

		if !gap.lastReported.IsZero() && at.Sub(gap.lastReported) < g.retry {
			continue
		}

		eligible = append(eligible, seq)
		gap.lastReported = at
	}

	if len(eligible) == 0 {
		return []NackPair{}
	}

	// Sort in forward sequence-number order so wrap-around groups correctly.
	sort.Slice(eligible, func(i, j int) bool {
		return seqDistance(eligible[i], g.highestSeq) < seqDistance(eligible[j], g.highestSeq)
	})

	return NackPairsFromSequenceNumbers(eligible)
}

// BuildTransportLayerNack returns the ready-to-send TransportLayerNack for the
// gaps due at the given time. senderSSRC identifies the feedback sender; the
// media SSRC is the one given at construction. It returns nil when there is
// nothing to request at this time.
func (g *NackFeedbackGenerator) BuildTransportLayerNack(senderSSRC uint32, at time.Time) *TransportLayerNack {
	pairs := g.BuildNackPairs(at)
	if len(pairs) == 0 {
		return nil
	}

	if len(pairs) > nackPairCapacity {
		pairs = pairs[:nackPairCapacity]
	}

	return &TransportLayerNack{
		SenderSSRC: senderSSRC,
		MediaSSRC:  g.mediaSSRC,
		Nacks:      pairs,
	}
}

// MissingSequenceNumbers returns the currently tracked, still-missing sequence
// numbers in forward sequence order regardless of whether they are due for a
// report. It is mainly intended for diagnostics and tests.
func (g *NackFeedbackGenerator) MissingSequenceNumbers() []uint16 {
	missing := make([]uint16, 0, len(g.gaps))
	for seq := range g.gaps {
		missing = append(missing, seq)
	}
	sort.Slice(missing, func(i, j int) bool {
		return seqDistance(missing[i], g.highestSeq) < seqDistance(missing[j], g.highestSeq)
	})

	return missing
}

// Reset forgets all loss state, as when the caller has confirmed that the
// sender restarted the stream. The next Received call is treated as the first
// packet of a fresh stream.
func (g *NackFeedbackGenerator) Reset() {
	g.started = false
	g.highestSeq = 0
	g.gaps = make(map[uint16]*nackGap)
}
