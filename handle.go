package socks5

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/things-go/go-socks5/statute"
)

// defaultBindTimeout is the default time the server waits for the inbound
// connection requested by a BIND command.
const defaultBindTimeout = 2 * time.Minute

// AddressRewriter is used to rewrite a destination transparently
type AddressRewriter interface {
	Rewrite(ctx context.Context, request *Request) (context.Context, *statute.AddrSpec)
}

// A Request represents request received by a server
type Request struct {
	statute.Request
	// AuthContext provided during negotiation
	AuthContext *AuthContext
	// LocalAddr of the network server listen
	LocalAddr net.Addr
	// RemoteAddr of the network that sent the request
	RemoteAddr net.Addr
	// DestAddr of the actual destination (might be affected by rewrite)
	DestAddr *statute.AddrSpec
	// Reader connect of request
	Reader io.Reader
	// RawDestAddr of the desired destination
	RawDestAddr *statute.AddrSpec
}

// ParseRequest creates a new Request from the tcp connection
func ParseRequest(bufConn io.Reader) (*Request, error) {
	hd, err := statute.ParseRequest(bufConn)
	if err != nil {
		return nil, err
	}
	return &Request{
		Request:     hd,
		RawDestAddr: &hd.DstAddr,
		Reader:      bufConn,
	}, nil
}

// handleRequest is used for request processing after authentication
func (sf *Server) handleRequest(write io.Writer, req *Request) error {
	var err error

	ctx := context.Background()
	// Resolve the address if we have a FQDN
	dest := req.RawDestAddr
	if dest.FQDN != "" {
		ctx, dest.IP, err = sf.resolver.Resolve(ctx, dest.FQDN)
		if err != nil {
			if err := SendReply(write, statute.RepHostUnreachable, nil); err != nil {
				return fmt.Errorf("failed to send reply, %v", err)
			}
			return fmt.Errorf("failed to resolve destination[%v], %v", dest.FQDN, err)
		}
	}

	// Apply any address rewrites
	req.DestAddr = req.RawDestAddr
	if sf.rewriter != nil {
		ctx, req.DestAddr = sf.rewriter.Rewrite(ctx, req)
	}

	// Check if this is allowed
	var ok bool
	ctx, ok = sf.rules.Allow(ctx, req)
	if !ok {
		if err := SendReply(write, statute.RepRuleFailure, nil); err != nil {
			return fmt.Errorf("failed to send reply, %v", err)
		}
		return fmt.Errorf("bind to %v blocked by rules", req.RawDestAddr)
	}

	var last Handler
	// Switch on the command
	switch req.Command {
	case statute.CommandConnect:
		last = sf.handleConnect
		if sf.userConnectHandle != nil {
			last = sf.userConnectHandle
		}
		if len(sf.userConnectMiddlewares) != 0 {
			return sf.userConnectMiddlewares.Execute(ctx, write, req, last)
		}
	case statute.CommandBind:
		last = sf.handleBind
		if sf.userBindHandle != nil {
			last = sf.userBindHandle
		}
		if len(sf.userBindMiddlewares) != 0 {
			return sf.userBindMiddlewares.Execute(ctx, write, req, last)
		}
	case statute.CommandAssociate:
		last = sf.handleAssociate
		if sf.userAssociateHandle != nil {
			last = sf.userAssociateHandle
		}
		if len(sf.userAssociateMiddlewares) != 0 {
			return sf.userAssociateMiddlewares.Execute(ctx, write, req, last)
		}
	default:
		if err := SendReply(write, statute.RepCommandNotSupported, nil); err != nil {
			return fmt.Errorf("failed to send reply, %v", err)
		}
		return fmt.Errorf("unsupported command[%v]", req.Command)
	}
	return last(ctx, write, req)
}

