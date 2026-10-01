package socks5

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/things-go/go-socks5/statute"
)

// This file contains a pure-Go test double standing in for a real GSS-API
// mechanism implementation (e.g. Kerberos). It is NOT a production-grade
// authentication implementation: context tokens are trivially forgeable and
// the "encryption" is a keyed XOR. It exists only to exercise the RFC 1961
// message exchange, protection-level negotiation and message encapsulation
// of the GSSAPIAuthenticator.

var fakeGSSKey = []byte("fake-gss-test-key")

// fakeGSS implements the message protection half of GSSEngine for tests.
type fakeGSS struct {
	key []byte

	mu       sync.Mutex
	wrapConf []bool // conf flags seen by Wrap, in order
	unwraps  int    // number of Unwrap calls
}

func (f *fakeGSS) Wrap(p []byte, conf bool) ([]byte, error) {
	f.mu.Lock()
	f.wrapConf = append(f.wrapConf, conf)
	f.mu.Unlock()

	flags := byte(0)
	body := make([]byte, len(p))
	copy(body, p)
	if conf {
		flags = 1
		for i := range body {
			body[i] ^= f.key[i%len(f.key)]
		}
	}
	token := make([]byte, 0, 3+len(body)+4)
	token = append(token, 'F', 'G', flags)
	token = append(token, body...)
	h := fnv.New32a()
	_, _ = h.Write(f.key)
	_, _ = h.Write([]byte{flags})
	_, _ = h.Write(body)
	var sum [4]byte
	binary.BigEndian.PutUint32(sum[:], h.Sum32())
	return append(token, sum[:]...), nil
}

func (f *fakeGSS) Unwrap(token []byte) ([]byte, error) {
	f.mu.Lock()
	f.unwraps++
	f.mu.Unlock()

	if len(token) < 3+4 || token[0] != 'F' || token[1] != 'G' {
		return nil, errors.New("fake gss: malformed token")
	}
	flags := token[2]
	body := token[3 : len(token)-4]
	h := fnv.New32a()
	_, _ = h.Write(f.key)
	_, _ = h.Write([]byte{flags})
	_, _ = h.Write(body)
	if binary.BigEndian.Uint32(token[len(token)-4:]) != h.Sum32() {
		return nil, errors.New("fake gss: message integrity check failed")
	}
	out := make([]byte, len(body))
	copy(out, body)
	if flags&1 == 1 {
		for i := range out {
			out[i] ^= f.key[i%len(f.key)]
		}
	}
	return out, nil
}

func (f *fakeGSS) stats() (wrapConf []bool, unwraps int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.wrapConf...), f.unwraps
}

// fakeGSSAcceptor is the server side of the test double.
type fakeGSSAcceptor struct {
	fakeGSS
	rounds    int // number of client tokens expected before completion
	failRound int // 1-based round to reject, 0 means never
	n         int
}

func (a *fakeGSSAcceptor) AcceptSecContext(inputToken []byte) ([]byte, bool, error) {
	a.n++
	if a.failRound == a.n {
		return nil, false, errors.New("fake gss: credentials rejected")
	}
	if want := fmt.Sprintf("client-token-%d", a.n); string(inputToken) != want {
		return nil, false, fmt.Errorf("fake gss: unexpected token %q", inputToken)
	}
	if a.n == a.rounds {
		return []byte("server-token-final"), true, nil
	}
	return []byte(fmt.Sprintf("server-token-%d", a.n)), false, nil
}

// fakeGSSInitiator is the client side of the test double.
type fakeGSSInitiator struct {
	fakeGSS
	rounds int
	n      int
}

func (i *fakeGSSInitiator) nextToken() []byte {
	i.n++
	return []byte(fmt.Sprintf("client-token-%d", i.n))
}

func (i *fakeGSSInitiator) complete() bool { return i.n == i.rounds }

