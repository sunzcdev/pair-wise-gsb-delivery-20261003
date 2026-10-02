// Package redcon implements a Redis compatible server framework
package redcon

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/tidwall/btree"
	"github.com/tidwall/match"
)

var (
	errUnbalancedQuotes       = &errProtocol{"unbalanced quotes in request"}
	errInvalidBulkLength      = &errProtocol{"invalid bulk length"}
	errInvalidMultiBulkLength = &errProtocol{"invalid multibulk length"}
	errDetached               = errors.New("detached")
	errIncompleteCommand      = errors.New("incomplete command")
	errTooMuchData            = errors.New("too much data")
	errStreamNotFinished      = errors.New("stream command argument was not fully consumed")
	errBulkLineTooLong        = &errProtocol{"bulk length line too long"}
)

const maxBufferCap = 262144

// DefaultMaxStreamBulk is the default declared bulk size, in bytes, at or
// above which a command is delivered to the opt-in stream handler instead of
// being fully buffered into Command.Args.
const DefaultMaxStreamBulk = 1 << 20

type errProtocol struct {
	msg string
}

func (err *errProtocol) Error() string {
	return "Protocol error: " + err.msg
}

// Conn represents a client connection
type Conn interface {
	// RemoteAddr returns the remote address of the client connection.
	RemoteAddr() string
	// Close closes the connection.
	Close() error
	// WriteError writes an error to the client.
	WriteError(msg string)
	// WriteString writes a string to the client.
	WriteString(str string)
	// WriteBulk writes bulk bytes to the client.
	WriteBulk(bulk []byte)
	// WriteBulkString writes a bulk string to the client.
	WriteBulkString(bulk string)
	// WriteInt writes an integer to the client.
	WriteInt(num int)
	// WriteInt64 writes a 64-bit signed integer to the client.
	WriteInt64(num int64)
	// WriteUint64 writes a 64-bit unsigned integer to the client.
	WriteUint64(num uint64)
	// WriteArray writes an array header. You must then write additional
	// sub-responses to the client to complete the response.
	// For example to write two strings:
	//
	//   c.WriteArray(2)
	//   c.WriteBulkString("item 1")
	//   c.WriteBulkString("item 2")
	WriteArray(count int)
	// WriteNull writes a null to the client
	WriteNull()
	// WriteRaw writes raw data to the client.
	WriteRaw(data []byte)
	// WriteAny writes any type to the client.
	//   nil             -> null
	//   error           -> error (adds "ERR " when first word is not uppercase)
	//   string          -> bulk-string
	//   numbers         -> bulk-string
	//   []byte          -> bulk-string
	//   bool            -> bulk-string ("0" or "1")
	//   slice           -> array
	//   map             -> array with key/value pairs
	//   SimpleString    -> string
	//   SimpleInt       -> integer
	//   everything-else -> bulk-string representation using fmt.Sprint()
	WriteAny(any interface{})
	// Context returns a user-defined context
	Context() interface{}
	// SetContext sets a user-defined context
	SetContext(v interface{})
	// SetReadBuffer updates the buffer read size for the connection
	SetReadBuffer(bytes int)
	// Detach return a connection that is detached from the server.
	// Useful for operations like PubSub.
	//
	//   dconn := conn.Detach()
	//   go func(){
	//       defer dconn.Close()
	//       cmd, err := dconn.ReadCommand()
	//       if err != nil{
	//           fmt.Printf("read failed: %v\n", err)
	//	         return
	//       }
	//       fmt.Printf("received command: %v", cmd)
	//       hconn.WriteString("OK")
	//       if err := dconn.Flush(); err != nil{
	//           fmt.Printf("write failed: %v\n", err)
	//	         return
	//       }
	//   }()
	Detach() DetachedConn
	// ReadPipeline returns all commands in current pipeline, if any
	// The commands are removed from the pipeline.
	ReadPipeline() []Command
	// PeekPipeline returns all commands in current pipeline, if any.
	// The commands remain in the pipeline.
	PeekPipeline() []Command
	// NetConn returns the base net.Conn connection
	NetConn() net.Conn
	// WriteBulkFrom write bulk from io.Reader, size n
	WriteBulkFrom(n int64, rb io.Reader)
}

// NewServer returns a new Redcon server configured on "tcp" network net.
func NewServer(addr string,
	handler func(conn Conn, cmd Command),
	accept func(conn Conn) bool,
	closed func(conn Conn, err error),
) *Server {
	return NewServerNetwork("tcp", addr, handler, accept, closed)
}

// NewServerTLS returns a new Redcon TLS server configured on "tcp" network net.
func NewServerTLS(addr string,
	handler func(conn Conn, cmd Command),
	accept func(conn Conn) bool,
	closed func(conn Conn, err error),
	config *tls.Config,
) *TLSServer {
	return NewServerNetworkTLS("tcp", addr, handler, accept, closed, config)
}

// NewServerNetwork returns a new Redcon server. The network net must be
// a stream-oriented network: "tcp", "tcp4", "tcp6", "unix" or "unixpacket"
func NewServerNetwork(
	net, laddr string,
	handler func(conn Conn, cmd Command),
	accept func(conn Conn) bool,
	closed func(conn Conn, err error),
) *Server {
	if handler == nil {
		panic("handler is nil")
	}
	s := newServer()
	s.net = net
	s.laddr = laddr
	s.handler = handler
	s.accept = accept
	s.closed = closed
	return s
}

// NewServerNetworkTLS returns a new TLS Redcon server. The network net must be
// a stream-oriented network: "tcp", "tcp4", "tcp6", "unix" or "unixpacket"
func NewServerNetworkTLS(
	net, laddr string,
	handler func(conn Conn, cmd Command),
	accept func(conn Conn) bool,
	closed func(conn Conn, err error),
	config *tls.Config,
) *TLSServer {
	if handler == nil {
		panic("handler is nil")
	}
	s := Server{
		net:     net,
		laddr:   laddr,
		handler: handler,
		accept:  accept,
		closed:  closed,
		conns:   make(map[*conn]bool),
	}

	tls := &TLSServer{
		config: config,
		Server: &s,
	}
	return tls
}

// Close stops listening on the TCP address.
// Already Accepted connections will be closed.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return errors.New("not serving")
	}
	s.done = true
	return s.ln.Close()
}

// ListenAndServe serves incoming connections.
func (s *Server) ListenAndServe() error {
	return s.ListenServeAndSignal(nil)
}

// Addr returns server's listen address
func (s *Server) Addr() net.Addr {
	return s.ln.Addr()
}

// Close stops listening on the TCP address.
// Already Accepted connections will be closed.
func (s *TLSServer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return errors.New("not serving")
	}
	s.done = true
	return s.ln.Close()
}

// ListenAndServe serves incoming connections.
func (s *TLSServer) ListenAndServe() error {
	return s.ListenServeAndSignal(nil)
}

func newServer() *Server {
	s := &Server{
		conns: make(map[*conn]bool),
	}
	return s
}

