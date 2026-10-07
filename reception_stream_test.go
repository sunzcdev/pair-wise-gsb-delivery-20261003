// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package rtcp

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

const (
	testSSRC      uint32 = 0x12345678
	testClockRate uint32 = 1000
)

func feed(t *testing.T, stream *ReceptionStream, seq uint16, rtpTime uint32, at time.Time) {
	t.Helper()
	err := stream.HandleRTP(RTPReceptionInfo{
		SSRC:           testSSRC,
		SequenceNumber: seq,
		RTPTimestamp:   rtpTime,
	}, at)
	assert.NoError(t, err)
}

// TestReceptionStreamFirstTwoReports walks a short sequence by hand and checks
// the first two emitted report blocks. The RTP clock is 1000 Hz, so arrival
// time and RTP timestamps are both in milliseconds.
func TestReceptionStreamFirstTwoReports(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	stream := NewReceptionStream(testSSRC, testClockRate, nil)

	// Packets 10,11,12 arrive evenly, then a gap (13 lost), then 14 arrives
	// 40 ms late relative to its RTP timestamp.
	feed(t, stream, 10, 100, start)
	feed(t, stream, 11, 120, start.Add(20*time.Millisecond))
	feed(t, stream, 12, 140, start.Add(40*time.Millisecond))

	first := stream.receptionReportAt(start.Add(40 * time.Millisecond))
	assert.Equal(t, ReceptionReport{
		SSRC:               testSSRC,
		FractionLost:       0,
		TotalLost:          0,
		LastSequenceNumber: 12,
		Jitter:             0,
	}, first)

	feed(t, stream, 14, 160, start.Add(100*time.Millisecond))

	second := stream.receptionReportAt(start.Add(100 * time.Millisecond))
	assert.Equal(t, ReceptionReport{
		SSRC:               testSSRC,
		FractionLost:       128, // 1/2 * 256
		TotalLost:          1,
		LastSequenceNumber: 14,
		Jitter:             2, // |60-20|/16 rounded down
	}, second)

	// A third report with no new packets reports no fresh fraction lost while
	// the cumulative total is unchanged.
	third := stream.receptionReportAt(start.Add(200 * time.Millisecond))
	assert.Equal(t, uint8(0), third.FractionLost)
	assert.Equal(t, uint32(1), third.TotalLost)
	assert.Equal(t, uint32(14), third.LastSequenceNumber)
	assert.Equal(t, uint32(2), third.Jitter)
}

func TestReceptionStreamDuplicates(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	stream := NewReceptionStream(testSSRC, testClockRate, nil)

	feed(t, stream, 100, 0, start)
	feed(t, stream, 101, 20, start.Add(20*time.Millisecond))

	// Resend the highest sequence, then a late duplicate of an older one.
	feed(t, stream, 101, 20, start.Add(25*time.Millisecond))
	feed(t, stream, 100, 0, start.Add(30*time.Millisecond))

	report := stream.receptionReportAt(start.Add(30 * time.Millisecond))
	assert.Equal(t, uint32(101), report.LastSequenceNumber)
	assert.Equal(t, uint32(0), report.TotalLost)
	assert.Equal(t, uint8(0), report.FractionLost)
	// Duplicates must not perturb the jitter estimate from the in-order pair.
	assert.Equal(t, uint32(0), report.Jitter)
}

func TestReceptionStreamSequenceWrap(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	stream := NewReceptionStream(testSSRC, testClockRate, nil)

	feed(t, stream, 65534, 0, start)
	feed(t, stream, 65535, 20, start.Add(20*time.Millisecond))
	feed(t, stream, 0, 40, start.Add(40*time.Millisecond))
	// Packet 1 is lost.
	feed(t, stream, 2, 80, start.Add(80*time.Millisecond))

	report := stream.receptionReportAt(start.Add(80 * time.Millisecond))
	assert.Equal(t, uint32(1<<16+2), report.LastSequenceNumber)
	assert.Equal(t, uint32(1), report.TotalLost)
	assert.Equal(t, uint8(51), report.FractionLost) // 1/5 * 256 rounded down
}

func TestReceptionStreamSourceRestart(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	stream := NewReceptionStream(testSSRC, testClockRate, nil)

	feed(t, stream, 500, 0, start)
	feed(t, stream, 501, 20, start.Add(20*time.Millisecond))

	// A jump larger than maxDropout is treated as a source restart.
	feed(t, stream, 10, 40, start.Add(40*time.Millisecond))
	feed(t, stream, 11, 60, start.Add(60*time.Millisecond))

	report := stream.receptionReportAt(start.Add(60 * time.Millisecond))
	assert.Equal(t, uint32(11), report.LastSequenceNumber)
	assert.Equal(t, uint32(0), report.TotalLost)
	assert.Equal(t, uint8(0), report.FractionLost)
	assert.Equal(t, uint32(0), report.Jitter)
}

