// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package rtcp

import (
	"sort"
	"sync"
	"time"
)

// Sequence-number validity thresholds from RFC 3550 appendix A.1.
const (
	// maxDropout is the maximum number of sequential packets that may be
	// missing without treating the jump as a source restart.
	maxDropout = 3000
	// maxMisorder is the maximum gap backwards that is still tolerated as a
	// reordered (late) packet rather than a sequence-number wrap.
	maxMisorder = 100
	// uint16Space is the number of values in a 16-bit sequence number.
	uint16Space = 1 << 16
	// uint24Max is the largest value that fits in the 24-bit lost field.
	uint24Max = (1 << 24) - 1
	// dlsrScale is the number of DLSR units (1/65536 s) in one second.
	dlsrScale = 65536
)

// RTPReceptionInfo carries the RTP packet metadata a receiver needs in order
// to account for a packet. The full packet is not required.
type RTPReceptionInfo struct {
	// SSRC is the synchronization source identifier of the packet.
	SSRC uint32
	// SequenceNumber is the 16-bit RTP sequence number of the packet.
	SequenceNumber uint16
	// RTPTimestamp is the RTP timestamp of the packet, in clock ticks.
	RTPTimestamp uint32
}

// ReceptionStream accumulates reception statistics for RTP packets coming from
// a single source and turns them into a ReceptionReport block as defined in
// RFC 3550 sections 6.4 and appendix A.
//
// The stream is safe for concurrent use: RTP packets and RTCP sender reports
// may be ingested while ReceptionReport blocks are produced. Each report is a
// snapshot taken at one consistent observation point, so its fields never mix
// state from different arrivals.
type ReceptionStream struct {
	ssrc      uint32
	clockRate uint32
	now       func() time.Time

	mu sync.Mutex

	// Sequence-number bookkeeping (appendix A.1/A.8).
	baseSeq       uint32
	maxSeq        uint16
	cycles        uint32
	received      uint32
	receivedPrior uint32
	expectedPrior uint32
	initialized   bool

	// Interarrival jitter (section 6.4.4 / appendix A.8), measured in RTP
	// clock ticks.
	jitter           uint32
	lastArrival      time.Time
	lastRTPTimestamp uint32
	referenceValid   bool

	// Most recent sender report timing, used for LSR and DLSR (section 6.4.1).
	lastSRNTP     uint32
	lastSRArrival time.Time
}

// NewReceptionStream returns a ReceptionStream tracking the source identified
// by ssrc. clockRate is the RTP clock frequency in hertz and is used to scale
// wall-clock arrival time into RTP timestamp units for jitter. now supplies
// the current time when reports are built; if nil, time.Now is used. The
// clock and time source are intentionally provided by the caller so the
// stream stays independent of any global clock.
func NewReceptionStream(ssrc uint32, clockRate uint32, now func() time.Time) *ReceptionStream {
	if now == nil {
		now = time.Now
	}

	return &ReceptionStream{
		ssrc:      ssrc,
		clockRate: clockRate,
		now:       now,
	}
}

