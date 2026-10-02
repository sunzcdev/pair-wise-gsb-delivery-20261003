// Package redcon implements a Redis compatible server framework
package redcon

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
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
	errStreamClosed           = errors.New("stream command closed")
	errStreamNotActive        = errors.New("no active stream argument")
	errStreamNotConsumed      = errors.New("stream argument was not consumed")
)

const maxBufferCap = 262144

const (
	defaultStreamBulkCutoff   = 64 * 1024
	defaultMaxBufferedCommand = 64 * 1024 * 1024
	defaultMaxBufferedLine    = 64 * 1024
)

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

// StreamHandler handles commands whose large bulk arguments can be read
// without first being copied into Command.Args.
type StreamHandler func(conn Conn, cmd *StreamCommand)

// StreamCommand is a command whose current bulk argument may be streamed.
// Args contains arguments before the active stream, with a nil placeholder
// for that argument. Arguments after it can be read with RemainingArgs.
type StreamCommand struct {
	rd       *streamRead
	Args     [][]byte
	ArgIndex int
	ArgSize  int64
	argc     int
	closed   bool
	err      error
	eof      bool
}

// Read reads data from the active bulk argument. io.EOF is returned after the
// declared number of bytes has been read. The terminating CRLF is consumed
// before the command boundary is released.
func (cmd *StreamCommand) Read(p []byte) (int, error) {
	if cmd.err != nil {
		return 0, cmd.err
	}
	if cmd.closed {
		return 0, errStreamClosed
	}
	if cmd.rd == nil || cmd.rd.cmd != cmd || !cmd.rd.active {
		if cmd.eof {
			return 0, io.EOF
		}
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	if int64(len(p)) > cmd.rd.remaining {
		p = p[:cmd.rd.remaining]
	}
	n, err := cmd.rd.read(p)
	if n > 0 {
		cmd.rd.remaining -= int64(n)
		if cmd.rd.remaining == 0 {
			if crlfErr := cmd.rd.readCRLF(); crlfErr != nil {
				cmd.err = crlfErr
				cmd.rd.err = crlfErr
				return n, crlfErr
			}
			cmd.rd.active = false
			cmd.eof = true
			return n, io.EOF
		}
	}
	if err != nil {
		cmd.err = err
		cmd.rd.err = err
	}
	return n, err
}

// NextLargeArg moves to the next bulk argument at or after the current
// position that is larger than the server's streaming cutoff. Smaller
// arguments are appended to Args.
func (cmd *StreamCommand) NextLargeArg() (bool, error) {
	if cmd.rd == nil || cmd.rd.cmd != cmd {
		return false, nil
	}
	return cmd.nextLargeArg(cmd.rd.streamBulkCutoff, true)
}

func (cmd *StreamCommand) nextLargeArg(limit int64, allowLarge bool) (bool, error) {
	if cmd.err != nil {
		return false, cmd.err
	}
	if cmd.closed {
		return false, errStreamClosed
	}
	if cmd.rd == nil || cmd.rd.cmd != cmd {
		return false, nil
	}
	if cmd.rd.active && cmd.rd.remaining > 0 {
		return true, nil
	}
	for cmd.rd.argIndex < cmd.argc {
		size, err := cmd.rd.readBulkHeader()
		if err != nil {
			cmd.err = err
			cmd.rd.err = err
			return false, err
		}
		cmd.rd.argIndex++
		if size >= limit {
			if !allowLarge {
				err := errTooMuchData
				cmd.err = err
				cmd.rd.err = err
				cmd.rd.active = true
				cmd.rd.remaining = size
				cmd.Args = append(cmd.Args, nil)
				return false, err
			}
			cmd.Args = append(cmd.Args, nil)
			cmd.ArgIndex = cmd.rd.argIndex - 1
			cmd.ArgSize = size
			cmd.rd.active = true
			cmd.rd.remaining = size
			return true, nil
		}
		if cmd.rd.buffered+int(size) > cmd.rd.maxBufferedCommand {
			err := errTooMuchData
			cmd.err = err
			cmd.rd.err = err
			return false, err
		}
		arg, err := cmd.rd.readBulk(size)
		if err != nil {
			cmd.err = err
			cmd.rd.err = err
			return false, err
		}
		cmd.rd.buffered += int(size)
		cmd.Args = append(cmd.Args, arg)
	}
	cmd.rd.cmd = nil
	return false, nil
}

// RemainingArgs reads all arguments after the active stream argument. Any
// later large argument is rejected to keep the result safely buffered.
func (cmd *StreamCommand) RemainingArgs() ([][]byte, error) {
	if cmd.err != nil {
		return nil, cmd.err
	}
	if cmd.closed {
		return nil, errStreamClosed
	}
	if cmd.rd == nil || cmd.rd.cmd != cmd {
		return cmd.Args, nil
	}
	if cmd.rd.remaining > 0 {
		return nil, errStreamNotConsumed
	}
	start := cmd.ArgIndex + 1
	_, err := cmd.nextLargeArg(cmd.rd.streamBulkCutoff, false)
	if err != nil {
		return nil, err
	}
	return cmd.Args[start:], nil
}

// Skip discards the active bulk argument and its terminating CRLF.
func (cmd *StreamCommand) Skip() error {
	if cmd.err != nil {
		return cmd.err
	}
	if cmd.closed {
		return errStreamClosed
	}
	if cmd.rd == nil || cmd.rd.cmd != cmd {
		return errStreamNotActive
	}
	if !cmd.rd.active {
		return nil
	}
	if err := cmd.rd.discardN(cmd.rd.remaining); err != nil {
		cmd.err = err
		cmd.rd.err = err
		return err
	}
	cmd.rd.remaining = 0
	if err := cmd.rd.readCRLF(); err != nil {
		cmd.err = err
		cmd.rd.err = err
		return err
	}
	cmd.rd.active = false
	if cmd.rd.argIndex >= cmd.argc {
		cmd.rd.cmd = nil
	}
	return nil
}

// Close discards the rest of the current command, including the active bulk
// and any later arguments, using bounded memory. The network connection is
// not closed; call Conn.Close when continuation is not possible.
func (cmd *StreamCommand) Close() error {
	if cmd.closed {
		return errStreamClosed
	}
	cmd.closed = true
	if cmd.rd != nil && cmd.rd.err != nil {
		return cmd.rd.err
	}
	var err error
	if cmd.rd != nil {
		err = cmd.rd.discardCommand(cmd)
	}
	cmd.err = err
	if cmd.rd != nil {
		cmd.rd.err = err
	}
	return err
}

// NewServer returns a new Redcon server configured on "tcp" network net.
func NewServer(addr string,
	handler func(conn Conn, cmd Command),
	accept func(conn Conn) bool,
	closed func(conn Conn, err error),
) *Server {
	return NewServerNetwork("tcp", addr, handler, accept, closed)
}

// NewStreamServer returns a server with opt-in streaming bulk support.
func NewStreamServer(addr string,
	handler StreamHandler,
	accept func(conn Conn) bool,
	closed func(conn Conn, err error),
) *Server {
	return NewStreamServerNetwork("tcp", addr, handler, accept, closed)
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

// NewStreamServerNetwork returns a streaming Redcon server bound to net.
func NewStreamServerNetwork(
	network, laddr string,
	handler StreamHandler,
	accept func(conn Conn) bool,
	closed func(conn Conn, err error),
) *Server {
	if handler == nil {
		panic("handler is nil")
	}
	s := newServer()
	s.net = network
	s.laddr = laddr
	s.streamHandler = handler
	s.accept = accept
	s.closed = closed
	return s
}

// ListenAndServeStream creates and serves a streaming TCP server.
func ListenAndServeStream(addr string,
	handler StreamHandler,
	accept func(conn Conn) bool,
	closed func(conn Conn, err error),
) error {
	return NewStreamServer(addr, handler, accept, closed).ListenAndServe()
}

// ListenAndServeStreamNetwork creates and serves a streaming server.
func ListenAndServeStreamNetwork(
	network, laddr string,
	handler StreamHandler,
	accept func(conn Conn) bool,
	closed func(conn Conn, err error),
) error {
	return NewStreamServerNetwork(network, laddr, handler, accept, closed).
		ListenAndServe()
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
		net:                net,
		laddr:              laddr,
		handler:            handler,
		accept:             accept,
		closed:             closed,
		conns:              make(map[*conn]bool),
		streamBulkCutoff:   defaultStreamBulkCutoff,
		maxBufferedCommand: defaultMaxBufferedCommand,
		maxBufferedLine:    defaultMaxBufferedLine,
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
		conns:              make(map[*conn]bool),
		streamBulkCutoff:   defaultStreamBulkCutoff,
		maxBufferedCommand: defaultMaxBufferedCommand,
		maxBufferedLine:    defaultMaxBufferedLine,
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
			rd:   newReader(lnconn, s.streamHandler != nil),
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
	if s.streamHandler != nil {
		handleStream(s, c)
		return
	}
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
			cmds, err := c.rd.readCommands(nil)
			if err != nil {
				if err, ok := err.(*errProtocol); ok {
					// All protocol errors should attempt a response to
					// the client. Ignore write errors.
					c.wr.WriteError("ERR " + err.Error())
					c.wr.Flush()
				}
				return err
			}
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
	}()
}

func handleStream(s *Server, c *conn) {
	var err error
	defer func() {
		if err != errDetached {
			c.conn.Close()
		}
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		if s.closed != nil {
			if err == io.EOF {
				err = nil
			}
			s.closed(c, err)
		}
	}()

	err = func() error {
		for {
			if c.idleClose != 0 {
				c.conn.SetReadDeadline(time.Now().Add(c.idleClose))
			}
			cmd, complete, err := c.rd.readStreamCommand(
				s.streamBulkCutoff, s.maxBufferedCommand, s.maxBufferedLine)
			if err != nil {
				if err, ok := err.(*errProtocol); ok {
					c.wr.WriteError("ERR " + err.Error())
					c.wr.Flush()
				}
				return err
			}
			s.streamHandler(c, cmd)
			streamErr := error(nil)
			if !complete && c.rd.stream != nil {
				streamErr = c.rd.stream.err
			}
			if c.detached {
				if !complete && streamErr == nil {
					if err := c.rd.drainStreamCommand(cmd); err != nil {
						return err
					}
				}
				return errDetached
			}
			if c.closed {
				return nil
			}
			if complete {
				if err := c.wr.Flush(); err != nil {
					return err
				}
				continue
			}
			if streamErr != nil {
				if err, ok := streamErr.(*errProtocol); ok {
					c.wr.WriteError("ERR " + err.Error())
					c.wr.Flush()
				}
				return streamErr
			}

			drained := make(chan error, 1)
			go func() {
				drained <- c.rd.drainStreamCommand(cmd)
			}()
			flushErr := c.wr.Flush()
			drainErr := <-drained
			if drainErr != nil {
				return drainErr
			}
			if flushErr != nil {
				return flushErr
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
	if c.rd.stream == nil || c.rd.stream.cmd == nil {
		c.wr.Flush()
	}
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

// Server defines a server for clients for managing client connections.
type Server struct {
	mu                 sync.Mutex
	net                string
	laddr              string
	handler            func(conn Conn, cmd Command)
	streamHandler      StreamHandler
	accept             func(conn Conn) bool
	closed             func(conn Conn, err error)
	conns              map[*conn]bool
	ln                 net.Listener
	done               bool
	idleClose          time.Duration
	streamBulkCutoff   int64
	maxBufferedCommand int
	maxBufferedLine    int

	// AcceptError is an optional function used to handle Accept errors.
	AcceptError func(err error)
}

// SetStreamLimits changes the maximum bulk argument buffered in Args and the
// maximum amount of header/small-argument data buffered for one command.
// Values below or equal to zero retain the corresponding default.
func (s *Server) SetStreamLimits(bulkCutoff int64, maxBufferedCommand int) {
	if bulkCutoff > 0 {
		s.streamBulkCutoff = bulkCutoff
	}
	if maxBufferedCommand > 0 {
		s.maxBufferedCommand = maxBufferedCommand
	}
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
	rd     *bufio.Reader
	source io.Reader
	buf    []byte
	start  int
	end    int
	cmds   []Command
	stream *streamRead
}

type streamRead struct {
	reader             *Reader
	source             io.Reader
	pending            [][]byte
	cmd                *StreamCommand
	argIndex           int
	active             bool
	remaining          int64
	buffered           int
	err                error
	streamBulkCutoff   int64
	maxBufferedCommand int
	maxBufferedLine    int
}

// NewReader returns a command reader which will read RESP or telnet commands.
func NewReader(rd io.Reader) *Reader {
	return newReader(rd, false)
}

func newReader(source io.Reader, streaming bool) *Reader {
	r := &Reader{
		source: source,
		buf:    make([]byte, 4096),
	}
	if !streaming {
		r.rd = bufio.NewReader(source)
	}
	return r
}

func (sr *streamRead) readByte() (byte, error) {
	if sr.err != nil {
		return 0, sr.err
	}
	var b [1]byte
	if err := sr.readRawFull(b[:]); err != nil {
		return 0, err
	}
	return b[0], nil
}

func (sr *streamRead) readRaw(p []byte) (int, error) {
	total := 0
	for len(p) > 0 && len(sr.pending) > 0 {
		chunk := sr.pending[0]
		n := copy(p, chunk)
		total += n
		p = p[n:]
		if n == len(chunk) {
			sr.pending = sr.pending[1:]
		} else {
			sr.pending[0] = chunk[n:]
		}
	}
	if total > 0 {
		return total, nil
	}
	tmp := make([]byte, 32*1024)
	n, err := sr.source.Read(tmp)
	if n == 0 {
		return 0, err
	}
	sr.pending = append(sr.pending, tmp[:n])
	n = 0
	for len(p) > 0 && len(sr.pending) > 0 {
		chunk := sr.pending[0]
		m := copy(p, chunk)
		n += m
		p = p[m:]
		if m == len(chunk) {
			sr.pending = sr.pending[1:]
		} else {
			sr.pending[0] = chunk[m:]
		}
	}
	return n, err
}

func (sr *streamRead) readRawFull(p []byte) error {
	for len(p) > 0 {
		n, err := sr.readRaw(p)
		if n > 0 {
			p = p[n:]
		}
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			sr.err = err
			return err
		}
	}
	return nil
}

func (sr *streamRead) read(p []byte) (int, error) {
	if sr.err != nil {
		return 0, sr.err
	}
	need := len(p)
	if sr.active && int64(need) > sr.remaining {
		need = int(sr.remaining)
	}
	p = p[:need]
	if len(sr.pending) > 0 {
		original := sr.pending[0]
		chunk := original
		if len(chunk) > need {
			chunk = chunk[:need]
		}
		n := copy(p, chunk)
		if n == len(original) {
			sr.pending = sr.pending[1:]
		} else {
			sr.pending[0] = original[n:]
		}
		return n, nil
	}
	tmp := make([]byte, len(p))
	n, err := sr.source.Read(tmp)
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		sr.err = err
		return n, err
	}
	take := n
	if sr.active && int64(take) > sr.remaining {
		take = int(sr.remaining)
	}
	copy(p, tmp[:take])
	if n > take {
		sr.pending = append([][]byte{tmp[take:n]}, sr.pending...)
	}
	return take, nil
}

func (sr *streamRead) readFull(p []byte) error {
	for len(p) > 0 {
		n, err := sr.read(p)
		if n > 0 {
			p = p[n:]
		}
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
				sr.err = err
			}
			return err
		}
	}
	return nil
}

func (sr *streamRead) readLine(max int) ([]byte, error) {
	var line []byte
	for {
		b, err := sr.readByte()
		if err != nil {
			if err == io.EOF && len(line) > 0 {
				err = io.ErrUnexpectedEOF
				sr.err = err
			}
			return nil, err
		}
		if b == '\n' {
			if len(line) == 0 || line[len(line)-1] != '\r' {
				return nil, &errProtocol{"expected CRLF"}
			}
			return line[:len(line)-1], nil
		}
		if len(line) >= max {
			return nil, errTooMuchData
		}
		line = append(line, b)
	}
}

func (sr *streamRead) readSizeLine(prefix byte, max int) (int64, error) {
	b, err := sr.readByte()
	if err != nil {
		return 0, err
	}
	if b != prefix {
		return 0, &errProtocol{"expected '" + string(prefix) + "', got '" + string(b) + "'"}
	}
	line, err := sr.readLine(max)
	if err != nil {
		return 0, err
	}
	size, err := strconv.ParseInt(string(line), 10, 64)
	if err != nil || size < 0 || size > math.MaxInt64 {
		return 0, errInvalidBulkLength
	}
	return size, nil
}

func (sr *streamRead) readSizeValueLine(max int) (int64, error) {
	line, err := sr.readLine(max)
	if err != nil {
		return 0, err
	}
	size, err := strconv.ParseInt(string(line), 10, 64)
	if err != nil || size < 0 || size > math.MaxInt64 {
		return 0, errInvalidMultiBulkLength
	}
	return size, nil
}

func (sr *streamRead) readCRLF() error {
	b, err := sr.readByte()
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
			sr.err = err
		}
		return err
	}
	if b != '\r' {
		err = errInvalidBulkLength
		sr.err = err
		return err
	}
	b, err = sr.readByte()
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
			sr.err = err
		}
		return err
	}
	if b != '\n' {
		err = errInvalidBulkLength
		sr.err = err
	}
	return err
}

