package ldap

import (
	"errors"
	"fmt"
	"sync"

	ber "github.com/go-asn1-ber/asn1-ber"
)

// This file implements the LDAP client-side transaction interface defined in
// RFC 5805 (Lightweight Directory Access Protocol (LDAP) Transactions).
//
// https://www.rfc-editor.org/rfc/rfc5805
//
// A transaction is bound to the connection that created it. The Txn handle
// holds that connection internally and every operation is sent on that same
// connection; there is intentionally no way to continue a transaction on a
// different connection.

const (
	// OIDStartTransaction is the extended request/response name for
	// Start Transaction (RFC 5805 section 2.1). The request carries no value;
	// the responseValue is the BER-encoded transaction identifier.
	OIDStartTransaction = "1.3.6.1.1.21.1"
	// OIDEndTransaction is the extended request/response name for
	// End Transaction (RFC 5805 section 2.3). The requestValue carries the
	// commit boolean; the responseValue carries an optional failed message ID.
	OIDEndTransaction = "1.3.6.1.1.21.3"
)

var (
	// ErrTxnCommitted is returned when an operation is attempted on a
	// transaction that has already been committed.
	ErrTxnCommitted = errors.New("ldap: transaction already committed")
	// ErrTxnAborted is returned when an operation is attempted on a
	// transaction that has been aborted, either by the caller or because the
	// server terminated it.
	ErrTxnAborted = errors.New("ldap: transaction aborted")
	// ErrTxnInDoubt is returned when an operation is attempted after a commit
	// request could not be conclusively confirmed. The transaction may have
	// committed or aborted; the caller cannot tell and must verify the
	// outcome out of band.
	ErrTxnInDoubt = errors.New("ldap: transaction outcome is in doubt")
)

// Transaction states. The lifecycle is active -> committed, active -> aborted
// (possibly "in doubt" after an unconfirmed commit), and every state except
// active is terminal.
const (
	txnStateActive uint8 = iota
	txnStateCommitted
	txnStateAborted
	txnStateInDoubt
)

// EndTxnResult reports the outcome of an End Transaction request.
type EndTxnResult struct {
	// Committed is true when all update operations were committed.
	Committed bool
	// FailedMessageID, when HasFailedMessageID is true, is the message ID of
	// the update operation that caused the transaction to fail (RFC 5805
	// section 2.3). The message ID is omitted for transaction-level failures
	// (for example an unknown or expired transaction identifier). The result
	// is returned even when Commit reports an error, so the failed message ID
	// is available to the caller.
	FailedMessageID    int64
	HasFailedMessageID bool
}

// Txn is a client-side handle for an RFC 5805 transaction. It is created by
// (*Conn).StartTxn and is usable only on the connection that created it.
type Txn struct {
	conn       *Conn
	identifier []byte

	mu    sync.Mutex
	state uint8
	// lastErr holds the error that terminated the transaction, or the last
	// failed in-transaction operation, so it remains observable after the
	// failure information is no longer directly returned.
	lastErr error
}

// StartTxn begins an LDAP transaction as described in RFC 5805 section 2.1.
// The returned *Txn is bound to this connection; update operations and the
// final Commit/Abort must all be issued through the handle on this connection.
func (l *Conn) StartTxn() (*Txn, error) {
	if l == nil || l.conn == nil {
		return nil, ErrNilConnection
	}

	msgCtx, err := l.doRequest(NewExtendedRequest(OIDStartTransaction, nil))
	if err != nil {
		return nil, err
	}
	defer l.finishMessage(msgCtx)

	packet, err := l.readPacket(msgCtx)
	if err != nil {
		return nil, err
	}
	if err := GetLDAPError(packet); err != nil {
		return nil, err
	}

	extResp, err := packetChild(packet, 1)
	if err != nil {
		return nil, err
	}

	var identifier []byte
	for _, child := range extResp.Children {
		if child.ClassType == ber.ClassContext && child.Tag == ber.TagEmbeddedPDV {
			identifier, err = packetData(child)
			if err != nil {
				return nil, err
			}
			break
		}
	}
	if identifier == nil {
		return nil, malformedf("start transaction response is missing the responseValue")
	}

	return &Txn{
		conn:       l,
		identifier: identifier,
		state:      txnStateActive,
	}, nil
}

// Active reports whether the transaction is still open. It returns false once
// the transaction has been committed, aborted, or left in doubt.
func (txn *Txn) Active() bool {
	if txn == nil {
		return false
	}
	txn.mu.Lock()
	defer txn.mu.Unlock()
	return txn.state == txnStateActive
}

// Identifier returns the opaque, BER-encoded transaction identifier returned
// by the server.
func (txn *Txn) Identifier() []byte {
	if txn == nil {
		return nil
	}
	out := make([]byte, len(txn.identifier))
	copy(out, txn.identifier)
	return out
}

