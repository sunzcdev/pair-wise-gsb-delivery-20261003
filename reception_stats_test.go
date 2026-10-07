// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package rtcp

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSSRC = 0x12345678

func packet(seq uint16, rtpTS uint32, arrival time.Time) PacketMetadata {
	return PacketMetadata{
		SequenceNumber: seq,
		RTPTimestamp:   rtpTS,
		Arrival:        arrival,
	}
}

// TestReceptionStatsTwoReports walks through a short, hand-computable sequence
// and checks the first two generated reports, including interval loss,
// jitter and LSR/DLSR.
func TestReceptionStatsTwoReports(t *testing.T) {
	t0 := time.Unix(1700000000, 0)
	stats := NewReceptionStats(testSSRC, 1000, nil)

	// The first packet puts the source on probation and anchors nothing yet;
	// packet 1 validates the source. Seq 1..4 then form a lossless window
	// with an even 20ms spacing, so jitter remains zero.
	assert.False(t, stats.ProcessPacket(packet(0, 1000, t0)))
	require.True(t, stats.ProcessPacket(packet(1, 1020, t0.Add(20*time.Millisecond))))
	require.True(t, stats.ProcessPacket(packet(2, 1040, t0.Add(40*time.Millisecond))))
	require.True(t, stats.ProcessPacket(packet(3, 1060, t0.Add(60*time.Millisecond))))
	require.True(t, stats.ProcessPacket(packet(4, 1080, t0.Add(80*time.Millisecond))))

	// First report: base is seq 1, seq 1..4 expected (4) and received (4):
	// nothing lost.
	first := stats.BuildReportAt(t0.Add(80 * time.Millisecond))
	assert.Equal(t, ReceptionReport{
		SSRC:               testSSRC,
		FractionLost:       0,
		TotalLost:          0,
		LastSequenceNumber: 4,
		Jitter:             0,
		LastSenderReport:   0,
		Delay:              0,
	}, first)

	// seq 5 arrives on time. seq 6 is lost. seq 7 has a normal 40ms RTP step
	// (two packets worth) but arrives 60ms after seq 5: it is 20ms late,
	// which is a genuine interarrival deviation (D = 20).
	require.True(t, stats.ProcessPacket(packet(5, 1100, t0.Add(100*time.Millisecond))))
	require.True(t, stats.ProcessPacket(packet(7, 1140, t0.Add(160*time.Millisecond))))

	// SR received at 165ms; LSR is the middle 32 bits of the NTP timestamp.
	stats.SenderReportReceived(0xAABBCCDD11223344, t0.Add(165*time.Millisecond))

	// 15.625ms after the SR -> DLSR 1024 (1/64 s in 1/65536 s units).
	second := stats.BuildReportAt(t0.Add(180_625 * time.Microsecond))

	// Interval: expected 3 (seq 5,6,7), received 2 (5 and 7), so
	// fraction = 1*256/3 = 85 and cumulative lost is 1.
	assert.Equal(t, ReceptionReport{
		SSRC:               testSSRC,
		FractionLost:       85,
		TotalLost:          1,
		LastSequenceNumber: 7,
		Jitter:             1,
		LastSenderReport:   0xCCDD1122,
		Delay:              1024,
	}, second)
}

// TestReceptionStatsFractionLost checks the interval fraction when a packet is
// lost after the source has been validated.
func TestReceptionStatsFractionLost(t *testing.T) {
	t0 := time.Unix(1700000000, 0)
	stats := NewReceptionStats(testSSRC, 1000, nil)

	_ = stats.ProcessPacket(packet(0, 0, t0))
	require.True(t, stats.ProcessPacket(packet(1, 0, t0.Add(time.Millisecond))))

	// Baseline report: seq 0..1 expected and received, nothing lost.
	assert.Equal(t, uint32(0), stats.BuildReport().TotalLost)

	// seq 2 lost, seq 3 received. Interval: expected 2, received 1.
	require.True(t, stats.ProcessPacket(packet(3, 0, t0.Add(2*time.Millisecond))))
	report := stats.BuildReportAt(t0.Add(3 * time.Millisecond))
	assert.Equal(t, uint8(128), report.FractionLost) // 1 * 256 / 2
	assert.Equal(t, uint32(1), report.TotalLost)
}

