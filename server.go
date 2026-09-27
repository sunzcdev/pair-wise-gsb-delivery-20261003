package smtp

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"
)

var ErrServerClosed = errors.New("smtp: server already closed")

// Logger interface is used by Server to report unexpected internal errors.
type Logger interface {
	Printf(format string, v ...interface{})
	Println(v ...interface{})
}

// A SMTP server.
type Server struct {
	// The type of network, "tcp" or "unix".
	Network string
	// TCP or Unix address to listen on.
	Addr string
	// The server TLS configuration.
	TLSConfig *tls.Config
	// Enable LMTP mode, as defined in RFC 2033.
	LMTP bool

	Domain            string
	MaxRecipients     int
	MaxMessageBytes   int64
	MaxLineLength     int
	AllowInsecureAuth bool
	Debug             io.Writer
	ErrorLog          Logger
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration

	// Advertise SMTPUTF8 (RFC 6531) capability.
	// Should be used only if backend supports it.
	EnableSMTPUTF8 bool

	// Advertise REQUIRETLS (RFC 8689) capability.
	// Should be used only if backend supports it.
	EnableREQUIRETLS bool

	// Advertise BINARYMIME (RFC 3030) capability.
	// Should be used only if backend supports it.
	EnableBINARYMIME bool

	// Advertise DSN (RFC 3461) capability.
	// Should be used only if backend supports it.
	EnableDSN bool

	// Advertise RRVS (RFC 7293) capability.
	// Should be used only if backend supports it.
	EnableRRVS bool

	// Advertise DELIVERBY (RFC 2852) capability.
	// Should be used only if backend supports it.
	EnableDELIVERBY bool
	// The minimum time, with seconds precision, that a client
	// may specify in the BY argument with return mode.
	// A zero value indicates no set minimum.
	// Only use if DELIVERBY is enabled.
	MinimumDeliverByTime time.Duration

	// Advertise MT-PRIORITY (RFC 6710) capability.
	// Should only be used if backend supports it.
	EnableMTPRIORITY bool
	// The priority profile mapping as defined
	// in RFC 6710 section 10.2.
	//
	// Default value of NONE to advertise no specific profile.
	MtPriorityProfile PriorityProfile

	// The server backend.
	Backend Backend

	wg   sync.WaitGroup
	done chan struct{}

	locker    sync.Mutex
	listeners []net.Listener
	conns     map[*Conn]struct{}
}

// New creates a new SMTP server.
func NewServer(be Backend) *Server {
	return &Server{
		// Doubled maximum line length per RFC 5321 (Section 4.5.3.1.6)
		MaxLineLength: 2000,

		Backend:  be,
		done:     make(chan struct{}, 1),
		ErrorLog: log.New(os.Stderr, "smtp/server ", log.LstdFlags),
		conns:    make(map[*Conn]struct{}),
	}
}

// Serve accepts incoming connections on the Listener l.
func (s *Server) Serve(l net.Listener) error {
	s.locker.Lock()
	s.listeners = append(s.listeners, l)
	s.locker.Unlock()

	var tempDelay time.Duration // how long to sleep on accept failure

	for {
		c, err := l.Accept()
		if err != nil {
			select {
			case <-s.done:
				// we called Close()
				return nil
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond
				} else {
					tempDelay *= 2
				}
				if max := 1 * time.Second; tempDelay > max {
					tempDelay = max
				}
				s.ErrorLog.Printf("accept error: %s; retrying in %s", err, tempDelay)
				time.Sleep(tempDelay)
				continue
			}
			return err
		}

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()

			err := s.handleConn(newConn(c, s))
			if err != nil {
				s.ErrorLog.Printf("error handling %v: %s", c.RemoteAddr(), err)
			}
		}()
	}
}