// Serve creates a new server and serves with the given net.Listener.
func Serve(ln net.Listener,
	handler func(conn Conn, cmd Command),
	accept func(conn Conn) bool,
	closed func(conn Conn, err error),
) error {
	s := newServer()
	s.mu.Lock()
	s.net = ln.Addr().Network()
	s.laddr = ln.Addr().String()
	s.ln = ln
	s.handler = handler
	s.accept = accept
	s.closed = closed
	s.mu.Unlock()
	return serve(s)
}

// ListenAndServe creates a new server and binds to addr configured on "tcp" network net.
func ListenAndServe(addr string,
	handler func(conn Conn, cmd Command),
	accept func(conn Conn) bool,
	closed func(conn Conn, err error),
) error {
	return ListenAndServeNetwork("tcp", addr, handler, accept, closed)
}

// ListenAndServeTLS creates a new TLS server and binds to addr configured on "tcp" network net.
func ListenAndServeTLS(addr string,
	handler func(conn Conn, cmd Command),
	accept func(conn Conn) bool,
	closed func(conn Conn, err error),
	config *tls.Config,
) error {
	return ListenAndServeNetworkTLS("tcp", addr, handler, accept, closed, config)
}

// ListenAndServeNetwork creates a new server and binds to addr. The network net must be
// a stream-oriented network: "tcp", "tcp4", "tcp6", "unix" or "unixpacket"
func ListenAndServeNetwork(
	net, laddr string,
	handler func(conn Conn, cmd Command),
	accept func(conn Conn) bool,
	closed func(conn Conn, err error),
) error {
	return NewServerNetwork(net, laddr, handler, accept, closed).ListenAndServe()
}

// ListenAndServeNetworkTLS creates a new TLS server and binds to addr. The network net must be
// a stream-oriented network: "tcp", "tcp4", "tcp6", "unix" or "unixpacket"
func ListenAndServeNetworkTLS(
	net, laddr string,
	handler func(conn Conn, cmd Command),
	accept func(conn Conn) bool,
	closed func(conn Conn, err error),
	config *tls.Config,
) error {
	return NewServerNetworkTLS(net, laddr, handler, accept, closed, config).ListenAndServe()
}

// ListenServeAndSignal serves incoming connections and passes nil or error
// when listening. signal can be nil.
func (s *Server) ListenServeAndSignal(signal chan error) error {
	ln, err := net.Listen(s.net, s.laddr)
	if err != nil {
		if signal != nil {
			signal <- err
		}
		return err
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	if signal != nil {
		signal <- nil
	}
	return serve(s)
}

// Serve serves incoming connections with the given net.Listener.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	s.ln = ln
	s.net = ln.Addr().Network()
	s.laddr = ln.Addr().String()
	s.mu.Unlock()
	return serve(s)
}

// ListenServeAndSignal serves incoming connections and passes nil or error
// when listening. signal can be nil.
func (s *TLSServer) ListenServeAndSignal(signal chan error) error {
	ln, err := tls.Listen(s.net, s.laddr, s.config)
	if err != nil {
		if signal != nil {
			signal <- err
		}
		return err
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	if signal != nil {
		signal <- nil
	}
	return serve(s.Server)
}

func serve(s *Server) error {
	defer func() {
		s.ln.Close()
		func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			for c := range s.conns {
				c.conn.Close()
			}
			s.conns = nil
		}()
	}()
	for {
		lnconn, err := s.ln.Accept()
		if err != nil {
			s.mu.Lock()
			done := s.done
			s.mu.Unlock()
			if done {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				// see https://github.com/tidwall/redcon/issues/46
				return nil
			}
			if s.AcceptError != nil {
				s.AcceptError(err)
			}
			continue
		}
		c := &conn{
			conn: lnconn,
			addr: lnconn.RemoteAddr().String(),
			wr:   NewWriter(lnconn),
			rd:   NewReader(lnconn),
		}
		s.mu.Lock()
		c.idleClose = s.idleClose
		s.conns[c] = true
		s.mu.Unlock()
		if s.accept != nil && !s.accept(c) {
			s.mu.Lock()
			delete(s.conns, c)
			s.mu.Unlock()
			c.Close()
			continue
		}
		go handle(s, c)
	}
}

// handle manages the server connection.
func handle(s *Server, c *conn) {
	var err error
	defer func() {
		if err != errDetached {
			// do not close the connection when a detach is detected.
			c.conn.Close()
		}
		func() {
			// remove the conn from the server
			s.mu.Lock()
			defer s.mu.Unlock()
			delete(s.conns, c)
			if s.closed != nil {
				if err == io.EOF {
					err = nil
				}
				s.closed(c, err)
			}
		}()
	}()

	err = func() error {
		// read commands and feed back to the client
		for {
			// read pipeline commands
			if c.idleClose != 0 {
				c.conn.SetReadDeadline(time.Now().Add(c.idleClose))
			}
			cmds, scmd, err := c.rd.readCommands(nil, s.maxBulk)
			if err != nil {
				if err, ok := err.(*errProtocol); ok {
					// All protocol errors should attempt a response to
					// the client. Ignore write errors.
					c.wr.WriteError("ERR " + err.Error())
					c.wr.Flush()
				}
				return err
			}
			if scmd != nil {
				// A command with a bulk argument at or above the stream
				// threshold. The argument bytes have not been read yet, so
				// the command is handed to the opt-in stream handler while
				// the stream is still open.
				if s.streamHandler == nil {
					return errStreamNotFinished
				}
				// Commands preceding the stream command in the same
				// pipeline read are handled first.
				c.cmds = cmds
				for len(c.cmds) > 0 {
					pcmd := c.cmds[0]
					if len(c.cmds) == 1 {
						c.cmds = nil
					} else {
						c.cmds = c.cmds[1:]
					}
					s.handler(c, pcmd)
				}
				if c.closed {
					return nil
				}
				if c.detached {
					// The stream was never entered because a preceding
					// command detached the connection.
					return errStreamNotFinished
				}
				scmd.conn = c
				scmd.maxBulk = s.maxBulk
				s.streamHandler.ServeRESPStream(c, scmd)
				if c.closed {
					return nil
				}
				if c.detached {
					// A stream must be fully resolved before a detach.
					if scmd.err != nil || scmd.phase != scmdDone {
						return errStreamNotFinished
					}
					return errDetached
				}
				if scmd.err != nil || scmd.phase != scmdDone {
					// The handler abandoned a half-read argument. There is
					// no safe point to resume the protocol, so the
					// connection is closed instead of executing a partial
					// command.
					// Best-effort bounded drain of what the handler did not
					// consume, so a reply it wrote can still be delivered and
					// the bytes are skipped with bounded memory. If the drain
					// fails (short read, cancellation) the connection is
					// simply closed.
					if scmd.err == nil {
						scmd.err = scmd.Close()
					}
					// Flush any reply the handler wrote before closing.
					c.wr.Flush()
					if scmd.err != nil {
						return scmd.err
					}
					return errStreamNotFinished
				}
				if err := c.wr.Flush(); err != nil {
					return err
				}
			} else {
				c.cmds = cmds
				for len(c.cmds) > 0 {
					cmd := c.cmds[0]
					if len(c.cmds) == 1 {
						c.cmds = nil
					} else {
						c.cmds = c.cmds[1:]
					}
					s.handler(c, cmd)
				}
				if c.detached {
					// client has been detached
					return errDetached
				}
				if c.closed {
					return nil
				}
				if err := c.wr.Flush(); err != nil {
					return err
				}
			}
		}
	}()
}

