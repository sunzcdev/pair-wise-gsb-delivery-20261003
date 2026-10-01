package socks5

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"strconv"

	"github.com/things-go/go-socks5/statute"
)

// gssMaxChunk is the maximum plaintext wrapped into a single
// GSS-API encapsulated message, leaving headroom for token overhead.
const gssMaxChunk = 32 * 1024

// GSSMessageProtector is the per-message protection half of a GSS-API
// security context (GSS_Wrap / GSS_Unwrap).
type GSSMessageProtector interface {
	// Wrap protects a message (GSS_Wrap). conf requests confidentiality
	// in addition to integrity, per the negotiated protection level.
	Wrap(plaintext []byte, conf bool) (token []byte, err error)
	// Unwrap verifies and extracts a protected message (GSS_Unwrap).
	Unwrap(token []byte) (plaintext []byte, err error)
}

// GSSEngine abstracts a GSS-API acceptor (RFC 2743) so the SOCKS5 server
// can perform GSS-API authentication (RFC 1961) without being bound to a
// concrete mechanism implementation such as Kerberos.
//
// An engine carries per-security-context state and is therefore NOT safe
// for concurrent use: a dedicated engine must be created per connection.
type GSSEngine interface {
	GSSMessageProtector
	// AcceptSecContext consumes a context-level token from the client
	// (GSS_Accept_sec_context). It returns the token to send back, if any,
	// and whether the security context is fully established.
	AcceptSecContext(inputToken []byte) (outputToken []byte, complete bool, err error)
}

// GSSAPIAuthenticator is used to handle GSS-API based authentication (RFC 1961).
// The actual mechanism (Kerberos, SPNEGO, ...) is provided by the injected
// GSSEngine factory, keeping this library mechanism-agnostic.
type GSSAPIAuthenticator struct {
	// NewEngine creates a fresh GSSEngine for each connection. Required.
	NewEngine func(userAddr string) (GSSEngine, error)
	// ProtectionLevel is the protection level required by the server,
	// one of statute.GSSProtectionNone, statute.GSSProtectionIntegrity or
	// statute.GSSProtectionConfidentiality. The client negotiates a level
	// not stronger than required (RFC 1961 section 4). When the negotiated
	// level is not GSSProtectionNone, all subsequent traffic on the
	// connection is exchanged as GSS_Wrap protected messages.
	ProtectionLevel byte
}

// GetCode implement interface Authenticator
func (a GSSAPIAuthenticator) GetCode() uint8 { return statute.MethodGSSAPI }

