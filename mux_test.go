package stun

import (
	"encoding/binary"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testMuxPackets(t *testing.T) (map[Protocol][]byte, [][]byte) {
	t.Helper()

	stunPacket := make([]byte, 20)
	binary.BigEndian.PutUint16(stunPacket[0:2], BindingRequest.Value())
	binary.BigEndian.PutUint32(stunPacket[4:8], magicCookie)

	turnPacket := []byte{0x40, 0x01, 0x00, 0x04, 0x54, 0x55, 0x52, 0x4e}

	dtlsPacket := make([]byte, 14)
	dtlsPacket[0] = 23
	dtlsPacket[1] = 0xfe
	dtlsPacket[2] = 0xff
	binary.BigEndian.PutUint16(dtlsPacket[11:13], 1)
	dtlsPacket[13] = 1

	rtpPacket := []byte{0x80, 0x60, 0x12, 0x34, 0, 0, 0, 1, 0, 0, 0, 1, 0xff}
	rtcpPacket := []byte{0x80, 200, 0, 0x1, 1, 2, 3, 4}

	valid := map[Protocol][]byte{
		ProtocolSTUN: stunPacket,
		ProtocolTURN: turnPacket,
		ProtocolDTLS: dtlsPacket,
		ProtocolRTP:  rtpPacket,
		ProtocolRTCP: rtcpPacket,
	}

	shortSTUN := make([]byte, 19)
	binary.BigEndian.PutUint32(shortSTUN[4:8], magicCookie)

	shortTURN := []byte{0x40, 0x01, 0x00, 0x04, 1}
	badTURNChannel := []byte{0x3f, 0xff, 0, 0}
	shortDTLS := make([]byte, 12)
	shortDTLS[0] = 23
	shortDTLS[1] = 0xfe
	shortDTLS[2] = 0xff
	badRTP := []byte{0x80, 0x60, 0, 0}

	malformed := [][]byte{
		[]byte("not a multiplexed packet"),
		shortSTUN,
		shortTURN,
		badTURNChannel,
		shortDTLS,
		badRTP,
	}

	return valid, malformed
}

func waitForCounter(t *testing.T, get func() uint64, want uint64) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := get(); got >= want {
			require.Equal(t, want, got)

			return
		}

		time.Sleep(time.Millisecond)
	}

	require.Equal(t, want, get())
}

func readMuxPacket(t *testing.T, conn MuxConn, want []byte) (net.Addr, []byte) {
	t.Helper()

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))

	buffer := make([]byte, len(want)+1)
	n, addr, err := conn.ReadFrom(buffer)
	require.NoError(t, err)
	require.Equal(t, len(want), n)
	assert.Equal(t, want, buffer[:n])

	return addr, buffer[:n]
}

func acceptMuxConn(t *testing.T, mux *Mux, protocol Protocol) MuxConn {
	t.Helper()

	conn, err := mux.Accept(protocol)
	require.NoError(t, err)

	return conn
}

func TestClassifyMultiplexedProtocols(t *testing.T) {
	packets, malformed := testMuxPackets(t)

	for protocol, packet := range packets {
		got, ok := Classify(packet)
		assert.Equal(t, protocol, got)
		assert.True(t, ok)
	}

	for _, packet := range malformed {
		got, ok := Classify(packet)
		assert.Equal(t, ProtocolUnknown, got)
		assert.False(t, ok)
	}

	coalescedDTLS := append(append([]byte{}, packets[ProtocolDTLS]...), packets[ProtocolDTLS]...)
	got, ok := Classify(coalescedDTLS)
	assert.Equal(t, ProtocolDTLS, got)
	assert.True(t, ok)

	turnPacketPadded := []byte{0x40, 0x01, 0x00, 0x01, 0x54, 0, 0, 0}
	got, ok = Classify(turnPacketPadded)
	assert.Equal(t, ProtocolTURN, got)
	assert.True(t, ok)
}

