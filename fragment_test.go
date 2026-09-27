package socks5

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/things-go/go-socks5/statute"
)

func TestFragmentReassembler_InOrderComplete(t *testing.T) {
	r := newFragmentReassembler()

	_, ok := r.process("client1", "target1", 1, []byte("hel"))
	require.False(t, ok)
	_, ok = r.process("client1", "target1", 2, []byte("lo"))
	require.False(t, ok)
	data, ok := r.process("client1", "target1", fragmentFlagEnd|3, []byte("!"))
	require.True(t, ok)
	require.Equal(t, "hello!", string(data))
	require.Empty(t, r.groups)
	require.Zero(t, r.totalSize)
}

func TestFragmentReassembler_RollbackDropsSequence(t *testing.T) {
	r := newFragmentReassembler()

	_, ok := r.process("client1", "target1", 1, []byte("pi"))
	require.False(t, ok)
	_, ok = r.process("client1", "target1", 2, []byte("ng"))
	require.False(t, ok)
	// sequence number rolls back: the whole sequence must be dropped
	_, ok = r.process("client1", "target1", 1, []byte("xx"))
	require.False(t, ok)
	require.Empty(t, r.groups)
	require.Zero(t, r.totalSize)
	// the stale end fragment must not be accepted as a new sequence
	_, ok = r.process("client1", "target1", fragmentFlagEnd|2, []byte("!!"))
	require.False(t, ok)
	// a fresh sequence still works
	_, ok = r.process("client1", "target1", 1, []byte("pi"))
	require.False(t, ok)
	data, ok := r.process("client1", "target1", fragmentFlagEnd|2, []byte("ng"))
	require.True(t, ok)
	require.Equal(t, "ping", string(data))
}

func TestFragmentReassembler_MissingLastFragment(t *testing.T) {
	r := newFragmentReassembler()

	_, ok := r.process("client1", "target1", 1, []byte("pi"))
	require.False(t, ok)
	_, ok = r.process("client1", "target1", 2, []byte("ng"))
	require.False(t, ok)
	// no terminating fragment: nothing is reassembled
	require.Len(t, r.groups, 1)
}

func TestFragmentReassembler_Timeout(t *testing.T) {
	r := newFragmentReassembler()
	now := time.Now()
	r.now = func() time.Time { return now }

	_, ok := r.process("client1", "target1", 1, []byte("pi"))
	require.False(t, ok)
	// let the reassembly timer expire
	now = now.Add(2 * defaultFragmentTimeout)
	// the stale group is purged; a late fragment is not a sequence start
	_, ok = r.process("client1", "target1", fragmentFlagEnd|2, []byte("ng"))
	require.False(t, ok)
	require.Empty(t, r.groups)
	require.Zero(t, r.totalSize)
}

func TestFragmentReassembler_GroupSizeLimit(t *testing.T) {
	r := newFragmentReassembler()
	r.maxGroupSize = 4

	_, ok := r.process("client1", "target1", 1, []byte("ab"))
	require.False(t, ok)
	// 2 + 3 bytes exceeds the per-group limit: drop the sequence
	_, ok = r.process("client1", "target1", 2, []byte("cde"))
	require.False(t, ok)
	require.Empty(t, r.groups)
	require.Zero(t, r.totalSize)
}

func TestFragmentReassembler_TotalSizeLimit(t *testing.T) {
	r := newFragmentReassembler()
	r.maxTotalSize = 3

	_, ok := r.process("client1", "target1", 1, []byte("ab"))
	require.False(t, ok)
	// 2 + 2 bytes exceeds the total cache limit: drop the new sequence
	_, ok = r.process("client2", "target1", 1, []byte("cd"))
	require.False(t, ok)
	require.Len(t, r.groups, 1)
	require.Equal(t, 2, r.totalSize)
}

func TestFragmentReassembler_InvalidAndDuplicate(t *testing.T) {
	r := newFragmentReassembler()

	// FRAG 0x80 carries sequence number 0, which is invalid
	_, ok := r.process("client1", "target1", fragmentFlagEnd, []byte("x"))
	require.False(t, ok)
	require.Empty(t, r.groups)

	_, ok = r.process("client1", "target1", 1, []byte("a"))
	require.False(t, ok)
	// a duplicate fragment is ignored, the sequence stays alive
	_, ok = r.process("client1", "target1", 1, []byte("a"))
	require.False(t, ok)
	data, ok := r.process("client1", "target1", fragmentFlagEnd|2, []byte("b"))
	require.True(t, ok)
	require.Equal(t, "ab", string(data))
}

func TestFragmentReassembler_BeyondAnnouncedEnd(t *testing.T) {
	r := newFragmentReassembler()

	_, ok := r.process("client1", "target1", 1, []byte("a"))
	require.False(t, ok)
	_, ok = r.process("client1", "target1", fragmentFlagEnd|3, []byte("c"))
	require.False(t, ok)
	// fragment 4 is beyond the announced end of the sequence
	_, ok = r.process("client1", "target1", 4, []byte("d"))
	require.False(t, ok)
	require.Empty(t, r.groups)
}