// TestReceptionStatsDuplicate verifies that a repeated packet neither advances
// the extended highest sequence nor turns received loss into a gain, even
// though it is accepted from a valid source.
func TestReceptionStatsDuplicate(t *testing.T) {
	t0 := time.Unix(1700000000, 0)
	stats := NewReceptionStats(testSSRC, 1000, nil)

	_ = stats.ProcessPacket(packet(0, 0, t0))
	require.True(t, stats.ProcessPacket(packet(1, 0, t0.Add(time.Millisecond))))
	require.True(t, stats.ProcessPacket(packet(2, 0, t0.Add(2*time.Millisecond))))

	// Baseline report anchors the interval.
	assert.Equal(t, uint32(0), stats.BuildReportAt(t0.Add(2*time.Millisecond)).TotalLost)

	for i := 0; i < 5; i++ {
		assert.True(t, stats.ProcessPacket(packet(2, 0, t0.Add(time.Duration(3+i)*time.Millisecond))))
	}

	report := stats.BuildReportAt(t0.Add(10 * time.Millisecond))
	assert.Equal(t, uint32(2), report.LastSequenceNumber)
	assert.Equal(t, uint32(0), report.TotalLost)
	assert.Equal(t, uint8(0), report.FractionLost)
}

// TestReceptionStatsSequenceWrap covers the 16-bit sequence number wrap.
func TestReceptionStatsSequenceWrap(t *testing.T) {
	t0 := time.Unix(1700000000, 0)
	stats := NewReceptionStats(testSSRC, 1000, nil)

	_ = stats.ProcessPacket(packet(65533, 0, t0))
	require.True(t, stats.ProcessPacket(packet(65534, 0, t0.Add(time.Millisecond))))
	require.True(t, stats.ProcessPacket(packet(65535, 0, t0.Add(2*time.Millisecond))))
	require.True(t, stats.ProcessPacket(packet(0, 0, t0.Add(3*time.Millisecond))))
	require.True(t, stats.ProcessPacket(packet(1, 0, t0.Add(4*time.Millisecond))))

	// Seq 65533..65533+65536+1 => extended highest is 0x10001 = 65537.
	report := stats.BuildReportAt(t0.Add(5 * time.Millisecond))
	assert.Equal(t, uint32(65537), report.LastSequenceNumber)
	assert.Equal(t, uint32(0), report.TotalLost)
	assert.Equal(t, uint8(0), report.FractionLost)
}

// TestReceptionStatsRestart covers a source that restarts its sequence number
// with an implausible jump. The jumped packets are rejected until the restart
// is revalidated, and counters are re-based on the restarted source.
func TestReceptionStatsRestart(t *testing.T) {
	t0 := time.Unix(1700000000, 0)
	stats := NewReceptionStats(testSSRC, 1000, nil)

	_ = stats.ProcessPacket(packet(0, 0, t0))
	require.True(t, stats.ProcessPacket(packet(1, 0, t0.Add(time.Millisecond))))
	require.True(t, stats.ProcessPacket(packet(2, 0, t0.Add(2*time.Millisecond))))

	// An implausible forward jump (> 3000) is ignored and arms the probe.
	assert.False(t, stats.ProcessPacket(packet(5000, 0, t0.Add(3*time.Millisecond))))
	// The following seq 5001 matches the probe and re-initializes the
	// source, putting it back on probation.
	assert.False(t, stats.ProcessPacket(packet(5001, 0, t0.Add(4*time.Millisecond))))
	// seq 5002 is the second consecutive packet and validates the source.
	require.True(t, stats.ProcessPacket(packet(5002, 0, t0.Add(5*time.Millisecond))))
	require.True(t, stats.ProcessPacket(packet(5003, 0, t0.Add(6*time.Millisecond))))

	// After the restart the window is seq 5001..5003, nothing lost.
	report := stats.BuildReportAt(t0.Add(7 * time.Millisecond))
	assert.Equal(t, uint32(5003), report.LastSequenceNumber)
	assert.Equal(t, uint32(0), report.TotalLost)
	assert.Equal(t, uint8(0), report.FractionLost)
}