func TestClassifyRejectsMalformedProtocolHeaders(t *testing.T) {
	packets, _ := testMuxPackets(t)

	badSTUNLength := append([]byte{}, packets[ProtocolSTUN]...)
	binary.BigEndian.PutUint16(badSTUNLength[2:4], 4)

	badSTUNCookie := append([]byte{}, packets[ProtocolSTUN]...)
	binary.BigEndian.PutUint32(badSTUNCookie[4:8], 0)

	badTURNLength := append([]byte{}, packets[ProtocolTURN]...)
	binary.BigEndian.PutUint16(badTURNLength[2:4], 8)

	badTURNPadding := append([]byte{}, packets[ProtocolTURN]...)
	badTURNPadding = append(badTURNPadding, 0, 0, 0, 0)

	badDTLSRecord := append([]byte{}, packets[ProtocolDTLS]...)
	binary.BigEndian.PutUint16(badDTLSRecord[11:13], 100)

	badRTPCCRC := []byte{0x81, 0x60, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	rtpWithExtension := []byte{
		0x90, 0x60, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0xbe, 0xde, 0, 1, 0, 0, 0, 0, 0xff,
	}
	badRTPExtension := append([]byte{}, rtpWithExtension...)
	binary.BigEndian.PutUint16(badRTPExtension[14:16], 2)

	rtcpWithMiddlePadding := []byte{
		0xa0, 200, 0, 1, 1, 2, 3, 4,
		0x80, 201, 0, 0,
	}

	for name, packet := range map[string][]byte{
		"stun length":         badSTUNLength,
		"stun cookie":         badSTUNCookie,
		"turn length":         badTURNLength,
		"turn padding":        badTURNPadding,
		"dtls record":         badDTLSRecord,
		"rtp csrc":            badRTPCCRC,
		"rtp extension":       badRTPExtension,
		"rtcp middle padding": rtcpWithMiddlePadding,
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := Classify(packet)
			assert.Equal(t, ProtocolUnknown, got)
			assert.False(t, ok)
		})
	}

	got, ok := Classify(rtpWithExtension)
	assert.Equal(t, ProtocolRTP, got)
	assert.True(t, ok)
}

func TestMuxInterleavesProtocolsAndPreservesPacket(t *testing.T) {
	serverConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)

	mux, err := NewMux(serverConn, OwnsSocket)
	require.NoError(t, err)
	defer func() { assert.NoError(t, mux.Close()) }()

	clientConn, err := net.DialUDP("udp4", nil, serverConn.LocalAddr().(*net.UDPAddr))
	require.NoError(t, err)
	defer func() { assert.NoError(t, clientConn.Close()) }()

	packets, malformedPackets := testMuxPackets(t)
	interleaved := [][]byte{
		packets[ProtocolSTUN],
		malformedPackets[0],
		packets[ProtocolDTLS],
		malformedPackets[1],
		packets[ProtocolRTP],
		packets[ProtocolRTCP],
		malformedPackets[2],
		packets[ProtocolTURN],
		malformedPackets[3],
		malformedPackets[4],
		malformedPackets[5],
	}

	for _, packet := range interleaved {
		_, err = clientConn.Write(packet)
		require.NoError(t, err)
	}

	waitForCounter(t, func() uint64 { return mux.Stats().MalformedPackets }, uint64(len(malformedPackets)))

	for protocol, packet := range packets {
		conn := acceptMuxConn(t, mux, protocol)
		require.NotNil(t, conn)
		assert.Equal(t, protocol, conn.Protocol())
		assert.Equal(t, clientConn.LocalAddr().String(), conn.RemoteAddr().String())

		addr, got := readMuxPacket(t, conn, packet)

		_, err = conn.WriteTo(append([]byte("reply-"), got...), addr)
		require.NoError(t, err)

		reply := make([]byte, len(packet)+len("reply-"))
		n, err := clientConn.Read(reply)
		require.NoError(t, err)
		assert.Equal(t, append([]byte("reply-"), packet...), reply[:n])
	}

	stats := mux.Stats()
	assert.Equal(t, uint64(len(interleaved)), stats.ReceivedPackets)
	assert.Equal(t, uint64(len(malformedPackets)), stats.MalformedPackets)
	assert.Len(t, stats.Queues, 5)

	for _, queueStats := range stats.Queues {
		assert.Equal(t, uint64(1), queueStats.Received)
		assert.Equal(t, clientConn.LocalAddr().String(), queueStats.RemoteAddr)
		assert.Equal(t, uint64(0), queueStats.DroppedQueueFull)
		assert.Equal(t, uint64(0), queueStats.DroppedConsumerClosed)
	}
}