func (sr *streamRead) readBulkHeader() (int64, error) {
	size, err := sr.readSizeLine('$', sr.maxBufferedLine)
	if err == nil && size < 0 {
		return 0, errInvalidBulkLength
	}
	return size, err
}

func (sr *streamRead) readBulk(size int64) ([]byte, error) {
	if size > int64(sr.maxBufferedCommand) || sr.buffered+int(size) > sr.maxBufferedCommand {
		return nil, errTooMuchData
	}
	buf := make([]byte, size)
	if err := sr.readRawFull(buf); err != nil {
		return nil, err
	}
	if err := sr.readCRLF(); err != nil {
		return nil, err
	}
	return buf, nil
}

func (sr *streamRead) discardN(n int64) error {
	if n < 0 {
		return errInvalidBulkLength
	}
	var buf [32 * 1024]byte
	for n > 0 {
		want := int64(len(buf))
		if want > n {
			want = n
		}
		got, err := sr.read(buf[:want])
		n -= int64(got)
		if err != nil {
			if err == io.EOF && n > 0 {
				err = io.ErrUnexpectedEOF
				sr.err = err
			}
			return err
		}
	}
	return nil
}

func (sr *streamRead) discardCommand(cmd *StreamCommand) error {
	if sr.active {
		if err := sr.discardN(sr.remaining); err != nil {
			return err
		}
		sr.active = false
		sr.remaining = 0
		if err := sr.readCRLF(); err != nil {
			return err
		}
	}
	for sr.argIndex < cmd.argc {
		size, err := sr.readBulkHeader()
		if err != nil {
			return err
		}
		sr.argIndex++
		if err := sr.discardN(size); err != nil {
			return err
		}
		if err := sr.readCRLF(); err != nil {
			return err
		}
	}
	sr.cmd = nil
	return nil
}

