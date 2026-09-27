package socks5

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/things-go/go-socks5/statute"
)

const fragTestDst = "10.0.0.1:53"

func mkFragDatagram(t *testing.T, frag byte, dst, data string) statute.Datagram {
	t.Helper()
	dg, err := statute.NewDatagram(dst, []byte(data))
	require.NoError(t, err)
	dg.Frag = frag
	return dg
}

func TestFragmentReassembler_InOrder(t *testing.T) {
	r := newFragmentReassembler(time.Minute, 0, 0)
	defer r.Close()

	_, ok := r.push("clientA", mkFragDatagram(t, 1, fragTestDst, "hello "))
	require.False(t, ok)
	_, ok = r.push("clientA", mkFragDatagram(t, 2, fragTestDst, "frag"))
	require.False(t, ok)
	dg, ok := r.push("clientA", mkFragDatagram(t, 0x83, fragTestDst, "mented"))
	require.True(t, ok)
	require.Equal(t, byte(0), dg.Frag)
	require.Equal(t, "hello fragmented", string(dg.Data))
	require.Equal(t, fragTestDst, dg.DstAddr.String())
	require.Equal(t, 0, r.groupCount())
}

func TestFragmentReassembler_SequenceRegression(t *testing.T) {
	r := newFragmentReassembler(time.Minute, 0, 0)
	defer r.Close()

	_, ok := r.push("clientA", mkFragDatagram(t, 1, fragTestDst, "AA"))
	require.False(t, ok)
	_, ok = r.push("clientA", mkFragDatagram(t, 2, fragTestDst, "BB"))
	require.False(t, ok)
	// sequence regression: the queue must be reinitialized and the
	// previously cached fragments abandoned (RFC 1928)
	_, ok = r.push("clientA", mkFragDatagram(t, 1, fragTestDst, "CC"))
	require.False(t, ok)
	dg, ok := r.push("clientA", mkFragDatagram(t, 0x82, fragTestDst, "DD"))
	require.True(t, ok)
	// stale fragments must not leak into the reassembled datagram
	require.Equal(t, "CCDD", string(dg.Data))
	require.Equal(t, 0, r.groupCount())
}

func TestFragmentReassembler_MissingLastFragmentExpires(t *testing.T) {
	r := newFragmentReassembler(50*time.Millisecond, 0, 0)
	defer r.Close()

	_, ok := r.push("clientA", mkFragDatagram(t, 1, fragTestDst, "AA"))
	require.False(t, ok)
	_, ok = r.push("clientA", mkFragDatagram(t, 2, fragTestDst, "BB"))
	require.False(t, ok)
	require.Equal(t, 1, r.groupCount())

	// the last fragment never arrives: the queue must be abandoned
	// once the reassembly timer expires
	require.Eventually(t, func() bool { return r.groupCount() == 0 },
		2*time.Second, 10*time.Millisecond)

	// a late last fragment must not resurrect the abandoned data
	_, ok = r.push("clientA", mkFragDatagram(t, 0x82, fragTestDst, "BB"))
	require.False(t, ok)
}

func TestFragmentReassembler_Close(t *testing.T) {
	r := newFragmentReassembler(time.Minute, 0, 0)

	_, ok := r.push("clientA", mkFragDatagram(t, 1, fragTestDst, "AA"))
	require.False(t, ok)
	require.Equal(t, 1, r.groupCount())

	// association closed: all reassembly state must be released
	r.Close()
	require.Equal(t, 0, r.groupCount())

	// nothing may be reassembled after close
	_, ok = r.push("clientA", mkFragDatagram(t, 0x81, fragTestDst, "BB"))
	require.False(t, ok)
}

