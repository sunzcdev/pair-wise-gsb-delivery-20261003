package socks5

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/proxy"

	"github.com/things-go/go-socks5/statute"
)

func TestSOCKS5_Connect(t *testing.T) {
	t.Run("connect", func(t *testing.T) {
		// Create a local listener
		l, err := net.Listen("tcp", "127.0.0.1:0") // nolint: noctx
		require.NoError(t, err)

		go func() {
			conn, err := l.Accept()
			require.NoError(t, err)
			defer conn.Close() // nolint: errcheck

			buf := make([]byte, 4)
			_, err = io.ReadAtLeast(conn, buf, 4)
			require.NoError(t, err)
			assert.Equal(t, []byte("ping"), buf)

			conn.Write([]byte("pong")) //nolint: errcheck
		}()
		lAddr := l.Addr().(*net.TCPAddr)

		// Create a socks server with UserPass auth.
		cator := UserPassAuthenticator{StaticCredentials{"foo": "bar"}}
		srv := NewServer(
			WithAuthMethods([]Authenticator{cator}),
			WithLogger(NewLogger(log.New(os.Stdout, "socks5: ", log.LstdFlags))),
			WithDialAndRequest(func(ctx context.Context, network, addr string, request *Request) (net.Conn, error) {
				require.Equal(t, network, "tcp")
				require.Equal(t, addr, lAddr.String())
				return net.Dial(network, addr) // nolint: noctx
			}),
		)

		// Start listening
		go func() {
			err := srv.ListenAndServe("tcp", "127.0.0.1:12365")
			require.NoError(t, err)
		}()
		time.Sleep(10 * time.Millisecond)

		// Get a local conn
		conn, err := net.Dial("tcp", "127.0.0.1:12365") // nolint: noctx
		require.NoError(t, err)

		// Connect, auth and connec to local
		req := bytes.NewBuffer(
			[]byte{
				statute.VersionSocks5, 2, statute.MethodNoAuth, statute.MethodUserPassAuth, // methods
				statute.UserPassAuthVersion, 3, 'f', 'o', 'o', 3, 'b', 'a', 'r', // userpass auth
			})
		reqHead := statute.Request{
			Version:  statute.VersionSocks5,
			Command:  statute.CommandConnect,
			Reserved: 0,
			DstAddr: statute.AddrSpec{
				FQDN:     "",
				IP:       net.ParseIP("127.0.0.1"),
				Port:     lAddr.Port,
				AddrType: statute.ATYPIPv4,
			},
		}
		req.Write(reqHead.Bytes())
		// Send a ping
		req.WriteString("ping")

		// Send all the bytes
		conn.Write(req.Bytes()) //nolint: errcheck

		// Verify response
		expected := []byte{
			statute.VersionSocks5, statute.MethodUserPassAuth, // response use UserPass auth
			statute.UserPassAuthVersion, statute.AuthSuccess, // response auth success
		}
		rspHead := statute.Request{
			Version:  statute.VersionSocks5,
			Command:  statute.RepSuccess,
			Reserved: 0,
			DstAddr: statute.AddrSpec{
				FQDN:     "",
				IP:       net.ParseIP("127.0.0.1"),
				Port:     0,
				AddrType: statute.ATYPIPv4,
			},
		}
		expected = append(expected, rspHead.Bytes()...)
		expected = append(expected, []byte("pong")...)

		out := make([]byte, len(expected))
		conn.SetDeadline(time.Now().Add(time.Second)) //nolint: errcheck
		_, err = io.ReadFull(conn, out)
		conn.SetDeadline(time.Time{}) //nolint: errcheck
		require.NoError(t, err)
		// Ignore the port
		out[12] = 0
		out[13] = 0
		assert.Equal(t, expected, out)
	})

	t.Run("connect/customerHandler", func(t *testing.T) {
		// Create a local listener
		l, err := net.Listen("tcp", "127.0.0.1:0") // nolint: noctx
		require.NoError(t, err)

		go func() {
			conn, err := l.Accept()
			require.NoError(t, err)
			defer conn.Close() // nolint: errcheck

			buf := make([]byte, 4)
			_, err = io.ReadAtLeast(conn, buf, 4)
			require.NoError(t, err)
			assert.Equal(t, []byte("ping"), buf)

			conn.Write([]byte("pong")) //nolint: errcheck
		}()
		lAddr := l.Addr().(*net.TCPAddr)

		// Create a socks server with UserPass auth.
		cator := UserPassAuthenticator{StaticCredentials{"foo": "bar"}}
		srv := NewServer(
			WithAuthMethods([]Authenticator{cator}),
			WithLogger(NewLogger(log.New(os.Stdout, "socks5: ", log.LstdFlags))),
			WithDialAndRequest(func(ctx context.Context, network, addr string, request *Request) (net.Conn, error) {
				require.Equal(t, network, "tcp")
				require.Equal(t, addr, lAddr.String())
				return net.Dial(network, addr) // nolint: noctx
			}),
			WithConnectHandle(func(ctx context.Context, writer io.Writer, request *Request) error {
				rsp := statute.Reply{
					Version:  statute.VersionSocks5,
					Response: 0x00,
					BndAddr: statute.AddrSpec{
						FQDN:     "",
						IP:       net.ParseIP("127.0.0.1"),
						Port:     0,
						AddrType: statute.ATYPIPv4,
					},
				}
				_, err := writer.Write(rsp.Bytes())
				writer.Write([]byte("gotcha!")) // nolint: errcheck
				if w, ok := writer.(closeWriter); ok {
					w.CloseWrite() // nolint: errcheck
				}
				return err
			}),
		)

		// Start listening
		go func() {
			err := srv.ListenAndServe("tcp", "127.0.0.1:12369")
			require.NoError(t, err)
		}()
		time.Sleep(10 * time.Millisecond)

		// Get a local conn
		conn, err := net.Dial("tcp", "127.0.0.1:12369") // nolint: noctx
		require.NoError(t, err)

		// Connect, auth and connec to local
		req := bytes.NewBuffer(
			[]byte{
				statute.VersionSocks5, 2, statute.MethodNoAuth, statute.MethodUserPassAuth, // methods
				statute.UserPassAuthVersion, 3, 'f', 'o', 'o', 3, 'b', 'a', 'r', // userpass auth
			})
		reqHead := statute.Request{
			Version:  statute.VersionSocks5,
			Command:  statute.CommandConnect,
			Reserved: 0,
			DstAddr: statute.AddrSpec{
				FQDN:     "",
				IP:       net.ParseIP("127.0.0.1"),
				Port:     lAddr.Port,
				AddrType: statute.ATYPIPv4,
			},
		}
		req.Write(reqHead.Bytes())
		// Send a ping
		req.WriteString("ping")

		// Send all the bytes
		conn.Write(req.Bytes()) //nolint: errcheck

		// Verify response
		expected := []byte{
			statute.VersionSocks5, statute.MethodUserPassAuth, // response use UserPass auth
			statute.UserPassAuthVersion, statute.AuthSuccess, // response auth success
		}
		rspHead := statute.Request{
			Version:  statute.VersionSocks5,
			Command:  statute.RepSuccess,
			Reserved: 0,
			DstAddr: statute.AddrSpec{
				FQDN:     "",
				IP:       net.ParseIP("127.0.0.1"),
				Port:     0,
				AddrType: statute.ATYPIPv4,
			},
		}
		expected = append(expected, rspHead.Bytes()...)
		expected = append(expected, []byte("gotcha!")...)

		out := make([]byte, len(expected))
		conn.SetDeadline(time.Now().Add(time.Second)) //nolint: errcheck
		_, err = io.ReadFull(conn, out)
		conn.SetDeadline(time.Time{}) //nolint: errcheck
		require.NoError(t, err)
		// Ignore the port
		out[12] = 0
		out[13] = 0
		assert.Equal(t, expected, out)
	})

	t.Run("connect/withMiddleware", func(t *testing.T) {
		var middlewareCalled bool

		// Create a local listener
		l, err := net.Listen("tcp", "127.0.0.1:0") // nolint: noctx
		require.NoError(t, err)

		go func() {
			conn, err := l.Accept()
			require.NoError(t, err)
			defer conn.Close() // nolint: errcheck

			buf := make([]byte, 4)
			_, err = io.ReadAtLeast(conn, buf, 4)
			require.NoError(t, err)
			assert.Equal(t, []byte("ping"), buf)

			conn.Write([]byte("pong")) //nolint: errcheck
		}()
		lAddr := l.Addr().(*net.TCPAddr)

		// Create a socks server with UserPass auth.
		cator := UserPassAuthenticator{StaticCredentials{"foo": "bar"}}
		srv := NewServer(
			WithAuthMethods([]Authenticator{cator}),
			WithLogger(NewLogger(log.New(os.Stdout, "socks5: ", log.LstdFlags))),
			WithDialAndRequest(func(ctx context.Context, network, addr string, request *Request) (net.Conn, error) {
				require.Equal(t, network, "tcp")
				require.Equal(t, addr, lAddr.String())
				return net.Dial(network, addr) // nolint: noctx
			}),
			WithConnectMiddleware(func(ctx context.Context, writer io.Writer, request *Request) error {
				middlewareCalled = true
				require.Equal(t, request.LocalAddr.String(), `127.0.0.1:12366`)
				return nil
			}),
		)

		// Start listening
		go func() {
			err := srv.ListenAndServe("tcp", "127.0.0.1:12366")
			require.NoError(t, err)
		}()
		time.Sleep(10 * time.Millisecond)

		// Get a local conn
		conn, err := net.Dial("tcp", "127.0.0.1:12366") // nolint: noctx
		require.NoError(t, err)

		// Connect, auth and connec to local
		req := bytes.NewBuffer(
			[]byte{
				statute.VersionSocks5, 2, statute.MethodNoAuth, statute.MethodUserPassAuth, // methods
				statute.UserPassAuthVersion, 3, 'f', 'o', 'o', 3, 'b', 'a', 'r', // userpass auth
			})
		reqHead := statute.Request{
			Version:  statute.VersionSocks5,
			Command:  statute.CommandConnect,
			Reserved: 0,
			DstAddr: statute.AddrSpec{
				FQDN:     "",
				IP:       net.ParseIP("127.0.0.1"),
				Port:     lAddr.Port,
				AddrType: statute.ATYPIPv4,
			},
		}
		req.Write(reqHead.Bytes())
		// Send a ping
		req.WriteString("ping")

		// Send all the bytes
		conn.Write(req.Bytes()) //nolint: errcheck

		// Verify response
		expected := []byte{
			statute.VersionSocks5, statute.MethodUserPassAuth, // response use UserPass auth
			statute.UserPassAuthVersion, statute.AuthSuccess, // response auth success
		}
		rspHead := statute.Request{
			Version:  statute.VersionSocks5,
			Command:  statute.RepSuccess,
			Reserved: 0,
			DstAddr: statute.AddrSpec{
				FQDN:     "",
				IP:       net.ParseIP("127.0.0.1"),
				Port:     0,
				AddrType: statute.ATYPIPv4,
			},
		}
		expected = append(expected, rspHead.Bytes()...)
		expected = append(expected, []byte("pong")...)

		out := make([]byte, len(expected))
		conn.SetDeadline(time.Now().Add(time.Second)) //nolint: errcheck
		_, err = io.ReadFull(conn, out)
		conn.SetDeadline(time.Time{}) //nolint: errcheck
		require.NoError(t, err)
		// Ignore the port
		out[12] = 0
		out[13] = 0
		assert.Equal(t, expected, out)
		assert.True(t, middlewareCalled, "middleware not called")
	})

	t.Run("connect/withMiddlewareError", func(t *testing.T) {
		var middlewareCalled bool

		// Create a local listener
		l, err := net.Listen("tcp", "127.0.0.1:0") // nolint: noctx
		require.NoError(t, err)

		go func() {
			conn, err := l.Accept()
			require.NoError(t, err)
			defer conn.Close() // nolint: errcheck

			buf := make([]byte, 4)
			_, err = io.ReadAtLeast(conn, buf, 4)
			require.NoError(t, err)
			assert.Equal(t, []byte("ping"), buf)

			conn.Write([]byte("pong")) //nolint: errcheck
		}()
		lAddr := l.Addr().(*net.TCPAddr)

		// Create a socks server with UserPass auth.
		cator := UserPassAuthenticator{StaticCredentials{"foo": "bar"}}
		srv := NewServer(
			WithAuthMethods([]Authenticator{cator}),
			WithLogger(NewLogger(log.New(os.Stdout, "socks5: ", log.LstdFlags))),
			WithDialAndRequest(func(ctx context.Context, network, addr string, request *Request) (net.Conn, error) {
				require.Equal(t, network, "tcp")
				require.Equal(t, addr, lAddr.String())
				return net.Dial(network, addr) // nolint: noctx
			}),
			WithConnectMiddleware(func(ctx context.Context, writer io.Writer, request *Request) error {
				middlewareCalled = true
				require.Equal(t, request.LocalAddr.String(), `127.0.0.1:12367`)
				return errors.New("Address is blocked!")
			}),
		)

		// Start listening
		go func() {
			err := srv.ListenAndServe("tcp", "127.0.0.1:12367")
			require.NoError(t, err)
		}()
		time.Sleep(10 * time.Millisecond)

		// Get a local conn
		conn, err := net.Dial("tcp", "127.0.0.1:12367") // nolint: noctx
		require.NoError(t, err)

		// Connect, auth and connec to local
		req := bytes.NewBuffer(
			[]byte{
				statute.VersionSocks5, 2, statute.MethodNoAuth, statute.MethodUserPassAuth, // methods
				statute.UserPassAuthVersion, 3, 'f', 'o', 'o', 3, 'b', 'a', 'r', // userpass auth
			})
		reqHead := statute.Request{
			Version:  statute.VersionSocks5,
			Command:  statute.CommandConnect,
			Reserved: 0,
			DstAddr: statute.AddrSpec{
				FQDN:     "",
				IP:       net.ParseIP("127.0.0.1"),
				Port:     lAddr.Port,
				AddrType: statute.ATYPIPv4,
			},
		}
		req.Write(reqHead.Bytes())
		// Send a ping
		req.WriteString("ping")

		// Send all the bytes
		conn.Write(req.Bytes()) //nolint: errcheck

		// Verify response
		expected := []byte{
			statute.VersionSocks5, statute.MethodUserPassAuth, // response use UserPass auth
			statute.UserPassAuthVersion, statute.AuthSuccess, // response auth success
		}
		rspHead := statute.Request{
			Version:  statute.VersionSocks5,
			Command:  statute.RepSuccess,
			Reserved: 0,
			DstAddr: statute.AddrSpec{
				FQDN:     "",
				IP:       net.ParseIP("127.0.0.1"),
				Port:     0,
				AddrType: statute.ATYPIPv4,
			},
		}
		expected = append(expected, rspHead.Bytes()...)
		expected = append(expected, []byte("pong")...)

		out := make([]byte, len(expected))
		conn.SetDeadline(time.Now().Add(time.Second)) //nolint: errcheck
		_, err = io.ReadFull(conn, out)
		conn.SetDeadline(time.Time{}) //nolint: errcheck
		require.ErrorIs(t, err, io.ErrUnexpectedEOF)
		assert.Equal(t, []byte{0x5, 0x2, 0x1, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0}, out)
		assert.True(t, middlewareCalled, "middleware not called")
	})
}

