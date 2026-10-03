// Modifier instance is provided to milter handlers to modify email messages

package milter

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net/textproto"
)

// postfix wants LF lines endings. Using CRLF results in double CR sequences.
func crlfToLF(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte{'\r', '\n'}, []byte{'\n'})
}

// Modifier provides access to Macros, Headers and Body data to callback handlers. It also defines a
// number of functions that can be used by callback handlers to modify processing of the email message
//
// A Modifier is only valid for the duration of the callback it was passed to.
// Once the callback returns, the Modifier is deactivated and all modification
// actions called on it return an error without sending any packets.
type Modifier struct {
	Macros  map[string]string
	Headers textproto.MIMEHeader

	session *milterSession

	// err is a sticky error set when a packet write fails. Once set, no
	// further packets are sent by this Modifier.
	err error
}

// ActionOption reports whether the given action capability has been
// negotiated by both the MTA and the milter during OPTNEG, that is, whether
// modification actions requiring it are allowed to be sent.
func (m *Modifier) ActionOption(opt OptAction) bool {
	return m.session != nil && m.session.actions&opt != 0
}

// checkModify verifies that the modifier is still active, that no previous
// write failed, that the required action capability was negotiated by both
// peers and that the session is at a stage where message modification is
// allowed.
func (m *Modifier) checkModify(action string, opt OptAction) error {
	if m.session == nil || m.session.modifier != m {
		return fmt.Errorf("milter: %s: modifier is no longer active, the callback it belongs to has already returned", action)
	}
	if m.err != nil {
		return m.err
	}
	if m.session.actions&opt == 0 {
		return fmt.Errorf("milter: %s: action capability %s was not negotiated by both peers", action, opt)
	}
	if m.session.stage != StageEOM {
		return fmt.Errorf("milter: %s: message modification is only allowed at end of message, current stage is %s", action, m.session.stage)
	}
	return nil
}

// send writes a single packet. A failed write is remembered and prevents any
// further packets from being sent by this Modifier.
func (m *Modifier) send(action string, msg *Message) error {
	if m.err != nil {
		return m.err
	}
	if err := m.session.WritePacket(msg); err != nil {
		m.err = fmt.Errorf("milter: %s: %w", action, err)
		return m.err
	}
	return nil
}

// writeModify validates the action and writes a single packet for it.
func (m *Modifier) writeModify(action string, opt OptAction, msg *Message) error {
	if err := m.checkModify(action, opt); err != nil {
		return err
	}
	return m.send(action, msg)
}

// AddRecipient appends a new envelope recipient for current message
func (m *Modifier) AddRecipient(r string) error {
	data := []byte(fmt.Sprintf("<%s>", r) + null)
	return m.writeModify("AddRecipient", OptAddRcpt, NewResponse('+', data).Response())
}

// DeleteRecipient removes an envelope recipient address from message
func (m *Modifier) DeleteRecipient(r string) error {
	data := []byte(fmt.Sprintf("<%s>", r) + null)
	return m.writeModify("DeleteRecipient", OptRemoveRcpt, NewResponse('-', data).Response())
}

// ReplaceBody substitutes message body with provided body
//
// Bodies larger than MaxBodyChunk are split into multiple packets. CRLF to
// LF conversion happens before chunking so a CRLF pair is never split across
// two packets, which would cause it to be escaped twice or not at all.
func (m *Modifier) ReplaceBody(body []byte) error {
	if err := m.checkModify("ReplaceBody", OptChangeBody); err != nil {
		return err
	}
	body = crlfToLF(body)
	for {
		chunk := body
		if len(chunk) > MaxBodyChunk {
			chunk = chunk[:MaxBodyChunk]
		}
		if err := m.send("ReplaceBody", NewResponse('b', chunk).Response()); err != nil {
			return err
		}
		body = body[len(chunk):]
		if len(body) == 0 {
			return nil
		}
	}
}

// AddHeader appends a new email message header the message
func (m *Modifier) AddHeader(name, value string) error {
	var buffer bytes.Buffer
	buffer.WriteString(name + null)
	buffer.Write(crlfToLF([]byte(value)))
	buffer.WriteString(null)
	return m.writeModify("AddHeader", OptAddHeader, NewResponse('h', buffer.Bytes()).Response())
}

// Quarantine a message by giving a reason to hold it
func (m *Modifier) Quarantine(reason string) error {
	return m.writeModify("Quarantine", OptQuarantine, NewResponse('q', []byte(reason+null)).Response())
}

// ChangeHeader replaces the header at the specified position with a new one.
// The index is per name.
func (m *Modifier) ChangeHeader(index int, name, value string) error {
	var buffer bytes.Buffer
	if err := binary.Write(&buffer, binary.BigEndian, uint32(index)); err != nil {
		return err
	}
	buffer.WriteString(name + null)
	buffer.Write(crlfToLF([]byte(value)))
	buffer.WriteString(null)
	return m.writeModify("ChangeHeader", OptChangeHeader, NewResponse('m', buffer.Bytes()).Response())
}

// InsertHeader inserts the header at the specified position
func (m *Modifier) InsertHeader(index int, name, value string) error {
	var buffer bytes.Buffer
	if err := binary.Write(&buffer, binary.BigEndian, uint32(index)); err != nil {
		return err
	}
	buffer.WriteString(name + null)
	buffer.Write(crlfToLF([]byte(value)))
	buffer.WriteString(null)
	return m.writeModify("InsertHeader", OptAddHeader, NewResponse('i', buffer.Bytes()).Response())
}

// ChangeFrom replaces the FROM envelope header with a new one
func (m *Modifier) ChangeFrom(value string) error {
	data := []byte(value + null)
	return m.writeModify("ChangeFrom", OptChangeFrom, NewResponse('e', data).Response())
}

// newModifier creates a new Modifier instance from milterSession
func newModifier(s *milterSession) *Modifier {
	return &Modifier{
		Macros:  s.macros,
		Headers: s.headers,
		session: s,
	}
}
