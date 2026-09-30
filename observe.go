package smtp

import (
	"strings"
	"time"
)

// CommandKind identifies the category of an SMTP command reported in a
// CommandEvent. It is the upper-cased command verb sent by the client.
type CommandKind string

const (
	CommandHELO     CommandKind = "HELO"
	CommandEHLO     CommandKind = "EHLO"
	CommandLHLO     CommandKind = "LHLO"
	CommandMAIL     CommandKind = "MAIL"
	CommandRCPT     CommandKind = "RCPT"
	CommandDATA     CommandKind = "DATA"
	CommandBDAT     CommandKind = "BDAT"
	CommandAUTH     CommandKind = "AUTH"
	CommandSTARTTLS CommandKind = "STARTTLS"
	CommandRSET     CommandKind = "RSET"
	CommandNOOP     CommandKind = "NOOP"
	CommandVRFY     CommandKind = "VRFY"
	CommandQUIT     CommandKind = "QUIT"
)

// RecipientResult describes the per-recipient outcome of a LMTP message
// transfer.
type RecipientResult struct {
	// Address is the recipient address. It is passed through
	// Server.ObserverAddressMask first if one is configured.
	Address string
	// StatusCode is the SMTP reply code sent for this recipient.
	StatusCode int
}

// CommandEvent is a structured audit record for a single completed SMTP
// command.
//
// Events never contain command arguments, authentication responses, SASL
// challenges, or message contents, so they are safe to forward to log
// aggregation platforms.
type CommandEvent struct {
	// Kind is the category of the completed command.
	Kind CommandKind
	// TransactionID identifies the mail transaction the command belongs to.
	// It is 0 before the first successful MAIL FROM command and increments
	// with every subsequent mail transaction on the same connection.
	TransactionID int
	// StatusCode is the SMTP reply code that completed the command.
	// Multi-stage commands produce a single event: an AUTH command reports
	// the final code of the whole challenge/response exchange and an EHLO
	// command reports the code of its (possibly multi-line) reply once.
	// It is 0 if the connection failed before any reply was written.
	StatusCode int
	// Duration is the wall-clock time taken to complete the command.
	Duration time.Duration
	// BodyBytes is the cumulative number of message body bytes received so
	// far in the current transaction. It is only meaningful for DATA and
	// BDAT commands.
	BodyBytes int64
	// Recipients holds the per-recipient results of a LMTP DATA or BDAT
	// LAST command, in the exact order the recipients were specified with
	// RCPT TO. It is nil for non-LMTP servers and other commands.
	Recipients []RecipientResult
}

// Observer receives structured audit events for commands completed on SMTP
// sessions. It is an alternative to Server.Debug that is safe to forward to
// log aggregation platforms: events never include authentication responses
// or message contents.
type Observer interface {
	// ObserveCommand is called after each SMTP command completes.
	//
	// It is called from the connection's goroutine, but never while
	// protocol-critical locks are held. A returned error or a panic is
	// logged and otherwise ignored: it never turns a completed delivery
	// into a failure. Implementations must be safe for concurrent use and
	// should not block.
	ObserveCommand(ev CommandEvent) error
}

// ObserverFunc is an adapter to allow the use of an ordinary function as an
// Observer.
type ObserverFunc func(ev CommandEvent) error

var _ Observer = (ObserverFunc)(nil)

// ObserveCommand calls f(ev).
func (f ObserverFunc) ObserveCommand(ev CommandEvent) error {
	return f(ev)
}

// MaskAddress returns addr with its local part replaced by "***", preserving
// the domain part. It is a reasonable default for Server.ObserverAddressMask.
func MaskAddress(addr string) string {
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		return "***" + addr[i:]
	}
	return "***"
}