func TestSOCKS5_Associate(t *testing.T) {
	t.Run("associate", func(t *testing.T) {
		locIP := net.ParseIP("127.0.0.1")
		// Create a local listener
		serverAddr := &net.UDPAddr{IP: locIP, Port: 12399}
		server, err := net.ListenUDP("udp", serverAddr)
		require.NoError(t, err)
		defer server.Close() // nolint: errcheck

		go func() {
			buf := make([]byte, 2048)
			for {
				n, remote, err := server.ReadFrom(buf)
				if err != nil {
					return
				}
				require.Equal(t, []byte("ping"), buf[:n])

				server.WriteTo([]byte("pong"), remote) //nolint: errcheck
			}
		}()

		clientAddr := &net.UDPAddr{IP: locIP, Port: 12499}
		client, err := net.ListenUDP("udp", clientAddr)
		require.NoError(t, err)
		defer client.Close() // nolint: errcheck

		// Create a socks server
		cator := UserPassAuthenticator{StaticCredentials{"foo": "bar"}}
		proxySrv := NewServer(
			WithAuthMethods([]Authenticator{cator}),
			WithLogger(NewLogger(log.New(os.Stdout, "socks5: ", log.LstdFlags))),
		)
		// Start listening
		go func() {
			err := proxySrv.ListenAndServe("tcp", "127.0.0.1:12355")
			require.NoError(t, err)
		}()
		time.Sleep(10 * time.Millisecond)

		// Get a local conn
		conn, err := net.Dial("tcp", "127.0.0.1:12355") // nolint: noctx
		require.NoError(t, err)

		// Connect, auth and connec to local
		req := bytes.NewBuffer(
			[]byte{
				statute.VersionSocks5, 2, statute.MethodNoAuth, statute.MethodUserPassAuth,
				statute.UserPassAuthVersion, 3, 'f', 'o', 'o', 3, 'b', 'a', 'r',
			})
		reqHead := statute.Request{
			Version:  statute.VersionSocks5,
			Command:  statute.CommandAssociate,
			Reserved: 0,
			DstAddr: statute.AddrSpec{
				FQDN:     "",
				IP:       clientAddr.IP,
				Port:     clientAddr.Port,
				AddrType: statute.ATYPIPv4,
			},
		}
		req.Write(reqHead.Bytes())
		// Send all the bytes
		conn.Write(req.Bytes()) //nolint: errcheck

		// Verify response
		expected := []byte{
			statute.VersionSocks5, statute.MethodUserPassAuth, // use user password auth
			statute.UserPassAuthVersion, statute.AuthSuccess, // response auth success
		}

		out := make([]byte, len(expected))
		conn.SetDeadline(time.Now().Add(time.Second)) //nolint: errcheck
		_, err = io.ReadFull(conn, out)
		conn.SetDeadline(time.Time{}) //nolint: errcheck
		require.NoError(t, err)
		require.Equal(t, expected, out)

		rspHead, err := statute.ParseReply(conn)
		require.NoError(t, err)
		require.Equal(t, statute.VersionSocks5, rspHead.Version)
		require.Equal(t, statute.RepSuccess, rspHead.Response)

		ipByte := []byte(serverAddr.IP.To4())
		portByte := make([]byte, 2)
		binary.BigEndian.PutUint16(portByte, uint16(serverAddr.Port))

		msgBytes := []byte{0, 0, 0, statute.ATYPIPv4}
		msgBytes = append(msgBytes, ipByte...)
		msgBytes = append(msgBytes, portByte...)
		msgBytes = append(msgBytes, []byte("ping")...)
		client.WriteTo(msgBytes, &net.UDPAddr{IP: locIP, Port: rspHead.BndAddr.Port}) //nolint: errcheck
		// t.Logf("proxy bind listen port: %d", rspHead.BndAddr.Port)
		response := make([]byte, 1024)
		n, _, err := client.ReadFrom(response)
		require.NoError(t, err)
		assert.Equal(t, []byte("pong"), response[n-4:n])
		time.Sleep(time.Second * 1)
	})

	t.Run("associate/withMiddleware", func(t *testing.T) {
		var middlewareCalled bool

		locIP := net.ParseIP("127.0.0.1")
		// Create a local listener
		serverAddr := &net.UDPAddr{IP: locIP, Port: 12399}
		server, err := net.ListenUDP("udp", serverAddr)
		require.NoError(t, err)
		defer server.Close() // nolint: errcheck

		go func() {
			buf := make([]byte, 2048)
			for {
				n, remote, err := server.ReadFrom(buf)
				if err != nil {
					return
				}
				require.Equal(t, []byte("ping"), buf[:n])

				server.WriteTo([]byte("pong"), remote) //nolint: errcheck
			}
		}()

		clientAddr := &net.UDPAddr{IP: locIP, Port: 12499}
		client, err := net.ListenUDP("udp", clientAddr)
		require.NoError(t, err)
		defer client.Close() // nolint: errcheck

		// Create a socks server
		cator := UserPassAuthenticator{StaticCredentials{"foo": "bar"}}
		proxySrv := NewServer(
			WithAuthMethods([]Authenticator{cator}),
			WithLogger(NewLogger(log.New(os.Stdout, "socks5: ", log.LstdFlags))),
			WithAssociateMiddleware(func(ctx context.Context, writer io.Writer, request *Request) error {
				require.Equal(t, request.DestAddr.Port, 12499)
				middlewareCalled = true
				return nil
			}),
		)
		// Start listening
		go func() {
			err := proxySrv.ListenAndServe("tcp", "127.0.0.1:12356")
			require.NoError(t, err)
		}()
		time.Sleep(10 * time.Millisecond)

		// Get a local conn
		conn, err := net.Dial("tcp", "127.0.0.1:12356") // nolint: noctx
		require.NoError(t, err)

		// Connect, auth and connec to local
		req := bytes.NewBuffer(
			[]byte{
				statute.VersionSocks5, 2, statute.MethodNoAuth, statute.MethodUserPassAuth,
				statute.UserPassAuthVersion, 3, 'f', 'o', 'o', 3, 'b', 'a', 'r',
			})
		reqHead := statute.Request{
			Version:  statute.VersionSocks5,
			Command:  statute.CommandAssociate,
			Reserved: 0,
			DstAddr: statute.AddrSpec{
				FQDN:     "",
				IP:       clientAddr.IP,
				Port:     clientAddr.Port,
				AddrType: statute.ATYPIPv4,
			},
		}
		req.Write(reqHead.Bytes())
		// Send all the bytes
		conn.Write(req.Bytes()) //nolint: errcheck

		// Verify response
		expected := []byte{
			statute.VersionSocks5, statute.MethodUserPassAuth, // use user password auth
			statute.UserPassAuthVersion, statute.AuthSuccess, // response auth success
		}

		out := make([]byte, len(expected))
		conn.SetDeadline(time.Now().Add(time.Second)) //nolint: errcheck
		_, err = io.ReadFull(conn, out)
		conn.SetDeadline(time.Time{}) //nolint: errcheck
		require.NoError(t, err)
		require.Equal(t, expected, out)

		rspHead, err := statute.ParseReply(conn)
		require.NoError(t, err)
		require.Equal(t, statute.VersionSocks5, rspHead.Version)
		require.Equal(t, statute.RepSuccess, rspHead.Response)

		ipByte := []byte(serverAddr.IP.To4())
		portByte := make([]byte, 2)
		binary.BigEndian.PutUint16(portByte, uint16(serverAddr.Port))

		msgBytes := []byte{0, 0, 0, statute.ATYPIPv4}
		msgBytes = append(msgBytes, ipByte...)
		msgBytes = append(msgBytes, portByte...)
		msgBytes = append(msgBytes, []byte("ping")...)
		client.WriteTo(msgBytes, &net.UDPAddr{IP: locIP, Port: rspHead.BndAddr.Port}) //nolint: errcheck
		// t.Logf("proxy bind listen port: %d", rspHead.BndAddr.Port)
		response := make([]byte, 1024)
		n, _, err := client.ReadFrom(response)
		require.NoError(t, err)
		assert.Equal(t, []byte("pong"), response[n-4:n])
		assert.True(t, middlewareCalled, "middleware not called")
		time.Sleep(time.Second * 1)
	})
}