// conn represents a client connection
type conn struct {
	conn      net.Conn
	wr        *Writer
	rd        *Reader
	addr      string
	ctx       interface{}
	detached  bool
	closed    bool
	cmds      []Command
	idleClose time.Duration
}

func (c *conn) Close() error {
	c.wr.Flush()
	c.closed = true
	return c.conn.Close()
}
func (c *conn) Context() interface{}        { return c.ctx }
func (c *conn) SetContext(v interface{})    { c.ctx = v }
func (c *conn) SetReadBuffer(n int)         {}
func (c *conn) WriteString(str string)      { c.wr.WriteString(str) }
func (c *conn) WriteBulk(bulk []byte)       { c.wr.WriteBulk(bulk) }
func (c *conn) WriteBulkString(bulk string) { c.wr.WriteBulkString(bulk) }
func (c *conn) WriteInt(num int)            { c.wr.WriteInt(num) }
func (c *conn) WriteInt64(num int64)        { c.wr.WriteInt64(num) }
func (c *conn) WriteUint64(num uint64)      { c.wr.WriteUint64(num) }
func (c *conn) WriteError(msg string)       { c.wr.WriteError(msg) }
func (c *conn) WriteArray(count int)        { c.wr.WriteArray(count) }
func (c *conn) WriteNull()                  { c.wr.WriteNull() }
func (c *conn) WriteRaw(data []byte)        { c.wr.WriteRaw(data) }
func (c *conn) WriteAny(v interface{})      { c.wr.WriteAny(v) }
func (c *conn) RemoteAddr() string          { return c.addr }
func (c *conn) ReadPipeline() []Command {
	cmds := c.cmds
	c.cmds = nil
	return cmds
}
func (c *conn) PeekPipeline() []Command {
	return c.cmds
}
func (c *conn) NetConn() net.Conn {
	return c.conn
}
func (c *conn) WriteBulkFrom(n int64, rb io.Reader) {
	c.wr.WriteBulkFrom(n, rb)
}

// BaseWriter returns the underlying connection writer, if any
func BaseWriter(c Conn) *Writer {
	if c, ok := c.(*conn); ok {
		return c.wr
	}
	return nil
}

// DetachedConn represents a connection that is detached from the server
type DetachedConn interface {
	// Conn is the original connection
	Conn
	// ReadCommand reads the next client command.
	ReadCommand() (Command, error)
	// Flush flushes any writes to the network.
	Flush() error
}

// Detach removes the current connection from the server loop and returns
// a detached connection. This is useful for operations such as PubSub.
// The detached connection must be closed by calling Close() when done.
// All writes such as WriteString() will not be written to the client
// until Flush() is called.
func (c *conn) Detach() DetachedConn {
	c.detached = true
	cmds := c.cmds
	c.cmds = nil
	return &detachedConn{conn: c, cmds: cmds}
}

type detachedConn struct {
	*conn
	cmds []Command
}

// Flush writes and Write* calls to the client.
func (dc *detachedConn) Flush() error {
	return dc.conn.wr.Flush()
}

// ReadCommand read the next command from the client.
func (dc *detachedConn) ReadCommand() (Command, error) {
	if len(dc.cmds) > 0 {
		cmd := dc.cmds[0]
		if len(dc.cmds) == 1 {
			dc.cmds = nil
		} else {
			dc.cmds = dc.cmds[1:]
		}
		return cmd, nil
	}
	cmd, err := dc.rd.ReadCommand()
	if err != nil {
		return Command{}, err
	}
	return cmd, nil
}

// Command represent a command
type Command struct {
	// Raw is a encoded RESP message.
	Raw []byte
	// Args is a series of arguments that make up the command.
	Args [][]byte
}

// StreamHandler responds to an RESP request that contains at least one bulk
// argument whose declared length is at least the server's stream threshold.
// Unlike the standard handler, the large argument is not buffered into
// memory; it is read from the connection on demand through the provided
// StreamCommand.
//
// The handler MUST fully consume every streamed argument by the time it
// returns, either by reading exactly the declared number of bytes or by
// calling StreamCommand.Close, which drains the remaining arguments with a
// bounded buffer. If the handler returns while a stream is only partially
// consumed without calling Close, the server performs a best-effort bounded
// drain of the rest of the command and then closes the connection rather
// than execute half a command or desynchronize the protocol stream. A
// handler can also call Conn.Close directly to reject and tear down the
// connection without draining.
type StreamHandler interface {
	ServeRESPStream(conn Conn, cmd *StreamCommand)
}

// StreamHandlerFunc is an adapter to allow the use of ordinary functions as
// StreamHandlers.
type StreamHandlerFunc func(conn Conn, cmd *StreamCommand)

// ServeRESPStream calls f(conn, cmd).
func (f StreamHandlerFunc) ServeRESPStream(conn Conn, cmd *StreamCommand) {
	f(conn, cmd)
}

// Stream phases.
const (
	scmdStream = iota // positioned at the start of a streamed bulk body
	scmdHeader        // body consumed; trailing header/next args pending
	scmdDone          // entire command consumed
	scmdFailed        // an unrecoverable read/protocol error occurred
)

// StreamCommand represents an RESP command whose large bulk arguments are
// read from the connection on demand instead of being fully buffered.
//
// Args holds the fully-buffered arguments preceding the first streamed
// argument; the streamed argument occupies a nil slot at StreamIndex. After
// a successful Next call, smaller arguments following the streamed one are
// appended to Args and the next large argument (if any) becomes the active
// stream.
type StreamCommand struct {
	// Raw is the RESP header of the command, up to and including the
	// length line of the first streamed argument.
	Raw []byte
	// Args are the command arguments. The streamed argument's slot is nil
	// while it is being read.
	Args [][]byte

	conn    *conn
	rd      *Reader
	maxBulk int64
	total   int // declared number of arguments in the array

	argIndex int   // index in Args of the current streamed argument
	size     int64 // declared size of the current streamed argument
	remain   int64 // unread bytes of the current streamed argument
	phase    int
	err      error
}

// StreamIndex returns the index in Args of the argument being streamed.
func (c *StreamCommand) StreamIndex() int { return c.argIndex }

// StreamSize returns the declared size, in bytes, of the streamed argument.
func (c *StreamCommand) StreamSize() int64 { return c.size }

// Remain returns the number of bytes still unread in the streamed argument.
func (c *StreamCommand) Remain() int64 { return c.remain }

// Total returns the declared number of arguments in the command.
func (c *StreamCommand) Total() int { return c.total }

// Err returns the first error encountered while reading the streamed
// command, if any.
func (c *StreamCommand) Err() error { return c.err }

