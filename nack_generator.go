// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package rtcp

import (
	"errors"
	"math"
	"sort"
	"time"
)

const (
	defaultNackReorderWindow = 50 * time.Millisecond
	defaultNackRetryInterval = 100 * time.Millisecond
	defaultNackMaxLifetime   = 2 * time.Second
	defaultNackMaxTracked    = 512

	// maxNackPairs is the largest number of NackPair entries a single
	// TransportLayerNack can carry (the report count field is a uint8 and the
	// two SSRC fields occupy the first two length units).
	maxNackPairs = math.MaxUint8 - tlnLength
)

var (
	// ErrNackStreamRestart is returned by PacketReceived when the sequence
	// number jump is larger than the configured loss window. The suspected
	// restart is not acted on until the caller confirms it with
	// ConfirmRestart.
	ErrNackStreamRestart = errors.New("rtcp: sequence gap exceeds the maximum tracked loss, stream restart suspected")

	// ErrNackInvalidConfig is returned by NewNackGenerator when the supplied
	// configuration cannot be used.
	ErrNackInvalidConfig = errors.New("rtcp: invalid nack generator configuration")
)

// ReceptionClass describes how PacketReceived classified an incoming RTP
// sequence number against the shared missing-packet state.
type ReceptionClass uint8

const (
	// ReceptionNew is reported for a sequence number that advances the stream.
	// Gaps opened before it are registered as missing.
	ReceptionNew ReceptionClass = iota + 1

	// ReceptionDuplicate is reported for a sequence number that was already
	// received, or one that fell inside an untracked gap because the bounded
	// state was exhausted. The missing state is left unchanged.
	ReceptionDuplicate

	// ReceptionLate is reported for a sequence number that was previously
	// registered as missing. It is removed from the missing state, cancelling
	// every pending and future retransmission request for it.
	ReceptionLate

	// ReceptionRestart is reported together with ErrNackStreamRestart when the
	// jump from the highest seen sequence number is larger than MaxTracked.
	// No state is modified until the caller invokes ConfirmRestart.
	ReceptionRestart
)

// NackGeneratorConfig configures a NackGenerator. Zero values are replaced
// with the documented defaults.
type NackGeneratorConfig struct {
	// SenderSSRC is used as the sender SSRC of generated feedback packets.
	SenderSSRC uint32

	// MediaSSRC identifies the RTP stream and is used as the media SSRC of
	// generated feedback packets. A separate generator must be used per
	// media stream.
	MediaSSRC uint32

	// ReorderWindow is the time a gap is observed before it is reported for
	// the first time, giving late packets a chance to arrive.
	ReorderWindow time.Duration

	// RetryInterval is the minimum time between two requests for the same
	// missing sequence number.
	RetryInterval time.Duration

	// MaxLifetime bounds how long a missing sequence number is requested at
	// all. Once it expires the sequence number is dropped from the state and
	// reported in NackFeedbackInfo.Expired.
	MaxLifetime time.Duration

	// MaxTracked bounds the number of simultaneously tracked missing
	// sequence numbers, keeping the generator state finite.
	MaxTracked int
}

// ReceptionResult is the result of feeding one received sequence number to
// the generator.
type ReceptionResult struct {
	// Class is how the sequence number was classified.
	Class ReceptionClass

	// Sequence is the received sequence number.
	Sequence uint16

	// ReorderDelay is the time between the gap being opened and filled. It is
	// only set for ReceptionLate.
	ReorderDelay time.Duration
}

// NackFeedbackInfo explains which missing sequence numbers drove a feedback
// packet, allowing callers to tell the first request, a retry and an expiry
// apart without decoding NackPair bitmaps.
type NackFeedbackInfo struct {
	// FirstRequests are sequence numbers reported for the first time after
	// the reorder window elapsed.
	FirstRequests []uint16

	// Retries are sequence numbers requested again after RetryInterval.
	Retries []uint16

	// Expired are sequence numbers that reached MaxLifetime without
	// arriving and were removed from the missing state.
	Expired []uint16
}

// nackEntry is the per-sequence loss state.
type nackEntry struct {
	extSeq           uint64
	firstSeenMissing time.Time
	lastSent         time.Time
	sentCount        int
}