// setupBindControlConn starts the socks server and performs the handshake and
// the BIND request on a fresh control connection, returning the control
// connection and the first reply (which carries the bind listen address).
func setupBindControlConn(t *testing.T, srv *Server, dst statute.AddrSpec) (net.Conn, statute.Reply) {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0") // nolint: noctx
	require.NoError(t, err)
	t.Cleanup(func() { l.Close() }) // nolint: errcheck
	go func() { srv.Serve(l) }()    // nolint: errcheck

	ctrl, err := net.Dial("tcp", l.Addr().String()) // nolint: noctx
	require.NoError(t, err)

	// handshake: no auth
	_, err = ctrl.Write([]byte{statute.VersionSocks5, 1, statute.MethodNoAuth})
	require.NoError(t, err)
	out := make([]byte, 2)
	_, err = io.ReadFull(ctrl, out)
	require.NoError(t, err)
	require.Equal(t, []byte{statute.VersionSocks5, statute.MethodNoAuth}, out)

	// BIND request
	reqHead := statute.Request{
		Version:  statute.VersionSocks5,
		Command:  statute.CommandBind,
		Reserved: 0,
		DstAddr:  dst,
	}
	_, err = ctrl.Write(reqHead.Bytes())
	require.NoError(t, err)

	// first reply: reports the address the server is listening on
	rep1, err := statute.ParseReply(ctrl)
	require.NoError(t, err)
	require.Equal(t, statute.RepSuccess, rep1.Response)
	require.NotZero(t, rep1.BndAddr.Port)

	return ctrl, rep1
}

