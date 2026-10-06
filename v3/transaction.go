package ldap

import (
	"bytes"
	"errors"
	"fmt"
	"sync"

	ber "github.com/go-asn1-ber/asn1-ber"
)

// extendedOperationNameTransaction is the requestName shared by the Start
// Transaction Request (RFC 5805 section 2.1) and the End Transaction Request
// (section 2.3).
const extendedOperationNameTransaction = "1.3.6.1.1.21.1"

var (
	// ErrTransactionFinished is returned when a transaction handle is used
	// after Commit or Abort completed.
	ErrTransactionFinished = errors.New("ldap: transaction is finished")
	// ErrTransactionAborted is returned when the server has aborted the
	// transaction (by closing the connection, failing an in-transaction update,
	// or refusing to start it). The handle can no longer be used.
	ErrTransactionAborted = errors.New("ldap: transaction was aborted")
	// ErrTransactionCommitUncertain is wrapped by the error returned from
	// Commit when the End Transaction Request could not be answered: the
	// connection was lost or the response could not be read, so the client
	// cannot tell whether the server committed the transaction. Callers must
	// not assume either outcome.
	ErrTransactionCommitUncertain = errors.New("ldap: transaction commit outcome is unknown")
	// ErrTransactionProtocolError is returned for server responses that do not
	// conform to RFC 5805, for example an in-transaction update response that
	// does not echo the Transaction Specification control.
	ErrTransactionProtocolError = errors.New("ldap: transaction protocol error")
)

// ControlTransaction is the Transaction Specification Control
// (1.3.6.1.1.21.2) defined in section 2.2 of RFC 5805. Its value is the
// transaction identifier the server returned in response to the Start
// Transaction extended operation.
type ControlTransaction struct {
	// Criticality should be true on controls attached to in-transaction
	// update requests so that a server which does not understand the control
	// rejects the request instead of applying the update non-transactionally.
	Criticality bool
	// Identifier is the opaque transaction identifier assigned by the server.
	Identifier []byte
}

// GetControlType returns the OID of the Transaction Specification Control.
func (c *ControlTransaction) GetControlType() string {
	return ControlTypeTransaction
}

// Encode returns the BER representation of the control. The control value is
// the bare transaction identifier OCTET STRING, as specified by RFC 5805.
func (c *ControlTransaction) Encode() *ber.Packet {
	packet := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "Control")
	packet.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString,
		ControlTypeTransaction, "Control Type ("+ControlTypeMap[ControlTypeTransaction]+")"))
	if c.Criticality {
		packet.AppendChild(ber.NewBoolean(ber.ClassUniversal, ber.TypePrimitive, ber.TagBoolean,
			true, "Criticality"))
	}

	// The control value is a single OCTET STRING whose payload is the
	// identifier, so write the identifier bytes directly instead of appending
	// a nested OCTET STRING (which would double-wrap the value).
	value := ber.Encode(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, nil,
		"Control Value (Transaction Specification)")
	value.Data.Write(c.Identifier)
	packet.AppendChild(value)

	return packet
}

// String returns a human-readable description.
func (c *ControlTransaction) String() string {
	return fmt.Sprintf("Control Type: %s (%q)  Criticality: %t  Identifier: %q",
		ControlTypeMap[ControlTypeTransaction], ControlTypeTransaction, c.Criticality, c.Identifier)
}

// TransactionOperation describes a single update request staged on a
// transaction. It maps the messageIDs reported in a failed End Transaction
// response back to the request the caller issued.
type TransactionOperation struct {
	// MessageID is the LDAP message ID of the update request.
	MessageID int64
	// Kind is "add", "modify", "delete" or "moddn".
	Kind string
	// DN identifies the entry targeted by the request.
	DN string
}