// NackGenerator is a stateful generator of TransportLayerNack feedback. It
// delays reporting gaps to tolerate reordering, retries unfulfilled requests
// on an interval, cancels them when late packets arrive and eventually gives
// up after a maximum lifetime.
//
// All timestamps are supplied by the caller, so the generator never starts
// goroutines or sleeps and is trivially testable with a virtual clock.
//
// NackGenerator is not safe for concurrent use.
type NackGenerator struct {
	config NackGeneratorConfig

	initialized bool
	lastExtSeq  uint64
	missing     map[uint16]nackEntry
}

// NewNackGenerator creates a NackGenerator with the given configuration.
func NewNackGenerator(config NackGeneratorConfig) (*NackGenerator, error) {
	if config.ReorderWindow == 0 {
		config.ReorderWindow = defaultNackReorderWindow
	}
	if config.RetryInterval == 0 {
		config.RetryInterval = defaultNackRetryInterval
	}
	if config.MaxLifetime == 0 {
		config.MaxLifetime = defaultNackMaxLifetime
	}
	if config.MaxTracked == 0 {
		config.MaxTracked = defaultNackMaxTracked
	}

	if config.ReorderWindow < 0 || config.RetryInterval < 0 || config.MaxLifetime < 0 {
		return nil, ErrNackInvalidConfig
	}
	if config.ReorderWindow >= config.MaxLifetime {
		return nil, ErrNackInvalidConfig
	}
	if config.MaxTracked < 0 {
		return nil, ErrNackInvalidConfig
	}

	return &NackGenerator{
		config:  config,
		missing: make(map[uint16]nackEntry, config.MaxTracked),
	}, nil
}

// PacketReceived records that the RTP packet with the given sequence number
// arrived at the given time. It updates the same missing state that feeds
// Feedback: forward jumps open gaps, late packets close them and duplicates
// leave the state untouched.
//
// A jump larger than MaxTracked is treated as an obvious sender restart. The
// state is not modified in that case; ErrNackStreamRestart is returned and
// the caller must confirm the restart with ConfirmRestart to rebuild the
// stream.
func (g *NackGenerator) PacketReceived(sequence uint16, arrival time.Time) (ReceptionResult, error) {
	result := ReceptionResult{Sequence: sequence}

	if !g.initialized {
		g.initialized = true
		g.lastExtSeq = uint64(sequence)
		result.Class = ReceptionNew

		return result, nil
	}

	highest := uint16(g.lastExtSeq)
	forwardDistance := sequence - highest

	switch {
	case forwardDistance == 0:
		result.Class = ReceptionDuplicate

		return result, nil

	case forwardDistance <= math.MaxInt16:
		return g.handleForward(sequence, arrival, forwardDistance, result)

	default:
		return g.handleBackward(sequence, arrival, highest, result)
	}
}

// ConfirmRestart rebuilds the stream state after the caller confirmed that
// the jump reported with ErrNackStreamRestart was a real sender restart. All
// pending loss state is discarded and sequence becomes the new baseline.
func (g *NackGenerator) ConfirmRestart(sequence uint16) {
	clear(g.missing)
	g.initialized = true
	g.lastExtSeq = uint64(sequence)
}