func TestFragmentReassembler_IsolatedByClientAndTarget(t *testing.T) {
	r := newFragmentReassembler()

	_, ok := r.process("client1", "target1", 1, []byte("pi"))
	require.False(t, ok)
	// another client towards the same target gets its own sequence
	_, ok = r.process("client2", "target1", 1, []byte("xx"))
	require.False(t, ok)
	// the same client towards another target gets its own sequence
	_, ok = r.process("client1", "target2", 1, []byte("yy"))
	require.False(t, ok)

	data, ok := r.process("client1", "target1", fragmentFlagEnd|2, []byte("ng"))
	require.True(t, ok)
	require.Equal(t, "ping", string(data))
	// the other sequences are still pending and unaffected
	require.Len(t, r.groups, 2)
}

func startUDPEchoTarget(t *testing.T) (*net.UDPAddr, chan []byte) {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	require.NoError(t, err)
	received := make(chan []byte, 16)
	go func() {
		buf := make([]byte, 2048)
		for {
			n, remote, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			data := append([]byte(nil), buf[:n]...)
			received <- data
			conn.WriteToUDP(data, remote) //nolint:errcheck
		}
	}()
	t.Cleanup(func() { conn.Close() }) //nolint:errcheck
	return conn.LocalAddr().(*net.UDPAddr), received
}

func startFragmentTestProxy(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0") // nolint: noctx
	require.NoError(t, err)
	srv := NewServer()
	go func() { srv.Serve(l) }()    //nolint:errcheck
	t.Cleanup(func() { l.Close() }) //nolint:errcheck
	return l.Addr().String()
}

// setupAssociation performs the SOCKS5 handshake and UDP ASSOCIATE request,
// returning the control connection and the UDP relay address.
func setupAssociation(t *testing.T, proxyAddr string, client *net.UDPConn) (net.Conn, *net.UDPAddr) {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr) // nolint: noctx
	require.NoError(t, err)

	req := bytes.NewBuffer([]byte{statute.VersionSocks5, 1, statute.MethodNoAuth})
	reqHead := statute.Request{
		Version: statute.VersionSocks5,
		Command: statute.CommandAssociate,
		DstAddr: statute.AddrSpec{
			IP:       net.ParseIP("127.0.0.1"),
			Port:     client.LocalAddr().(*net.UDPAddr).Port,
			AddrType: statute.ATYPIPv4,
		},
	}
	req.Write(reqHead.Bytes())
	_, err = conn.Write(req.Bytes())
	require.NoError(t, err)

	require.NoError(t, conn.SetDeadline(time.Now().Add(2*time.Second)))
	out := make([]byte, 2)
	_, err = io.ReadFull(conn, out)
	require.NoError(t, err)
	require.Equal(t, []byte{statute.VersionSocks5, statute.MethodNoAuth}, out)
	rspHead, err := statute.ParseReply(conn)
	require.NoError(t, err)
	require.Equal(t, statute.RepSuccess, rspHead.Response)
	require.NoError(t, conn.SetDeadline(time.Time{}))

	return conn, &net.UDPAddr{IP: rspHead.BndAddr.IP, Port: rspHead.BndAddr.Port}
}

func newUDPClient(t *testing.T) *net.UDPConn {
	t.Helper()
	client, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	require.NoError(t, err)
	t.Cleanup(func() { client.Close() }) //nolint:errcheck
	return client
}

func sendFragment(t *testing.T, client *net.UDPConn, bind, target *net.UDPAddr, frag byte, data []byte) {
	t.Helper()
	d := statute.Datagram{
		Frag: frag,
		DstAddr: statute.AddrSpec{
			IP:       target.IP,
			Port:     target.Port,
			AddrType: statute.ATYPIPv4,
		},
		Data: data,
	}
	_, err := client.WriteToUDP(d.Bytes(), bind)
	require.NoError(t, err)
}

func readClientDatagram(t *testing.T, client *net.UDPConn) statute.Datagram {
	t.Helper()
	require.NoError(t, client.SetReadDeadline(time.Now().Add(2*time.Second)))
	defer client.SetReadDeadline(time.Time{}) //nolint:errcheck
	buf := make([]byte, 2048)
	n, _, err := client.ReadFromUDP(buf)
	require.NoError(t, err)
	d, err := statute.ParseDatagram(buf[:n])
	require.NoError(t, err)
	return d
}

func requireNoDatagram(t *testing.T, received chan []byte) {
	t.Helper()
	select {
	case data := <-received:
		t.Fatalf("unexpected datagram forwarded to target: %q", data)
	case <-time.After(300 * time.Millisecond):
	}
}

func requireDatagram(t *testing.T, received chan []byte, expected string) {
	t.Helper()
	select {
	case data := <-received:
		require.Equal(t, expected, string(data))
	case <-time.After(2 * time.Second):
		t.Fatalf("expected datagram %q was not forwarded to target", expected)
	}
}