// handleConnect is used to handle a connect command
func (sf *Server) handleConnect(ctx context.Context, writer io.Writer, request *Request) error {
	// Attempt to connect
	var target net.Conn
	var err error

	if sf.dialWithRequest != nil {
		target, err = sf.dialWithRequest(ctx, "tcp", request.DestAddr.String(), request)
	} else {
		dial := sf.dial
		if dial == nil {
			dial = func(ctx context.Context, net_, addr string) (net.Conn, error) {
				return net.Dial(net_, addr) // nolint: noctx
			}
		}
		target, err = dial(ctx, "tcp", request.DestAddr.String())
	}
	if err != nil {
		msg := err.Error()
		resp := statute.RepHostUnreachable
		if strings.Contains(msg, "refused") {
			resp = statute.RepConnectionRefused
		} else if strings.Contains(msg, "network is unreachable") {
			resp = statute.RepNetworkUnreachable
		}
		if err := SendReply(writer, resp, nil); err != nil {
			return fmt.Errorf("failed to send reply, %v", err)
		}
		return fmt.Errorf("connect to %v failed, %v", request.RawDestAddr, err)
	}
	defer target.Close() // nolint: errcheck

	// Send success
	if err := SendReply(writer, statute.RepSuccess, target.LocalAddr()); err != nil {
		return fmt.Errorf("failed to send reply, %v", err)
	}

	// Start proxying
	errCh := make(chan error, 2)
	sf.goFunc(func() { errCh <- sf.Proxy(target, request.Reader) })
	sf.goFunc(func() { errCh <- sf.Proxy(writer, target) })
	// Wait
	for i := 0; i < 2; i++ {
		e := <-errCh
		if e != nil {
			// return from this function closes target (and conn).
			return e
		}
	}
	return nil
}

// handleBind is used to handle a bind command.
//
// Per RFC 1928 section 6, the server sends two replies to the client over
// the control connection. The first reply reports the address the server is
// listening on; the second reply reports the connecting host's address and is
// only sent after an inbound connection from the expected peer has been
// accepted. Bidirectional relaying starts only after the second reply.
func (sf *Server) handleBind(_ context.Context, writer io.Writer, request *Request) error {
	timeout := sf.bindTimeout
	if timeout <= 0 {
		timeout = defaultBindTimeout
	}

	// Bind a listener for the inbound connection.
	listenIP := sf.bindIP
	if listenIP == nil {
		if localAddr, ok := request.LocalAddr.(*net.TCPAddr); ok {
			listenIP = localAddr.IP
		} else {
			listenIP = net.IPv4zero
		}
	}
	bindLn, err := net.ListenTCP("tcp", &net.TCPAddr{IP: listenIP, Port: 0})
	if err != nil {
		sf.logger.Errorf("bind listener for %v failed, %v", request.DestAddr, err)
		if replyErr := SendReply(writer, statute.RepServerFailure, nil); replyErr != nil {
			return fmt.Errorf("failed to send reply, %v", replyErr)
		}
		return fmt.Errorf("listen for bind failed, %v", err)
	}
	defer bindLn.Close() // nolint: errcheck

	// First reply: report the listener address, not the overall success.
	if err := SendReply(writer, statute.RepSuccess, bindLn.Addr()); err != nil {
		return fmt.Errorf("failed to send first bind reply, %v", err)
	}

	// Detect a prematurely closed control connection without consuming any
	// application data the client might send after the second reply: bytes are
	// pumped into a pipe that feeds the later client-to-peer relay.
	pipeReader, pipeWriter := io.Pipe()
	clientGone := make(chan struct{})
	go func() {
		_, _ = io.Copy(pipeWriter, request.Reader)
		pipeWriter.Close() // nolint: errcheck
		close(clientGone)
	}()
	defer pipeReader.Close() // nolint: errcheck

	deadline := time.Now().Add(timeout)
	type acceptResult struct {
		conn net.Conn
		err  error
	}
	for {
		if err := bindLn.SetDeadline(deadline); err != nil {
			if replyErr := SendReply(writer, statute.RepServerFailure, nil); replyErr != nil {
				return fmt.Errorf("failed to send reply, %v", replyErr)
			}
			return fmt.Errorf("set bind deadline failed, %v", err)
		}

		acceptCh := make(chan acceptResult, 1)
		go func() {
			inbound, acceptErr := bindLn.Accept()
			acceptCh <- acceptResult{conn: inbound, err: acceptErr}
		}()

		select {
		case <-clientGone:
			return errors.New("bind control connection closed by client")
		case res := <-acceptCh:
			if res.err != nil {
				var netErr net.Error
				if errors.As(res.err, &netErr) && netErr.Timeout() {
					if replyErr := SendReply(writer, statute.RepTTLExpired, nil); replyErr != nil {
						return fmt.Errorf("failed to send reply, %v", replyErr)
					}
					return fmt.Errorf("timed out waiting for inbound bind connection, %v", res.err)
				}
				if replyErr := SendReply(writer, statute.RepServerFailure, nil); replyErr != nil {
					return fmt.Errorf("failed to send reply, %v", replyErr)
				}
				return fmt.Errorf("accept inbound bind connection failed, %v", res.err)
			}

			if !bindSourceAllowed(request.DestAddr, res.conn.RemoteAddr()) {
				sf.logger.Errorf(
					"reject inbound bind connection from %v, expected %v",
					res.conn.RemoteAddr(), request.DestAddr,
				)
				res.conn.Close() // nolint: errcheck
				continue
			}

			// Second reply: report the connected peer's address.
			if err := SendReply(writer, statute.RepSuccess, res.conn.RemoteAddr()); err != nil {
				res.conn.Close() // nolint: errcheck
				return fmt.Errorf("failed to send second bind reply, %v", err)
			}

			return sf.relayBind(writer, pipeReader, res.conn)
		}
	}
}

