package ftpserver

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// cccShutdownTimeout is the maximum time to wait for the client close_notify
// after a CCC command. It's a variable so that tests can shorten it.
var cccShutdownTimeout = 30 * time.Second //nolint:gochecknoglobals

// Handle the "USER" command
func (c *clientHandler) handleUSER(user string) error {
	if verifier, ok := c.server.driver.(MainDriverExtensionUserVerifier); ok {
		err := verifier.PreAuthUser(c, user)
		if err != nil {
			c.writeMessage(StatusNotLoggedIn, fmt.Sprintf("User rejected: %v", err))
			_ = c.disconnect()

			return nil
		}
	}

	if c.isTLSRequired() && !c.HasTLSForControl() {
		c.writeMessage(StatusServiceNotAvailable, "TLS is required")
		_ = c.disconnect()

		return nil
	}

	if c.HasTLSForControl() {
		if c.handleUserTLS(user) {
			return nil
		}
	}

	c.user = user
	c.writeMessage(StatusUserOK, "OK")

	return nil
}

func (c *clientHandler) handleUserTLS(user string) bool {
	verifier, interfaceFound := c.server.driver.(MainDriverExtensionTLSVerifier)

	if !interfaceFound {
		return false
	}

	tlsConn, interfaceFound := c.conn.(*tls.Conn)

	if !interfaceFound {
		return false
	}

	driver, err := verifier.VerifyConnection(c, user, tlsConn)
	if err != nil {
		c.writeMessage(StatusNotLoggedIn, fmt.Sprintf("TLS verification failed: %v", err))
		_ = c.disconnect()

		return true
	}

	if driver != nil {
		c.user = user
		c.driver = driver
		c.writeMessage(StatusUserLoggedIn, "TLS certificate ok, continue")

		return true
	}

	return false
}

// Handle the "PASS" command
func (c *clientHandler) handlePASS(param string) error {
	var err error
	var msg string
	c.driver, err = c.server.driver.AuthUser(c, c.user, param)

	dpa, ok := c.server.driver.(MainDriverExtensionPostAuthMessage)
	if ok {
		msg = dpa.PostAuthMessage(c, c.user, err)
	}

	switch {
	case err == nil && c.driver == nil:
		c.writeMessage(StatusNotLoggedIn, "Unexpected exception (driver is nil)")
		_ = c.disconnect()
	case err != nil:
		if msg == "" {
			msg = fmt.Sprintf("Authentication error: %v", err)
		}

		c.writeMessage(StatusNotLoggedIn, msg)
		_ = c.disconnect()
	default: // err == nil && c.driver != nil
		if msg == "" {
			msg = "Password ok, continue"
		}

		c.writeMessage(StatusUserLoggedIn, msg)
	}

	return nil
}

// Handle the "CCC" command: it reverts the control connection to clear text.
// The data connections keep the protection level set by the PROT command.
func (c *clientHandler) handleCCC(_ string) error {
	switch {
	case !c.server.settings.EnableCCC:
		c.writeMessage(StatusCommandNotImplemented, "CCC is disabled")
	case c.server.settings.TLSRequired == ImplicitEncryption:
		c.writeMessage(StatusBadCommandSequence, "CCC is not supported on implicit TLS connections")
	case !c.HasTLSForControl():
		c.writeMessage(StatusBadCommandSequence, "CCC requires a TLS-protected control connection")
	case c.isTLSRequired():
		c.writeMessage(StatusActionNotTakenPolicy, "CCC is not allowed: TLS is required")
	default:
		c.writeMessage(StatusOK, "Control connection will now switch to clear text")

		if err := c.clearControlConnection(); err != nil {
			c.logger.Warn("Couldn't switch the control connection to clear text", "err", err)
			// Our side of the TLS layer is already shut down, so the channel
			// state is unknown: report the failure in clear text and give up
			// on the connection.
			c.writeClearLine(StatusServiceNotAvailable, "CCC shutdown failed: closing control connection")
			_ = c.disconnect()
		}
	}

	return nil
}

