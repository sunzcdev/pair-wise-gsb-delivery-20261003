package smtp

import (
	"strings"
	"time"
)

// CommandEvent is a structured audit record describing a single completed
// SMTP command.
//
// Events never contain authentication credentials, SASL responses or
// message contents.
type CommandEvent struct {
	// Command is the SMTP command verb, e.g. "EHLO", "MAIL" or "DATA".
	Command string

	// TransactionID identifies the mail transaction the command belongs to.
	// It is unique per connection and is assigned when a MAIL command
	// succeeds. It is zero for commands issued outside of a mail
	// transaction.
	TransactionID uint64

	// Code is the SMTP reply code sent to the client when the command
	// completed. Multi-line replies (e.g. EHLO) and multi-step exchanges
	// (e.g. AUTH) produce a single event carrying the final reply code.
	Code int

	// Duration is the time taken to process the command, including the
	// message transfer for DATA and BDAT commands.
	Duration time.Duration

	// BodyBytes is the cumulative size of the message body received so far.
	// It is only meaningful for DATA and BDAT commands.
	BodyBytes int64

	// Recipients contains per-recipient results of an LMTP message
	// transaction, in the order the recipients were specified by the
	// client. It is nil for non-LMTP sessions and non-data commands.
	Recipients []RecipientStatus
}

// RecipientStatus describes the delivery status of a single recipient
// of an LMTP transaction.
type RecipientStatus struct {
	// Address is the recipient address. If Server.ObserverMaskAddresses
	// is enabled, the local part is replaced with "***".
	Address string

	// Code is the SMTP reply code sent for this recipient.
	Code int
}

// SessionObserver receives structured audit events for SMTP commands
// completed on a connection.
//
// ObserveCommand is called synchronously after a command has been fully
// processed and its reply has been sent. It is never called while
// protocol-critical locks are held. Returned errors and panics are
// logged and never affect the session state.
type SessionObserver interface {
	ObserveCommand(evt *CommandEvent) error
}

// SessionObserverFunc is an adapter to allow the use of an ordinary
// function as a SessionObserver.
type SessionObserverFunc func(evt *CommandEvent) error

var _ SessionObserver = (SessionObserverFunc)(nil)

// ObserveCommand calls f(evt).
func (f SessionObserverFunc) ObserveCommand(evt *CommandEvent) error {
	return f(evt)
}

// maskAddress replaces the local part of an address with "***".
func maskAddress(addr string) string {
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		return "***" + addr[i:]
	}
	return "***"
}