func TestSOCKS5_Bind(t *testing.T) {
	t.Run("bind", func(t *testing.T) {
		srv := NewServer(
			WithLogger(NewLogger(log.New(os.Stdout, "socks5: ", log.LstdFlags))),
		)
		ctrl, rep1 := setupBindControlConn(t, srv, statute.AddrSpec{
			AddrType: statute.ATYPIPv4,
			IP:       net.ParseIP("127.0.0.1"),
			Port:     0,
		})
		defer ctrl.Close() // nolint: errcheck

		// the external service reverse-connects to the reported listen address
		peer, err := net.Dial("tcp", (&net.TCPAddr{IP: rep1.BndAddr.IP, Port: rep1.BndAddr.Port}).String()) // nolint: noctx
		require.NoError(t, err)
		defer peer.Close() // nolint: errcheck

		// second reply: reports the address of the connected peer
		rep2, err := statute.ParseReply(ctrl)
		require.NoError(t, err)
		require.Equal(t, statute.RepSuccess, rep2.Response)
		require.Equal(t, peer.LocalAddr().(*net.TCPAddr).Port, rep2.BndAddr.Port)
		require.True(t, rep2.BndAddr.IP.Equal(net.ParseIP("127.0.0.1")))

		// data flows from the client (control connection) to the peer
		_, err = ctrl.Write([]byte("ping"))
		require.NoError(t, err)
		buf := make([]byte, 4)
		peer.SetDeadline(time.Now().Add(2 * time.Second)) //nolint: errcheck
		_, err = io.ReadFull(peer, buf)
		require.NoError(t, err)
		require.Equal(t, []byte("ping"), buf)

		// data flows from the peer back to the client
		_, err = peer.Write([]byte("pong"))
		require.NoError(t, err)
		ctrl.SetDeadline(time.Now().Add(2 * time.Second)) //nolint: errcheck
		_, err = io.ReadFull(ctrl, buf)
		require.NoError(t, err)
		require.Equal(t, []byte("pong"), buf)
	})

	t.Run("bind/timeout", func(t *testing.T) {
		srv := NewServer(
			WithBindTimeout(200*time.Millisecond),
			WithLogger(NewLogger(log.New(os.Stdout, "socks5: ", log.LstdFlags))),
		)
		ctrl, _ := setupBindControlConn(t, srv, statute.AddrSpec{
			AddrType: statute.ATYPIPv4,
			IP:       net.ParseIP("127.0.0.1"),
			Port:     0,
		})
		defer ctrl.Close() // nolint: errcheck

		// no inbound connection arrives: the second reply reports the failure
		ctrl.SetDeadline(time.Now().Add(2 * time.Second)) //nolint: errcheck
		rep2, err := statute.ParseReply(ctrl)
		require.NoError(t, err)
		require.Equal(t, statute.RepTTLExpired, rep2.Response)
	})

	t.Run("bind/unexpectedPeer", func(t *testing.T) {
		srv := NewServer(
			WithLogger(NewLogger(log.New(os.Stdout, "socks5: ", log.LstdFlags))),
		)
		// expect a peer from port 1, which no real connection will match
		ctrl, rep1 := setupBindControlConn(t, srv, statute.AddrSpec{
			AddrType: statute.ATYPIPv4,
			IP:       net.ParseIP("127.0.0.1"),
			Port:     1,
		})
		defer ctrl.Close() // nolint: errcheck

		peer, err := net.Dial("tcp", (&net.TCPAddr{IP: rep1.BndAddr.IP, Port: rep1.BndAddr.Port}).String()) // nolint: noctx
		require.NoError(t, err)
		defer peer.Close() // nolint: errcheck

		// the inbound connection comes from an unexpected source: the second
		// reply reports the failure and the peer connection is closed
		ctrl.SetDeadline(time.Now().Add(2 * time.Second)) //nolint: errcheck
		rep2, err := statute.ParseReply(ctrl)
		require.NoError(t, err)
		require.Equal(t, statute.RepConnectionRefused, rep2.Response)

		peer.SetDeadline(time.Now().Add(2 * time.Second)) //nolint: errcheck
		_, err = peer.Read(make([]byte, 1))
		require.Error(t, err)
	})

	t.Run("bind/controlConnClosed", func(t *testing.T) {
		srv := NewServer(
			WithLogger(NewLogger(log.New(os.Stdout, "socks5: ", log.LstdFlags))),
		)
		ctrl, rep1 := setupBindControlConn(t, srv, statute.AddrSpec{
			AddrType: statute.ATYPIPv4,
			IP:       net.ParseIP("127.0.0.1"),
			Port:     0,
		})

		// the client goes away before the inbound connection arrives:
		// the bind listener must be torn down
		require.NoError(t, ctrl.Close())

		bindAddr := (&net.TCPAddr{IP: rep1.BndAddr.IP, Port: rep1.BndAddr.Port}).String()
		require.Eventually(t, func() bool {
			conn, err := net.DialTimeout("tcp", bindAddr, 100*time.Millisecond) // nolint: noctx
			if err != nil {
				return true
			}
			conn.Close() // nolint: errcheck
			return false
		}, 2*time.Second, 50*time.Millisecond)
	})
}