func (rd *Reader) readStreamCommand(cutoff int64, maxCommand, maxLine int) (*StreamCommand, bool, error) {
	sr := rd.stream
	if sr == nil {
		sr = &streamRead{
			reader:             rd,
			source:             rd.source,
			streamBulkCutoff:   cutoff,
			maxBufferedCommand: maxCommand,
			maxBufferedLine:    maxLine,
		}
		rd.stream = sr
	}
	sr.reset(cutoff, maxCommand, maxLine)
	first, err := sr.readByte()
	if err != nil {
		sr.err = err
		return nil, false, err
	}
	if first != '*' {
		legacy := newReader(sr.legacySource(first), false)
		cmd, err := legacy.ReadCommand()
		if err != nil {
			sr.err = err
			return nil, false, err
		}
		return commandToStream(cmd), true, nil
	}
	argc, err := sr.readSizeValueLine(maxLine)
	if err != nil {
		sr.err = err
		return nil, false, err
	}
	if argc == 0 || argc > math.MaxInt32 {
		sr.err = errInvalidMultiBulkLength
		return nil, false, sr.err
	}
	cmd := &StreamCommand{rd: sr, argc: int(argc)}
	sr.cmd = cmd
	for sr.argIndex < cmd.argc {
		size, err := sr.readBulkHeader()
		if err != nil {
			sr.err = err
			return nil, false, err
		}
		sr.argIndex++
		if size >= cutoff {
			cmd.Args = append(cmd.Args, nil)
			cmd.ArgIndex = sr.argIndex - 1
			cmd.ArgSize = size
			sr.active = true
			sr.remaining = size
			sr.err = nil
			rd.stream = sr
			return cmd, false, nil
		}
		if size > int64(maxCommand) || sr.buffered+int(size) > maxCommand {
			sr.err = errTooMuchData
			return nil, false, errTooMuchData
		}
		arg, err := sr.readBulk(size)
		if err != nil {
			sr.err = err
			return nil, false, err
		}
		sr.buffered += int(size)
		cmd.Args = append(cmd.Args, arg)
	}
	sr.cmd = nil
	return cmd, true, nil
}

