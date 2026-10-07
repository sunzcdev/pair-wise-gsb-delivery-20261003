// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package mux

import (
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/pion/stun/v4"
	"github.com/stretchr/testify/require"
)

func stunPacket(t *testing.T) []byte {
	t.Helper()

	return stun.MustBuild(stun.TransactionID, stun.BindingRequest).Raw
}

func dtlsPacket() []byte {
	// Content type 22 (handshake), version 1.2, epoch 0, sequence 1,
	// three payload bytes.
	return []byte{22, 254, 253, 0, 0, 0, 0, 0, 0, 0, 1, 0, 3, 0xAA, 0xBB, 0xCC}
}

func turnPacket() []byte {
	// Channel 0x4001, four payload bytes.
	return []byte{0x40, 0x01, 0x00, 0x04, 'd', 'a', 't', 'a'}
}

func rtpPacket(seq byte) []byte {
	// Version 2, payload type 96, 12-byte header, four payload bytes.
	return []byte{0x80, 96, 0, 1, 0, 0, 0, 0, 0, 0, 0, 1, seq, 2, 3, 4}
}

func rtcpPacket() []byte {
	// Version 2, packet type 200 (SR), length 1 (eight bytes total).
	return []byte{0x80, 200, 0, 1, 0, 0, 0, 0}
}

func TestClassify(t *testing.T) {
	validSTUN := stunPacket(t)

	badCookie := make([]byte, len(validSTUN))
	copy(badCookie, validSTUN)
	badCookie[4] ^= 0xFF

	badSTUNLen := make([]byte, len(validSTUN))
	copy(badSTUNLen, validSTUN)
	badSTUNLen[3] = 4 // Declares an attribute that is not there.

	cases := []struct {
		name string
		data []byte
		want Protocol
	}{
		{"empty", nil, ProtocolUnknown},
		{"stun", validSTUN, ProtocolSTUN},
		{"stun short", validSTUN[:19], ProtocolUnknown},
		{"stun bad cookie", badCookie, ProtocolUnknown},
		{"stun bad length", badSTUNLen, ProtocolUnknown},
		{"dtls", dtlsPacket(), ProtocolDTLS},
		{"dtls short", dtlsPacket()[:12], ProtocolUnknown},
		{"dtls bad version", []byte{22, 1, 2, 0, 0, 0, 0, 0, 0, 0, 1, 0, 3, 1, 2, 3}, ProtocolUnknown},
		{"dtls bad content type", []byte{30, 254, 253, 0, 0, 0, 0, 0, 0, 0, 1, 0, 3, 1, 2, 3}, ProtocolUnknown},
		{"dtls bad length", []byte{22, 254, 253, 0, 0, 0, 0, 0, 0, 0, 1, 0, 9, 1, 2, 3}, ProtocolUnknown},
		{"turn", turnPacket(), ProtocolTURN},
		{"turn short", turnPacket()[:3], ProtocolUnknown},
		{"turn bad length", []byte{0x40, 0x01, 0x00, 0x08, 'd', 'a', 't', 'a'}, ProtocolUnknown},
		{"rtp", rtpPacket(1), ProtocolRTP},
		{"rtp short", rtpPacket(1)[:11], ProtocolUnknown},
		{"rtp csrc overflow", []byte{0x8F, 96, 0, 1, 0, 0, 0, 0, 0, 0, 0, 1}, ProtocolUnknown},
		{"rtcp", rtcpPacket(), ProtocolRTCP},
		{"rtcp short", rtcpPacket()[:3], ProtocolUnknown},
		{"rtcp bad length", []byte{0x80, 200, 0, 5, 0, 0, 0, 0}, ProtocolUnknown},
		{"zrtp range", []byte{16, 0, 0, 0}, ProtocolUnknown},
		{"out of range", []byte{192, 0, 0, 0}, ProtocolUnknown},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, classify(tc.data))
		})
	}
}

func newTestMux(t *testing.T, cfg Config) (*Mux, *net.UDPConn) {
	t.Helper()

	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)

	cfg.Conn = udp
	m, err := NewMux(cfg)
	require.NoError(t, err)

	return m, udp
}

func newSender(t *testing.T) *net.UDPConn {
	t.Helper()

	sender, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sender.Close()) })

	return sender
}

func send(t *testing.T, sender *net.UDPConn, addr net.Addr, data []byte) {
	t.Helper()

	_, err := sender.WriteTo(data, addr)
	require.NoError(t, err)
}

func readPacket(t *testing.T, conn *Conn) ([]byte, net.Addr) {
	t.Helper()

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	buf := make([]byte, 2048)
	n, addr, err := conn.ReadFrom(buf)
	require.NoError(t, err)

	return buf[:n], addr
}