// TestReceptionStatsNoSenderReport checks reports before any SR is received.
func TestReceptionStatsNoSenderReport(t *testing.T) {
	var now time.Time
	clock := func() time.Time { return now }
	stats := NewReceptionStats(testSSRC, 1000, clock)

	now = time.Unix(1700000000, 0)
	_ = stats.ProcessPacket(PacketMetadata{SequenceNumber: 500})
	require.True(t, stats.ProcessPacket(PacketMetadata{SequenceNumber: 501}))

	report := stats.BuildReport()
	assert.Equal(t, uint32(testSSRC), report.SSRC)
	assert.Equal(t, uint32(501), report.LastSequenceNumber)
	assert.Equal(t, uint32(0), report.LastSenderReport)
	assert.Equal(t, uint32(0), report.Delay)
}

// TestReceptionStatsJitterClockRate verifies arrival time is converted to the
// source's RTP clock units for jitter.
func TestReceptionStatsJitterClockRate(t *testing.T) {
	t0 := time.Unix(1700000000, 0)
	stats := NewReceptionStats(testSSRC, 90000, nil)

	// 20ms = 1800 RTP units at 90kHz.
	_ = stats.ProcessPacket(packet(0, 90000, t0))
	require.True(t, stats.ProcessPacket(packet(1, 91800, t0.Add(20*time.Millisecond))))
	require.True(t, stats.ProcessPacket(packet(2, 93600, t0.Add(40*time.Millisecond))))
	// seq 3 arrives 20ms late: transit deviation 1800 units.
	require.True(t, stats.ProcessPacket(packet(3, 95400, t0.Add(80*time.Millisecond))))

	report := stats.BuildReportAt(t0.Add(80 * time.Millisecond))
	assert.Equal(t, uint32(112), report.Jitter) // 1800/16 = 112
}

// TestReceptionStatsLSR extracts the middle 32 bits of the SR NTP timestamp.
func TestReceptionStatsLSR(t *testing.T) {
	t0 := time.Unix(1700000000, 0)
	stats := NewReceptionStats(testSSRC, 1000, nil)

	stats.SenderReportReceived(0xAABBCCDD11223344, t0)

	report := stats.BuildReportAt(t0.Add(time.Second))
	assert.Equal(t, uint32(0xCCDD1122), report.LastSenderReport)
	assert.Equal(t, uint32(65536), report.Delay) // 1 s in 1/65536 s units
}

// TestReceptionStatsUnvalidated reports only the initial sequence number
// before the source is validated, with no loss.
func TestReceptionStatsUnvalidated(t *testing.T) {
	t0 := time.Unix(1700000000, 0)
	stats := NewReceptionStats(testSSRC, 1000, nil)

	_ = stats.ProcessPacket(packet(42, 0, t0))

	report := stats.BuildReportAt(t0.Add(time.Millisecond))
	assert.Equal(t, ReceptionReport{SSRC: testSSRC, LastSequenceNumber: 42}, report)
}

// TestReceptionStatsConcurrent exercises concurrent reception and reporting to
// detect data races; run with -race.
func TestReceptionStatsConcurrent(t *testing.T) {
	t0 := time.Unix(1700000000, 0)
	stats := NewReceptionStats(testSSRC, 1000, nil)

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := uint16(0); i < 1000; i++ {
			stats.ProcessPacket(packet(i, uint32(i), t0.Add(time.Duration(i)*time.Millisecond)))
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			_ = stats.BuildReportAt(t0.Add(time.Duration(i) * time.Millisecond))
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			stats.SenderReportReceived(uint64(i)<<32, t0.Add(time.Duration(i)*time.Millisecond))
		}
	}()

	wg.Wait()

	final := stats.BuildReportAt(t0.Add(time.Second))
	assert.Equal(t, uint32(999), final.LastSequenceNumber)
	assert.Equal(t, uint32(0), final.TotalLost)
}