// LastError returns the most recent error reported by an in-transaction
// operation or by Commit/Abort, or nil if there has been none.
func (txn *Txn) LastError() error {
	if txn == nil {
		return nil
	}
	txn.mu.Lock()
	defer txn.mu.Unlock()
	return txn.lastErr
}

// checkState returns an error if the transaction is no longer active.
func (txn *Txn) checkState() error {
	switch txn.state {
	case txnStateCommitted:
		return ErrTxnCommitted
	case txnStateAborted:
		return fmt.Errorf("%w: %v", ErrTxnAborted, txn.lastErr)
	case txnStateInDoubt:
		return fmt.Errorf("%w: %v", ErrTxnInDoubt, txn.lastErr)
	default:
		return nil
	}
}

// withTxnControl returns a copy of controls with any existing Transaction
// Specification control removed and the handle's own critical control
// appended. The caller's slice is never modified.
func (txn *Txn) withTxnControl(controls []Control) []Control {
	out := make([]Control, 0, len(controls)+1)
	for _, c := range controls {
		if c == nil || c.GetControlType() == ControlTypeTxnSpec {
			continue
		}
		out = append(out, c)
	}
	out = append(out, NewControlTxnSpec(txn.identifier))
	return out
}

// run checks the transaction state, records per-operation failures and marks
// the transaction aborted when a failure means it can no longer be used
// (a broken connection, but not an ordinary LDAP result-code failure, which
// leaves the transaction usable per RFC 5805 section 3).
func (txn *Txn) run(op func() error) error {
	txn.mu.Lock()
	if err := txn.checkState(); err != nil {
		txn.mu.Unlock()
		return err
	}
	txn.mu.Unlock()

	err := op()
	if err == nil {
		return nil
	}

	txn.mu.Lock()
	defer txn.mu.Unlock()
	txn.lastErr = err
	// A transport failure kills the connection (the reader loop closes it),
	// so the transaction can never continue on it. The delivered error is not
	// necessarily an *Error with code ErrorNetwork - a torn read surfaces the
	// underlying I/O error - therefore also treat a closing connection as
	// fatal. An ordinary LDAP result-code failure leaves the connection and
	// the transaction usable (RFC 5805 section 3).
	if IsErrorWithCode(err, ErrorNetwork) || txn.conn.IsClosing() {
		txn.state = txnStateAborted
	}
	return err
}

// Add performs an AddRequest within the transaction. The request is sent on
// the originating connection with the Transaction Specification control
// attached. A result-code failure does not abort the transaction; a connection
// failure does.
func (txn *Txn) Add(req *AddRequest) error {
	if txn == nil {
		return NewError(ErrorNetwork, errors.New("ldap: nil transaction handle"))
	}
	if req == nil {
		return NewError(ErrorNetwork, errors.New("AddRequest cannot be nil"))
	}
	clone := *req
	clone.Controls = txn.withTxnControl(req.Controls)
	return txn.run(func() error { return txn.conn.Add(&clone) })
}

// Del performs a DelRequest within the transaction.
func (txn *Txn) Del(req *DelRequest) error {
	if txn == nil {
		return NewError(ErrorNetwork, errors.New("ldap: nil transaction handle"))
	}
	if req == nil {
		return NewError(ErrorNetwork, errors.New("DelRequest cannot be nil"))
	}
	clone := *req
	clone.Controls = txn.withTxnControl(req.Controls)
	return txn.run(func() error { return txn.conn.Del(&clone) })
}

// Modify performs a ModifyRequest within the transaction.
func (txn *Txn) Modify(req *ModifyRequest) error {
	if txn == nil {
		return NewError(ErrorNetwork, errors.New("ldap: nil transaction handle"))
	}
	if req == nil {
		return NewError(ErrorNetwork, errors.New("ModifyRequest cannot be nil"))
	}
	clone := *req
	clone.Controls = txn.withTxnControl(req.Controls)
	return txn.run(func() error { return txn.conn.Modify(&clone) })
}

// ModifyDN performs a ModifyDNRequest within the transaction.
func (txn *Txn) ModifyDN(req *ModifyDNRequest) error {
	if txn == nil {
		return NewError(ErrorNetwork, errors.New("ldap: nil transaction handle"))
	}
	if req == nil {
		return NewError(ErrorNetwork, errors.New("ModifyDNRequest cannot be nil"))
	}
	clone := *req
	clone.Controls = txn.withTxnControl(req.Controls)
	return txn.run(func() error { return txn.conn.ModifyDN(&clone) })
}

// Commit sends an End Transaction request with commit set to TRUE and marks
// the transaction committed.
func (txn *Txn) Commit() (*EndTxnResult, error) {
	if txn == nil {
		return nil, NewError(ErrorNetwork, errors.New("ldap: nil transaction handle"))
	}
	return txn.end(true)
}

// Abort sends an End Transaction request with commit set to FALSE and marks
// the transaction aborted.
func (txn *Txn) Abort() error {
	if txn == nil {
		return NewError(ErrorNetwork, errors.New("ldap: nil transaction handle"))
	}
	_, err := txn.end(false)
	return err
}

