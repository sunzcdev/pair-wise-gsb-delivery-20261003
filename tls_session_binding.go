package ftpserver

import (
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"time"
)

var (
	errDataSessionReuseRequired = errors.New("data connection must resume the control connection TLS session")
	errSessionTicketInvalid     = errors.New("invalid TLS session ticket")
	errSessionTicketNotBound    = errors.New("TLS session ticket is not bound to this control connection")
)

// sessionTicketEnvelopeVersion is the version byte of the session ticket
// envelope we emit. Tickets coming from another deployment or produced with a
// different ticket key simply fail to authenticate and are ignored or
// rejected depending on the context.
const sessionTicketEnvelopeVersion = 1

// sessionBindingTagLen is the size in bytes of the random tag that binds a
// session ticket to a single control connection.
const sessionBindingTagLen = 32

// sessionBindingTagPrefix is used together with SessionState.Extra so our
// entry remains recognizable if another layer also appends to Extra.
var sessionBindingTagPrefix = []byte("FTPS-BIND-")

// tlsSessionBinding carries everything needed to mint TLS session tickets
// bound to one control connection and to verify that an incoming data
// connection resumed exactly that session.
type tlsSessionBinding struct {
	ticketAEAD cipher.AEAD // authenticates the ticket contents, server-wide
	tag        []byte      // random, unique per control connection
}

func newTLSSessionBinding(ticketAEAD cipher.AEAD) (*tlsSessionBinding, error) {
	tag := make([]byte, sessionBindingTagLen)
	if _, err := io.ReadFull(rand.Reader, tag); err != nil {
		return nil, err
	}

	return &tlsSessionBinding{
		ticketAEAD: ticketAEAD,
		tag:        tag,
	}, nil
}

// buildSessionBoundTLSConfig clones the driver-provided TLS configuration and
// installs WrapSession/UnwrapSession hooks that bind every emitted ticket to
// the control connection identified by this tag.
//
// strictUnwrap controls how a ticket that does not belong to this binding is
// handled:
//   - false is used on the control connection itself: a foreign or stale
//     ticket is ignored (nil, nil), so a client presenting an old ticket
//     simply performs a full handshake and gets a fresh, correctly bound
//     ticket.
//   - true is used on data connections: a foreign ticket terminates the
//     handshake with an error, as does a malformed one. Returning (nil, nil)
//     would let crypto/tls silently fall back to a full handshake, which the
//     policy exists to forbid.
func (binding *tlsSessionBinding) buildSessionBoundTLSConfig(base *tls.Config, strictUnwrap bool) *tls.Config {
	cfg := base.Clone()
	// Tickets are the only resumption mechanism we can attach a binding to,
	// so they must be enabled regardless of the driver configuration.
	cfg.SessionTicketsDisabled = false
	cfg.WrapSession = binding.wrapSession
	cfg.UnwrapSession = func(identity []byte, state tls.ConnectionState) (*tls.SessionState, error) {
		return binding.unwrapSession(identity, state, strictUnwrap)
	}

	return cfg
}

func (binding *tlsSessionBinding) wrapSession(
	_ tls.ConnectionState,
	session *tls.SessionState,
) ([]byte, error) {
	session.Extra = replaceBindingTagExtra(session.Extra, binding.tag)

	stateBytes, err := session.Bytes()
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, binding.ticketAEAD.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	// Envelope: version byte || random nonce || AEAD ciphertext.
	ciphertext := binding.ticketAEAD.Seal(nil, nonce, stateBytes, nil)
	ticket := make([]byte, 0, 1+len(nonce)+len(ciphertext))
	ticket = append(ticket, sessionTicketEnvelopeVersion)
	ticket = append(ticket, nonce...)
	ticket = append(ticket, ciphertext...)

	return ticket, nil
}

func (binding *tlsSessionBinding) unwrapSession(
	identity []byte,
	_ tls.ConnectionState,
	strict bool,
) (*tls.SessionState, error) {
	stateBytes, tag, err := binding.openTicket(identity)
	if err != nil {
		if strict {
			// Terminate the data connection handshake.
			return nil, err
		}

		// Ignore tickets we cannot read or that were produced by another
		// configuration and perform a full handshake instead.
		return nil, nil //nolint:nilnil
	}

	if subtle.ConstantTimeCompare(tag, binding.tag) != 1 {
		if strict {
			return nil, errSessionTicketNotBound
		}

		return nil, nil //nolint:nilnil
	}

	session, err := tls.ParseSessionState(stateBytes)
	if err != nil {
		if strict {
			return nil, err
		}

		return nil, nil //nolint:nilnil
	}

	return session, nil
}

