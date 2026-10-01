// Go MySQL Driver - A MySQL-Driver for Go's database/sql package
//
// Copyright 2026 The Go-MySQL-Driver Authors. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

package mysql

import (
	"bytes"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// Size limits for the components of an XID, as defined by the XA
// specification and enforced by MySQL.
const (
	// XAMaxGTrIDLength is the maximum length in bytes of the global
	// transaction identifier part of an XID.
	XAMaxGTrIDLength = 64
	// XAMaxBQualLength is the maximum length in bytes of the branch
	// qualifier part of an XID.
	XAMaxBQualLength = 64
)

var (
	// ErrXAIndeterminate is returned (wrapped) when the response to an XA
	// command was lost after the command was sent to the server. The
	// outcome of the branch is unknown: it may or may not have been
	// prepared, committed or rolled back. The caller must not assume
	// either outcome; in particular, the absence of the XID from a later
	// XARecover call is not proof that a commit succeeded. Resolve the
	// branch by retrying the decision on a (new) connection.
	ErrXAIndeterminate = errors.New("mysql: xa branch outcome is indeterminate")

	// ErrXAIllegalState is returned (wrapped) when an XA branch operation
	// is attempted in a branch state that does not allow it. The error is
	// detected locally; no command is sent to the server.
	ErrXAIllegalState = errors.New("mysql: illegal xa branch state transition")
)

// XID is a global transaction identifier as defined by the XA
// specification. It consists of a format identifier, a global transaction
// identifier (gtrid) and a branch qualifier (bqual). All three components
// are preserved verbatim by the driver and are encoded into XA statements
// without loss.
type XID struct {
	// FormatID identifies the format of the gtrid and bqual parts.
	// The value 0 means the OSI CCR format; negative values are reserved.
	FormatID int32
	// GlobalTransactionID (gtrid) identifies the global transaction.
	// It must be 1 to XAMaxGTrIDLength bytes long and may contain
	// arbitrary binary data.
	GlobalTransactionID []byte
	// BranchQualifier (bqual) identifies the branch within the global
	// transaction. It may be empty and at most XAMaxBQualLength bytes
	// long, and may contain arbitrary binary data.
	BranchQualifier []byte
}

// Validate checks an XID to be sent to the server against the XA size
// limits.
func (xid XID) Validate() error {
	if n := len(xid.GlobalTransactionID); n == 0 || n > XAMaxGTrIDLength {
		return fmt.Errorf("mysql: xa gtrid length %d is out of range [1, %d]", n, XAMaxGTrIDLength)
	}
	if n := len(xid.BranchQualifier); n > XAMaxBQualLength {
		return fmt.Errorf("mysql: xa bqual length %d exceeds %d", n, XAMaxBQualLength)
	}
	return nil
}

// Equal reports whether xid and other are identical in all three
// components.
func (xid XID) Equal(other XID) bool {
	return xid.FormatID == other.FormatID &&
		bytes.Equal(xid.GlobalTransactionID, other.GlobalTransactionID) &&
		bytes.Equal(xid.BranchQualifier, other.BranchQualifier)
}

// clone returns a deep copy of the XID.
func (xid XID) clone() XID {
	return XID{
		FormatID:            xid.FormatID,
		GlobalTransactionID: bytes.Clone(xid.GlobalTransactionID),
		BranchQualifier:     bytes.Clone(xid.BranchQualifier),
	}
}

// String returns the XID in the SQL literal form used by MySQL XA
// statements: 'gtrid','bqual',formatID. Backslash escaping is used, which
// is the correct form unless the session runs with NO_BACKSLASH_ESCAPES.
func (xid XID) String() string {
	return string(xid.appendSQL(nil, false))
}

// appendSQL appends the SQL literal form of the XID to buf, honoring the
// NO_BACKSLASH_ESCAPES server mode for string literal escaping.
func (xid XID) appendSQL(buf []byte, noBackslashEscapes bool) []byte {
	escape := escapeBytesBackslash
	if noBackslashEscapes {
		escape = escapeBytesQuotes
	}
	buf = escape(buf, xid.GlobalTransactionID, false)
	buf = append(buf, ',')
	buf = escape(buf, xid.BranchQualifier, false)
	buf = append(buf, ',')
	return strconv.AppendInt(buf, int64(xid.FormatID), 10)
}

// sql returns the SQL literal form of the XID for the given connection.
func (xid XID) sql(mc *mysqlConn) string {
	return string(xid.appendSQL(nil, mc.status&statusNoBackslashEscapes != 0))
}

// XABranchState is the state of an XA transaction branch.
type XABranchState uint8

const (
	// XAStateActive means the branch was started and work is in progress.
	XAStateActive XABranchState = iota
	// XAStateIdle means the branch was ended but not yet prepared.
	XAStateIdle
	// XAStatePrepared means the branch was prepared and detached from its
	// connection. It persists on the server until it is committed or
	// rolled back, on any connection.
	XAStatePrepared
	// XAStateCommitted means the branch was committed.
	XAStateCommitted
	// XAStateRolledBack means the branch was rolled back.
	XAStateRolledBack
	// XAStateUnknown means the outcome of the last operation is
	// indeterminate because its response was lost.
	XAStateUnknown
)

func (s XABranchState) String() string {
	switch s {
	case XAStateActive:
		return "active"
	case XAStateIdle:
		return "idle"
	case XAStatePrepared:
		return "prepared"
	case XAStateCommitted:
		return "committed"
	case XAStateRolledBack:
		return "rolled back"
	case XAStateUnknown:
		return "unknown"
	}
	return "invalid"
}

// XAConn manages XA transaction branches on a single connection.
//
// *mysqlConn implements this interface. To use it with database/sql,
// obtain the raw connection from sql.Conn.Raw:
//
//	var xa mysql.XAConn
//	err := conn.Raw(func(raw any) error {
//		c, ok := raw.(mysql.XAConn)
//		if !ok {
//			return errors.New("not a mysql connection")
//		}
//		xa = c
//		return nil
//	})
type XAConn interface {
	// XAStart starts a new branch of a global transaction on this
	// connection (XA START) and returns a handle for it.
	XAStart(xid XID) (*XABranch, error)
	// XARecover returns the XIDs of all branches the server holds in
	// prepared state (XA RECOVER). It may be called on any connection,
	// including one different from the connection a branch was prepared
	// on.
	XARecover() ([]XID, error)
	// XACommit commits the prepared branch identified by xid (XA COMMIT).
	XACommit(xid XID) error
	// XARollback rolls back the branch identified by xid (XA ROLLBACK).
	XARollback(xid XID) error
}

// XABranch is a handle for one branch of a distributed (XA) transaction.
//
// A branch is tied to its connection while it is active or idle. After a
// successful Prepare it is detached: it then exists on the server
// independently of any connection and survives connection loss. Unlike a
// plain local transaction, a prepared branch is never rolled back
// implicitly when a connection is closed or returned to the pool; it must
// be resolved explicitly with XACommit or XARollback, which may run on a
// different connection. Prepared branches left behind by failed
// coordinators can be discovered with XARecover.
type XABranch struct {
	xid   XID
	state XABranchState
	mc    *mysqlConn // attached connection; nil once detached
}

// XID returns the identifier of the branch.
func (b *XABranch) XID() XID {
	return b.xid.clone()
}

// State returns the current state of the branch.
func (b *XABranch) State() XABranchState {
	return b.state
}

// End disassociates the branch from the current work (XA END). The branch
// must be active.
func (b *XABranch) End() error {
	if b.state != XAStateActive {
		return fmt.Errorf("%w: cannot end branch in state %s", ErrXAIllegalState, b.state)
	}
	if err := b.mc.xaExec("XA END " + b.xid.sql(b.mc)); err != nil {
		return err
	}
	b.state = XAStateIdle
	return nil
}

// Prepare asks the server to prepare the branch (XA PREPARE). The branch
// must be idle. On success the branch is detached from its connection and
// survives its closure. If the outcome becomes indeterminate, the branch
// is detached as well and its state is XAStateUnknown.
func (b *XABranch) Prepare() error {
	if b.state != XAStateIdle {
		return fmt.Errorf("%w: cannot prepare branch in state %s", ErrXAIllegalState, b.state)
	}
	err := b.mc.xaExec("XA PREPARE " + b.xid.sql(b.mc))
	if err == nil {
		b.state = XAStatePrepared
		b.mc = nil
		return nil
	}
	if errors.Is(err, ErrXAIndeterminate) {
		b.state = XAStateUnknown
		b.mc = nil
	}
	return err
}

// Commit commits the prepared branch (XA COMMIT) using conn, which may be
// any connection, including one different from the connection the branch
// was started on. The branch must be prepared.
func (b *XABranch) Commit(conn XAConn) error {
	if b.state != XAStatePrepared {
		return fmt.Errorf("%w: cannot commit branch in state %s", ErrXAIllegalState, b.state)
	}
	if conn == nil {
		return errors.New("mysql: xa branch is detached; a connection is required to commit")
	}
	err := conn.XACommit(b.xid)
	if err != nil {
		if errors.Is(err, ErrXAIndeterminate) {
			b.state = XAStateUnknown
		}
		return err
	}
	b.state = XAStateCommitted
	return nil
}

// Rollback rolls the branch back (XA ROLLBACK). A prepared branch may be
// rolled back using any connection; an idle branch is rolled back using
// its attached connection and conn is ignored. The final decision is
// always made by the caller; the driver never rolls a branch back
// implicitly.
func (b *XABranch) Rollback(conn XAConn) error {
	switch b.state {
	case XAStateIdle:
		conn = b.mc
	case XAStatePrepared:
		if conn == nil {
			return errors.New("mysql: xa branch is detached; a connection is required to roll back")
		}
	default:
		return fmt.Errorf("%w: cannot roll back branch in state %s", ErrXAIllegalState, b.state)
	}
	err := conn.XARollback(b.xid)
	if err != nil {
		if errors.Is(err, ErrXAIndeterminate) {
			b.state = XAStateUnknown
			b.mc = nil
		}
		return err
	}
	b.state = XAStateRolledBack
	b.mc = nil
	return nil
}

// xaExec executes an XA statement and classifies failures for XA
// semantics: a write failure means the statement was not sent and the
// branch is unaffected; a lost response after a successful send makes the
// outcome indeterminate; a server error packet is a definitive rejection.
func (mc *mysqlConn) xaExec(query string) error {
	handleOk := mc.clearResult()
	if err := mc.writeCommandPacketStr(comQuery, query); err != nil {
		return mc.markBadConn(err)
	}
	if _, _, err := handleOk.readResultSetHeaderPacket(); err != nil {
		var merr *MySQLError
		if errors.As(err, &merr) {
			return merr
		}
		return fmt.Errorf("%w: %v", ErrXAIndeterminate, err)
	}
	return handleOk.discardResults()
}

// XAStart implements XAConn.
func (mc *mysqlConn) XAStart(xid XID) (*XABranch, error) {
	if err := xid.Validate(); err != nil {
		return nil, err
	}
	if mc.closed.Load() {
		return nil, driver.ErrBadConn
	}
	if err := mc.xaExec("XA START " + xid.sql(mc)); err != nil {
		return nil, err
	}
	return &XABranch{xid: xid.clone(), state: XAStateActive, mc: mc}, nil
}

// XACommit implements XAConn.
func (mc *mysqlConn) XACommit(xid XID) error {
	if err := xid.Validate(); err != nil {
		return err
	}
	if mc.closed.Load() {
		return driver.ErrBadConn
	}
	return mc.xaExec("XA COMMIT " + xid.sql(mc))
}

// XARollback implements XAConn.
func (mc *mysqlConn) XARollback(xid XID) error {
	if err := xid.Validate(); err != nil {
		return err
	}
	if mc.closed.Load() {
		return driver.ErrBadConn
	}
	return mc.xaExec("XA ROLLBACK " + xid.sql(mc))
}

// XARecover implements XAConn.
func (mc *mysqlConn) XARecover() ([]XID, error) {
	if mc.closed.Load() {
		return nil, driver.ErrBadConn
	}
	rows, err := mc.query("XA RECOVER", nil)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	if len(rows.rs.columns) != 4 {
		return nil, fmt.Errorf("mysql: unexpected XA RECOVER result with %d columns", len(rows.rs.columns))
	}

	var xids []XID
	dest := make([]driver.Value, 4)
	for {
		if err := rows.Next(dest); err != nil {
			if err == io.EOF {
				return xids, nil
			}
			return nil, err
		}
		xid, err := parseXARecoverRow(dest)
		if err != nil {
			return nil, err
		}
		xids = append(xids, xid)
	}
}

// parseXARecoverRow parses one XA RECOVER result row. The columns are
// formatID, gtrid_length, bqual_length and data, where data is the
// concatenation of gtrid and bqual.
func parseXARecoverRow(dest []driver.Value) (XID, error) {
	var xid XID

	text, ok := dest[3].([]byte)
	if !ok {
		return xid, fmt.Errorf("mysql: malformed XA RECOVER row: %w", ErrMalformPkt)
	}
	formatID, err := xaRecoverInt(dest[0])
	if err != nil {
		return xid, err
	}
	gtridLen, err := xaRecoverInt(dest[1])
	if err != nil {
		return xid, err
	}
	bqualLen, err := xaRecoverInt(dest[2])
	if err != nil {
		return xid, err
	}
	if gtridLen < 0 || bqualLen < 0 || gtridLen+bqualLen > int64(len(text)) {
		return xid, fmt.Errorf("mysql: malformed XA RECOVER row: %w", ErrMalformPkt)
	}

	xid.FormatID = int32(formatID)
	xid.GlobalTransactionID = bytes.Clone(text[:gtridLen])
	xid.BranchQualifier = bytes.Clone(text[gtridLen : gtridLen+bqualLen])
	return xid, nil
}

func xaRecoverInt(v driver.Value) (int64, error) {
	switch v := v.(type) {
	case int64:
		return v, nil
	case []byte:
		n, err := strconv.ParseInt(string(v), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("mysql: malformed XA RECOVER row: %w", ErrMalformPkt)
		}
		return n, nil
	default:
		return 0, fmt.Errorf("mysql: malformed XA RECOVER row: %w", ErrMalformPkt)
	}
}

var _ XAConn = (*mysqlConn)(nil)