func TestMuxFullQueueDoesNotBlockSTUNAndUsesPolicy(t *testing.T) {
	for _, policy := range []OverflowPolicy{OverflowDropNewest, OverflowDropOldest} {
		t.Run(policyName(policy), func(t *testing.T) {
			serverConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			require.NoError(t, err)

			mux, err := NewMux(
				serverConn,
				OwnsSocket,
				WithQueueCapacity(1),
				WithOverflowPolicy(policy),
			)
			require.NoError(t, err)
			defer func() { assert.NoError(t, mux.Close()) }()

			clientConn, err := net.DialUDP("udp4", nil, serverConn.LocalAddr().(*net.UDPAddr))
			require.NoError(t, err)
			defer func() { _ = clientConn.Close() }()

			packets, _ := testMuxPackets(t)
			firstRTP := append([]byte{0x80, 0x60, 0, 1}, packets[ProtocolRTP][4:]...)
			latestRTP := append([]byte{0x80, 0x60, 0, 3}, packets[ProtocolRTP][4:]...)

			for i := 0; i < 3; i++ {
				packet := append([]byte{0x80, 0x60, 0, byte(i + 1)}, packets[ProtocolRTP][4:]...)
				_, err = clientConn.Write(packet)
				require.NoError(t, err)
			}

			_, err = clientConn.Write(packets[ProtocolSTUN])
			require.NoError(t, err)

			rtpConn := acceptMuxConn(t, mux, ProtocolRTP)
			waitForCounter(t, func() uint64 { return rtpConn.Stats().DroppedQueueFull }, 2)

			stunConn := acceptMuxConn(t, mux, ProtocolSTUN)
			stunAddr, stunPacket := readMuxPacket(t, stunConn, packets[ProtocolSTUN])
			assert.Equal(t, clientConn.LocalAddr().String(), stunAddr.String())

			rtpBuffer := make([]byte, len(packets[ProtocolRTP]))
			require.NoError(t, rtpConn.SetReadDeadline(time.Now().Add(2*time.Second)))
			var rtpN int
			var rtpAddr net.Addr
			rtpN, rtpAddr, err = rtpConn.ReadFrom(rtpBuffer)
			require.NoError(t, err)
			rtpPacket := rtpBuffer[:rtpN]
			assert.Equal(t, clientConn.LocalAddr().String(), rtpAddr.String())
			if policy == OverflowDropOldest {
				assert.Equal(t, latestRTP, rtpPacket)
			} else {
				assert.Equal(t, firstRTP, rtpPacket)
			}

			require.NoError(t, rtpConn.SetReadDeadline(time.Now().Add(20*time.Millisecond)))
			_, _, err = rtpConn.ReadFrom(make([]byte, len(packets[ProtocolRTP])))
			assert.True(t, errors.Is(err, os.ErrDeadlineExceeded))

			queueStats := rtpConn.Stats()
			assert.Equal(t, 1, queueStats.Capacity)
			assert.Equal(t, policy, queueStats.OverflowPolicy)
			assert.Equal(t, uint64(3), queueStats.Received)
			assert.Equal(t, uint64(2), queueStats.DroppedQueueFull)
			assert.Equal(t, stunPacket, packets[ProtocolSTUN])
		})
	}
}

func TestMuxCloseOneConsumerKeepsRemainingProtocols(t *testing.T) {
	serverConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)

	mux, err := NewMux(serverConn, OwnsSocket, WithQueueCapacity(1))
	require.NoError(t, err)
	defer func() { assert.NoError(t, mux.Close()) }()

	clientConn, err := net.DialUDP("udp4", nil, serverConn.LocalAddr().(*net.UDPAddr))
	require.NoError(t, err)
	defer func() { _ = clientConn.Close() }()

	packets, _ := testMuxPackets(t)

	_, err = clientConn.Write(packets[ProtocolRTP])
	require.NoError(t, err)

	rtpConn := acceptMuxConn(t, mux, ProtocolRTP)
	waitForCounter(t, func() uint64 { return rtpConn.Stats().Received }, 1)

	require.NoError(t, rtpConn.Close())
	assert.True(t, rtpConn.Closed())

	_, _, err = rtpConn.ReadFrom(make([]byte, 64))
	assert.ErrorIs(t, err, net.ErrClosed)

	_, err = clientConn.Write(packets[ProtocolRTP])
	require.NoError(t, err)

	waitForCounter(t, func() uint64 { return rtpConn.Stats().DroppedConsumerClosed }, 1)

	newRTPConn := acceptMuxConn(t, mux, ProtocolRTP)
	readMuxPacket(t, newRTPConn, packets[ProtocolRTP])
	assert.False(t, newRTPConn.Closed())

	for _, protocol := range []Protocol{ProtocolSTUN, ProtocolDTLS, ProtocolRTCP, ProtocolTURN} {
		_, err = clientConn.Write(packets[protocol])
		require.NoError(t, err)

		conn := acceptMuxConn(t, mux, protocol)
		addr, packet := readMuxPacket(t, conn, packets[protocol])
		_, err = conn.WriteTo(packet, addr)
		require.NoError(t, err)

		reply := make([]byte, len(packet))
		n, err := clientConn.Read(reply)
		require.NoError(t, err)
		assert.Equal(t, packet, reply[:n])
		assert.False(t, conn.Closed())
	}
}