// clearControlConnection shuts the TLS layer of the control connection down
// and carries on with the underlying clear-text connection.
func (c *clientHandler) clearControlConnection() error {
	tlsConn, ok := c.conn.(*tls.Conn)
	if !ok || c.rawConn == nil {
		return errors.New("control connection is not a TLS connection")
	}

	rawConn := c.rawConn
	tlsVersion := tlsConn.ConnectionState().Version

	// Send our close_notify. Closing the TLS connection doesn't close the
	// underlying connection: it was wrapped in a noCloseConn by handleAUTH.
	if err := tlsConn.Close(); err != nil {
		c.logger.Debug("Couldn't send the TLS close_notify", "err", err)
	}

	// tls.Conn.Close leaves an expired write deadline on the underlying
	// connection: clear it or no clear-text answer could ever be sent.
	if err := rawConn.SetDeadline(time.Time{}); err != nil {
		return newNetworkError("couldn't reset the CCC deadlines", err)
	}

	// Wait for the client close_notify: it's a TLS record and must not be
	// interpreted as clear-text commands.
	if err := rawConn.SetReadDeadline(time.Now().Add(cccShutdownTimeout)); err != nil {
		return newNetworkError("couldn't set the CCC read deadline", err)
	}

	pending, err := readPeerCloseNotify(rawConn, tlsVersion)

	if errReset := rawConn.SetReadDeadline(time.Time{}); errReset != nil && err == nil {
		err = newNetworkError("couldn't reset the CCC read deadline", errReset)
	}

	if err != nil {
		return err
	}

	// Switch to the clear-text connection, carrying over the commands that
	// were already received: decrypted data buffered by the previous reader
	// and clear-text bytes read while looking for the close_notify.
	sources := make([]io.Reader, 0, 3) //nolint:mnd

	if buffered := c.reader.Buffered(); buffered > 0 {
		decrypted, _ := c.reader.Peek(buffered)
		sources = append(sources, bytes.NewReader(decrypted))
	}

	if len(pending) > 0 {
		sources = append(sources, bytes.NewReader(pending))
	}

	sources = append(sources, rawConn)

	c.conn = rawConn
	c.rawConn = nil
	c.reader = bufio.NewReaderSize(io.MultiReader(sources...), maxCommandSize)
	c.writer = bufio.NewWriter(rawConn)
	c.setTLSForControl(false)

	if c.debug {
		c.logger.Debug("Control connection switched to clear text")
	}

	return nil
}

// readPeerCloseNotify consumes the client close_notify TLS record from the raw
// connection. It returns the leading bytes that don't belong to a TLS record,
// so they can be prepended to the clear-text command stream: some clients
// switch to clear text without sending a close_notify.
func readPeerCloseNotify(conn net.Conn, tlsVersion uint16) ([]byte, error) {
	for {
		header := make([]byte, 5) //nolint:mnd

		if _, err := io.ReadFull(conn, header); err != nil {
			return nil, fmt.Errorf("couldn't read the client close_notify: %w", err)
		}

		// TLS record: content type (1 byte), version (2 bytes), length (2 bytes)
		isAlert := header[0] == 0x15
		isAppData := header[0] == 0x17

		if (!isAlert && !isAppData) || header[1] != 0x03 {
			// Not a TLS record: the client switched to clear text without
			// sending a close_notify.
			return header, nil
		}

		if _, err := io.CopyN(io.Discard, conn, int64(binary.BigEndian.Uint16(header[3:5]))); err != nil {
			return nil, fmt.Errorf("couldn't read the client close_notify: %w", err)
		}

		// TLS 1.2 and below send the close_notify as a plain alert record.
		// TLS 1.3 encrypts it, so it shows up as application data: the first
		// record received after CCC is the close_notify.
		if isAlert || tlsVersion >= tls.VersionTLS13 {
			return nil, nil
		}
		// Anything else is application data pipelined after CCC: it can't be
		// decrypted anymore (the TLS connection is closed) and is dropped.
	}
}

// writeClearLine writes a response line directly on the raw connection,
// bypassing the TLS layer. It's only used when a CCC shutdown fails.
func (c *clientHandler) writeClearLine(code int, message string) {
	if c.rawConn == nil {
		return
	}

	if _, err := fmt.Fprintf(c.rawConn, "%d %s\r\n", code, message); err != nil {
		c.logger.Warn(
			"Clear-text answer couldn't be sent",
			"line", message,
			"err", err,
		)
	}
}