func TestFragmentReassembler_IsolatedPerClientAndTarget(t *testing.T) {
	r := newFragmentReassembler(time.Minute, 0, 0)
	defer r.Close()

	// two clients sending fragments for the same target
	_, ok := r.push("clientA", mkFragDatagram(t, 1, fragTestDst, "AA"))
	require.False(t, ok)
	_, ok = r.push("clientB", mkFragDatagram(t, 1, fragTestDst, "XX"))
	require.False(t, ok)
	// same client, another target
	_, ok = r.push("clientA", mkFragDatagram(t, 1, "10.0.0.2:53", "PP"))
	require.False(t, ok)
	require.Equal(t, 3, r.groupCount())

	// clientB completes its own sequence; clientA's data must not mix in
	dg, ok := r.push("clientB", mkFragDatagram(t, 0x82, fragTestDst, "YY"))
	require.True(t, ok)
	require.Equal(t, "XXYY", string(dg.Data))

	// clientA's sequence for the same target is still pending and
	// completes only with its own fragments
	dg, ok = r.push("clientA", mkFragDatagram(t, 0x82, fragTestDst, "BB"))
	require.True(t, ok)
	require.Equal(t, "AABB", string(dg.Data))

	dg, ok = r.push("clientA", mkFragDatagram(t, 0x82, "10.0.0.2:53", "QQ"))
	require.True(t, ok)
	require.Equal(t, "PPQQ", string(dg.Data))

	require.Equal(t, 0, r.groupCount())
}

func TestFragmentReassembler_GroupSizeLimit(t *testing.T) {
	r := newFragmentReassembler(time.Minute, 4, 0)
	defer r.Close()

	_, ok := r.push("clientA", mkFragDatagram(t, 1, fragTestDst, "AAAA"))
	require.False(t, ok)
	// exceeds the per-group limit: the whole sequence must be abandoned
	_, ok = r.push("clientA", mkFragDatagram(t, 2, fragTestDst, "B"))
	require.False(t, ok)
	require.Equal(t, 0, r.groupCount())

	// the trailing fragment alone must not complete anything
	_, ok = r.push("clientA", mkFragDatagram(t, 0x82, fragTestDst, "B"))
	require.False(t, ok)
}

func TestFragmentReassembler_TotalSizeLimit(t *testing.T) {
	r := newFragmentReassembler(time.Minute, 0, 6)
	defer r.Close()

	_, ok := r.push("clientA", mkFragDatagram(t, 1, fragTestDst, "AAAA"))
	require.False(t, ok)
	// would push the association-wide cache over the limit: rejected
	_, ok = r.push("clientB", mkFragDatagram(t, 1, fragTestDst, "BBBB"))
	require.False(t, ok)
	require.Equal(t, 1, r.groupCount())

	// clientA's sequence still fits and completes normally
	dg, ok := r.push("clientA", mkFragDatagram(t, 0x82, fragTestDst, "CC"))
	require.True(t, ok)
	require.Equal(t, "AAAACC", string(dg.Data))
}

func TestFragmentReassembler_SingleFragmentMessage(t *testing.T) {
	r := newFragmentReassembler(time.Minute, 0, 0)
	defer r.Close()

	// FRAG = X'80': a complete message in a single fragment
	dg, ok := r.push("clientA", mkFragDatagram(t, 0x80, fragTestDst, "whole"))
	require.True(t, ok)
	require.Equal(t, "whole", string(dg.Data))
	require.Equal(t, 0, r.groupCount())
}