func (sr *streamRead) reset(cutoff int64, maxCommand, maxLine int) {
	sr.cmd = nil
	sr.argIndex = 0
	sr.active = false
	sr.remaining = 0
	sr.buffered = 0
	sr.err = nil
	sr.streamBulkCutoff = cutoff
	sr.maxBufferedCommand = maxCommand
	sr.maxBufferedLine = maxLine
}

func (sr *streamRead) legacySource(first byte) io.Reader {
	readers := []io.Reader{bytes.NewReader([]byte{first})}
	for _, chunk := range sr.pending {
		readers = append(readers, bytes.NewReader(chunk))
	}
	readers = append(readers, sr.source)
	return io.MultiReader(readers...)
}

func commandToStream(cmd Command) *StreamCommand {
	return &StreamCommand{Args: cmd.Args, argc: len(cmd.Args)}
}

func (rd *Reader) drainStreamCommand(cmd *StreamCommand) error {
	if rd.stream == nil || cmd.rd != rd.stream {
		return nil
	}
	if cmd.closed || rd.stream.err != nil {
		return rd.stream.err
	}
	return rd.stream.discardCommand(cmd)
}

func parseInt(b []byte) (int, bool) {
	i, err := strconv.Atoi(unsafe.String(unsafe.SliceData(b), len(b)))
	return i, err == nil
}

