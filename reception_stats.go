// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package rtcp

import (
	"sync"
	"time"
)

const (
	// maxMisorder is the maximum out-of-order packet distance tolerated
	// before a jump is considered a source restart, as defined in
	// RFC 3550 Appendix A.1.
	maxMisorder = 100
	// maxDropout is the maximum sequence gap tolerated while still
	// extending the sequence space, as defined in RFC 3550 Appendix A.1.
	maxDropout = 3000
	// seqModMinusMisorder is (2^16 - maxMisorder), the threshold above
	// which a packet is treated as a reordered duplicate rather than an
	// implausible forward jump.
	seqModMinusMisorder = (1 << 16) - maxMisorder
	// minSequential is the number of consecutive packets required before
	// a source is considered valid, as defined in RFC 3550 Appendix A.1.
	minSequential = 2
)

// PacketMetadata carries the per-packet information required to update the
// reception statistics of a single source. The RTP timestamp is expressed in
// the source's clock units; arrival is expressed with the same time source
// that was supplied to NewReceptionStats.
type PacketMetadata struct {
	// SequenceNumber is the 16-bit RTP sequence number of the packet.
	SequenceNumber uint16
	// RTPTimestamp is the 32-bit RTP timestamp of the packet.
	RTPTimestamp uint32
	// Arrival is the wallclock time at which the packet arrived. It is
	// optional: when the zero value is passed, the time source given to
	// NewReceptionStats is used instead.
	Arrival time.Time
}

// ReceptionStats accumulates reception statistics for RTP packets coming from
// a single synchronization source and produces RFC 3550 reception report
// blocks.
//
// ReceptionStats implements the algorithms in RFC 3550 Appendix A.1 (sequence
// number handling and packet loss), Appendix A.3 (reception report block) and
// Appendix A.8 (interarrival jitter, LSR and DLSR).
//
// ProcessPacket and SenderReportReceived may run concurrently with
// BuildReport. Every report is produced from a single, consistent observation
// point: packet handling is blocked while a report is being assembled.
//
// A ReceptionStats instance tracks one source. Use one instance per source
// SSRC when several sources are received.
type ReceptionStats struct {
	mu        sync.Mutex
	ssrc      uint32
	clockRate uint32
	now       func() time.Time

	// Fields maintained by updateSequence (RFC 3550 A.1).
	maxSeq        uint32 // highest sequence number seen, stored in its low 16 bits
	cycles        uint32 // shifted count of sequence number cycles
	baseSeq       uint32 // first sequence number received for the source
	badSeq        uint32
	probation     uint32
	received      uint32 // packets arriving from the source, incl. duplicates
	expectedPrior uint32 // packets expected at the last report interval
	receivedPrior uint32 // packets received at the last report interval
	sourceValid   bool   // whether minSequential consecutive packets arrived

	// Interarrival jitter state (RFC 3550 A.8).
	jitter uint32
	// jitterStarted reports whether the first accepted packet anchored the
	// relative arrival and RTP timestamp origins used for transit.
	jitterStarted      bool
	jitterArrivalUnits int64
	jitterRTPBase      uint32
	// lastTransit is the signed relative transit time of the previous packet.
	lastTransit int32

	// Last sender report state (RFC 3550 A.3).
	lastSR     uint32 // middle 32 bits of the NTP timestamp of the last SR
	lastSRTime time.Time
}

// NewReceptionStats returns a ReceptionStats tracking the source identified by
// ssrc. The RTP clock frequency of the source (for example 90000 for video)
// must be given so that arrival times can be expressed in RTP timestamp units
// when estimating the interarrival jitter.
//
// The now argument supplies the time source used for packet arrivals passed
// without an explicit Arrival time, sender report arrival times and DLSR. When
// now is nil, time.Now is used.
func NewReceptionStats(ssrc uint32, clockRate uint32, now func() time.Time) *ReceptionStats {
	if now == nil {
		now = time.Now
	}

	return &ReceptionStats{
		ssrc:      ssrc,
		clockRate: clockRate,
		now:       now,
		badSeq:    uint32(1) << 16,
	}
}