func TestReceptionStreamLatePacketAndLossClamp(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	stream := NewReceptionStream(testSSRC, testClockRate, nil)

	// Jump ahead by maxDropout - 1 to register a large cumulative loss.
	feed(t, stream, 10, 0, start)
	feed(t, stream, 10+maxDropout-1, uint32(maxDropout-1)*20, start.Add((maxDropout-1)*20*time.Millisecond))

	before := stream.receptionReportAt(start.Add((maxDropout - 1) * 20 * time.Millisecond))
	assert.Equal(t, uint32(maxDropout-2), before.TotalLost)
	assert.Equal(t, uint8(255), before.FractionLost) // 2998/3000*256 rounds to 255

	// A late packet just below the maximum, within the misorder window, fills
	// one gap but must not roll the extended highest sequence backwards.
	feed(t, stream, 10+maxDropout-2, uint32(maxDropout-2)*20, start.Add(((maxDropout-1)*20+5)*time.Millisecond))
	after := stream.receptionReportAt(start.Add(((maxDropout-1)*20 + 5) * time.Millisecond))
	assert.Equal(t, uint32(10+maxDropout-1), after.LastSequenceNumber)
	assert.Equal(t, uint32(maxDropout-3), after.TotalLost)
	assert.Equal(t, uint8(0), after.FractionLost) // new loss is negative -> 0
}

func TestReceptionStreamNoSenderReport(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	stream := NewReceptionStream(testSSRC, testClockRate, nil)
	feed(t, stream, 1, 0, start)

	report := stream.receptionReportAt(start.Add(time.Second))
	assert.Equal(t, uint32(0), report.LastSenderReport)
	assert.Equal(t, uint32(0), report.Delay)
}

func TestReceptionStreamSenderReportLSRDLSR(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	stream := NewReceptionStream(testSSRC, testClockRate, nil)
	feed(t, stream, 1, 0, start)

	// NTP timestamp whose middle 32 bits are 0x11223344.
	const ntp = uint64(0x1122334455660000)
	sr := &SenderReport{SSRC: testSSRC, NTPTime: ntp}
	assert.NoError(t, stream.HandleSenderReport(sr, start.Add(time.Second)))

	report := stream.receptionReportAt(start.Add(2 * time.Second))
	assert.Equal(t, uint32(0x33445566), report.LastSenderReport)
	// 1 second since the SR => 65536 DLSR units.
	assert.Equal(t, uint32(dlsrScale), report.Delay)
}

func TestReceptionStreamSSRCMismatch(t *testing.T) {
	stream := NewReceptionStream(testSSRC, testClockRate, nil)
	err := stream.HandleRTP(RTPReceptionInfo{SSRC: testSSRC + 1}, time.Now())
	assert.ErrorIs(t, err, errSSRCMismatch)

	err = stream.HandleSenderReport(&SenderReport{SSRC: testSSRC + 1}, time.Now())
	assert.ErrorIs(t, err, errSSRCMismatch)
}

func TestReceptionStreamEmptyReport(t *testing.T) {
	stream := NewReceptionStream(testSSRC, testClockRate, nil)
	report := stream.ReceptionReport()
	assert.Equal(t, testSSRC, report.SSRC)
	assert.Equal(t, uint32(0), report.LastSequenceNumber)
	assert.Equal(t, uint32(0), report.Jitter)
}

func TestReceptionTrackerMultipleSources(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := func() time.Time { return start.Add(time.Second) }
	tracker := NewReceptionTracker(testClockRate, now)

	assert.NoError(t, tracker.HandleRTP(RTPReceptionInfo{SSRC: 2, SequenceNumber: 10}, start))
	assert.NoError(t, tracker.HandleRTP(RTPReceptionInfo{SSRC: 1, SequenceNumber: 20}, start))

	reports := tracker.ReceptionReports()
	assert.Len(t, reports, 2)
	assert.Equal(t, uint32(1), reports[0].SSRC)
	assert.Equal(t, uint32(20), reports[0].LastSequenceNumber)
	assert.Equal(t, uint32(2), reports[1].SSRC)
	assert.Equal(t, uint32(10), reports[1].LastSequenceNumber)

	// Blocks marshal directly into an RR packet.
	rr := ReceiverReport{SSRC: 99, Reports: reports}
	raw, err := rr.Marshal()
	assert.NoError(t, err)

	var decoded ReceiverReport
	assert.NoError(t, decoded.Unmarshal(raw))
	assert.Equal(t, rr.SSRC, decoded.SSRC)
	assert.Equal(t, rr.Reports, decoded.Reports)
}

func TestReceptionStreamConcurrent(t *testing.T) {
	start := time.Unix(0, 0)
	stream := NewReceptionStream(testSSRC, testClockRate, nil)

	const total = 5000

	// Seed the stream so concurrent duplicates cannot be mistaken for the
	// first packet of a restarted source.
	feed(t, stream, 0, 0, start)

	var readers sync.WaitGroup
	stop := make(chan struct{})

	// Concurrently generate snapshots while the sequence is ingested.
	for g := 0; g < 4; g++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = stream.ReceptionReport()
				}
			}
		}()
	}

	for seq := 1; seq < total; seq++ {
		at := start.Add(time.Duration(seq) * 20 * time.Millisecond)
		err := stream.HandleRTP(RTPReceptionInfo{
			SSRC:           testSSRC,
			SequenceNumber: uint16(seq),
			RTPTimestamp:   uint32(seq) * 20,
		}, at)
		assert.NoError(t, err)
	}

	close(stop)
	readers.Wait()

	report := stream.ReceptionReport()
	assert.Equal(t, uint32(total-1), report.LastSequenceNumber)
	assert.Equal(t, uint32(0), report.TotalLost)
}