func Test_SocksWithProxy(t *testing.T) {
	// Create a local listener
	l, err := net.Listen("tcp", "127.0.0.1:0") // nolint: noctx
	require.NoError(t, err)

	go func() {
		conn, err := l.Accept()
		require.NoError(t, err)
		defer conn.Close() // nolint: errcheck

		buf := make([]byte, 4)
		_, err = io.ReadAtLeast(conn, buf, 4)
		require.NoError(t, err)
		require.Equal(t, []byte("ping"), buf)

		conn.Write([]byte("pong")) //nolint: errcheck
	}()
	lAddr := l.Addr().(*net.TCPAddr)

	// Create a socks server with UserPass auth.
	cator := UserPassAuthenticator{StaticCredentials{"foo": "bar"}}
	serv := NewServer(
		WithAuthMethods([]Authenticator{cator}),
		WithLogger(NewLogger(log.New(os.Stdout, "socks5: ", log.LstdFlags))),
	)
	// Start socks server
	go func() {
		err := serv.ListenAndServe("tcp", "127.0.0.1:12395")
		require.NoError(t, err)
	}()
	time.Sleep(10 * time.Millisecond)

	// client
	dial, err := proxy.SOCKS5("tcp", "127.0.0.1:12395", &proxy.Auth{User: "foo", Password: "bar"}, proxy.Direct)
	require.NoError(t, err)

	// Connect, auth and connect to local
	conn, err := dial.Dial("tcp", lAddr.String())
	require.NoError(t, err)

	// Send a ping
	conn.Write([]byte("ping")) //nolint: errcheck

	out := make([]byte, 4)
	conn.SetDeadline(time.Now().Add(time.Second)) //nolint: errcheck
	_, err = io.ReadFull(conn, out)
	conn.SetDeadline(time.Time{}) //nolint: errcheck
	require.NoError(t, err)
	require.Equal(t, []byte("pong"), out)
}