// TransactionOperationResult is the per-operation information the server may
// include in the End Transaction response value when the transaction cannot be
// committed. The protocol only mandates the messageID; the remaining fields
// are populated from the optional embedded LDAPResult-style protocol op.
type TransactionOperationResult struct {
	// MessageID references the update request that failed.
	MessageID int64
	// ResultCode is the LDAP resultCode from the embedded protocol response.
	// It is zero when the server reports only the messageID.
	ResultCode uint16
	// MatchedDN and DiagnosticMessage come from the embedded protocol
	// response and are empty when only the messageID is reported.
	MatchedDN         string
	DiagnosticMessage string
}

// TransactionResultError reports that a transaction could not be committed.
// It implements error and preserves both the End Transaction LDAP result and
// the per-operation failure information returned by the server.
type TransactionResultError struct {
	// ResultCode is the resultCode of the End Transaction response.
	ResultCode uint16
	// MatchedDN and DiagnosticMessage come from the End Transaction response.
	MatchedDN         string
	DiagnosticMessage string
	// FailedOperations retains the information the server returned for the
	// updates that prevented the commit.
	FailedOperations []TransactionOperationResult
}

func (e *TransactionResultError) Error() string {
	return fmt.Sprintf("ldap: transaction commit failed (code %d, %q): %s, %d operation(s) failed",
		e.ResultCode, e.MatchedDN, e.DiagnosticMessage, len(e.FailedOperations))
}

// Transaction represents an LDAP transaction started per RFC 5805. A
// transaction is bound to the connection that started it: update requests
// staged through the handle are sent on that connection and the handle cannot
// be resumed on a different Conn. The zero value is not usable; obtain an
// instance with Conn.StartTransaction.
type Transaction struct {
	conn       *Conn
	identifier []byte

	mu         sync.Mutex
	finished   bool
	aborted    bool
	operations map[int64]*TransactionOperation
}

// Identifier returns the opaque identifier the server assigned to the
// transaction.
func (t *Transaction) Identifier() []byte {
	return t.identifier
}

// Operations returns the update requests staged on the transaction so far,
// keyed by their LDAP message ID.
func (t *Transaction) Operations() map[int64]TransactionOperation {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[int64]TransactionOperation, len(t.operations))
	for id, op := range t.operations {
		out[id] = *op
	}
	return out
}

// Operation resolves a messageID to the request the caller staged, or nil if
// the messageID was not part of this transaction.
func (t *Transaction) Operation(messageID int64) *TransactionOperation {
	t.mu.Lock()
	defer t.mu.Unlock()
	if op, ok := t.operations[messageID]; ok {
		return op
	}
	return nil
}

// Finished reports whether Commit or Abort has already been called.
func (t *Transaction) Finished() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.finished
}

// Aborted reports whether the transaction has been aborted, either by an
// explicit Abort or by the server.
func (t *Transaction) Aborted() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.aborted
}

// StartTransaction issues the Start Transaction extended operation
// (RFC 5805 section 2.1) and returns a transaction handle bound to this
// connection. Update operations are staged on the handle and made permanent
// with Commit or discarded with Abort.
func (l *Conn) StartTransaction() (*Transaction, error) {
	request := NewExtendedRequest(extendedOperationNameTransaction, nil)

	msgCtx, err := l.doRequest(request)
	if err != nil {
		return nil, err
	}
	defer l.finishMessage(msgCtx)

	packet, err := l.readPacket(msgCtx)
	if err != nil {
		return nil, err
	}

	response, err := decodeExtendedResponse(packet)
	if err != nil {
		return nil, err
	}
	if response.Value == nil {
		return nil, NewError(ErrorUnexpectedResponse, fmt.Errorf("%w: start transaction response has no value",
			ErrTransactionProtocolError))
	}

	identifier, err := packetData(response.Value)
	if err != nil {
		return nil, NewError(ErrorUnexpectedResponse, fmt.Errorf("%w: start transaction response value is not an octet string",
			ErrTransactionProtocolError))
	}

	return &Transaction{
		conn:       l,
		identifier: identifier,
		operations: map[int64]*TransactionOperation{},
	}, nil
}