// Read reads up to len(p) bytes of the current streamed bulk argument into p.
// It follows io.Reader semantics: io.EOF is returned once the declared
// number of bytes have all been read. Read never returns bytes belonging to
// the trailing CRLF or to another command in the pipeline.
func (c *StreamCommand) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if c.phase == scmdFailed {
		return 0, c.err
	}
	if c.phase != scmdStream {
		return 0, io.EOF
	}
	if c.remain == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > c.remain {
		p = p[:c.remain]
	}
	n, err := c.rd.readFull(p)
	c.remain -= int64(n)
	if err != nil {
		c.fail(unwrapEOF(err))
		return n, c.err
	}
	if c.remain == 0 {
		return n, io.EOF
	}
	return n, nil
}

// ReadAll reads and returns all remaining bytes of the current streamed
// argument. It is a convenience for arguments the caller is willing to hold
// in memory; prefer Read for very large arguments.
func (c *StreamCommand) ReadAll() ([]byte, error) {
	if c.phase == scmdFailed {
		return nil, c.err
	}
	if c.phase != scmdStream {
		return nil, io.EOF
	}
	buf := make([]byte, c.remain)
	if _, err := io.ReadFull(c, buf); err != nil {
		return buf, err
	}
	return buf, nil
}

// Next consumes the trailing CRLF of the current streamed argument and reads
// any remaining arguments. Arguments smaller than the stream threshold are
// buffered and appended to Args. It reports whether another streamed
// argument follows; when true the caller reads it with Read and calls Next
// again.
//
// Next requires the current argument to be fully read (Remain returns zero);
// use Read or Close to consume remaining bytes first.
func (c *StreamCommand) Next() (more bool, err error) {
	if c.phase == scmdFailed {
		return false, c.err
	}
	if c.phase == scmdDone {
		return false, nil
	}
	if c.remain != 0 {
		err = errStreamNotFinished
		c.fail(err)
		return false, err
	}
	if c.phase == scmdStream {
		if err := c.rd.expectCRLF(); err != nil {
			c.fail(err)
			return false, err
		}
		c.phase = scmdHeader
	}
	for c.argIndex+1 < c.total {
		line, err := c.rd.readBulkHeader()
		if err != nil {
			c.fail(err)
			return false, err
		}
		idx := c.argIndex + 1
		if line.size >= c.maxBulk {
			c.argIndex = idx
			c.ensureSlot(idx)
			c.size = line.size
			c.remain = line.size
			c.phase = scmdStream
			return true, nil
		}
		arg := make([]byte, line.size)
		if _, err := c.rd.readFull(arg); err != nil {
			c.fail(unwrapEOF(err))
			return false, c.err
		}
		if err := c.rd.expectCRLF(); err != nil {
			c.fail(err)
			return false, err
		}
		c.argIndex = idx
		c.ensureSlot(idx)
		c.Args[idx] = arg
	}
	c.phase = scmdDone
	return false, nil
}

// Close finalizes the command. Any unread bytes of the streamed argument and
// every remaining argument are discarded with a bounded buffer, without
// allocating the declared length and without reading past the end of the
// command. After a successful Close the server can safely process the next
// command on the same connection.
//
// When discarding fails (for example on a short read) Close returns the
// error and the server closes the connection. Close is safe at any point and
// may be called repeatedly.
func (c *StreamCommand) Close() error {
	switch c.phase {
	case scmdDone:
		return nil
	case scmdFailed:
		return c.err
	}
	// Drain the active streamed argument, if it is still open.
	if c.phase == scmdStream {
		if c.remain > 0 {
			if err := c.rd.discard(c.remain); err != nil {
				c.fail(unwrapEOF(err))
				return c.err
			}
			c.remain = 0
		}
		if err := c.rd.expectCRLF(); err != nil {
			c.fail(err)
			return err
		}
		c.phase = scmdHeader
	}
	// Drain the remaining arguments in bounded chunks.
	for c.argIndex+1 < c.total {
		line, err := c.rd.readBulkHeader()
		if err != nil {
			c.fail(err)
			return err
		}
		if err := c.rd.discardBulk(line.size); err != nil {
			c.fail(unwrapEOF(err))
			return c.err
		}
		c.argIndex++
	}
	c.phase = scmdDone
	return nil
}

func (c *StreamCommand) ensureSlot(idx int) {
	for len(c.Args) <= idx {
		c.Args = append(c.Args, nil)
	}
}

func (c *StreamCommand) fail(err error) {
	if c.phase != scmdFailed {
		c.phase = scmdFailed
		c.err = err
	}
}

