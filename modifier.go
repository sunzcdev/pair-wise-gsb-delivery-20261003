// Modifier instance is provided to milter handlers to modify email messages

package milter

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net/textproto"
	"sync"
)

// postfix wants LF lines endings. Using CRLF results in double CR sequences.
func crlfToLF(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte{'\r', '\n'}, []byte{'\n'})
}

// Modifier provides access to Macros, Headers and Body data to callback
// handlers. It also defines a number of functions that can be used by callback
// handlers to modify processing of the email message.
//
// A Modifier is only usable while the callback it was passed to is running: it
// expires as soon as that callback returns. Modification actions are only
// allowed during the end-of-message (Body) callback and only succeed when the
// required action capability was offered by the MTA during negotiation. An
// action that fails validation never produces a packet; after a write failure
// the Modifier is invalidated and all subsequent actions fail as well.
type Modifier struct {
	Macros  map[string]string
	Headers textproto.MIMEHeader

	mu          sync.Mutex
	actions     OptAction
	stage       Stage
	done        bool
	writeFail   error
	writePacket func(*Message) error
}

// Errors returned by Modifier methods.
var (
	// ErrExpired is returned when using a Modifier after the callback it was
	// passed to has finished.
	ErrExpired = errors.New("milter: modifier: expired")
	// ErrInvalid is returned when using a Modifier after a previous
	// modification failed to be sent to the MTA.
	ErrInvalid = errors.New("milter: modifier: invalidated by write failure")
)

// ActionNotSupportedError indicates that a modification action was attempted
// although the MTA did not negotiate the capability required to perform it.
type ActionNotSupportedError struct {
	// Action is the modification action that was attempted.
	Action ModifyActCode
	// Required is the capability the MTA failed to offer.
	Required OptAction
}

func (e *ActionNotSupportedError) Error() string {
	return fmt.Sprintf("milter: modifier: action %c (%s) not supported, required capability %s",
		e.Action, modifyActName(e.Action), e.Required)
}

// ActionNotAllowedError indicates that a modification action was attempted in
// a protocol stage that does not allow it.
type ActionNotAllowedError struct {
	// Action is the modification action that was attempted.
	Action ModifyActCode
	// Stage is the protocol stage the action was attempted in.
	Stage Stage
}

func (e *ActionNotAllowedError) Error() string {
	return fmt.Sprintf("milter: modifier: action %c (%s) not allowed in stage %d",
		e.Action, modifyActName(e.Action), e.Stage)
}

// WriteError wraps the transport error returned while sending a modification
// action. The failing action may have been delivered to the MTA partially.
type WriteError struct {
	// Action is the modification action that failed to be sent.
	Action ModifyActCode
	// Err is the underlying write error.
	Err error
}

func (e *WriteError) Error() string {
	return fmt.Sprintf("milter: modifier: writing action %c (%s): %v",
		e.Action, modifyActName(e.Action), e.Err)
}

func (e *WriteError) Unwrap() error {
	return e.Err
}

func modifyActName(code ModifyActCode) string {
	switch code {
	case ActAddRcpt:
		return "SMFIR_ADDRCPT"
	case ActDelRcpt:
		return "SMFIR_DELRCPT"
	case ActReplBody:
		return "SMFIR_REPLBODY"
	case ActAddHeader:
		return "SMFIR_ADDHEADER"
	case ActChangeHeader:
		return "SMFIR_CHGHEADER"
	case ActInsertHeader:
		return "SMFIR_INSHEADER"
	case ActQuarantine:
		return "SMFIR_QUARANTINE"
	case ActChangeFrom:
		return "SMFIR_CHGFROM"
	default:
		return "UNKNOWN"
	}
}

// actionRequired maps modification actions to the capability that must be
// offered by both sides during negotiation.
func actionRequired(code ModifyActCode) OptAction {
	switch code {
	case ActAddHeader, ActInsertHeader:
		return OptAddHeader
	case ActReplBody:
		return OptChangeBody
	case ActAddRcpt:
		return OptAddRcpt
	case ActDelRcpt:
		return OptRemoveRcpt
	case ActChangeHeader:
		return OptChangeHeader
	case ActQuarantine:
		return OptQuarantine
	case ActChangeFrom:
		return OptChangeFrom
	default:
		return 0
	}
}

// ActionSupported reports whether the MTA negotiated support for the action.
// It returns false for unknown actions and for modifiers that are no longer
// usable.
func (m *Modifier) ActionSupported(code ModifyActCode) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.done || m.writeFail != nil {
		return false
	}
	required := actionRequired(code)
	if required == 0 {
		return false
	}
	return m.actions&required != 0
}

// check verifies that the modification action may be written and returns an
// error naming the action and the missing capability or offending stage.
func (m *Modifier) check(code ModifyActCode) error {
	if m.done {
		return ErrExpired
	}
	if m.writeFail != nil {
		return fmt.Errorf("%w: action %c (%s): %v", ErrInvalid, code, modifyActName(code), m.writeFail)
	}
	if m.stage != StageEOM {
		return &ActionNotAllowedError{Action: code, Stage: m.stage}
	}
	required := actionRequired(code)
	if required == 0 {
		return &ActionNotSupportedError{Action: code, Required: 0}
	}
	if m.actions&required == 0 {
		return &ActionNotSupportedError{Action: code, Required: required}
	}
	return nil
}