// Add stages an AddRequest within the transaction. The Transaction
// Specification Control is attached automatically; controls already present
// on the request are preserved but a caller-supplied transaction control is
// replaced so the correct identifier is always used. The caller's request is
// not modified.
func (t *Transaction) Add(req *AddRequest) error {
	if req == nil {
		return NewError(ErrorNetwork, errors.New("AddRequest cannot be nil"))
	}
	clone := *req
	clone.Controls = t.withControl(req.Controls)
	return t.update("add", req.DN, &clone)
}

// Modify stages a ModifyRequest within the transaction.
func (t *Transaction) Modify(req *ModifyRequest) error {
	if req == nil {
		return NewError(ErrorNetwork, errors.New("ModifyRequest cannot be nil"))
	}
	clone := *req
	clone.Controls = t.withControl(req.Controls)
	return t.update("modify", req.DN, &clone)
}

// Del stages a DelRequest within the transaction.
func (t *Transaction) Del(req *DelRequest) error {
	if req == nil {
		return NewError(ErrorNetwork, errors.New("DelRequest cannot be nil"))
	}
	clone := *req
	clone.Controls = t.withControl(req.Controls)
	return t.update("delete", req.DN, &clone)
}

// ModifyDN stages a ModifyDNRequest within the transaction.
func (t *Transaction) ModifyDN(req *ModifyDNRequest) error {
	if req == nil {
		return NewError(ErrorNetwork, errors.New("ModifyDNRequest cannot be nil"))
	}
	clone := *req
	clone.Controls = t.withControl(req.Controls)
	return t.update("moddn", req.DN, &clone)
}

// withControl returns a control slice equivalent to controls but carrying the
// single, authoritative Transaction Specification Control for this handle.
func (t *Transaction) withControl(controls []Control) []Control {
	out := make([]Control, 0, len(controls)+1)
	for _, control := range controls {
		if control != nil && control.GetControlType() == ControlTypeTransaction {
			continue
		}
		out = append(out, control)
	}
	out = append(out, &ControlTransaction{
		Criticality: true,
		Identifier:  t.identifier,
	})
	return out
}

// update sends one staged update request on the originating connection,
// records it against its message ID and validates the response. Any failure
// aborts the handle, because RFC 5805 leaves the transaction unusable once an
// update cannot be completed or the connection is lost.
func (t *Transaction) update(kind, dn string, req request) error {
	t.mu.Lock()
	if t.finished {
		t.mu.Unlock()
		return ErrTransactionFinished
	}
	if t.aborted {
		t.mu.Unlock()
		return ErrTransactionAborted
	}
	conn := t.conn
	t.mu.Unlock()

	msgCtx, err := conn.doRequest(req)
	if err != nil {
		t.markAborted()
		return err
	}
	defer conn.finishMessage(msgCtx)

	t.recordOperation(msgCtx.id, kind, dn)

	packet, err := conn.readPacket(msgCtx)
	if err != nil {
		t.markAborted()
		if !IsErrorWithCode(err, ErrorNetwork) {
			err = NewError(ErrorNetwork, err)
		}
		return err
	}

	if err := t.validateResponse(kind, packet); err != nil {
		t.markAborted()
		return err
	}

	return nil
}

func (t *Transaction) recordOperation(messageID int64, kind, dn string) {
	t.mu.Lock()
	t.operations[messageID] = &TransactionOperation{
		MessageID: messageID,
		Kind:      kind,
		DN:        dn,
	}
	t.mu.Unlock()
}

func (t *Transaction) markAborted() {
	t.mu.Lock()
	t.aborted = true
	t.mu.Unlock()
}