// relayBind shuffles data in both directions between the SOCKS client and
// the accepted inbound connection until either side terminates.
func (sf *Server) relayBind(writer io.Writer, clientReader io.Reader, inbound net.Conn) error {
	defer inbound.Close() // nolint: errcheck

	errCh := make(chan error, 2)
	sf.goFunc(func() { errCh <- sf.Proxy(inbound, clientReader) })
	sf.goFunc(func() { errCh <- sf.Proxy(writer, inbound) })
	for i := 0; i < 2; i++ {
		if e := <-errCh; e != nil {
			return e
		}
	}
	return nil
}

// bindSourceAllowed reports whether an inbound connection matches the
// expected peer described in the BIND request. An unspecified address or a
// zero port in the request acts as a wildcard, mirroring RFC 1928 semantics.
func bindSourceAllowed(expected *statute.AddrSpec, actual net.Addr) bool {
	if expected == nil {
		return false
	}
	tcpAddr, ok := actual.(*net.TCPAddr)
	if !ok {
		return false
	}
	ipOK := expected.IP.IsUnspecified() || len(expected.IP) == 0 || expected.IP.Equal(tcpAddr.IP)
	portOK := expected.Port == 0 || expected.Port == tcpAddr.Port
	return ipOK && portOK
}

// handleAssociate is used to handle a connect command
func (sf *Server) handleAssociate(ctx context.Context, writer io.Writer, request *Request) error {
	// Attempt to connect
	dial := sf.dial
	if dial == nil {
		dial = func(_ context.Context, net_, addr string) (net.Conn, error) {
			return net.Dial(net_, addr) // nolint: noctx
		}
	}

	var udpAddr *net.UDPAddr
	if sf.useBindIpBaseResolveAsUdpAddr {
		if sf.bindIP != nil {
			var err error
			udpAddr, err = net.ResolveUDPAddr("udp", sf.bindIP.String()+":0")
			if err != nil {
				if err := SendReply(writer, statute.RepServerFailure, nil); err != nil {
					return fmt.Errorf("failed to send reply, %v", err)
				}
				return fmt.Errorf("failed to resolve udp addr, %v", err)
			}
		}
	} else {
		tcpAddr, ok := request.LocalAddr.(*net.TCPAddr)
		if !ok {
			if err := SendReply(writer, statute.RepServerFailure, nil); err != nil {
				return fmt.Errorf("failed to send reply, %v", err)
			}
			return fmt.Errorf("local address is not TCP: %T", request.LocalAddr)
		}
		udpAddr = &net.UDPAddr{IP: tcpAddr.IP, Port: 0}
	}
	bindLn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		if err := SendReply(writer, statute.RepServerFailure, nil); err != nil {
			return fmt.Errorf("failed to send reply, %v", err)
		}
		return fmt.Errorf("listen udp failed, %v", err)
	}

	sf.logger.Errorf("client want to used addr %v, listen addr: %s", request.DestAddr, bindLn.LocalAddr())
	// send BND.ADDR and BND.PORT, client used
	if err = SendReply(writer, statute.RepSuccess, bindLn.LocalAddr()); err != nil {
		return fmt.Errorf("failed to send reply, %v", err)
	}

	sf.goFunc(func() {
		// read from client and write to remote server
		conns := sync.Map{}
		bufPool := sf.bufferPool.Get()
		defer func() {
			sf.bufferPool.Put(bufPool)
			bindLn.Close() // nolint: errcheck
			conns.Range(func(key, value any) bool {
				if connTarget, ok := value.(net.Conn); !ok {
					sf.logger.Errorf("conns has illegal item %v:%v", key, value)
				} else {
					connTarget.Close() // nolint: errcheck
				}
				return true
			})
		}()
		for {
			n, srcAddr, err := bindLn.ReadFromUDP(bufPool[:cap(bufPool)])
			if err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
					return
				}
				continue
			}
			pk, err := statute.ParseDatagram(bufPool[:n])
			if err != nil {
				continue
			}

			// check src addr whether equal requst.DestAddr
			srcEqual := ((request.DestAddr.IP.IsUnspecified()) || request.DestAddr.IP.Equal(srcAddr.IP)) && (request.DestAddr.Port == 0 || request.DestAddr.Port == srcAddr.Port) //nolint:lll
			if !srcEqual {
				continue
			}

			connKey := srcAddr.String() + "--" + pk.DstAddr.String()

			if target, ok := conns.Load(connKey); !ok {
				// if the 'connection' doesn't exist, create one and store it
				targetNew, err := dial(ctx, "udp", pk.DstAddr.String())
				if err != nil {
					sf.logger.Errorf("connect to %v failed, %v", pk.DstAddr, err)
					// TODO:continue or return Error?
					continue
				}
				conns.Store(connKey, targetNew)
				// read from remote server and write to original client
				sf.goFunc(func() {
					bufPool := sf.bufferPool.Get()
					defer func() {
						targetNew.Close() // nolint: errcheck
						conns.Delete(connKey)
						sf.bufferPool.Put(bufPool)
					}()

					for {
						buf := bufPool[:cap(bufPool)]
						n, err := targetNew.Read(buf)
						if err != nil {
							if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
								return
							}
							sf.logger.Errorf("read data from remote %s failed, %v", addrString(targetNew.RemoteAddr()), err)
							return
						}
						tmpBufPool := sf.bufferPool.Get()
						proBuf := tmpBufPool
						proBuf = append(proBuf, pk.Header()...)
						proBuf = append(proBuf, buf[:n]...)
						if _, err := bindLn.WriteTo(proBuf, srcAddr); err != nil {
							sf.bufferPool.Put(tmpBufPool)
							sf.logger.Errorf("write data to client %s failed, %v", srcAddr, err)
							return
						}
						sf.bufferPool.Put(tmpBufPool)
					}
				})
				if _, err := targetNew.Write(pk.Data); err != nil {
					sf.logger.Errorf("write data to remote server %s failed, %v", addrString(targetNew.RemoteAddr()), err)
					return
				}
			} else {
				conn, ok := target.(net.Conn)
				if !ok {
					sf.logger.Errorf("invalid connection type in pool: %T", target)
					return
				}
				if _, err := conn.Write(pk.Data); err != nil {
					sf.logger.Errorf("write data to remote server %s failed, %v", addrString(conn.RemoteAddr()), err)
					return
				}
			}
		}
	})

	buf := sf.bufferPool.Get()
	defer sf.bufferPool.Put(buf)

	for {
		_, err := request.Reader.Read(buf[:cap(buf)])
		// sf.logger.Errorf("read data from client %s, %d bytesm, err is %+v", request.RemoteAddr.String(), num, err)
		if err != nil {
			bindLn.Close() // nolint: errcheck
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
	}
}