func (rd *Reader) readCommands(leftover *int) ([]Command, error) {
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
										return nil, errUnbalancedQuotes
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
										return nil, errUnbalancedQuotes
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
							return nil, errUnbalancedQuotes
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
						return nil, errInvalidMultiBulkLength
					}
					count, ok := parseInt(b[1 : i-1])
					if !ok || count <= 0 {
						return nil, errInvalidMultiBulkLength
					}
					marks = marks[:0]
					for j := 0; j < count; j++ {
						// read bulk length
						i++
						if i < len(b) {
							if b[i] != '$' {
								return nil, &errProtocol{"expected '$', got '" +
									string(b[i]) + "'"}
							}
							si := i
							for ; i < len(b); i++ {
								if b[i] == '\n' {
									if b[i-1] != '\r' {
										return nil, errInvalidBulkLength
									}
									size, ok := parseInt(b[si+1 : i-1])
									if !ok || size < 0 {
										return nil, errInvalidBulkLength
									}
									if i+size+2 >= len(b) {
										// not ready
										break outer2
									}
									if b[i+size+2] != '\n' ||
										b[i+size+1] != '\r' {
										return nil, errInvalidBulkLength
									}
									i++
									marks = append(marks, i, i+size)
									i += size + 1
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
		return cmds, nil
	}
	if rd.rd == nil {
		return nil, errIncompleteCommand
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
		return nil, err
	}
	rd.end += n
	return rd.readCommands(leftover)
}

// ReadCommands reads the next pipeline commands.
func (rd *Reader) ReadCommands() ([]Command, error) {
	for {
		if len(rd.cmds) > 0 {
			cmds := rd.cmds
			rd.cmds = nil
			return cmds, nil
		}
		cmds, err := rd.readCommands(nil)
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
	cmds, err := rd.readCommands(nil)
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
	cmds, err := rd.readCommands(&leftover)
	if err != nil {
		return Command{}, err
	}
	if leftover > 0 {
		return Command{}, errTooMuchData
	}
	return cmds[0], nil

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
