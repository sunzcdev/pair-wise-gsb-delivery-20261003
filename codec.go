package rpc2

import (
	"bufio"
	"encoding/gob"
	"io"
	"sync"
)

// A Codec implements reading and writing of RPC requests and responses.
// The client calls ReadHeader to read a message header.
// The implementation must populate either Request or Response argument.
// Depending on which argument is populated, ReadRequestBody or
// ReadResponseBody is called right after ReadHeader.
// ReadRequestBody and ReadResponseBody may be called with a nil
// argument to force the body to be read and then discarded.
type Codec interface {
	// ReadHeader must read a message and populate either the request
	// or the response by inspecting the incoming message.
	ReadHeader(*Request, *Response) error

	// ReadRequestBody into args argument of handler function.
	ReadRequestBody(interface{}) error

	// ReadResponseBody into reply argument of handler function.
	ReadResponseBody(interface{}) error

	// WriteRequest must be safe for concurrent use by multiple goroutines.
	WriteRequest(*Request, interface{}) error

	// WriteResponse must be safe for concurrent use by multiple goroutines.
	WriteResponse(*Response, interface{}) error

	// Close is called when client/server finished with the connection.
	Close() error
}

// Request is a header written before every RPC call.
type Request struct {
	Seq          uint64 // sequence number chosen by client
	Method       string
	Capabilities Capabilities // optional protocol extensions advertised by the sender
}

// Response is a header written before every RPC return.
type Response struct {
	Seq          uint64 // echoes that of the request
	Error        string // error, if any.
	Capabilities Capabilities
}

const (
	// HelloMethod is a notification sent once, as soon as an endpoint receives
	// its first frame from a peer that supports protocol extensions. It carries
	// no response. It is only sent to peers that advertised support for
	// extensions, so legacy endpoints are never confused by it. The method
	// name must not be registered as a handler.
	HelloMethod = "rpc2.__hello"

	// CancelMethod is the name of the control request used to cancel an
	// in-flight call. The request body identifies the sequence number of
	// the call to cancel. It is sent as a notification and must not be
	// registered as a handler.
	CancelMethod = "rpc2.__cancel"
)

// Capabilities describes the optional protocol extensions supported by
// an endpoint. It is piggybacked on frame headers so that older endpoints
// simply ignore the fields they do not know about.
type Capabilities struct {
	// Cancel reports that the endpoint can receive CancelMethod notifications.
	Cancel bool
}

// localCapabilities is advertised in every frame header sent by this package.
var localCapabilities = Capabilities{Cancel: true}

// CancelRequest is the body of a CancelMethod notification.
type CancelRequest struct {
	// Seq is the sequence number of the call that should be canceled.
	Seq uint64
}

type gobCodec struct {
	rwc    io.ReadWriteCloser
	dec    *gob.Decoder
	enc    *gob.Encoder
	encBuf *bufio.Writer
	mutex  sync.Mutex
}

type message struct {
	Seq          uint64
	Method       string
	Error        string
	Capabilities Capabilities
}

// NewGobCodec returns a new rpc2.Codec using gob encoding/decoding on conn.
func NewGobCodec(conn io.ReadWriteCloser) Codec {
	buf := bufio.NewWriter(conn)
	return &gobCodec{
		rwc:    conn,
		dec:    gob.NewDecoder(conn),
		enc:    gob.NewEncoder(buf),
		encBuf: buf,
	}
}

func (c *gobCodec) ReadHeader(req *Request, resp *Response) error {
	var msg message
	if err := c.dec.Decode(&msg); err != nil {
		return err
	}

	if msg.Method != "" {
		req.Seq = msg.Seq
		req.Method = msg.Method
		req.Capabilities = msg.Capabilities
	} else {
		resp.Seq = msg.Seq
		resp.Error = msg.Error
		resp.Capabilities = msg.Capabilities
	}
	return nil
}

func (c *gobCodec) ReadRequestBody(body interface{}) error {
	return c.dec.Decode(body)
}

func (c *gobCodec) ReadResponseBody(body interface{}) error {
	return c.dec.Decode(body)
}

func (c *gobCodec) WriteRequest(r *Request, body interface{}) (err error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	msg := message{Seq: r.Seq, Method: r.Method, Capabilities: localCapabilities}
	if err = c.enc.Encode(&msg); err != nil {
		return
	}
	if err = c.enc.Encode(body); err != nil {
		return
	}
	return c.encBuf.Flush()
}

func (c *gobCodec) WriteResponse(r *Response, body interface{}) (err error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	msg := message{Seq: r.Seq, Error: r.Error, Capabilities: localCapabilities}
	if err = c.enc.Encode(&msg); err != nil {
		return
	}
	if err = c.enc.Encode(body); err != nil {
		return
	}
	return c.encBuf.Flush()
}

func (c *gobCodec) Close() error {
	return c.rwc.Close()
}