// SendReply is used to send a reply message
// rep: reply status see statute's statute file
func SendReply(w io.Writer, rep uint8, bindAddr net.Addr) error {
	rsp := statute.Reply{
		Version:  statute.VersionSocks5,
		Response: rep,
		BndAddr: statute.AddrSpec{
			AddrType: statute.ATYPIPv4,
			IP:       net.IPv4zero,
			Port:     0,
		},
	}

	if rsp.Response == statute.RepSuccess {
		if tcpAddr, ok := bindAddr.(*net.TCPAddr); ok && tcpAddr != nil {
			rsp.BndAddr.IP = tcpAddr.IP
			rsp.BndAddr.Port = tcpAddr.Port
		} else if udpAddr, ok := bindAddr.(*net.UDPAddr); ok && udpAddr != nil {
			rsp.BndAddr.IP = udpAddr.IP
			rsp.BndAddr.Port = udpAddr.Port
		} else {
			rsp.Response = statute.RepAddrTypeNotSupported
		}

		if rsp.BndAddr.IP.To4() != nil {
			rsp.BndAddr.AddrType = statute.ATYPIPv4
		} else if rsp.BndAddr.IP.To16() != nil {
			rsp.BndAddr.AddrType = statute.ATYPIPv6
		}
	}
	// Send the message
	_, err := w.Write(rsp.Bytes())
	return err
}

type closeWriter interface {
	CloseWrite() error
}

// addrString returns the string representation of a net.Addr, or "<nil>" if the address is nil.
func addrString(addr net.Addr) string {
	if addr == nil {
		return "<nil>"
	}
	return addr.String()
}

// Proxy is used to suffle data from src to destination, and sends errors
// down a dedicated channel
func (sf *Server) Proxy(dst io.Writer, src io.Reader) error {
	buf := sf.bufferPool.Get()
	defer sf.bufferPool.Put(buf)
	_, err := io.CopyBuffer(dst, src, buf[:cap(buf)])
	if tcpConn, ok := dst.(closeWriter); ok {
		tcpConn.CloseWrite() //nolint: errcheck
	}
	return err
}