func TestMuxOwnsSocket(t *testing.T) {
	serverConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)

	mux, err := NewMux(serverConn, OwnsSocket)
	require.NoError(t, err)
	require.NoError(t, mux.Close())

	_, _, err = serverConn.ReadFrom(make([]byte, 1))
	assert.ErrorIs(t, err, net.ErrClosed)
}

func TestMuxDoesNotOwnSocket(t *testing.T) {
	serverConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)

	mux, err := NewMux(serverConn, DoesNotOwnSocket)
	require.NoError(t, err)
	require.NoError(t, mux.Close())

	require.NoError(t, serverConn.SetReadDeadline(time.Now().Add(2*time.Second)))

	clientConn, err := net.DialUDP("udp4", nil, serverConn.LocalAddr().(*net.UDPAddr))
	require.NoError(t, err)
	defer func() { _ = clientConn.Close() }()

	_, err = clientConn.Write([]byte("socket survives"))
	require.NoError(t, err)

	buffer := make([]byte, len("socket survives"))
	n, _, err := serverConn.ReadFrom(buffer)
	require.NoError(t, err)
	assert.Equal(t, "socket survives", string(buffer[:n]))

	assert.NoError(t, serverConn.Close())
}

func TestMuxPendingConnectionLimitDoesNotBlockExistingSTUN(t *testing.T) {
	serverConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)

	mux, err := NewMux(serverConn, OwnsSocket, WithPendingConnectionLimit(1))
	require.NoError(t, err)
	defer func() { assert.NoError(t, mux.Close()) }()

	packets, _ := testMuxPackets(t)

	firstConn, err := net.DialUDP("udp4", nil, serverConn.LocalAddr().(*net.UDPAddr))
	require.NoError(t, err)
	defer func() { _ = firstConn.Close() }()

	secondConn, err := net.DialUDP("udp4", nil, serverConn.LocalAddr().(*net.UDPAddr))
	require.NoError(t, err)
	defer func() { _ = secondConn.Close() }()

	thirdConn, err := net.DialUDP("udp4", nil, serverConn.LocalAddr().(*net.UDPAddr))
	require.NoError(t, err)
	defer func() { _ = thirdConn.Close() }()

	_, err = firstConn.Write(packets[ProtocolSTUN])
	require.NoError(t, err)

	stunConn := acceptMuxConn(t, mux, ProtocolSTUN)
	readMuxPacket(t, stunConn, packets[ProtocolSTUN])

	_, err = secondConn.Write(packets[ProtocolSTUN])
	require.NoError(t, err)

	_, err = thirdConn.Write(packets[ProtocolSTUN])
	require.NoError(t, err)

	waitForCounter(t, func() uint64 { return mux.Stats().PendingDropped }, 1)

	_, err = firstConn.Write(packets[ProtocolSTUN])
	require.NoError(t, err)
	readMuxPacket(t, stunConn, packets[ProtocolSTUN])

	assert.Equal(t, 2, mux.Stats().ActiveConns)
	assert.Equal(t, 1, mux.Stats().PendingConns)
}

func TestNewMuxValidatesOptions(t *testing.T) {
	serverConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)

	_, err = NewMux(nil, OwnsSocket)
	assert.Error(t, err)

	_, err = NewMux(serverConn, SocketOwnership(99))
	assert.Error(t, err)

	_, err = NewMux(serverConn, OwnsSocket, WithQueueCapacity(0))
	assert.Error(t, err)

	_, err = NewMux(serverConn, OwnsSocket, WithProtocolQueue(ProtocolUnknown, 1, OverflowDropNewest))
	assert.Error(t, err)

	_, err = NewMux(serverConn, OwnsSocket, WithPendingConnectionLimit(0))
	assert.Error(t, err)

	assert.NoError(t, serverConn.Close())
}

func policyName(policy OverflowPolicy) string {
	if policy == OverflowDropOldest {
		return "oldest"
	}

	return "newest"
}