// TestRequest_Associate_Fragments exercises the full UDP ASSOCIATE path:
// fragmented datagrams from the client are reassembled by the proxy before
// being forwarded to the target, and fragments from another client socket
// for the same target are neither mixed in nor forwarded.
func TestRequest_Associate_Fragments(t *testing.T) {
	// target UDP server recording what it receives
	targetConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	require.NoError(t, err)
	defer targetConn.Close() // nolint: errcheck
	received := make(chan string, 8)
	go func() {
		buf := make([]byte, 2048)
		for {
			n, err := targetConn.Read(buf)
			if err != nil {
				return
			}
			received <- string(buf[:n])
		}
	}()
	targetAddr := targetConn.LocalAddr().(*net.UDPAddr)

	// socks5 proxy server
	l, err := net.Listen("tcp", "127.0.0.1:0") // nolint: noctx
	require.NoError(t, err)
	defer l.Close() // nolint: errcheck
	srv := NewServer()
	go srv.Serve(l) // nolint: errcheck

	// TCP control connection: negotiate no-auth, then UDP ASSOCIATE
	ctrl, err := net.Dial("tcp", l.Addr().String()) // nolint: noctx
	require.NoError(t, err)
	defer ctrl.Close() // nolint: errcheck
	_, err = ctrl.Write([]byte{statute.VersionSocks5, 1, statute.MethodNoAuth})
	require.NoError(t, err)
	resp := make([]byte, 2)
	_, err = io.ReadFull(ctrl, resp)
	require.NoError(t, err)
	require.Equal(t, []byte{statute.VersionSocks5, statute.MethodNoAuth}, resp)
	_, err = ctrl.Write([]byte{
		statute.VersionSocks5, statute.CommandAssociate, 0,
		statute.ATYPIPv4, 0, 0, 0, 0, 0, 0,
	})
	require.NoError(t, err)
	reply := make([]byte, 10)
	_, err = io.ReadFull(ctrl, reply)
	require.NoError(t, err)
	require.Equal(t, byte(statute.RepSuccess), reply[1])
	relayAddr := &net.UDPAddr{
		IP:   net.IPv4(reply[4], reply[5], reply[6], reply[7]),
		Port: int(binary.BigEndian.Uint16(reply[8:10])),
	}

	newClient := func(t *testing.T) *net.UDPConn {
		t.Helper()
		c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
		require.NoError(t, err)
		t.Cleanup(func() { c.Close() }) // nolint: errcheck
		return c
	}
	sendFrag := func(t *testing.T, c *net.UDPConn, frag byte, data string) {
		t.Helper()
		dg, err := statute.NewDatagram(targetAddr.String(), []byte(data))
		require.NoError(t, err)
		dg.Frag = frag
		_, err = c.WriteToUDP(dg.Bytes(), relayAddr)
		require.NoError(t, err)
	}

	clientA := newClient(t)
	clientB := newClient(t)

	// interleave fragments of two clients aiming at the same target
	sendFrag(t, clientA, 1, "hello ")
	sendFrag(t, clientB, 1, "garbage-")
	sendFrag(t, clientA, 2, "frag")
	sendFrag(t, clientB, 0x82, "data")
	sendFrag(t, clientA, 0x83, "mented")

	// each client's sequence is reassembled independently and forwarded as
	// one datagram; fragments of the two clients must never be mixed
	got := make(map[string]bool, 2)
	for i := 0; i < 2; i++ {
		select {
		case msg := <-received:
			got[msg] = true
		case <-time.After(2 * time.Second):
			t.Fatal("target did not receive the reassembled datagrams")
		}
	}
	require.Equal(t, map[string]bool{"hello fragmented": true, "garbage-data": true}, got)

	// an incomplete sequence (missing last fragment) is never forwarded
	sendFrag(t, clientA, 1, "never")
	sendFrag(t, clientA, 2, " delivered")
	select {
	case got := <-received:
		t.Fatalf("target received incomplete fragment sequence: %q", got)
	case <-time.After(300 * time.Millisecond):
	}

	// closing the control connection tears the association down;
	// further datagrams must not be forwarded anymore
	require.NoError(t, ctrl.Close())
	time.Sleep(200 * time.Millisecond)
	sendFrag(t, clientA, 1, "after")
	sendFrag(t, clientA, 0x82, " close")
	select {
	case got := <-received:
		t.Fatalf("target received datagram after association closed: %q", got)
	case <-time.After(300 * time.Millisecond):
	}
}