// gssClientHandshake performs the client side of the RFC 1961 exchange and
// returns the connection to use for subsequent traffic, protected according
// to the negotiated level.
func gssClientHandshake(conn net.Conn, init *fakeGSSInitiator, wantLevel byte) (net.Conn, byte, error) {
	if _, err := conn.Write(statute.NewMethodRequest(statute.VersionSocks5, []byte{statute.MethodGSSAPI}).Bytes()); err != nil {
		return nil, 0, err
	}
	rep, err := statute.ParseMethodReply(conn)
	if err != nil {
		return nil, 0, err
	}
	if rep.Method != statute.MethodGSSAPI {
		return nil, 0, fmt.Errorf("server refused gss-api method: 0x%02x", rep.Method)
	}
	// context-level token exchange
	for {
		if _, err := conn.Write(statute.NewGSSMessage(statute.GSSMsgAuthentication, init.nextToken()).Bytes()); err != nil {
			return nil, 0, err
		}
		msg, err := statute.ParseGSSMessage(conn)
		if err != nil {
			return nil, 0, err
		}
		if msg.MTyp == statute.GSSMsgAbort {
			return nil, 0, errors.New("server aborted gss-api authentication")
		}
		if msg.MTyp != statute.GSSMsgAuthentication {
			return nil, 0, fmt.Errorf("unexpected message type: 0x%02x", msg.MTyp)
		}
		if init.complete() {
			break
		}
	}
	// protection-level negotiation
	msg, err := statute.ParseGSSMessage(conn)
	if err != nil {
		return nil, 0, err
	}
	if msg.MTyp != statute.GSSMsgProtectionLevel {
		return nil, 0, fmt.Errorf("unexpected message type: 0x%02x", msg.MTyp)
	}
	level, err := init.Unwrap(msg.Token)
	if err != nil {
		return nil, 0, err
	}
	if len(level) != 1 {
		return nil, 0, errors.New("invalid protection-level token")
	}
	chosen := wantLevel
	if chosen > level[0] {
		chosen = level[0]
	}
	token, err := init.Wrap([]byte{chosen}, false)
	if err != nil {
		return nil, 0, err
	}
	if _, err := conn.Write(statute.NewGSSMessage(statute.GSSMsgProtectionLevel, token).Bytes()); err != nil {
		return nil, 0, err
	}
	if chosen != statute.GSSProtectionNone {
		conf := chosen == statute.GSSProtectionConfidentiality
		return newGSSProtectedConn(conn, conn, init, conf), chosen, nil
	}
	return conn, chosen, nil
}

func startEchoServer(t *testing.T) *net.TCPAddr {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0") // nolint: noctx
	require.NoError(t, err)
	t.Cleanup(func() { l.Close() }) // nolint: errcheck
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close() // nolint: errcheck
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return l.Addr().(*net.TCPAddr)
}

func connectRequestBytes(addr *net.TCPAddr) []byte {
	return statute.Request{
		Version:  statute.VersionSocks5,
		Command:  statute.CommandConnect,
		Reserved: 0,
		DstAddr: statute.AddrSpec{
			IP:       addr.IP,
			Port:     addr.Port,
			AddrType: statute.ATYPIPv4,
		},
	}.Bytes()
}

// gssRoundTrip runs a full GSS-API authenticated CONNECT and ping/pong
// exchange, returning the server-side engine for inspection.
func gssRoundTrip(t *testing.T, protectionLevel byte, rounds int, clientLevel byte) (*fakeGSSAcceptor, byte) {
	t.Helper()
	echoAddr := startEchoServer(t)

	var mu sync.Mutex
	var serverEngine *fakeGSSAcceptor
	cator := GSSAPIAuthenticator{
		NewEngine: func(userAddr string) (GSSEngine, error) {
			engine := &fakeGSSAcceptor{
				fakeGSS: fakeGSS{key: fakeGSSKey},
				rounds:  rounds,
			}
			mu.Lock()
			serverEngine = engine
			mu.Unlock()
			return engine, nil
		},
		ProtectionLevel: protectionLevel,
	}
	srv := NewServer(WithAuthMethods([]Authenticator{cator}))
	l, err := net.Listen("tcp", "127.0.0.1:0") // nolint: noctx
	require.NoError(t, err)
	t.Cleanup(func() { l.Close() }) // nolint: errcheck
	go func() { _ = srv.Serve(l) }()

	conn, err := net.Dial("tcp", l.Addr().String()) // nolint: noctx
	require.NoError(t, err)
	defer conn.Close() // nolint: errcheck
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))

	init := &fakeGSSInitiator{fakeGSS: fakeGSS{key: fakeGSSKey}, rounds: rounds}
	pconn, negotiated, err := gssClientHandshake(conn, init, clientLevel)
	require.NoError(t, err)

	// CONNECT to the echo server through the negotiated channel
	_, err = pconn.Write(connectRequestBytes(echoAddr))
	require.NoError(t, err)
	rep := make([]byte, 10)
	_, err = io.ReadFull(pconn, rep)
	require.NoError(t, err)
	require.Equal(t, statute.VersionSocks5, rep[0])
	require.Equal(t, statute.RepSuccess, rep[1])

	// proxied data must pass through the negotiated protection as well
	_, err = pconn.Write([]byte("ping"))
	require.NoError(t, err)
	pong := make([]byte, 4)
	_, err = io.ReadFull(pconn, pong)
	require.NoError(t, err)
	require.Equal(t, []byte("ping"), pong)

	mu.Lock()
	engine := serverEngine
	mu.Unlock()
	require.NotNil(t, engine)
	return engine, negotiated
}

func TestGSSAPIAuth_MultiRoundContinue(t *testing.T) {
	// 3 context-level token exchanges before the context is established
	engine, negotiated := gssRoundTrip(t, statute.GSSProtectionIntegrity, 3, statute.GSSProtectionIntegrity)
	assert.Equal(t, statute.GSSProtectionIntegrity, negotiated)
	assert.Equal(t, 3, engine.n)
}