// validateResponse confirms the response is the expected application result,
// succeeded, and echoed the Transaction Specification Control with the same
// identifier.
func (t *Transaction) validateResponse(kind string, packet *ber.Packet) error {
	expectedTag, err := expectedApplicationTag(kind)
	if err != nil {
		return NewError(ErrorNetwork, err)
	}

	protocolOp, err := packetChild(packet, 1)
	if err != nil {
		return err
	}
	if protocolOp.Tag != ber.Tag(expectedTag) {
		return NewError(ErrorNetwork, fmt.Errorf("%w: unexpected response tag %d for %s operation, want %d",
			ErrTransactionProtocolError, protocolOp.Tag, kind, expectedTag))
	}

	if err := GetLDAPError(packet); err != nil {
		return fmt.Errorf("%w: %w", ErrTransactionAborted, err)
	}

	controls, err := decodeResponseControls(packet)
	if err != nil {
		return NewError(ErrorNetwork, fmt.Errorf("%w: %w", ErrTransactionProtocolError, err))
	}
	control := FindControl(controls, ControlTypeTransaction)
	if control == nil {
		return NewError(ErrorNetwork, fmt.Errorf("%w: %s response does not echo the transaction specification control",
			ErrTransactionProtocolError, kind))
	}

	txControl, ok := control.(*ControlTransaction)
	if !ok {
		return NewError(ErrorNetwork, fmt.Errorf("%w: %s response control is not a transaction specification control",
			ErrTransactionProtocolError, kind))
	}
	if !bytes.Equal(txControl.Identifier, t.identifier) {
		return NewError(ErrorNetwork, fmt.Errorf("%w: %s response carries transaction identifier %q, want %q",
			ErrTransactionProtocolError, kind, txControl.Identifier, t.identifier))
	}

	return nil
}

// Commit sends an End Transaction request asking the server to make the
// staged updates permanent. A nil return means the transaction committed.
//
// If the server refuses to commit, the returned error is a
// *TransactionResultError carrying the result code and the operations that
// failed. If the request could not be completed because the connection was
// lost, the error wraps ErrTransactionCommitUncertain: the transaction may or
// may not have been committed and the caller must not assume either outcome.
func (t *Transaction) Commit() error {
	return t.finish(true)
}

// Abort sends an End Transaction request that discards the staged updates.
// The handle is finished regardless of whether the server acknowledged the
// abort, so it can never be reused.
func (t *Transaction) Abort() error {
	return t.finish(false)
}

func (t *Transaction) finish(commit bool) error {
	t.mu.Lock()
	if t.finished {
		t.mu.Unlock()
		return ErrTransactionFinished
	}
	if t.aborted {
		t.finished = true
		t.mu.Unlock()
		return ErrTransactionAborted
	}
	t.finished = true
	if !commit {
		t.aborted = true
	}
	conn := t.conn
	identifier := t.identifier
	t.mu.Unlock()

	value := ber.Encode(ber.ClassContext, ber.TypePrimitive, 1, nil, "Extended Request Value: End Transaction")
	seq := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "End Transaction Request Value")
	seq.AppendChild(ber.NewBoolean(ber.ClassContext, ber.TypePrimitive, 0, commit, "Commit"))
	identifierChild := ber.Encode(ber.ClassContext, ber.TypePrimitive, 1, nil, "Transaction Identifier")
	identifierChild.Data.Write(identifier)
	seq.AppendChild(identifierChild)
	value.AppendChild(seq)

	request := NewExtendedRequest(extendedOperationNameTransaction, value)

	msgCtx, err := conn.doRequest(request)
	if err != nil {
		return t.finishTransportError(commit, err)
	}
	defer conn.finishMessage(msgCtx)

	packet, err := conn.readPacket(msgCtx)
	if err != nil {
		return t.finishTransportError(commit, err)
	}

	if err := GetLDAPError(packet); err != nil {
		if !commit {
			return err
		}
		resultErr, ok := err.(*Error)
		if !ok {
			return err
		}
		return &TransactionResultError{
			ResultCode:        resultErr.ResultCode,
			MatchedDN:         resultErr.MatchedDN,
			DiagnosticMessage: resultErr.Err.Error(),
			FailedOperations:  parseEndTransactionResponseValue(packet),
		}
	}

	return nil
}