func unwrapEOF(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

// Server defines a server for clients for managing client connections.
type Server struct {
	mu            sync.Mutex
	net           string
	laddr         string
	handler       func(conn Conn, cmd Command)
	streamHandler StreamHandler
	maxBulk       int64
	accept        func(conn Conn) bool
	closed        func(conn Conn, err error)
	conns         map[*conn]bool
	ln            net.Listener
	done          bool
	idleClose     time.Duration

	// AcceptError is an optional function used to handle Accept errors.
	AcceptError func(err error)
}

// TLSServer defines a server for clients for managing client connections.
type TLSServer struct {
	*Server
	config *tls.Config
}

// Writer allows for writing RESP messages.
type Writer struct {
	w   io.Writer
	b   []byte
	err error

	// buff use io buffer write to w(io.Writer)
	// for io.Copy r(io.Reader) to w(io.Writer)
	buff *bufio.Writer
}

// NewWriter creates a new RESP writer.
func NewWriter(wr io.Writer) *Writer {
	return &Writer{
		w:    wr,
		buff: bufio.NewWriter(wr),
	}
}

func (w *Writer) WriteBulkFrom(n int64, r io.Reader) {
	if w != nil && w.err != nil {
		return
	}
	w.buff.Write(appendPrefix(w.b, '$', n))
	io.Copy(w.buff, r)
	w.buff.Write([]byte{'\r', '\n'})
}

// WriteNull writes a null to the client
func (w *Writer) WriteNull() {
	if w.err != nil {
		return
	}
	w.b = AppendNull(w.b)
}

// WriteArray writes an array header. You must then write additional
// sub-responses to the client to complete the response.
// For example to write two strings:
//
//	c.WriteArray(2)
//	c.WriteBulkString("item 1")
//	c.WriteBulkString("item 2")
func (w *Writer) WriteArray(count int) {
	if w.err != nil {
		return
	}
	w.b = AppendArray(w.b, count)
}

// WriteBulk writes bulk bytes to the client.
func (w *Writer) WriteBulk(bulk []byte) {
	if w.err != nil {
		return
	}
	w.b = AppendBulk(w.b, bulk)
}

// WriteBulkString writes a bulk string to the client.
func (w *Writer) WriteBulkString(bulk string) {
	if w.err != nil {
		return
	}
	w.b = AppendBulkString(w.b, bulk)
}

// Buffer returns the unflushed buffer. This is a copy so changes
// to the resulting []byte will not affect the writer.
func (w *Writer) Buffer() []byte {
	if w.err != nil {
		return nil
	}
	return append([]byte(nil), w.b...)
}

// SetBuffer replaces the unflushed buffer with new bytes.
func (w *Writer) SetBuffer(raw []byte) {
	if w.err != nil {
		return
	}
	w.b = w.b[:0]
	w.b = append(w.b, raw...)
}

// Flush writes all unflushed Write* calls to the underlying writer.
func (w *Writer) Flush() error {
	if w.buff != nil {
		w.buff.Flush()
	}

	if w.err != nil {
		return w.err
	}
	_, w.err = w.w.Write(w.b)
	if cap(w.b) > maxBufferCap || w.err != nil {
		w.b = nil
	} else {
		w.b = w.b[:0]
	}
	return w.err
}

// WriteError writes an error to the client.
func (w *Writer) WriteError(msg string) {
	if w.err != nil {
		return
	}
	w.b = AppendError(w.b, msg)
}

// WriteString writes a string to the client.
func (w *Writer) WriteString(msg string) {
	if w.err != nil {
		return
	}
	w.b = AppendString(w.b, msg)
}

// WriteInt writes an integer to the client.
func (w *Writer) WriteInt(num int) {
	if w.err != nil {
		return
	}
	w.WriteInt64(int64(num))
}

// WriteInt64 writes a 64-bit signed integer to the client.
func (w *Writer) WriteInt64(num int64) {
	if w.err != nil {
		return
	}
	w.b = AppendInt(w.b, num)
}

// WriteUint64 writes a 64-bit unsigned integer to the client.
func (w *Writer) WriteUint64(num uint64) {
	if w.err != nil {
		return
	}
	w.b = AppendUint(w.b, num)
}

// WriteRaw writes raw data to the client.
func (w *Writer) WriteRaw(data []byte) {
	if w.err != nil {
		return
	}
	w.b = append(w.b, data...)
}

// WriteAny writes any type to client.
//
//	nil             -> null
//	error           -> error (adds "ERR " when first word is not uppercase)
//	string          -> bulk-string
//	numbers         -> bulk-string
//	[]byte          -> bulk-string
//	bool            -> bulk-string ("0" or "1")
//	slice           -> array
//	map             -> array with key/value pairs
//	SimpleString    -> string
//	SimpleInt       -> integer
//	everything-else -> bulk-string representation using fmt.Sprint()
func (w *Writer) WriteAny(v interface{}) {
	if w.err != nil {
		return
	}
	w.b = AppendAny(w.b, v)
}

// Reader represent a reader for RESP or telnet commands.
type Reader struct {
	rd    *bufio.Reader
	buf   []byte
	start int
	end   int
	cmds  []Command
}

// NewReader returns a command reader which will read RESP or telnet commands.
func NewReader(rd io.Reader) *Reader {
	return &Reader{
		rd:  bufio.NewReader(rd),
		buf: make([]byte, 4096),
	}
}

func parseInt(b []byte) (int, bool) {
	i, err := strconv.Atoi(unsafe.String(unsafe.SliceData(b), len(b)))
	return i, err == nil
}

func (rd *Reader) readCommands(leftover *int, maxBulk int64) ([]Command, *StreamCommand, error) {
	var cmds []Command
	b := rd.buf[rd.start:rd.end]
	if rd.end-rd.start == 0 && len(rd.buf) > 4096 {
		rd.buf = rd.buf[:4096]
		rd.start = 0
		rd.end = 0
	}
	if len(b) > 0 {
		// we have data, yay!
		// but is this enough data for a complete command? or multiple?
	next:
		switch b[0] {
		default:
			// just a plain text command
			for i := 0; i < len(b); i++ {
				if b[i] == '\n' {
					var line []byte
					if i > 0 && b[i-1] == '\r' {
						line = b[:i-1]
					} else {
						line = b[:i]
					}
					var cmd Command
					var quote bool
					var quotech byte
					var escape bool
				outer:
					for {
						nline := make([]byte, 0, len(line))
						for i := 0; i < len(line); i++ {
							c := line[i]
							if !quote {
								if c == ' ' {
									if len(nline) > 0 {
										cmd.Args = append(cmd.Args, nline)
									}
									line = line[i+1:]
									continue outer
								}
								if c == '"' || c == '\'' {
									if i != 0 {
										return nil, nil, errUnbalancedQuotes
									}
									quotech = c
									quote = true
									line = line[i+1:]
									continue outer
								}
							} else {
								if escape {
									escape = false
									switch c {
									case 'n':
										c = '\n'
									case 'r':
										c = '\r'
									case 't':
										c = '\t'
									}
								} else if c == quotech {
									quote = false
									quotech = 0
									cmd.Args = append(cmd.Args, nline)
									line = line[i+1:]
									if len(line) > 0 && line[0] != ' ' {
										return nil, nil, errUnbalancedQuotes
									}
									continue outer
								} else if c == '\\' {
									escape = true
									continue
								}
							}
							nline = append(nline, c)
						}
						if quote {
							return nil, nil, errUnbalancedQuotes
						}
						if len(line) > 0 {
							cmd.Args = append(cmd.Args, line)
						}
						break
					}
					if len(cmd.Args) > 0 {
						// convert this to resp command syntax
						var wr Writer
						wr.WriteArray(len(cmd.Args))
						for i := range cmd.Args {
							wr.WriteBulk(cmd.Args[i])
							cmd.Args[i] = append([]byte(nil), cmd.Args[i]...)
						}
						cmd.Raw = wr.b
						cmds = append(cmds, cmd)
					}
					b = b[i+1:]
					if len(b) > 0 {
						goto next
					} else {
						goto done
					}
				}
			}
		case '*':
			// resp formatted command
			marks := make([]int, 0, 16)
		outer2:
			for i := 1; i < len(b); i++ {
				if b[i] == '\n' {
					if b[i-1] != '\r' {
						return nil, nil, errInvalidMultiBulkLength
					}
					// The '*' case is only reached when b[0] is the array
					// header, so the command starts at offset zero.
					const cmdStart = 0
					count, ok := parseInt(b[1 : i-1])
					if !ok || count <= 0 {
						return nil, nil, errInvalidMultiBulkLength
					}
					marks = marks[:0]
					for j := 0; j < count; j++ {
						// read bulk length
						i++
						if i < len(b) {
							if b[i] != '$' {
								return nil, nil, &errProtocol{"expected '$', got '" +
									string(b[i]) + "'"}
							}
							si := i
							for ; i < len(b); i++ {
								if b[i] == '\n' {
									if b[i-1] != '\r' {
										return nil, nil, errInvalidBulkLength
									}
									size, perr := parseBulkSize(b[si+1 : i-1])
									if perr != nil {
										return nil, nil, perr
									}
									if maxBulk > 0 && size >= maxBulk {
										// Large argument: stop parsing here
										// and expose a live stream.
										prefix := b[cmdStart : i+1]
										raw := prefix
										args := make([][]byte, j)
										for h := 0; h < len(marks); h += 2 {
											args[h/2] = prefix[marks[h]:marks[h+1]]
										}
										if rd.rd != nil {
											raw = append([]byte(nil), prefix...)
											for k := range args {
												args[k] = append([]byte(nil), args[k]...)
											}
										}
										scmd := &StreamCommand{
											rd:       rd,
											Raw:      raw,
											Args:     args,
											total:    count,
											argIndex: j,
											size:     size,
											remain:   size,
											phase:    scmdStream,
										}
										// Position at the first body byte.
										b = b[i+1:]
										rd.start = rd.end - len(b)
										return cmds, scmd, nil
									}
									n := int(size)
									if int64(n) != size {
										// Length overflows the platform's
										// int and cannot be buffered.
										return nil, nil, errInvalidBulkLength
									}
									if i+n+2 >= len(b) {
										// not ready
										break outer2
									}
									if b[i+n+2] != '\n' ||
										b[i+n+1] != '\r' {
										return nil, nil, errInvalidBulkLength
									}
									i++
									marks = append(marks, i, i+n)
									i += n + 1
									break
								}
							}
						}
					}
					if len(marks) == count*2 {
						var cmd Command
						if rd.rd != nil {
							// make a raw copy of the entire command when
							// there's a underlying reader.
							cmd.Raw = append([]byte(nil), b[:i+1]...)
						} else {
							// just assign the slice
							cmd.Raw = b[:i+1]
						}
						cmd.Args = make([][]byte, len(marks)/2)
						// slice up the raw command into the args based on
						// the recorded marks.
						for h := 0; h < len(marks); h += 2 {
							cmd.Args[h/2] = cmd.Raw[marks[h]:marks[h+1]]
						}
						cmds = append(cmds, cmd)
						b = b[i+1:]
						if len(b) > 0 {
							goto next
						} else {
							goto done
						}
					}
				}
			}
		}
	done:
		rd.start = rd.end - len(b)
	}
	if leftover != nil {
		*leftover = rd.end - rd.start
	}
	if len(cmds) > 0 {
		return cmds, nil, nil
	}
	if rd.rd == nil {
		return nil, nil, errIncompleteCommand
	}
	if rd.end == len(rd.buf) {
		// at the end of the buffer.
		if rd.start == rd.end {
			// rewind the to the beginning
			rd.start, rd.end = 0, 0
		} else {
			// must grow the buffer
			newbuf := make([]byte, len(rd.buf)*2)
			copy(newbuf, rd.buf)
			rd.buf = newbuf
		}
	}
	n, err := rd.rd.Read(rd.buf[rd.end:])
	if err != nil {
		return nil, nil, err
	}
	rd.end += n
	return rd.readCommands(leftover, maxBulk)
}

// ReadCommands reads the next pipeline commands.
func (rd *Reader) ReadCommands() ([]Command, error) {
	for {
		if len(rd.cmds) > 0 {
			cmds := rd.cmds
			rd.cmds = nil
			return cmds, nil
		}
		cmds, _, err := rd.readCommands(nil, 0)
		if err != nil {
			return []Command{}, err
		}
		rd.cmds = cmds
	}
}

// ReadCommand reads the next command.
func (rd *Reader) ReadCommand() (Command, error) {
	if len(rd.cmds) > 0 {
		cmd := rd.cmds[0]
		rd.cmds = rd.cmds[1:]
		return cmd, nil
	}
	cmds, _, err := rd.readCommands(nil, 0)
	if err != nil {
		return Command{}, err
	}
	rd.cmds = cmds
	return rd.ReadCommand()
}

// Parse parses a raw RESP message and returns a command.
func Parse(raw []byte) (Command, error) {
	rd := Reader{buf: raw, end: len(raw)}
	var leftover int
	cmds, _, err := rd.readCommands(&leftover, 0)
	if err != nil {
		return Command{}, err
	}
	if leftover > 0 {
		return Command{}, errTooMuchData
	}
	return cmds[0], nil

}

// streamChunkSize is the maximum number of bytes buffered while streaming a
// single bulk argument. It bounds memory use regardless of the declared
// length.
const streamChunkSize = 1 << 16

// maxBulkLine bounds the length of a "$<len>\r\n" header line so that a
// malicious or corrupted length line cannot grow the buffer without limit.
const maxBulkLine = 128

// parseBulkSize parses the decimal payload length from a bulk header line
// (the bytes between '$' and '\r'). Negative values are invalid, and values
// that do not fit in int64 are reported as a protocol error.
func parseBulkSize(b []byte) (int64, error) {
	n, err := strconv.ParseInt(unsafe.String(unsafe.SliceData(b), len(b)), 10, 64)
	if err != nil || n < 0 {
		return 0, errInvalidBulkLength
	}
	return n, nil
}

// bulkHeader is a parsed "$<size>\r\n" line.
type bulkHeader struct {
	size int64
}

// readBulkHeader reads and parses the next bulk argument header.
func (rd *Reader) readBulkHeader() (bulkHeader, error) {
	line, err := rd.readLine()
	if err != nil {
		return bulkHeader{}, err
	}
	if len(line) == 0 || line[0] != '$' {
		got := ""
		if len(line) > 0 {
			got = string(line[0])
		}
		return bulkHeader{}, &errProtocol{"expected '$', got '" + got + "'"}
	}
	size, err := parseBulkSize(line[1:])
	if err != nil {
		return bulkHeader{}, err
	}
	return bulkHeader{size: size}, nil
}

// readLine reads the next CR/LF terminated line (without the terminator),
// growing the internal buffer by at most maxBulkLine bytes while doing so.
func (rd *Reader) readLine() ([]byte, error) {
	for {
		b := rd.buf[rd.start:rd.end]
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			if i == 0 || b[i-1] != '\r' {
				return nil, errInvalidBulkLength
			}
			line := b[:i-1]
			rd.start += i + 1
			return line, nil
		}
		if rd.end-rd.start >= maxBulkLine {
			return nil, errBulkLineTooLong
		}
		if err := rd.fill(streamChunkSize); err != nil {
			return nil, err
		}
	}
}