// writeAction validates the action and, only if validation succeeds, sends a
// single packet. No packet is ever written when an error is returned unless the
// write itself fails, in which case the Modifier is invalidated.
func (m *Modifier) writeAction(code ModifyActCode, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.check(code); err != nil {
		return err
	}
	if err := m.writePacket(NewResponse(byte(code), data).Response()); err != nil {
		m.writeFail = err
		return &WriteError{Action: code, Err: err}
	}
	return nil
}

// finish marks the Modifier as expired: all later uses fail with ErrExpired
// and can no longer produce any packet.
func (m *Modifier) finish() {
	m.mu.Lock()
	m.done = true
	m.mu.Unlock()
}

// AddRecipient appends a new envelope recipient for current message.
// Requires OptAddRcpt and the end-of-message stage.
func (m *Modifier) AddRecipient(r string) error {
	data := []byte(fmt.Sprintf("<%s>", r) + null)
	return m.writeAction(ActAddRcpt, data)
}

// DeleteRecipient removes an envelope recipient address from message.
// Requires OptRemoveRcpt and the end-of-message stage.
func (m *Modifier) DeleteRecipient(r string) error {
	data := []byte(fmt.Sprintf("<%s>", r) + null)
	return m.writeAction(ActDelRcpt, data)
}

// ReplaceBody substitutes message body with provided body. Requires
// OptChangeBody and the end-of-message stage.
//
// The body is sent as a sequence of chunks no larger than MaxBodyChunk. CRLF
// sequences are normalized to LF over the whole input, including sequences
// spanning a chunk boundary.
func (m *Modifier) ReplaceBody(body []byte) error {
	code := ActReplBody

	m.mu.Lock()
	if err := m.check(code); err != nil {
		m.mu.Unlock()
		return err
	}

	// Normalize CRLF on the full input first so a CR/LF pair spanning a chunk
	// boundary is neither dropped nor converted twice.
	body = crlfToLF(body)
	for {
		chunk := body
		if len(chunk) > MaxBodyChunk {
			chunk = chunk[:MaxBodyChunk]
		}
		body = body[len(chunk):]
		// A single zero-length chunk still signals "replace the whole body
		// with nothing" and terminates the replacement.
		if err := m.writePacket(NewResponse(byte(code), chunk).Response()); err != nil {
			m.writeFail = err
			m.mu.Unlock()
			return &WriteError{Action: code, Err: err}
		}
		if len(body) == 0 {
			break
		}
	}
	m.mu.Unlock()
	return nil
}

// AddHeader appends a new email message header the message. Requires
// OptAddHeader and the end-of-message stage.
func (m *Modifier) AddHeader(name, value string) error {
	var buffer bytes.Buffer
	buffer.WriteString(name + null)
	buffer.Write(crlfToLF([]byte(value)))
	buffer.WriteString(null)
	return m.writeAction(ActAddHeader, buffer.Bytes())
}

// Quarantine a message by giving a reason to hold it. Requires OptQuarantine
// and the end-of-message stage.
func (m *Modifier) Quarantine(reason string) error {
	return m.writeAction(ActQuarantine, []byte(reason+null))
}

// ChangeHeader replaces the header at the specified position with a new one.
// The index is per name. Requires OptChangeHeader and the end-of-message
// stage.
func (m *Modifier) ChangeHeader(index int, name, value string) error {
	var buffer bytes.Buffer
	if err := binary.Write(&buffer, binary.BigEndian, uint32(index)); err != nil {
		return fmt.Errorf("milter: modifier: action %c (%s): %w",
			ActChangeHeader, modifyActName(ActChangeHeader), err)
	}
	buffer.WriteString(name + null)
	buffer.Write(crlfToLF([]byte(value)))
	buffer.WriteString(null)
	return m.writeAction(ActChangeHeader, buffer.Bytes())
}

// InsertHeader inserts the header at the specified position. Requires
// OptAddHeader and the end-of-message stage.
func (m *Modifier) InsertHeader(index int, name, value string) error {
	var buffer bytes.Buffer
	if err := binary.Write(&buffer, binary.BigEndian, uint32(index)); err != nil {
		return fmt.Errorf("milter: modifier: action %c (%s): %w",
			ActInsertHeader, modifyActName(ActInsertHeader), err)
	}
	buffer.WriteString(name + null)
	buffer.Write(crlfToLF([]byte(value)))
	buffer.WriteString(null)
	return m.writeAction(ActInsertHeader, buffer.Bytes())
}

// ChangeFrom replaces the FROM envelope header with a new one. Requires
// OptChangeFrom and the end-of-message stage.
func (m *Modifier) ChangeFrom(value string) error {
	data := []byte(value + null)
	return m.writeAction(ActChangeFrom, data)
}

// newModifier creates a new Modifier instance from milterSession
func newModifier(s *milterSession, stage Stage) *Modifier {
	return &Modifier{
		Macros:      s.macros,
		Headers:     s.headers,
		actions:     s.actions,
		stage:       stage,
		writePacket: s.WritePacket,
	}
}