// ProcessPacket records the arrival of a single RTP packet. It returns whether
// the packet belongs to a validated source. A reordered duplicate is accepted
// and counted as a received packet but never extends the sequence space.
// Packets received during probation and implausible jumps are rejected.
func (r *ReceptionStats) ProcessPacket(packet PacketMetadata) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	arrival := packet.Arrival
	if arrival.IsZero() {
		arrival = r.now()
	}

	valid := r.updateSequence(packet.SequenceNumber)
	if valid {
		if !r.jitterStarted {
			r.anchorJitter(packet.RTPTimestamp, arrival)
		}
		r.updateJitter(packet.RTPTimestamp, arrival)
	}

	return valid
}

// SenderReportReceived records the arrival of an RTCP sender report from the
// tracked source. ntpTime is the 64-bit NTP timestamp carried by the SR; the
// middle 32 bits are retained as the LSR value. arrival is the time at which
// the SR was received and defaults to the configured time source when zero.
func (r *ReceptionStats) SenderReportReceived(ntpTime uint64, arrival time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if arrival.IsZero() {
		arrival = r.now()
	}

	// Middle 32 bits out of 64 (RFC 3550 LSR): the top 16 bits of the NTP
	// timestamp are dropped and the following 32 bits retained.
	r.lastSR = uint32(ntpTime >> 16)
	r.lastSRTime = arrival
}

// BuildReport returns a ReceptionReport block reflecting the statistics at the
// instant it is called. Fraction lost is computed over the interval since the
// previous report while the other counters are cumulative. The observation
// time used for DLSR defaults to the configured time source.
func (r *ReceptionStats) BuildReport() ReceptionReport {
	return r.BuildReportAt(r.now())
}

// BuildReportAt is BuildReport with an explicit observation time. It allows a
// group of report blocks for several sources to share a single observation
// point.
func (r *ReceptionStats) BuildReportAt(observation time.Time) ReceptionReport {
	r.mu.Lock()
	defer r.mu.Unlock()

	report := ReceptionReport{SSRC: r.ssrc}

	extendedMax := r.extendedHighestSequence()
	expected := uint32(0)
	if extendedMax >= r.baseSeq {
		expected = extendedMax - r.baseSeq + 1
	}

	lost := int64(expected) - int64(r.received)
	switch {
	case lost < 0:
		lost = 0
	case lost > (1<<24)-1:
		// The cumulative number lost is a 24-bit signed field.
		lost = (1 << 24) - 1
	}

	expectedInterval := expected - r.expectedPrior
	receivedInterval := r.received - r.receivedPrior

	report.TotalLost = uint32(lost)
	report.LastSequenceNumber = extendedMax
	report.Jitter = r.jitter

	if expectedInterval > 0 {
		lostInterval := int64(expectedInterval - receivedInterval)
		if lostInterval < 0 {
			lostInterval = 0
		}

		report.FractionLost = uint8(lostInterval * 256 / int64(expectedInterval))
	}

	r.expectedPrior = expected
	r.receivedPrior = r.received

	if !r.lastSRTime.IsZero() {
		report.LastSenderReport = r.lastSR
		report.Delay = ntpDelay16(observation.Sub(r.lastSRTime))
	}

	return report
}