// fill makes at least one more byte available to read, compacting and
// growing the buffer as needed, never beyond max bytes of capacity.
func (rd *Reader) fill(max int) error {
	if rd.rd == nil {
		return io.EOF
	}
	if rd.start > 0 {
		copy(rd.buf, rd.buf[rd.start:rd.end])
		rd.end -= rd.start
		rd.start = 0
	}
	if rd.end == len(rd.buf) {
		n := len(rd.buf) * 2
		if n > max {
			n = max
		}
		if n <= len(rd.buf) {
			n = len(rd.buf) + 1
			if n > max {
				return errBulkLineTooLong
			}
		}
		newbuf := make([]byte, n)
		copy(newbuf, rd.buf[:rd.end])
		rd.buf = newbuf
	}
	n, err := rd.rd.Read(rd.buf[rd.end:])
	rd.end += n
	if n > 0 {
		return nil
	}
	if err != nil {
		return err
	}
	return io.EOF
}

// readStream returns up to n bytes of the active bulk body without reading
// past it. The returned slice aliases the internal buffer; bytes are
// consumed from the stream.
func (rd *Reader) readStream(n int64) ([]byte, error) {
	if n <= 0 {
		return nil, nil
	}
	b := rd.buf[rd.start:rd.end]
	if len(b) == 0 {
		if err := rd.fill(streamChunkSize); err != nil {
			return nil, err
		}
		b = rd.buf[rd.start:rd.end]
	}
	if int64(len(b)) > n {
		b = b[:n]
	}
	rd.start += len(b)
	return b, nil
}

