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

// defaultBindTimeout is the default timeout for waiting the inbound connection of a bind command
const defaultBindTimeout = 2 * time.Minute

// handleBind is used to handle a bind command
//
// It implements the two-phase reply of RFC 1928: the first reply reports the
// address the server is listening on, the second reply is sent only after an
// inbound connection from the expected peer has arrived, and reports the
// address of that peer. Afterwards data is relayed between the control
// connection and the inbound connection.
func (sf *Server) handleBind(_ context.Context, writer io.Writer, request *Request) error {
	// Listen on the same local address as the control connection, or bindIP if set
	listenIP := sf.bindIP
	if listenIP == nil {
		if tcpAddr, ok := request.LocalAddr.(*net.TCPAddr); ok {
			listenIP = tcpAddr.IP
		}
	}
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: listenIP, Port: 0})
	if err != nil {
		if err := SendReply(writer, statute.RepServerFailure, nil); err != nil {
			return fmt.Errorf("failed to send reply, %v", err)
		}
		return fmt.Errorf("bind listen failed, %v", err)
	}
	defer listener.Close() // nolint: errcheck

	// First reply: report the address the server is listening on.
	// The bind is not complete yet, the client must wait for the second reply.
	if err := SendReply(writer, statute.RepSuccess, listener.Addr()); err != nil {
		return fmt.Errorf("failed to send reply, %v", err)
	}

	timeout := sf.bindTimeout
	if timeout <= 0 {
		timeout = defaultBindTimeout
	}

	type acceptResult struct {
		conn net.Conn
		err  error
	}
	acceptCh := make(chan acceptResult, 1)
	sf.goFunc(func() {
		conn, err := listener.Accept()
		acceptCh <- acceptResult{conn, err}
	})

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	// Monitor the control connection while waiting: the client must neither
	// send data nor disconnect before the second reply. The read is bounded
	// by a read deadline so it can be interrupted once the inbound
	// connection arrives or the wait times out.
	var ctrlCh chan error
	ctrlConn, canDeadline := writer.(net.Conn)
	if canDeadline {
		if err := ctrlConn.SetReadDeadline(time.Now().Add(timeout)); err == nil {
			ctrlCh = make(chan error, 1)
			sf.goFunc(func() {
				_, err := request.Reader.Read(make([]byte, 1))
				ctrlCh <- err
			})
		}
	}

	var inbound net.Conn
	select {
	case res := <-acceptCh:
		if res.err != nil {
			return fmt.Errorf("bind accept failed, %v", res.err)
		}
		inbound = res.conn
	case err := <-ctrlCh:
		// The control connection was closed by the client (or the client
		// unexpectedly sent data) while waiting for the inbound connection.
		return fmt.Errorf("bind: control connection terminated while waiting for inbound connection, %v", err)
	case <-timer.C:
		if err := SendReply(writer, statute.RepTTLExpired, nil); err != nil {
			return fmt.Errorf("failed to send reply, %v", err)
		}
		return errors.New("bind: timed out waiting for inbound connection")
	}
	listener.Close() // nolint: errcheck

	// Stop the control connection monitor before reusing request.Reader for
	// proxying: force the pending read to return, wait for the goroutine to
	// finish, then clear the deadline.
	if ctrlCh != nil {
		ctrlConn.SetReadDeadline(time.Now()) // nolint: errcheck
		<-ctrlCh
		ctrlConn.SetReadDeadline(time.Time{}) // nolint: errcheck
	}

	// The inbound connection must come from the expected peer.
	remoteTCP, ok := inbound.RemoteAddr().(*net.TCPAddr)
	if !ok {
		inbound.Close() // nolint: errcheck
		if err := SendReply(writer, statute.RepConnectionRefused, nil); err != nil {
			return fmt.Errorf("failed to send reply, %v", err)
		}
		return fmt.Errorf("bind: unexpected inbound peer type %T", inbound.RemoteAddr())
	}
	dest := request.DestAddr
	match := (dest.IP == nil || dest.IP.IsUnspecified() || dest.IP.Equal(remoteTCP.IP)) &&
		(dest.Port == 0 || dest.Port == remoteTCP.Port)
	if !match {
		inbound.Close() // nolint: errcheck
		if err := SendReply(writer, statute.RepConnectionRefused, nil); err != nil {
			return fmt.Errorf("failed to send reply, %v", err)
		}
		return fmt.Errorf("bind: unexpected inbound peer %v, expect %v", remoteTCP, dest)
	}

	// Second reply: report the address of the connected peer.
	if err := SendReply(writer, statute.RepSuccess, inbound.RemoteAddr()); err != nil {
		inbound.Close() // nolint: errcheck
		return fmt.Errorf("failed to send reply, %v", err)
	}
	defer inbound.Close() // nolint: errcheck

	// Start proxying between the control connection and the inbound connection
	errCh := make(chan error, 2)
	sf.goFunc(func() { errCh <- sf.Proxy(inbound, request.Reader) })
	sf.goFunc(func() { errCh <- sf.Proxy(writer, inbound) })
	// Wait
	for i := 0; i < 2; i++ {
		e := <-errCh
		if e != nil {
			// return from this function closes inbound (and conn).
			return e
		}
	}
	return nil
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