func TestSOCKS5_Associate_FragmentReassembly(t *testing.T) {
	targetAddr, received := startUDPEchoTarget(t)
	proxyAddr := startFragmentTestProxy(t)
	client := newUDPClient(t)
	conn, bindAddr := setupAssociation(t, proxyAddr, client)
	defer conn.Close() //nolint:errcheck

	sendFragment(t, client, bindAddr, targetAddr, 1, []byte("pi"))
	sendFragment(t, client, bindAddr, targetAddr, fragmentFlagEnd|2, []byte("ng"))

	// the target only sees the fully reassembled datagram
	requireDatagram(t, received, "ping")
	// the reply carries FRAG 0 and the echoed payload
	rsp := readClientDatagram(t, client)
	require.Equal(t, byte(0), rsp.Frag)
	require.Equal(t, "ping", string(rsp.Data))
}

func TestSOCKS5_Associate_FragmentRollback(t *testing.T) {
	targetAddr, received := startUDPEchoTarget(t)
	proxyAddr := startFragmentTestProxy(t)
	client := newUDPClient(t)
	conn, bindAddr := setupAssociation(t, proxyAddr, client)
	defer conn.Close() //nolint:errcheck

	sendFragment(t, client, bindAddr, targetAddr, 1, []byte("pi"))
	sendFragment(t, client, bindAddr, targetAddr, 2, []byte("ng"))
	// the sequence number rolls back: the sequence must be discarded
	sendFragment(t, client, bindAddr, targetAddr, 1, []byte("xx"))
	requireNoDatagram(t, received)
	// the stale end fragment of the dropped sequence is rejected too
	sendFragment(t, client, bindAddr, targetAddr, fragmentFlagEnd|2, []byte("!!"))
	requireNoDatagram(t, received)
	// a fresh, well-formed sequence is still reassembled
	sendFragment(t, client, bindAddr, targetAddr, 1, []byte("pi"))
	sendFragment(t, client, bindAddr, targetAddr, fragmentFlagEnd|2, []byte("ng"))
	requireDatagram(t, received, "ping")
}

func TestSOCKS5_Associate_FragmentMissingLast(t *testing.T) {
	targetAddr, received := startUDPEchoTarget(t)
	proxyAddr := startFragmentTestProxy(t)
	client := newUDPClient(t)
	conn, bindAddr := setupAssociation(t, proxyAddr, client)
	defer conn.Close() //nolint:errcheck

	// no fragment carries the end flag: nothing may be forwarded
	sendFragment(t, client, bindAddr, targetAddr, 1, []byte("pi"))
	sendFragment(t, client, bindAddr, targetAddr, 2, []byte("ng"))
	requireNoDatagram(t, received)
}

func TestSOCKS5_Associate_FragmentAssociationClose(t *testing.T) {
	targetAddr, received := startUDPEchoTarget(t)
	proxyAddr := startFragmentTestProxy(t)
	client := newUDPClient(t)

	conn, bindAddr := setupAssociation(t, proxyAddr, client)
	// buffer an incomplete sequence, then close the association
	sendFragment(t, client, bindAddr, targetAddr, 1, []byte("pi"))
	requireNoDatagram(t, received)
	conn.Close() //nolint:errcheck

	// a new association must not inherit the stale fragment state
	conn2, bindAddr2 := setupAssociation(t, proxyAddr, client)
	defer conn2.Close() //nolint:errcheck
	sendFragment(t, client, bindAddr2, targetAddr, fragmentFlagEnd|2, []byte("ng"))
	requireNoDatagram(t, received)
	// a complete sequence on the new association works as usual
	sendFragment(t, client, bindAddr2, targetAddr, 1, []byte("pi"))
	sendFragment(t, client, bindAddr2, targetAddr, fragmentFlagEnd|2, []byte("ng"))
	requireDatagram(t, received, "ping")
}

func TestSOCKS5_Associate_FragmentOtherClientSameTarget(t *testing.T) {
	targetAddr, received := startUDPEchoTarget(t)
	proxyAddr := startFragmentTestProxy(t)

	client1 := newUDPClient(t)
	conn1, bindAddr1 := setupAssociation(t, proxyAddr, client1)
	defer conn1.Close() //nolint:errcheck

	client2 := newUDPClient(t)
	conn2, bindAddr2 := setupAssociation(t, proxyAddr, client2)
	defer conn2.Close() //nolint:errcheck

	// client1 starts a fragment sequence towards the target
	sendFragment(t, client1, bindAddr1, targetAddr, 1, []byte("pi"))
	// client2 sends the terminating fragment towards the same target;
	// the fragments of the two clients must never be combined
	sendFragment(t, client2, bindAddr2, targetAddr, fragmentFlagEnd|2, []byte("ng"))
	requireNoDatagram(t, received)
	// client1 completes its own sequence
	sendFragment(t, client1, bindAddr1, targetAddr, fragmentFlagEnd|2, []byte("ng"))
	requireDatagram(t, received, "ping")
}