func TestMuxInterleaved(t *testing.T) {
	m, _ := newTestMux(t, Config{OwnConn: true})
	t.Cleanup(func() { require.NoError(t, m.Close()) })
	sender := newSender(t)

	const rounds = 3
	payloads := map[Protocol][]byte{
		ProtocolSTUN: stunPacket(t),
		ProtocolDTLS: dtlsPacket(),
		ProtocolTURN: turnPacket(),
		ProtocolRTP:  rtpPacket(1),
		ProtocolRTCP: rtcpPacket(),
	}
	order := []Protocol{ProtocolRTP, ProtocolSTUN, ProtocolRTCP, ProtocolTURN, ProtocolDTLS}

	for round := 0; round < rounds; round++ {
		for _, proto := range order {
			send(t, sender, m.LocalAddr(), payloads[proto])
		}
	}

	for _, proto := range order {
		conn := m.Conn(proto)
		require.NotNil(t, conn)
		require.Equal(t, proto, conn.Protocol())
		for round := 0; round < rounds; round++ {
			data, addr := readPacket(t, conn)
			// Datagram boundaries and source address are preserved.
			require.Equal(t, payloads[proto], data)
			require.Equal(t, sender.LocalAddr().String(), addr.String())
		}
		require.Zero(t, m.Malformed())
	}
}

func TestMuxMalformed(t *testing.T) {
	m, _ := newTestMux(t, Config{OwnConn: true})
	t.Cleanup(func() { require.NoError(t, m.Close()) })
	sender := newSender(t)

	malformed := [][]byte{
		{0, 1, 2},                            // Too short for STUN.
		{22, 254, 253, 0, 0},                 // Truncated DTLS record.
		{0x40, 0x01, 0x00, 0x08, 1, 2, 3, 4}, // ChannelData length mismatch.
		{0x80, 96, 0, 1},                     // Truncated RTP header.
		{0x80, 200, 0, 5, 0, 0, 0, 0},        // RTCP length mismatch.
		{0xC0, 0xFF},                         // Outside RFC 7983 ranges.
	}
	for _, data := range malformed {
		send(t, sender, m.LocalAddr(), data)
	}

	require.Eventually(t, func() bool {
		return m.Malformed() == uint64(len(malformed)) //nolint:gosec
	}, 5*time.Second, 5*time.Millisecond)

	// Malformed datagrams do not disturb valid traffic.
	want := stunPacket(t)
	send(t, sender, m.LocalAddr(), want)
	data, _ := readPacket(t, m.Conn(ProtocolSTUN))
	require.Equal(t, want, data)
}

func TestMuxQueueOverflowDropNewest(t *testing.T) {
	m, _ := newTestMux(t, Config{OwnConn: true, QueueSize: 4, Overflow: DropNewest})
	t.Cleanup(func() { require.NoError(t, m.Close()) })
	sender := newSender(t)

	const total = 10
	for seq := 0; seq < total; seq++ {
		send(t, sender, m.LocalAddr(), rtpPacket(byte(seq))) //nolint:gosec
	}

	rtpConn := m.Conn(ProtocolRTP)
	require.Eventually(t, func() bool {
		stats := rtpConn.Stats()

		return stats.Received+stats.Dropped == total
	}, 5*time.Second, 5*time.Millisecond)

	stats := rtpConn.Stats()
	require.Equal(t, uint64(4), stats.Received)
	require.Equal(t, uint64(6), stats.Dropped)

	// The first four datagrams are kept.
	for seq := 0; seq < 4; seq++ {
		data, _ := readPacket(t, rtpConn)
		require.Equal(t, rtpPacket(byte(seq)), data) //nolint:gosec
	}

	// A stalled media consumer must not block STUN.
	want := stunPacket(t)
	send(t, sender, m.LocalAddr(), want)
	data, _ := readPacket(t, m.Conn(ProtocolSTUN))
	require.Equal(t, want, data)
}

func TestMuxQueueOverflowDropOldest(t *testing.T) {
	m, _ := newTestMux(t, Config{OwnConn: true, QueueSize: 4, Overflow: DropOldest})
	t.Cleanup(func() { require.NoError(t, m.Close()) })
	sender := newSender(t)

	const total = 10
	for seq := 0; seq < total; seq++ {
		send(t, sender, m.LocalAddr(), rtpPacket(byte(seq))) //nolint:gosec
	}

	rtpConn := m.Conn(ProtocolRTP)
	require.Eventually(t, func() bool {
		return rtpConn.Stats().Dropped == 6
	}, 5*time.Second, 5*time.Millisecond)

	// The last four datagrams are kept.
	for seq := 6; seq < total; seq++ {
		data, _ := readPacket(t, rtpConn)
		require.Equal(t, rtpPacket(byte(seq)), data) //nolint:gosec
	}
}