// Feedback produces the TransportLayerNack packet that is ready to send at
// the given time, together with a classification of the missing sequence
// numbers involved.
//
// Expired entries are always removed and listed in the info. Entries are
// requested once after ReorderWindow and retried every RetryInterval until
// MaxLifetime. It returns nil when no sequence number is due for a request.
func (g *NackGenerator) Feedback(now time.Time) (*TransportLayerNack, NackFeedbackInfo) {
	info := NackFeedbackInfo{}
	if !g.initialized {
		return nil, info
	}

	type pending struct {
		sequence uint16
		extSeq   uint64
		retry    bool
	}

	ready := make([]pending, 0, len(g.missing))
	expired := make([]pending, 0)

	for sequence, entry := range g.missing {
		age := now.Sub(entry.firstSeenMissing)
		switch {
		case age >= g.config.MaxLifetime:
			expired = append(expired, pending{sequence: sequence, extSeq: entry.extSeq})
			delete(g.missing, sequence)

		case entry.sentCount == 0 && age >= g.config.ReorderWindow:
			ready = append(ready, pending{sequence: sequence, extSeq: entry.extSeq})

		case entry.sentCount > 0 && now.Sub(entry.lastSent) >= g.config.RetryInterval:
			ready = append(ready, pending{sequence: sequence, extSeq: entry.extSeq, retry: true})
		}
	}

	sort.Slice(expired, func(i, j int) bool { return expired[i].extSeq < expired[j].extSeq })
	sort.Slice(ready, func(i, j int) bool { return ready[i].extSeq < ready[j].extSeq })

	info.Expired = make([]uint16, 0, len(expired))
	for _, item := range expired {
		info.Expired = append(info.Expired, item.sequence)
	}

	if len(ready) == 0 {
		return nil, info
	}

	sequences := make([]uint16, 0, len(ready))
	for _, item := range ready {
		sequences = append(sequences, item.sequence)
	}

	pairs, included := nackPairsCapped(sequences, maxNackPairs)
	if len(included) == 0 {
		return nil, info
	}

	includedSet := make(map[uint16]struct{}, len(included))
	for _, sequence := range included {
		includedSet[sequence] = struct{}{}
	}

	for _, item := range ready {
		if _, ok := includedSet[item.sequence]; !ok {
			continue
		}

		entry := g.missing[item.sequence]
		if entry.sentCount == 0 {
			info.FirstRequests = append(info.FirstRequests, item.sequence)
		} else {
			info.Retries = append(info.Retries, item.sequence)
		}

		entry.lastSent = now
		entry.sentCount++
		g.missing[item.sequence] = entry
	}

	return &TransportLayerNack{
		SenderSSRC: g.config.SenderSSRC,
		MediaSSRC:  g.config.MediaSSRC,
		Nacks:      pairs,
	}, info
}

// Pending returns the number of missing sequence numbers currently tracked.
func (g *NackGenerator) Pending() int {
	return len(g.missing)
}

func (g *NackGenerator) handleForward(
	sequence uint16,
	arrival time.Time,
	forwardDistance uint16,
	result ReceptionResult,
) (ReceptionResult, error) {
	if int(forwardDistance)-1 > g.config.MaxTracked {
		result.Class = ReceptionRestart

		return result, ErrNackStreamRestart
	}

	highest := uint16(g.lastExtSeq)
	for offset := uint16(1); offset < forwardDistance; offset++ {
		missingSequence := highest + offset
		if _, ok := g.missing[missingSequence]; ok {
			continue
		}

		if len(g.missing) >= g.config.MaxTracked {
			continue
		}

		g.missing[missingSequence] = nackEntry{
			extSeq:           g.lastExtSeq + uint64(offset),
			firstSeenMissing: arrival,
		}
	}

	g.lastExtSeq += uint64(forwardDistance)
	result.Class = ReceptionNew

	return result, nil
}

func (g *NackGenerator) handleBackward(
	sequence uint16,
	arrival time.Time,
	highest uint16,
	result ReceptionResult,
) (ReceptionResult, error) {
	backwardDistance := uint64(highest - sequence)
	if backwardDistance > g.lastExtSeq {
		result.Class = ReceptionDuplicate

		return result, nil
	}

	entry, ok := g.missing[sequence]
	if !ok {
		result.Class = ReceptionDuplicate

		return result, nil
	}

	delete(g.missing, sequence)
	result.Class = ReceptionLate
	result.ReorderDelay = arrival.Sub(entry.firstSeenMissing)

	return result, nil
}

// nackPairsCapped packs sorted sequence numbers into at most maxPairs
// NackPair values and also returns the sequence numbers represented.
// Sequence numbers that do not fit because of the pair limit are left out
// and keep their previous request timing.
func nackPairsCapped(sequences []uint16, maxPairs int) ([]NackPair, []uint16) {
	pairs := make([]NackPair, 0, maxPairs)
	included := make([]uint16, 0, len(sequences))

	packetID := sequences[0]
	bitmap := PacketBitmap(0)
	members := []uint16{packetID}

	flush := func() {
		pairs = append(pairs, NackPair{PacketID: packetID, LostPackets: bitmap})
		included = append(included, members...)
	}

	for i := 1; i < len(sequences); i++ {
		next := sequences[i]
		distance := next - packetID
		if distance > 16 {
			flush()
			if len(pairs) == maxPairs {
				return pairs, included
			}

			packetID = next
			bitmap = 0
			members = []uint16{next}

			continue
		}

		bitmap |= 1 << (distance - 1)
		members = append(members, next)
	}

	flush()

	return pairs, included
}