// readFull reads exactly len(p) bytes of the active bulk body.
func (rd *Reader) readFull(p []byte) (int, error) {
	off := 0
	for off < len(p) {
		chunk, err := rd.readStream(int64(len(p) - off))
		if len(chunk) > 0 {
			off += copy(p[off:], chunk)
		}
		if err != nil {
			return off, err
		}
		if len(chunk) == 0 {
			return off, io.EOF
		}
	}
	return off, nil
}

// discard drops exactly n bytes of the active bulk body using bounded
// memory. It never reads beyond the declared boundary.
func (rd *Reader) discard(n int64) error {
	for n > 0 {
		chunk, err := rd.readStream(n)
		if len(chunk) > 0 {
			n -= int64(len(chunk))
		}
		if err != nil {
			return err
		}
		if len(chunk) == 0 {
			return io.EOF
		}
	}
	return nil
}

// discardBulk drops a full bulk argument: its size-byte payload plus the
// trailing CRLF. The terminator is verified to be CRLF.
func (rd *Reader) discardBulk(size int64) error {
	if err := rd.discard(size); err != nil {
		return err
	}
	return rd.expectCRLF()
}

// expectCRLF consumes and verifies a two-byte CRLF sequence.
func (rd *Reader) expectCRLF() error {
	var crlf [2]byte
	if _, err := rd.readFull(crlf[:]); err != nil {
		return err
	}
	if crlf[0] != '\r' || crlf[1] != '\n' {
		return errInvalidBulkLength
	}
	return nil
}

// A Handler responds to an RESP request.
type Handler interface {
	ServeRESP(conn Conn, cmd Command)
}

// The HandlerFunc type is an adapter to allow the use of
// ordinary functions as RESP handlers. If f is a function
// with the appropriate signature, HandlerFunc(f) is a
// Handler that calls f.
type HandlerFunc func(conn Conn, cmd Command)

// ServeRESP calls f(w, r)
func (f HandlerFunc) ServeRESP(conn Conn, cmd Command) {
	f(conn, cmd)
}

// ServeMux is an RESP command multiplexer.
type ServeMux struct {
	handlers map[string]Handler
}

// NewServeMux allocates and returns a new ServeMux.
func NewServeMux() *ServeMux {
	return &ServeMux{
		handlers: make(map[string]Handler),
	}
}

// HandleFunc registers the handler function for the given command.
func (m *ServeMux) HandleFunc(command string, handler func(conn Conn, cmd Command)) {
	if handler == nil {
		panic("redcon: nil handler")
	}
	m.Handle(command, HandlerFunc(handler))
}

// Handle registers the handler for the given command.
// If a handler already exists for command, Handle panics.
func (m *ServeMux) Handle(command string, handler Handler) {
	if command == "" {
		panic("redcon: invalid command")
	}
	if handler == nil {
		panic("redcon: nil handler")
	}
	if _, exist := m.handlers[command]; exist {
		panic("redcon: multiple registrations for " + command)
	}

	m.handlers[command] = handler
}

// ServeRESP dispatches the command to the handler.
func (m *ServeMux) ServeRESP(conn Conn, cmd Command) {
	command := strings.ToLower(string(cmd.Args[0]))

	if handler, ok := m.handlers[command]; ok {
		handler.ServeRESP(conn, cmd)
	} else {
		conn.WriteError("ERR unknown command '" + command + "'")
	}
}

// PubSub is a Redis compatible pub/sub server
type PubSub struct {
	mu     sync.RWMutex
	nextid uint64
	initd  bool
	chans  *btree.BTree
	conns  map[Conn]*pubSubConn
}

// Subscribe a connection to PubSub
func (ps *PubSub) Subscribe(conn Conn, channel string) {
	ps.subscribe(conn, false, channel)
}

// Psubscribe a connection to PubSub
func (ps *PubSub) Psubscribe(conn Conn, channel string) {
	ps.subscribe(conn, true, channel)
}

// Publish a message to subscribers
func (ps *PubSub) Publish(channel, message string) int {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	if !ps.initd {
		return 0
	}
	var sent int
	// write messages to all clients that are subscribed on the channel
	pivot := &pubSubEntry{pattern: false, channel: channel}
	ps.chans.Ascend(pivot, func(item interface{}) bool {
		entry := item.(*pubSubEntry)
		if entry.channel != pivot.channel || entry.pattern != pivot.pattern {
			return false
		}
		entry.sconn.writeMessage(entry.pattern, "", channel, message)
		sent++
		return true
	})

	// match on and write all psubscribe clients
	pivot = &pubSubEntry{pattern: true}
	ps.chans.Ascend(pivot, func(item interface{}) bool {
		entry := item.(*pubSubEntry)
		if match.Match(channel, entry.channel) {
			entry.sconn.writeMessage(entry.pattern, entry.channel, channel,
				message)
		}
		sent++
		return true
	})

	return sent
}

type pubSubConn struct {
	id      uint64
	mu      sync.Mutex
	conn    Conn
	dconn   DetachedConn
	entries map[*pubSubEntry]bool
}

type pubSubEntry struct {
	pattern bool
	sconn   *pubSubConn
	channel string
}

func (sconn *pubSubConn) writeMessage(pat bool, pchan, channel, msg string) {
	sconn.mu.Lock()
	defer sconn.mu.Unlock()
	if pat {
		sconn.dconn.WriteArray(4)
		sconn.dconn.WriteBulkString("pmessage")
		sconn.dconn.WriteBulkString(pchan)
		sconn.dconn.WriteBulkString(channel)
		sconn.dconn.WriteBulkString(msg)
	} else {
		sconn.dconn.WriteArray(3)
		sconn.dconn.WriteBulkString("message")
		sconn.dconn.WriteBulkString(channel)
		sconn.dconn.WriteBulkString(msg)
	}
	sconn.dconn.Flush()
}