// finishTransportError maps a transport failure while ending the transaction.
// A failure while committing leaves the outcome unknown because the server may
// have processed the End Transaction Request before the connection broke. An
// abort failure simply reports the network error; the transaction is already
// discarded on the client side.
func (t *Transaction) finishTransportError(commit bool, err error) error {
	if commit {
		return fmt.Errorf("%w: %w", ErrTransactionCommitUncertain, err)
	}
	return err
}

func expectedApplicationTag(kind string) (uint8, error) {
	switch kind {
	case "add":
		return ApplicationAddResponse, nil
	case "modify":
		return ApplicationModifyResponse, nil
	case "delete":
		return ApplicationDelResponse, nil
	case "moddn":
		return ApplicationModifyDNResponse, nil
	default:
		return 0, fmt.Errorf("unsupported transaction operation kind %q", kind)
	}
}

func decodeResponseControls(packet *ber.Packet) ([]Control, error) {
	controlsChild, ok, err := packetChildIfPresent(packet, 2)
	if err != nil {
		return nil, err
	}
	controls := make([]Control, 0)
	if !ok {
		return controls, nil
	}
	for _, child := range controlsChild.Children {
		control, err := DecodeControl(child)
		if err != nil {
			return nil, fmt.Errorf("failed to decode response control: %w", err)
		}
		controls = append(controls, control)
	}
	return controls, nil
}

// parseEndTransactionResponseValue decodes the EndTxnResponseValue
// (RFC 5805 section 2.3.2) optionally carried in responseValue [11] of a
// failed End Transaction response. Each element is a SEQUENCE { messageID,
// op UpdateOperation } where the embedded update response (LDAPResult) is
// optional: servers are only required to return the messageID.
func parseEndTransactionResponseValue(packet *ber.Packet) []TransactionOperationResult {
	extResp, err := packetChild(packet, 1)
	if err != nil {
		return nil
	}

	var value *ber.Packet
	for _, child := range extResp.Children {
		if child.ClassType == ber.ClassContext && child.Tag == 11 {
			value = child
			break
		}
	}
	if value == nil || value.Data == nil {
		return nil
	}

	inner, err := ber.DecodePacketErr(value.Data.Bytes())
	if err != nil || inner == nil {
		return nil
	}

	results := make([]TransactionOperationResult, 0, len(inner.Children))
	for _, element := range inner.Children {
		if element == nil {
			continue
		}
		result, ok := decodeFailedOperation(element)
		if !ok {
			continue
		}
		results = append(results, result)
	}
	return results
}

func decodeFailedOperation(element *ber.Packet) (TransactionOperationResult, bool) {
	var result TransactionOperationResult

	messageIDChild, err := packetChild(element, 0)
	// EndTxnResponseValue is a SEQUENCE OF SEQUENCE { messageID MessageID, op
	// UpdateOperation }: the messageID here is a plain universal INTEGER, not
	// an implicitly tagged field.
	if err != nil || messageIDChild.ClassType != ber.ClassUniversal ||
		messageIDChild.Tag != ber.TagInteger {
		return result, false
	}
	data, err := packetData(messageIDChild)
	if err != nil {
		return result, false
	}
	messageID, err := ber.ParseInt64(data)
	if err != nil {
		return result, false
	}
	result.MessageID = messageID

	protoOp, ok, err := packetChildIfPresent(element, 1)
	if err != nil || !ok {
		return result, true
	}

	if codeChild, err := packetChild(protoOp, 0); err == nil {
		if code, err := packetInt64(codeChild); err == nil {
			result.ResultCode = uint16(code)
		}
	}
	if matchedChild, err := packetChild(protoOp, 1); err == nil {
		result.MatchedDN, _ = packetString(matchedChild)
	}
	if diagChild, err := packetChild(protoOp, 2); err == nil {
		result.DiagnosticMessage, _ = packetString(diagChild)
	}

	return result, true
}