// end implements the End Transaction extended operation. When commit is true
// and the result cannot be determined (the connection broke or the response
// was unreadable), the transaction is placed in the "in doubt" state and the
// returned error wraps ErrTxnInDoubt.
func (txn *Txn) end(commit bool) (*EndTxnResult, error) {
	txn.mu.Lock()
	if err := txn.checkState(); err != nil {
		txn.mu.Unlock()
		return nil, err
	}
	txn.mu.Unlock()

	value := encodeEndTxnRequestValue(commit)
	req := NewExtendedRequest(OIDEndTransaction, value)

	msgCtx, err := txn.conn.doRequest(req)
	if err != nil {
		return nil, txn.finishEndFailure(commit, err)
	}
	defer txn.conn.finishMessage(msgCtx)

	packet, readErr := txn.conn.readPacket(msgCtx)
	if readErr != nil {
		return nil, txn.finishEndFailure(commit, readErr)
	}

	result, respErr := decodeEndTxnResponse(packet)

	txn.mu.Lock()
	defer txn.mu.Unlock()
	if respErr != nil {
		// A response carrying an LDAP result code is a definite answer: a
		// failed End Transaction rolls the updates back (RFC 5805 3.2). A
		// structurally unreadable response tells us nothing about whether the
		// server committed, so a commit is in doubt in that case.
		var ldapErr *Error
		if errors.As(respErr, &ldapErr) && !errors.Is(respErr, ErrMalformedPacket) {
			txn.lastErr = respErr
			txn.state = txnStateAborted
			return result, respErr
		}
		if commit {
			txn.state = txnStateInDoubt
			txn.lastErr = fmt.Errorf("%w: %v", ErrTxnInDoubt, respErr)
			return result, txn.lastErr
		}
		txn.lastErr = respErr
		txn.state = txnStateAborted
		return result, respErr
	}

	if commit {
		txn.state = txnStateCommitted
		result.Committed = true
	} else {
		txn.state = txnStateAborted
	}
	return result, nil
}

// finishEndFailure records a transport-level failure of the End Transaction
// request. A failed commit cannot be distinguished from a failed abort when
// no response was received, so commit=true yields the in-doubt state.
func (txn *Txn) finishEndFailure(commit bool, err error) error {
	txn.mu.Lock()
	defer txn.mu.Unlock()

	if commit {
		txn.state = txnStateInDoubt
		txn.lastErr = fmt.Errorf("%w: %v", ErrTxnInDoubt, err)
		return txn.lastErr
	}
	txn.state = txnStateAborted
	txn.lastErr = err
	return err
}

// encodeEndTxnRequestValue builds the EndTransactionRequestValue:
//
//	EndTransactionRequestValue ::= SEQUENCE {
//	    commit        BOOLEAN DEFAULT TRUE }
//
// The field is omitted for the default (commit) and present as [1] BOOLEAN
// FALSE for an abort.
func encodeEndTxnRequestValue(commit bool) *ber.Packet {
	seq := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "End Transaction Request Value")
	if !commit {
		seq.AppendChild(ber.NewBoolean(ber.ClassContext, ber.TypePrimitive, ber.Tag(1), false, "Commit"))
	}
	return seq
}

// decodeEndTxnResponse parses an End Transaction extended response. The
// response is decoded even when GetLDAPError reports a result-code failure, so
// the optional failed-operation message ID is preserved on failure.
func decodeEndTxnResponse(packet *ber.Packet) (*EndTxnResult, error) {
	result := &EndTxnResult{}

	respErr := GetLDAPError(packet)

	extResp, err := packetChild(packet, 1)
	if err != nil {
		return result, err
	}
	if extResp.ClassType != ber.ClassApplication || extResp.Tag != ApplicationExtendedResponse {
		return result, fmt.Errorf("ldap: unexpected response: %d", extResp.Tag)
	}

	for _, child := range extResp.Children {
		if child.ClassType != ber.ClassContext || child.Tag != ber.TagEmbeddedPDV {
			continue
		}
		// EndTransactionResponseValue ::= SEQUENCE { messageID MessageID }
		data, err := packetData(child)
		if err != nil {
			return result, err
		}
		value, err := ber.DecodePacketErr(data)
		if err != nil {
			return result, malformedf("end transaction responseValue is not a BER packet: %v", err)
		}
		if value.ClassType != ber.ClassUniversal || value.TagType != ber.TypeConstructed || value.Tag != ber.TagSequence {
			return result, malformedf("end transaction responseValue is not a SEQUENCE")
		}
		msgIDChild, err := packetChild(value, 0)
		if err != nil {
			return result, err
		}
		messageID, err := packetInt64(msgIDChild)
		if err != nil {
			return result, err
		}
		result.FailedMessageID = messageID
		result.HasFailedMessageID = true
		break
	}

	return result, respErr
}