// bgrunner runs in the background and reads incoming commands from the
// detached client.
func (sconn *pubSubConn) bgrunner(ps *PubSub) {
	defer func() {
		// client connection has ended, disconnect from the PubSub instances
		// and close the network connection.
		ps.mu.Lock()
		defer ps.mu.Unlock()
		for entry := range sconn.entries {
			ps.chans.Delete(entry)
		}
		delete(ps.conns, sconn.conn)
		sconn.mu.Lock()
		defer sconn.mu.Unlock()
		sconn.dconn.Close()
	}()
	for {
		cmd, err := sconn.dconn.ReadCommand()
		if err != nil {
			return
		}
		if len(cmd.Args) == 0 {
			continue
		}
		switch strings.ToLower(string(cmd.Args[0])) {
		case "psubscribe", "subscribe":
			if len(cmd.Args) < 2 {
				func() {
					sconn.mu.Lock()
					defer sconn.mu.Unlock()
					sconn.dconn.WriteError(fmt.Sprintf("ERR wrong number of "+
						"arguments for '%s'", cmd.Args[0]))
					sconn.dconn.Flush()
				}()
				continue
			}
			command := strings.ToLower(string(cmd.Args[0]))
			for i := 1; i < len(cmd.Args); i++ {
				if command == "psubscribe" {
					ps.Psubscribe(sconn.conn, string(cmd.Args[i]))
				} else {
					ps.Subscribe(sconn.conn, string(cmd.Args[i]))
				}
			}
		case "unsubscribe", "punsubscribe":
			pattern := strings.ToLower(string(cmd.Args[0])) == "punsubscribe"
			if len(cmd.Args) == 1 {
				ps.unsubscribe(sconn.conn, pattern, true, "")
			} else {
				for i := 1; i < len(cmd.Args); i++ {
					channel := string(cmd.Args[i])
					ps.unsubscribe(sconn.conn, pattern, false, channel)
				}
			}
		case "quit":
			func() {
				sconn.mu.Lock()
				defer sconn.mu.Unlock()
				sconn.dconn.WriteString("OK")
				sconn.dconn.Flush()
				sconn.dconn.Close()
			}()
			return
		case "ping":
			var msg string
			switch len(cmd.Args) {
			case 1:
			case 2:
				msg = string(cmd.Args[1])
			default:
				func() {
					sconn.mu.Lock()
					defer sconn.mu.Unlock()
					sconn.dconn.WriteError(fmt.Sprintf("ERR wrong number of "+
						"arguments for '%s'", cmd.Args[0]))
					sconn.dconn.Flush()
				}()
				continue
			}
			func() {
				sconn.mu.Lock()
				defer sconn.mu.Unlock()
				sconn.dconn.WriteArray(2)
				sconn.dconn.WriteBulkString("pong")
				sconn.dconn.WriteBulkString(msg)
				sconn.dconn.Flush()
			}()
		default:
			func() {
				sconn.mu.Lock()
				defer sconn.mu.Unlock()
				sconn.dconn.WriteError(fmt.Sprintf("ERR Can't execute '%s': "+
					"only (P)SUBSCRIBE / (P)UNSUBSCRIBE / PING / QUIT are "+
					"allowed in this context", cmd.Args[0]))
				sconn.dconn.Flush()
			}()
		}
	}
}

// byEntry is a "less" function that sorts the entries in a btree. The tree
// is sorted be (pattern, channel, conn.id). All pattern=true entries are at
// the end (right) of the tree.
func byEntry(a, b interface{}) bool {
	aa := a.(*pubSubEntry)
	bb := b.(*pubSubEntry)
	if !aa.pattern && bb.pattern {
		return true
	}
	if aa.pattern && !bb.pattern {
		return false
	}
	if aa.channel < bb.channel {
		return true
	}
	if aa.channel > bb.channel {
		return false
	}
	var aid uint64
	var bid uint64
	if aa.sconn != nil {
		aid = aa.sconn.id
	}
	if bb.sconn != nil {
		bid = bb.sconn.id
	}
	return aid < bid
}

func (ps *PubSub) subscribe(conn Conn, pattern bool, channel string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	// initialize the PubSub instance
	if !ps.initd {
		ps.conns = make(map[Conn]*pubSubConn)
		ps.chans = btree.New(byEntry)
		ps.initd = true
	}

	// fetch the pubSubConn
	sconn, ok := ps.conns[conn]
	if !ok {
		// initialize a new pubSubConn, which runs on a detached connection,
		// and attach it to the PubSub channels/conn btree
		ps.nextid++
		dconn := conn.Detach()
		sconn = &pubSubConn{
			id:      ps.nextid,
			conn:    conn,
			dconn:   dconn,
			entries: make(map[*pubSubEntry]bool),
		}
		ps.conns[conn] = sconn
	}
	sconn.mu.Lock()
	defer sconn.mu.Unlock()

	// add an entry to the pubsub btree
	entry := &pubSubEntry{
		pattern: pattern,
		channel: channel,
		sconn:   sconn,
	}
	ps.chans.Set(entry)
	sconn.entries[entry] = true

	// send a message to the client
	sconn.dconn.WriteArray(3)
	if pattern {
		sconn.dconn.WriteBulkString("psubscribe")
	} else {
		sconn.dconn.WriteBulkString("subscribe")
	}
	sconn.dconn.WriteBulkString(channel)
	var count int
	for entry := range sconn.entries {
		if entry.pattern == pattern {
			count++
		}
	}
	sconn.dconn.WriteInt(count)
	sconn.dconn.Flush()

	// start the background client operation
	if !ok {
		go sconn.bgrunner(ps)
	}
}

func (ps *PubSub) unsubscribe(conn Conn, pattern, all bool, channel string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	// fetch the pubSubConn. This must exist
	sconn := ps.conns[conn]
	sconn.mu.Lock()
	defer sconn.mu.Unlock()

	removeEntry := func(entry *pubSubEntry) {
		if entry != nil {
			ps.chans.Delete(entry)
			delete(sconn.entries, entry)
		}
		sconn.dconn.WriteArray(3)
		if pattern {
			sconn.dconn.WriteBulkString("punsubscribe")
		} else {
			sconn.dconn.WriteBulkString("unsubscribe")
		}
		if entry != nil {
			sconn.dconn.WriteBulkString(entry.channel)
		} else {
			sconn.dconn.WriteNull()
		}
		var count int
		for entry := range sconn.entries {
			if entry.pattern == pattern {
				count++
			}
		}
		sconn.dconn.WriteInt(count)
	}
	if all {
		// unsubscribe from all (p)subscribe entries
		var entries []*pubSubEntry
		for entry := range sconn.entries {
			if entry.pattern == pattern {
				entries = append(entries, entry)
			}
		}
		if len(entries) == 0 {
			removeEntry(nil)
		} else {
			for _, entry := range entries {
				removeEntry(entry)
			}
		}
	} else {
		// unsubscribe single channel from (p)subscribe.
		for entry := range sconn.entries {
			if entry.pattern == pattern && entry.channel == channel {
				removeEntry(entry)
				break
			}
		}
	}
	sconn.dconn.Flush()
}

// SetIdleClose will automatically close idle connections after the specified
// duration. Use zero to disable this feature.
func (s *Server) SetIdleClose(dur time.Duration) {
	s.mu.Lock()
	s.idleClose = dur
	s.mu.Unlock()
}

// SetStreamHandler opts in to streaming command handling. Commands that
// contain a bulk argument with a declared length of at least maxBulk bytes
// are delivered to h instead of being fully buffered and passed to the
// regular handler. A maxBulk value <= 0 uses DefaultMaxStreamBulk.
//
// The regular handler continues to handle all other commands unchanged,
// including commands whose arguments are all smaller than maxBulk.
func (s *Server) SetStreamHandler(h StreamHandler, maxBulk int64) {
	if maxBulk <= 0 {
		maxBulk = DefaultMaxStreamBulk
	}
	s.mu.Lock()
	s.streamHandler = h
	s.maxBulk = maxBulk
	s.mu.Unlock()
}