// Authenticate implement interface Authenticator
func (a GSSAPIAuthenticator) Authenticate(reader io.Reader, writer io.Writer, userAddr string) (*AuthContext, error) {
	if a.NewEngine == nil {
		return nil, fmt.Errorf("gss-api: NewEngine is required")
	}
	if a.ProtectionLevel > statute.GSSProtectionConfidentiality {
		return nil, fmt.Errorf("gss-api: invalid protection level %d", a.ProtectionLevel)
	}
	// reply the client to use GSS-API auth
	if _, err := writer.Write([]byte{statute.VersionSocks5, statute.MethodGSSAPI}); err != nil {
		return nil, err
	}
	engine, err := a.NewEngine(userAddr)
	if err != nil {
		return nil, err
	}
	// Exchange context-level tokens until the security context is established.
	for {
		msg, err := statute.ParseGSSMessage(reader)
		if err != nil {
			return nil, err
		}
		if msg.Ver != statute.GSSAuthVersion {
			return nil, fmt.Errorf("gss-api: unsupported auth version: %v", msg.Ver)
		}
		if msg.MTyp != statute.GSSMsgAuthentication {
			return nil, fmt.Errorf("gss-api: unexpected message type: 0x%02x", msg.MTyp)
		}
		out, complete, err := engine.AcceptSecContext(msg.Token)
		if err != nil {
			// Abort the authentication (RFC 1961 section 3).
			_, _ = writer.Write(statute.NewGSSMessage(statute.GSSMsgAbort, nil).Bytes())
			return nil, fmt.Errorf("gss-api: %w", err)
		}
		if len(out) > 0 {
			if _, err := writer.Write(statute.NewGSSMessage(statute.GSSMsgAuthentication, out).Bytes()); err != nil {
				return nil, err
			}
		}
		if complete {
			break
		}
	}
	// Protection-level negotiation (RFC 1961 section 4):
	// the server announces its required level wrapped with conf_flag FALSE.
	token, err := engine.Wrap([]byte{a.ProtectionLevel}, false)
	if err != nil {
		return nil, fmt.Errorf("gss-api: %w", err)
	}
	if _, err := writer.Write(statute.NewGSSMessage(statute.GSSMsgProtectionLevel, token).Bytes()); err != nil {
		return nil, err
	}
	msg, err := statute.ParseGSSMessage(reader)
	if err != nil {
		return nil, err
	}
	if msg.Ver != statute.GSSAuthVersion || msg.MTyp != statute.GSSMsgProtectionLevel {
		return nil, fmt.Errorf("gss-api: unexpected message type: 0x%02x", msg.MTyp)
	}
	level, err := engine.Unwrap(msg.Token)
	if err != nil {
		return nil, fmt.Errorf("gss-api: %w", err)
	}
	if len(level) != 1 || level[0] > a.ProtectionLevel {
		_, _ = writer.Write(statute.NewGSSMessage(statute.GSSMsgAbort, nil).Bytes())
		return nil, fmt.Errorf("gss-api: unacceptable protection level")
	}
	negotiated := level[0]

	ctx := &AuthContext{
		Method: statute.MethodGSSAPI,
		Payload: map[string]string{
			"protection_level": strconv.Itoa(int(negotiated)),
		},
	}
	// Honor the negotiated protection level: encapsulate all subsequent
	// traffic instead of silently falling back to plaintext forwarding.
	if negotiated != statute.GSSProtectionNone {
		conf := negotiated == statute.GSSProtectionConfidentiality
		ctx.WrapConn = func(conn net.Conn) net.Conn {
			return newGSSProtectedConn(conn, reader, engine, conf)
		}
	}
	return ctx, nil
}

// gssProtectedConn encapsulates a connection with GSS-API message
// protection (RFC 1961 section 5) after a successful GSS-API authentication.
type gssProtectedConn struct {
	net.Conn
	reader  io.Reader // reader over Conn, may buffer
	engine  GSSMessageProtector
	conf    bool
	readBuf bytes.Buffer
}

func newGSSProtectedConn(conn net.Conn, reader io.Reader, engine GSSMessageProtector, conf bool) *gssProtectedConn {
	return &gssProtectedConn{
		Conn:   conn,
		reader: reader,
		engine: engine,
		conf:   conf,
	}
}

// Read unwraps encapsulated messages from the underlying connection.
func (c *gssProtectedConn) Read(p []byte) (int, error) {
	for c.readBuf.Len() == 0 {
		msg, err := statute.ParseGSSMessage(c.reader)
		if err != nil {
			return 0, err
		}
		if msg.Ver != statute.GSSAuthVersion || msg.MTyp != statute.GSSMsgEncapsulated {
			return 0, fmt.Errorf("gss-api: unexpected message type: 0x%02x", msg.MTyp)
		}
		plaintext, err := c.engine.Unwrap(msg.Token)
		if err != nil {
			return 0, fmt.Errorf("gss-api: %w", err)
		}
		c.readBuf.Write(plaintext)
	}
	return c.readBuf.Read(p)
}

// Write wraps plaintext into encapsulated messages on the underlying connection.
func (c *gssProtectedConn) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > gssMaxChunk {
			chunk = chunk[:gssMaxChunk]
		}
		token, err := c.engine.Wrap(chunk, c.conf)
		if err != nil {
			return total, fmt.Errorf("gss-api: %w", err)
		}
		if len(token) > statute.GSSMaxTokenSize {
			return total, statute.ErrGSSMessageTooLarge
		}
		if _, err := c.Conn.Write(statute.NewGSSMessage(statute.GSSMsgEncapsulated, token).Bytes()); err != nil {
			return total, err
		}
		total += len(chunk)
		p = p[len(chunk):]
	}
	return total, nil
}