func TestGSSAPIAuth_IntegrityOnly(t *testing.T) {
	engine, negotiated := gssRoundTrip(t, statute.GSSProtectionIntegrity, 1, statute.GSSProtectionIntegrity)
	assert.Equal(t, statute.GSSProtectionIntegrity, negotiated)

	// the traffic must be integrity protected (wrapped), never downgraded
	// to plaintext, and never escalated to confidentiality
	wrapConf, unwraps := engine.stats()
	assert.Greater(t, unwraps, 0)
	assert.NotEmpty(t, wrapConf)
	for _, conf := range wrapConf {
		assert.False(t, conf)
	}
}

func TestGSSAPIAuth_Confidentiality(t *testing.T) {
	engine, negotiated := gssRoundTrip(t, statute.GSSProtectionConfidentiality, 1, statute.GSSProtectionConfidentiality)
	assert.Equal(t, statute.GSSProtectionConfidentiality, negotiated)

	wrapConf, _ := engine.stats()
	hasConf := false
	for _, conf := range wrapConf {
		hasConf = hasConf || conf
	}
	assert.True(t, hasConf, "expected confidentiality-protected messages")
}

func TestGSSAPIAuth_NoProtection(t *testing.T) {
	engine, negotiated := gssRoundTrip(t, statute.GSSProtectionNone, 1, statute.GSSProtectionNone)
	assert.Equal(t, statute.GSSProtectionNone, negotiated)

	// only the protection-level negotiation itself is wrapped
	_, unwraps := engine.stats()
	assert.Equal(t, 1, unwraps)
}

func TestGSSAPIAuth_Failure(t *testing.T) {
	cator := GSSAPIAuthenticator{
		NewEngine: func(userAddr string) (GSSEngine, error) {
			return &fakeGSSAcceptor{
				fakeGSS:   fakeGSS{key: fakeGSSKey},
				rounds:    3,
				failRound: 2, // reject the second token
			}, nil
		},
		ProtectionLevel: statute.GSSProtectionIntegrity,
	}
	srv := NewServer(WithAuthMethods([]Authenticator{cator}))
	l, err := net.Listen("tcp", "127.0.0.1:0") // nolint: noctx
	require.NoError(t, err)
	t.Cleanup(func() { l.Close() }) // nolint: errcheck
	go func() { _ = srv.Serve(l) }()

	conn, err := net.Dial("tcp", l.Addr().String()) // nolint: noctx
	require.NoError(t, err)
	defer conn.Close() // nolint: errcheck
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))

	init := &fakeGSSInitiator{fakeGSS: fakeGSS{key: fakeGSSKey}, rounds: 3}
	_, _, err = gssClientHandshake(conn, init, statute.GSSProtectionIntegrity)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "aborted")
}

func TestGSSAPIAuth_TamperedMessage(t *testing.T) {
	echoAddr := startEchoServer(t)

	cator := GSSAPIAuthenticator{
		NewEngine: func(userAddr string) (GSSEngine, error) {
			return &fakeGSSAcceptor{
				fakeGSS: fakeGSS{key: fakeGSSKey},
				rounds:  1,
			}, nil
		},
		ProtectionLevel: statute.GSSProtectionIntegrity,
	}
	srv := NewServer(WithAuthMethods([]Authenticator{cator}))
	l, err := net.Listen("tcp", "127.0.0.1:0") // nolint: noctx
	require.NoError(t, err)
	t.Cleanup(func() { l.Close() }) // nolint: errcheck
	go func() { _ = srv.Serve(l) }()

	conn, err := net.Dial("tcp", l.Addr().String()) // nolint: noctx
	require.NoError(t, err)
	defer conn.Close() // nolint: errcheck
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))

	init := &fakeGSSInitiator{fakeGSS: fakeGSS{key: fakeGSSKey}, rounds: 1}
	_, negotiated, err := gssClientHandshake(conn, init, statute.GSSProtectionIntegrity)
	require.NoError(t, err)
	require.Equal(t, statute.GSSProtectionIntegrity, negotiated)

	// Wrap a CONNECT request, then flip a payload byte before sending:
	// the server must reject the tampered message instead of forwarding it.
	token, err := init.Wrap(connectRequestBytes(echoAddr), false)
	require.NoError(t, err)
	token[5] ^= 0xff
	_, err = conn.Write(statute.NewGSSMessage(statute.GSSMsgEncapsulated, token).Bytes())
	require.NoError(t, err)

	// The server detects the tampering and tears the connection down.
	_, err = statute.ParseGSSMessage(conn)
	require.Error(t, err)
}

func TestGSSAPIAuth_NoEngine(t *testing.T) {
	cator := GSSAPIAuthenticator{}
	rsp := new(bytes.Buffer)
	_, err := cator.Authenticate(bytes.NewReader(nil), rsp, "")
	require.Error(t, err)
}
