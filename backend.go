package smtp

import (
	"context"
	"io"

	"github.com/emersion/go-sasl"
)

var (
	ErrAuthFailed = &SMTPError{
		Code:         535,
		EnhancedCode: EnhancedCode{5, 7, 8},
		Message:      "Authentication failed",
	}
	ErrAuthRequired = &SMTPError{
		Code:         502,
		EnhancedCode: EnhancedCode{5, 7, 0},
		Message:      "Please authenticate first",
	}
	ErrAuthUnsupported = &SMTPError{
		Code:         502,
		EnhancedCode: EnhancedCode{5, 7, 0},
		Message:      "Authentication not supported",
	}
	ErrAuthUnknownMechanism = &SMTPError{
		Code:         504,
		EnhancedCode: EnhancedCode{5, 7, 4},
		Message:      "Unsupported authentication mechanism",
	}
)

// A SMTP server backend.
type Backend interface {
	NewSession(c *Conn) (Session, error)
}

// BackendFunc is an adapter to allow the use of an ordinary function as a
// Backend.
type BackendFunc func(c *Conn) (Session, error)

var _ Backend = (BackendFunc)(nil)

// NewSession calls f(c).
func (f BackendFunc) NewSession(c *Conn) (Session, error) {
	return f(c)
}

// Session is used by servers to respond to an SMTP client.
//
// The methods are called when the remote client issues the matching command.
type Session interface {
	// Discard currently processed message.
	Reset()

	// Free all resources associated with session.
	Logout() error

	// Set return path for currently processed message.
	Mail(from string, opts *MailOptions) error
	// Add recipient for currently processed message.
	Rcpt(to string, opts *RcptOptions) error
	// Set currently processed message contents and send it.
	//
	// r must be consumed before Data returns.
	Data(r io.Reader) error
}

// ContextSession is an add-on interface for Session. It can be implemented
// by backends that need to track the lifetime of a mail transaction, e.g.
// to abort an anti-spam check or an object storage upload when the
// transaction is terminated early.
//
// A new transaction context is created when a MAIL command is accepted and
// is cancelled when the transaction ends, that is:
//
//   - when the transaction completes (DATA or BDAT ... LAST finishes,
//     successfully or with a backend error),
//   - when the client issues RSET, HELO/EHLO/LHLO or STARTTLS,
//   - when a new MAIL command replaces the current transaction,
//   - when the backend rejects the MAIL command itself,
//   - when the connection is closed.
//
// A cancelled context is never reused: the next MAIL command on the same
// connection always starts a transaction with a fresh context.
//
// Note on timeliness: cancellation caused by a client disconnect is only
// detected at points where the server itself reads from the connection
// (the next command, the next BDAT chunk relay, or the message data
// reader hitting EOF). The server deliberately does not read the
// connection from a separate goroutine to notice disconnects earlier,
// because concurrent reads would steal protocol data from the command and
// data readers. As a consequence, a backend blocked in DataContext without
// performing I/O may not observe a disconnect until the transaction
// otherwise ends; such backends should use their own deadlines if prompt
// detection is required.
type ContextSession interface {
	Session

	// MailContext is like Mail but additionally receives the context of
	// the newly started transaction.
	MailContext(ctx context.Context, from string, opts *MailOptions) error
	// RcptContext is like Rcpt but additionally receives the context of
	// the current transaction.
	RcptContext(ctx context.Context, to string, opts *RcptOptions) error
	// DataContext is like Data but additionally receives the context of
	// the current transaction.
	//
	// r must be consumed before DataContext returns.
	DataContext(ctx context.Context, r io.Reader) error
}

// LMTPSession is an add-on interface for Session. It can be implemented by
// LMTP servers to provide extra functionality.
type LMTPSession interface {
	Session

	// LMTPData is the LMTP-specific version of Data method.
	// It can be optionally implemented by the backend to provide
	// per-recipient status information when it is used over LMTP
	// protocol.
	//
	// LMTPData implementation sets status information using passed
	// StatusCollector by calling SetStatus once per each AddRcpt
	// call, even if AddRcpt was called multiple times with
	// the same argument. SetStatus must not be called after
	// LMTPData returns.
	//
	// Return value of LMTPData itself is used as a status for
	// recipients that got no status set before using StatusCollector.
	LMTPData(r io.Reader, status StatusCollector) error
}

// StatusCollector allows a backend to provide per-recipient status
// information.
type StatusCollector interface {
	SetStatus(rcptTo string, err error)
}

// AuthSession is an add-on interface for Session. It provides support for the
// AUTH extension.
type AuthSession interface {
	Session

	AuthMechanisms() []string
	Auth(mech string) (sasl.Server, error)
}