func TestMuxCloseSubConn(t *testing.T) {
	m, _ := newTestMux(t, Config{OwnConn: true})
	t.Cleanup(func() { require.NoError(t, m.Close()) })
	sender := newSender(t)

	rtpConn := m.Conn(ProtocolRTP)
	require.NoError(t, rtpConn.Close())

	_, _, err := rtpConn.ReadFrom(make([]byte, 64))
	require.ErrorIs(t, err, net.ErrClosed)
	_, err = rtpConn.WriteTo(rtpPacket(1), m.LocalAddr())
	require.ErrorIs(t, err, net.ErrClosed)

	// Datagrams for the closed protocol are dropped and counted.
	send(t, sender, m.LocalAddr(), rtpPacket(1))
	require.Eventually(t, func() bool {
		return rtpConn.Stats().Dropped == 1
	}, 5*time.Second, 5*time.Millisecond)

	// The remaining protocols still work in both directions.
	payloads := map[Protocol][]byte{
		ProtocolSTUN: stunPacket(t),
		ProtocolDTLS: dtlsPacket(),
		ProtocolTURN: turnPacket(),
		ProtocolRTCP: rtcpPacket(),
	}
	for proto, want := range payloads {
		send(t, sender, m.LocalAddr(), want)
		data, _ := readPacket(t, m.Conn(proto))
		require.Equal(t, want, data)

		_, err = m.Conn(proto).WriteTo(want, sender.LocalAddr())
		require.NoError(t, err)
	}

	require.NoError(t, sender.SetReadDeadline(time.Now().Add(5*time.Second)))
	buf := make([]byte, 2048)
	for range payloads {
		_, _, err = sender.ReadFrom(buf)
		require.NoError(t, err)
	}
}

func TestMuxOwnership(t *testing.T) {
	t.Run("owned socket is closed", func(t *testing.T) {
		m, udp := newTestMux(t, Config{OwnConn: true})
		require.NoError(t, m.Close())

		_, err := udp.WriteTo([]byte{1}, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1})
		require.Error(t, err)
	})

	t.Run("borrowed socket stays open", func(t *testing.T) {
		m, udp := newTestMux(t, Config{OwnConn: false})
		t.Cleanup(func() { require.NoError(t, udp.Close()) })
		require.NoError(t, m.Close())

		echo, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, echo.Close()) })

		_, err = udp.WriteTo([]byte{1, 2, 3}, echo.LocalAddr())
		require.NoError(t, err)

		require.NoError(t, echo.SetReadDeadline(time.Now().Add(5*time.Second)))
		buf := make([]byte, 16)
		n, _, err := echo.ReadFrom(buf)
		require.NoError(t, err)
		require.Equal(t, []byte{1, 2, 3}, buf[:n])
	})
}

func TestMuxReadDeadline(t *testing.T) {
	m, _ := newTestMux(t, Config{OwnConn: true})
	t.Cleanup(func() { require.NoError(t, m.Close()) })
	sender := newSender(t)

	conn := m.Conn(ProtocolDTLS)

	// A deadline in the past fails immediately.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(-time.Second)))
	_, _, err := conn.ReadFrom(make([]byte, 64))
	require.Error(t, err)
	require.ErrorIs(t, err, os.ErrDeadlineExceeded)

	// A future deadline expires with a timeout net.Error.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(100*time.Millisecond)))
	start := time.Now()
	_, _, err = conn.ReadFrom(make([]byte, 64))
	require.Error(t, err)
	var netErr net.Error
	require.True(t, errors.As(err, &netErr))
	require.True(t, netErr.Timeout())
	require.GreaterOrEqual(t, time.Since(start), 100*time.Millisecond)

	// Clearing the deadline restores blocking reads.
	require.NoError(t, conn.SetReadDeadline(time.Time{}))
	want := dtlsPacket()
	send(t, sender, m.LocalAddr(), want)
	data, _ := readPacket(t, conn)
	require.Equal(t, want, data)
}

func TestMuxCloseIdempotent(t *testing.T) {
	m, _ := newTestMux(t, Config{OwnConn: true})
	require.NoError(t, m.Close())
	require.NoError(t, m.Close())

	_, _, err := m.Conn(ProtocolSTUN).ReadFrom(make([]byte, 64))
	require.ErrorIs(t, err, net.ErrClosed)
}