func (s *Server) handleConn(c *Conn) error {
	s.locker.Lock()
	s.conns[c] = struct{}{}
	s.locker.Unlock()

	defer func() {
		c.Close()
		// Wait for backend work spawned by this connection (e.g. BDAT
		// Data handlers) to terminate.
		c.wg.Wait()

		s.locker.Lock()
		delete(s.conns, c)
		s.locker.Unlock()
	}()

	// The server may have been shut down between Accept and this point.
	select {
	case <-s.done:
		return nil
	default:
	}

	if tlsConn, ok := c.conn.(*tls.Conn); ok {
		if d := s.ReadTimeout; d != 0 {
			c.conn.SetReadDeadline(time.Now().Add(d))
		}
		if d := s.WriteTimeout; d != 0 {
			c.conn.SetWriteDeadline(time.Now().Add(d))
		}
		if err := tlsConn.Handshake(); err != nil {
			return err
		}
	}

	c.greet()

	for {
		// If the server is shutting down, terminate the connection as
		// soon as it is idle. A connection in the middle of a mail
		// transaction is left alone so that no response is interleaved
		// with the message body being read.
		select {
		case <-s.done:
			if !c.transactionActive() {
				c.writeResponse(421, EnhancedCode{4, 3, 2}, "Server shutting down, bye")
				return nil
			}
		default:
		}

		line, err := c.readLine()
		if err == nil {
			cmd, arg, err := parseCmd(line)
			if err != nil {
				c.protocolError(501, EnhancedCode{5, 5, 2}, "Bad command")
				continue
			}

			c.handle(cmd, arg)
		} else {
			if err == io.EOF || errors.Is(err, net.ErrClosed) {
				return nil
			}
			if err == ErrTooLongLine {
				c.writeResponse(500, EnhancedCode{5, 4, 0}, "Too long line, closing connection")
				return nil
			}

			if neterr, ok := err.(net.Error); ok && neterr.Timeout() {
				c.writeResponse(421, EnhancedCode{4, 4, 2}, "Idle timeout, bye bye")
				return nil
			}

			c.writeResponse(421, EnhancedCode{4, 4, 0}, "Connection error, sorry")
			return err
		}
	}
}

func (s *Server) network() string {
	if s.Network != "" {
		return s.Network
	}
	if s.LMTP {
		return "unix"
	}
	return "tcp"
}

// ListenAndServe listens on the network address s.Addr and then calls Serve
// to handle requests on incoming connections.
//
// If s.Addr is blank and LMTP is disabled, ":smtp" is used.
func (s *Server) ListenAndServe() error {
	network := s.network()

	addr := s.Addr
	if !s.LMTP && addr == "" {
		addr = ":smtp"
	}

	l, err := net.Listen(network, addr)
	if err != nil {
		return err
	}

	return s.Serve(l)
}

// ListenAndServeTLS listens on the TCP network address s.Addr and then calls
// Serve to handle requests on incoming TLS connections.
//
// If s.Addr is blank and LMTP is disabled, ":smtps" is used.
func (s *Server) ListenAndServeTLS() error {
	network := s.network()

	addr := s.Addr
	if !s.LMTP && addr == "" {
		addr = ":smtps"
	}

	l, err := tls.Listen(network, addr, s.TLSConfig)
	if err != nil {
		return err
	}

	return s.Serve(l)
}

// Close immediately closes all active listeners and connections.
//
// Close returns any error returned from closing the server's underlying
// listener(s).
func (s *Server) Close() error {
	select {
	case <-s.done:
		return ErrServerClosed
	default:
		close(s.done)
	}

	var err error
	s.locker.Lock()
	for _, l := range s.listeners {
		if lerr := l.Close(); lerr != nil && err == nil {
			err = lerr
		}
	}

	for conn := range s.conns {
		conn.Close()
	}
	s.locker.Unlock()

	return err
}

// Shutdown gracefully shuts down the server without interrupting any
// active mail transactions. Shutdown works by first closing all open
// listeners, then closing all idle connections, and then waiting
// indefinitely for connections with a transaction in progress to
// complete it and shut down.
//
// If the provided context expires before the shutdown is complete,
// Shutdown forcibly closes the remaining connections, which cancels any
// backend work in progress, waits for all connection handlers and backend
// work to terminate, and returns the context's error. Otherwise it returns
// any error returned from closing the Server's underlying Listener(s).
func (s *Server) Shutdown(ctx context.Context) error {
	select {
	case <-s.done:
		return ErrServerClosed
	default:
		close(s.done)
	}

	var err error
	s.locker.Lock()
	for _, l := range s.listeners {
		if lerr := l.Close(); lerr != nil && err == nil {
			err = lerr
		}
	}

	// Close connections that are not in the middle of a mail transaction
	// right away. Connections with a transaction in progress are closed by
	// their handlers once the transaction is completed.
	for c := range s.conns {
		c.closeIfIdle()
	}
	s.locker.Unlock()

	connDone := make(chan struct{})
	go func() {
		defer close(connDone)
		s.wg.Wait()
	}()

	select {
	case <-ctx.Done():
		// The deadline has been reached: forcibly close the remaining
		// connections. Closing a connection makes its message body
		// readers fail, which cancels any backend work in progress. No
		// response is written, so nothing gets interleaved with a
		// message body being transmitted.
		s.locker.Lock()
		for c := range s.conns {
			c.Close()
		}
		s.locker.Unlock()

		// Make sure all connection handlers and the backend work they
		// spawned have terminated before returning.
		<-connDone
		return ctx.Err()
	case <-connDone:
		return err
	}
}