/*****************************    auth        *******************************/

func TestNoAuth_Server(t *testing.T) {
	req := bytes.NewBuffer(nil)
	rsp := new(bytes.Buffer)
	s := NewServer(WithAuthMethods([]Authenticator{&NoAuthAuthenticator{}}))

	ctx, err := s.authenticate(rsp, req, "", []byte{statute.MethodNoAuth})
	require.NoError(t, err)
	assert.Equal(t, statute.MethodNoAuth, ctx.Method)
	assert.Equal(t, []byte{statute.VersionSocks5, statute.MethodNoAuth}, rsp.Bytes())
}

func TestPasswordAuth_Valid_Server(t *testing.T) {
	req := bytes.NewBuffer([]byte{1, 3, 'f', 'o', 'o', 3, 'b', 'a', 'r'})
	rsp := new(bytes.Buffer)
	cator := UserPassAuthenticator{
		StaticCredentials{"foo": "bar"},
	}
	s := NewServer(WithAuthMethods([]Authenticator{cator}))

	ctx, err := s.authenticate(rsp, req, "", []byte{statute.MethodUserPassAuth})
	require.NoError(t, err)
	assert.Equal(t, statute.MethodUserPassAuth, ctx.Method)

	val, ok := ctx.Payload["username"]
	require.True(t, ok)
	require.Equal(t, "foo", val)

	val, ok = ctx.Payload["password"]
	require.True(t, ok)
	require.Equal(t, "bar", val)

	assert.Equal(t, []byte{statute.VersionSocks5, statute.MethodUserPassAuth, 1, statute.AuthSuccess}, rsp.Bytes())
}