// HandleRTP accounts for a single received RTP packet using its metadata and
// the time at which it arrived. A repeated (duplicate) packet increments the
// total received count but is never mistaken for a new sequence number: it
// does not extend the highest sequence number, does not add loss, and does not
// feed the jitter estimator.
func (s *ReceptionStream) HandleRTP(info RTPReceptionInfo, arrival time.Time) error {
	if info.SSRC != s.ssrc {
		return errSSRCMismatch
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.received++

	if !s.initialized {
		s.initSequence(info)
		s.updateJitter(info.RTPTimestamp, arrival)

		return nil
	}

	// Compute the signed 16-bit distance so a step backwards (a late packet)
	// is not mistaken for a large positive jump.
	udelta := int32(int16(info.SequenceNumber - s.maxSeq))
	switch {
	case udelta == 0:
		// Duplicate of the highest sequence seen so far: counted above, but it
		// carries no new sequence or timing information.
		return nil
	case udelta > 0 && udelta <= maxDropout:
		// In order, possibly with gaps. A forward step across the 16-bit edge
		// adds one cycle count.
		if info.SequenceNumber < s.maxSeq {
			s.cycles += uint16Space
		}
		s.maxSeq = info.SequenceNumber
		s.updateJitter(info.RTPTimestamp, arrival)
	case udelta < 0 && -udelta <= maxMisorder:
		// Reordered (late) packet. It is a new sequence number below the
		// current maximum, so it is already counted; no jitter update.
	default:
		// The jump is too large to be loss or reordering: the source has
		// restarted its sequence number. Restart reception bookkeeping.
		s.initSequence(info)
		s.updateJitter(info.RTPTimestamp, arrival)
	}

	return nil
}

// initSequence establishes or resets sequence-number state as described in
// RFC 3550 appendix A.1 ("init_seq"), treating the given packet as the first
// packet of the source.
func (s *ReceptionStream) initSequence(info RTPReceptionInfo) {
	s.baseSeq = uint32(info.SequenceNumber)
	s.maxSeq = info.SequenceNumber
	s.cycles = 0
	s.receivedPrior = 0
	s.expectedPrior = 0
	s.received = 1
	s.initialized = true

	// The jitter estimate restarts with this packet as its reference.
	s.jitter = 0
	s.referenceValid = false
	s.lastArrival = time.Time{}
}

// updateJitter advances the interarrival jitter estimator with the packet's
// RTP timestamp and arrival time. The first accepted packet only seeds the
// reference arrival/timestamp pair. Differences, rather than absolute times,
// are used to avoid overflow from converting large wall-clock values.
func (s *ReceptionStream) updateJitter(rtpTimestamp uint32, arrival time.Time) {
	if s.clockRate == 0 {
		return
	}

	if !s.referenceValid {
		s.lastArrival = arrival
		s.lastRTPTimestamp = rtpTimestamp
		s.referenceValid = true

		return
	}

	arrivalDeltaTicks := arrival.Sub(s.lastArrival).Nanoseconds() * int64(s.clockRate) / int64(time.Second)
	rtpDelta := int64(rtpTimestamp - s.lastRTPTimestamp)
	d := int64(arrivalDeltaTicks) - rtpDelta
	if d < 0 {
		d = -d
	}

	// RFC 3550 section 6.4.4 / appendix A.8: J += (|D| - J)/16. The
	// difference must stay signed so a smaller deviation reduces jitter.
	s.jitter = uint32(int64(s.jitter) + (d-int64(s.jitter))/16)
	s.lastArrival = arrival
	s.lastRTPTimestamp = rtpTimestamp
}

// HandleSenderReport records the most recent RTCP sender report from the
// tracked source so that later report blocks can populate LSR and DLSR. The
// SR must originate from the tracked SSRC.
func (s *ReceptionStream) HandleSenderReport(sr *SenderReport, arrival time.Time) error {
	if sr.SSRC != s.ssrc {
		return errSSRCMismatch
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.lastSRNTP = ntpMiddle32(sr.NTPTime)
	s.lastSRArrival = arrival

	return nil
}

// ReceptionReport builds a ReceptionReport block from the statistics observed
// up to now. The wall clock is taken from the caller-supplied time source so
// DLSR reflects a caller-chosen observation instant.
func (s *ReceptionStream) ReceptionReport() ReceptionReport {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.buildReport(s.now())
}

// receptionReportAt is the same snapshot as ReceptionReport but evaluated at
// an explicitly provided observation time.
func (s *ReceptionStream) receptionReportAt(now time.Time) ReceptionReport {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.buildReport(now)
}

// buildReport computes every field of the report block in one place while the
// caller holds mu, guaranteeing a consistent observation point.
func (s *ReceptionStream) buildReport(now time.Time) ReceptionReport {
	report := ReceptionReport{SSRC: s.ssrc}
	if !s.initialized {
		report.LastSequenceNumber = s.baseSeq

		return report
	}

	extendedMax := s.cycles + uint32(s.maxSeq)
	expected := extendedMax - s.baseSeq + 1

	// Total packets lost (section 6.4.1): expected minus actually received,
	// clamped to the 24-bit field range.
	totalLost := clampLost(int64(expected) - int64(s.received))

	// Fraction lost over the last interval (appendix A.8).
	expectedInterval := expected - s.expectedPrior
	receivedInterval := s.received - s.receivedPrior
	fractionLost := uint8(0)
	if expectedInterval > 0 {
		lostInterval := int64(expectedInterval) - int64(receivedInterval)
		if lostInterval > 0 {
			fractionLost = uint8(lostInterval * 256 / int64(expectedInterval))
		}
	}

	s.expectedPrior = expected
	s.receivedPrior = s.received

	report.FractionLost = fractionLost
	report.TotalLost = totalLost
	report.LastSequenceNumber = extendedMax
	report.Jitter = s.jitter
	report.LastSenderReport = s.lastSRNTP
	if !s.lastSRArrival.IsZero() {
		elapsedNanos := now.Sub(s.lastSRArrival).Nanoseconds()
		if elapsedNanos > 0 {
			report.Delay = uint32(elapsedNanos * dlsrScale / int64(time.Second))
		}
	}

	return report
}

// clampLost bounds a lost-packet count to the 24-bit cumulative lost field,
// following RFC 3550 appendix A.8's saturation and zero-floor rules.
func clampLost(lost int64) uint32 {
	switch {
	case lost < 0:
		return 0
	case lost > uint24Max:
		return uint24Max
	default:
		return uint32(lost)
	}
}

// ntpMiddle32 extracts the middle 32 bits of a 64-bit NTP timestamp, which is
// the value carried in the LSR field (RFC 3550 sections 4 and 6.4.1).
func ntpMiddle32(ntp uint64) uint32 {
	return uint32((ntp << 16) >> 32)
}

// ReceptionTracker keeps one ReceptionStream per source so a receiver tracking
// several SSRCs can ingest packets and build a full set of report blocks.
type ReceptionTracker struct {
	clockRate uint32
	now       func() time.Time

	mu      sync.Mutex
	streams map[uint32]*ReceptionStream
}

// NewReceptionTracker creates a tracker whose streams all use clockRate for
// jitter and now as their observation clock.
func NewReceptionTracker(clockRate uint32, now func() time.Time) *ReceptionTracker {
	if now == nil {
		now = time.Now
	}

	return &ReceptionTracker{
		clockRate: clockRate,
		now:       now,
		streams:   make(map[uint32]*ReceptionStream),
	}
}

// Stream returns the stream for ssrc, creating it on first use.
func (t *ReceptionTracker) Stream(ssrc uint32) *ReceptionStream {
	t.mu.Lock()
	defer t.mu.Unlock()

	stream, ok := t.streams[ssrc]
	if !ok {
		stream = NewReceptionStream(ssrc, t.clockRate, t.now)
		t.streams[ssrc] = stream
	}

	return stream
}

// HandleRTP routes an RTP packet to the stream for its source.
func (t *ReceptionTracker) HandleRTP(info RTPReceptionInfo, arrival time.Time) error {
	return t.Stream(info.SSRC).HandleRTP(info, arrival)
}

// HandleSenderReport routes a sender report to the stream for its source.
func (t *ReceptionTracker) HandleSenderReport(sr *SenderReport, arrival time.Time) error {
	return t.Stream(sr.SSRC).HandleSenderReport(sr, arrival)
}

// ReceptionReports returns a snapshot report block for every tracked source.
// The blocks are ordered by SSRC for deterministic output and each reflects
// the same caller-provided observation time.
func (t *ReceptionTracker) ReceptionReports() []ReceptionReport {
	t.mu.Lock()
	ssrcs := make([]uint32, 0, len(t.streams))
	for ssrc := range t.streams {
		ssrcs = append(ssrcs, ssrc)
	}
	sort.Slice(ssrcs, func(i, j int) bool { return ssrcs[i] < ssrcs[j] })

	streams := make([]*ReceptionStream, 0, len(ssrcs))
	for _, ssrc := range ssrcs {
		streams = append(streams, t.streams[ssrc])
	}
	now := t.now()
	t.mu.Unlock()

	reports := make([]ReceptionReport, 0, len(ssrcs))
	for _, stream := range streams {
		reports = append(reports, stream.receptionReportAt(now))
	}

	return reports
}