// openTicket verifies the envelope and returns the serialized SessionState
// together with the binding tag stored in SessionState.Extra.
func (binding *tlsSessionBinding) openTicket(ticket []byte) ([]byte, []byte, error) {
	nonceSize := binding.ticketAEAD.NonceSize()
	if len(ticket) < 1+nonceSize+1 || ticket[0] != sessionTicketEnvelopeVersion {
		return nil, nil, errSessionTicketInvalid
	}

	nonce := ticket[1 : 1+nonceSize]
	ciphertext := ticket[1+nonceSize:]

	stateBytes, err := binding.ticketAEAD.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, nil, errSessionTicketInvalid
	}

	session, err := tls.ParseSessionState(stateBytes)
	if err != nil {
		return nil, nil, errSessionTicketInvalid
	}

	tag := findBindingTagExtra(session.Extra)
	if tag == nil {
		return nil, nil, errSessionTicketNotBound
	}

	return stateBytes, tag, nil
}

func replaceBindingTagExtra(extras [][]byte, tag []byte) [][]byte {
	entry := make([]byte, 0, len(sessionBindingTagPrefix)+4+len(tag))
	entry = append(entry, sessionBindingTagPrefix...)
	entry = binary.BigEndian.AppendUint32(entry, uint32(len(tag)))
	entry = append(entry, tag...)

	result := make([][]byte, 0, len(extras)+1)

	for _, extra := range extras {
		if !isBindingTagExtra(extra) {
			result = append(result, extra)
		}
	}

	result = append(result, entry)

	return result
}

func findBindingTagExtra(extras [][]byte) []byte {
	for _, extra := range extras {
		if !isBindingTagExtra(extra) {
			continue
		}

		prefixLen := len(sessionBindingTagPrefix)
		if len(extra) < prefixLen+4 {
			return nil
		}

		tagLen := binary.BigEndian.Uint32(extra[prefixLen : prefixLen+4])
		start := prefixLen + 4
		if uint32(len(extra)-start) < tagLen {
			return nil
		}

		return extra[start : start+int(tagLen)]
	}

	return nil
}

func isBindingTagExtra(extra []byte) bool {
	return len(extra) >= len(sessionBindingTagPrefix) &&
		string(extra[:len(sessionBindingTagPrefix)]) == string(sessionBindingTagPrefix)
}

// dataTLSConfig returns the TLS configuration to use for a TLS-protected data
// connection. When the session binding policy is enabled, the configuration
// rejects any ticket that does not belong to this control connection.
func (c *clientHandler) dataTLSConfig(base *tls.Config) *tls.Config {
	if !c.tlsDataSessionReuseEnabled() {
		return base
	}

	binding := c.controlDataSessionBinding
	if binding == nil {
		// Should not happen on an AUTH TLS upgraded connection, but stay
		// conservative: do not accept an unbound data session.
		var err error

		binding, err = c.server.newControlDataSessionBinding()
		if err != nil {
			c.logger.Error("Could not create data session binding", "err", err)

			return base
		}

		c.controlDataSessionBinding = binding
	}

	return binding.buildSessionBoundTLSConfig(base, true)
}

func (c *clientHandler) tlsDataSessionReuseEnabled() bool {
	return c.server.settings.TLSDataSessionRequirement == TLSDataSessionRequired &&
		c.server.settings.TLSRequired != ImplicitEncryption
}

// verifyDataTLSConnection enforces the session binding policy after the data
// connection TLS handshake: the data connection must be a resumption of the
// control connection session. Malformed or foreign tickets are already turned
// into handshake errors by the strict UnwrapSession hook, while a ticket
// presented but ignored by crypto/tls (or a plain full handshake) ends up
// with DidResume == false and is rejected here.
func (c *clientHandler) verifyDataTLSConnection(conn net.Conn, timeout time.Duration) (net.Conn, error) {
	tlsConn, ok := conn.(*tls.Conn)
	if !ok || !c.tlsDataSessionReuseEnabled() {
		return conn, nil
	}

	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	if err := tlsConn.SetDeadline(time.Now().Add(timeout)); err != nil {
		_ = conn.Close()

		return nil, err
	}

	if err := tlsConn.Handshake(); err != nil {
		_ = conn.Close()

		return nil, errDataSessionReuseRequired
	}

	_ = tlsConn.SetDeadline(time.Time{})

	if !tlsConn.ConnectionState().DidResume {
		_ = conn.Close()

		return nil, errDataSessionReuseRequired
	}

	return conn, nil
}