func TestPasswordAuth_Invalid_Server(t *testing.T) {
	req := bytes.NewBuffer([]byte{1, 3, 'f', 'o', 'o', 3, 'b', 'a', 'z'})
	rsp := new(bytes.Buffer)
	cator := UserPassAuthenticator{
		StaticCredentials{"foo": "bar"},
	}
	s := NewServer(WithAuthMethods([]Authenticator{cator}))

	ctx, err := s.authenticate(rsp, req, "", []byte{statute.MethodNoAuth, statute.MethodUserPassAuth})
	require.True(t, errors.Is(err, statute.ErrUserAuthFailed))
	require.Nil(t, ctx)

	assert.Equal(t, []byte{statute.VersionSocks5, statute.MethodUserPassAuth, 1, statute.AuthFailure}, rsp.Bytes())
}

func TestNoSupportedAuth_Server(t *testing.T) {
	req := bytes.NewBuffer(nil)
	rsp := new(bytes.Buffer)
	cator := UserPassAuthenticator{
		StaticCredentials{"foo": "bar"},
	}

	s := NewServer(WithAuthMethods([]Authenticator{cator}))

	ctx, err := s.authenticate(rsp, req, "", []byte{statute.MethodNoAuth})
	require.True(t, errors.Is(err, statute.ErrNoSupportedAuth))
	require.Nil(t, ctx)

	assert.Equal(t, []byte{statute.VersionSocks5, statute.MethodNoAcceptable}, rsp.Bytes())
}