// updateSequence implements the update_seq routine of RFC 3550 Appendix A.1.
// It returns true when the packet belongs to the validated source and must be
// counted as received and used for the jitter estimate.
func (r *ReceptionStats) updateSequence(seq uint16) bool {
	ushift := uint32(seq)

	if !r.sourceValid {
		if r.probation == 0 {
			// First packet from a source: initialize the sequence space.
			r.initSequence(ushift)

			return false
		}

		maxSeq := uint16(r.maxSeq)
		if seq != maxSeq+1 {
			// Wait for another minSequential consecutive packets from the
			// new candidate.
			r.maxSeq = ushift
			r.probation = minSequential

			return false
		}

		r.updateMaxSeq(ushift)
		r.received++
		r.probation--

		if r.probation != 0 {
			return false
		}

		r.sourceValid = true

		return true
	}

	maxSeq := uint16(r.maxSeq)
	udelta := uint32(seq - maxSeq)

	if udelta < maxDropout {
		// In order, with permissible gap.
		if ushift < uint32(maxSeq) {
			// Sequence number wrapped: count another 64K cycle.
			r.cycles += uint32(1) << 16
		}
		r.updateMaxSeq(ushift)
	} else if udelta <= seqModMinusMisorder {
		// The sequence number made a very large jump.
		if ushift == r.badSeq {
			// Two sequential packets indicate the other side restarted
			// without telling us, so re-synchronize.
			r.initSequence(ushift)

			// The restart initializer is counted by initSequence and the
			// source is back on probation, so skip the common tail.
			return false
		}

		r.badSeq = (ushift + 1) & 0xFFFF

		return false
	}

	// udelta > seqModMinusMisorder: a duplicate or reordered packet from a
	// valid source. It is received but does not extend the sequence space.
	r.received++

	return true
}

// initSequence implements the init_seq routine of RFC 3550 Appendix A.1. The
// initializing packet itself counts as a received packet and as the first of
// the minSequential consecutive packets required to validate the source.
func (r *ReceptionStats) initSequence(seq uint32) {
	r.baseSeq = seq
	r.maxSeq = seq
	r.badSeq = uint32(1) << 16
	r.received = 1
	r.probation = minSequential - 1
	r.sourceValid = false

	// Reset the jitter reference: the (re)started source is a new timeline.
	r.jitter = 0
	r.lastTransit = 0
	r.jitterStarted = false
	r.jitterArrivalUnits = 0
	r.jitterRTPBase = 0
}

// updateMaxSeq stores the highest sequence number seen, keeping only its low
// 16 bits as in the reference implementation.
func (r *ReceptionStats) updateMaxSeq(seq uint32) {
	r.maxSeq = seq & 0xFFFF
}

// extendedHighestSequence returns cycles + the highest sequence number seen.
func (r *ReceptionStats) extendedHighestSequence() uint32 {
	return r.cycles + r.maxSeq
}

// anchorJitter records the arrival and RTP timestamp origins of the first
// observed packet so that transit time is relative.
func (r *ReceptionStats) anchorJitter(rtpTimestamp uint32, arrival time.Time) {
	r.jitterStarted = true
	r.jitterArrivalUnits = r.clockUnits(arrival)
	r.jitterRTPBase = rtpTimestamp
}

// updateJitter implements the jitter estimate of RFC 3550 Appendix A.8.
func (r *ReceptionStats) updateJitter(rtpTimestamp uint32, arrival time.Time) {
	arrivalUnits := r.clockUnits(arrival)
	if !r.jitterStarted {
		r.anchorJitter(rtpTimestamp, arrival)
	}

	relativeArrival := uint32(arrivalUnits - r.jitterArrivalUnits)
	relativeRTP := rtpTimestamp - r.jitterRTPBase

	// transit is the relative transit time of this packet, kept signed so
	// that an early arrival produces a negative value.
	transit := int32(relativeArrival) - int32(relativeRTP)
	// D is the difference between this and the previous transit time, signed
	// so that early packets produce a negative deviation.
	d := transit - r.lastTransit
	r.lastTransit = transit

	if d < 0 {
		d = -d
	}

	r.jitter += uint32(d) / 16
}

// clockUnits expresses an absolute time in the source's RTP clock units.
func (r *ReceptionStats) clockUnits(t time.Time) int64 {
	if r.clockRate == 0 {
		return 0
	}

	return t.UnixNano() * int64(r.clockRate) / int64(time.Second)
}

// ntpDelay16 converts a duration to 1/65536 second units as used by DLSR.
func ntpDelay16(d time.Duration) uint32 {
	if d <= 0 {
		return 0
	}

	return uint32(d * 65536 / time.Second)
}
