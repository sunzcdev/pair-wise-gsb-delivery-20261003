// Go MySQL Driver - A MySQL-Driver for Go's database/sql package
//
// Copyright 2012 The Go-MySQL-Driver Authors. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

package mysql

import (
	"database/sql"
	"database/sql/driver"
	"io"
	"math"
	"reflect"
)

type resultSet struct {
	columns     []mysqlField
	columnNames []string
	done        bool
}

type mysqlRows struct {
	mc     *mysqlConn
	rs     resultSet
	finish func()

	// Server-side cursor state. Only used by binaryRows when the statement
	// was executed with CURSOR_TYPE_READ_ONLY.
	stmtID    uint32
	fetchSize int
	// cursorOpen is true while a server-side cursor is open and the client
	// may request more batches with COM_STMT_FETCH.
	cursorOpen bool
	// rowInBatch counts the binary row packets already consumed from the
	// current batch. When it reaches fetchSize the next packet is the
	// batch terminating EOF/OK packet.
	rowInBatch int
	// batchRequested tracks whether the current batch has been requested
	// from the server. The first batch is not sent until the first call to
	// Next.
	batchRequested bool
}

type binaryRows struct {
	mysqlRows
}

type textRows struct {
	mysqlRows
}

func (rows *mysqlRows) Columns() []string {
	if rows.rs.columnNames != nil {
		return rows.rs.columnNames
	}

	columns := make([]string, len(rows.rs.columns))
	if rows.mc != nil && rows.mc.cfg.ColumnsWithAlias {
		for i := range columns {
			if tableName := rows.rs.columns[i].tableName; len(tableName) > 0 {
				columns[i] = tableName + "." + rows.rs.columns[i].name
			} else {
				columns[i] = rows.rs.columns[i].name
			}
		}
	} else {
		for i := range columns {
			columns[i] = rows.rs.columns[i].name
		}
	}

	rows.rs.columnNames = columns
	return columns
}

func (rows *mysqlRows) tinyInt1IsBool(i int) bool {
	if rows.mc == nil || !rows.mc.cfg.tinyInt1IsBool {
		return false
	}
	column := rows.rs.columns[i]
	return column.fieldType == fieldTypeTiny &&
		column.length == 1 &&
		column.flags&(flagUnsigned|flagZeroFill) == 0
}

func (rows *mysqlRows) ColumnTypeDatabaseTypeName(i int) string {
	if rows.tinyInt1IsBool(i) {
		return "BOOLEAN"
	}
	return rows.rs.columns[i].typeDatabaseName()
}

// func (rows *mysqlRows) ColumnTypeLength(i int) (length int64, ok bool) {
// 	return int64(rows.rs.columns[i].length), true
// }

func (rows *mysqlRows) ColumnTypeNullable(i int) (nullable, ok bool) {
	return rows.rs.columns[i].flags&flagNotNULL == 0, true
}

func (rows *mysqlRows) ColumnTypePrecisionScale(i int) (int64, int64, bool) {
	column := rows.rs.columns[i]
	decimals := int64(column.decimals)

	switch column.fieldType {
	case fieldTypeDecimal, fieldTypeNewDecimal:
		precision := int64(column.length)
		if column.flags&flagUnsigned == 0 {
			precision-- // signed decimals include space for the sign
		}
		if decimals > 0 {
			precision-- // decimals with a scale include a decimal point
		}
		return precision, decimals, true
	case fieldTypeTimestamp, fieldTypeDateTime, fieldTypeTime:
		return decimals, decimals, true
	case fieldTypeFloat, fieldTypeDouble:
		if decimals == 0x1f {
			return math.MaxInt64, math.MaxInt64, true
		}
		return math.MaxInt64, decimals, true
	}

	return 0, 0, false
}

func (rows *mysqlRows) ColumnTypeScanType(i int) reflect.Type {
	if rows.tinyInt1IsBool(i) {
		if rows.rs.columns[i].flags&flagNotNULL != 0 {
			return reflect.TypeFor[bool]()
		}
		return reflect.TypeFor[sql.NullBool]()
	}
	return rows.rs.columns[i].scanType()
}

func (rows *mysqlRows) convertTinyInt1ToBool(dest []driver.Value) {
	if rows.mc == nil || !rows.mc.cfg.tinyInt1IsBool {
		return
	}
	for i, v := range dest {
		if !rows.tinyInt1IsBool(i) || v == nil {
			continue
		}
		if n, ok := v.(int64); ok {
			dest[i] = n != 0
		}
	}
}

func (rows *mysqlRows) Close() (err error) {
	if f := rows.finish; f != nil {
		f()
		rows.finish = nil
	}

	mc := rows.mc
	if mc == nil {
		return nil
	}
	if err := mc.error(); err != nil {
		return err
	}

	// Remove unread packets from stream
	if !rows.rs.done {
		if rows.cursorOpen {
			err = rows.closeCursor()
			rows.cursorOpen = false
		} else {
			err = mc.skipRows()
		}
	}
	if err == nil {
		handleOk := mc.clearResult()
		if err = handleOk.discardResults(); err != nil {
			return err
		}
	}

	rows.mc = nil
	return err
}

func (rows *mysqlRows) HasNextResultSet() (b bool) {
	if rows.mc == nil {
		return false
	}
	return rows.mc.status&statusMoreResultsExists != 0
}

// closeCursor releases a server-side cursor that is still open when the
// caller stops iterating early. Per the protocol a COM_STMT_FETCH with
// numRows = 0 asks the server to close the cursor, and the server replies
// with the final EOF/OK packet carrying SERVER_STATUS_LAST_ROW_SENT. This
// guarantees no cursor or pending rows remain associated with the connection
// before it is reused or returned to the pool.
func (rows *mysqlRows) closeCursor() error {
	mc := rows.mc
	if mc == nil {
		return nil
	}

	// First drain the batch that is already in flight. After the
	// rowInBatch row packets already consumed, this batch still contains
	// fetchSize-rowInBatch row packets followed by one terminating
	// EOF/OK packet. A short final batch ends earlier.
	alreadyFinished := false
	if rows.batchRequested {
		// Read the rest of the in-flight batch (rows plus its terminator).
		for {
			data, err := mc.readPacket()
			if err != nil {
				return err
			}
			if data[0] != iOK {
				if data[0] == iERR {
					rows.mc = nil
					return mc.handleErrorPacket(data)
				}
				mc.status = readResultSetRowStatus(mc, data)
				if mc.status&statusLastRowSent != 0 {
					alreadyFinished = true
				}
				break
			}
			rows.rowInBatch++
		}
	}

	if alreadyFinished {
		return nil
	}

	if err := mc.writeStmtFetchPacket(rows.stmtID, 0); err != nil {
		return err
	}

	// The server closes the cursor and replies with the final EOF/OK
	// packet carrying SERVER_STATUS_LAST_ROW_SENT.
	for {
		data, err := mc.readPacket()
		if err != nil {
			return err
		}
		switch data[0] {
		case iERR:
			rows.mc = nil
			return mc.handleErrorPacket(data)
		case iEOF:
			mc.status = readResultSetRowStatus(mc, data)
			if mc.status&statusLastRowSent != 0 {
				return nil
			}
		}
		// Any other packet is an unexpected trailing row; skip it.
	}
}

func (rows *mysqlRows) nextResultSet() (int, error) {
	if rows.mc == nil {
		return 0, io.EOF
	}
	if err := rows.mc.error(); err != nil {
		return 0, err
	}

	// Remove unread packets from stream
	if !rows.rs.done {
		if rows.cursorOpen {
			if err := rows.closeCursor(); err != nil {
				return 0, err
			}
			rows.cursorOpen = false
		} else {
			if err := rows.mc.skipRows(); err != nil {
				return 0, err
			}
		}
		rows.rs.done = true
	}

	if !rows.HasNextResultSet() {
		rows.mc = nil
		return 0, io.EOF
	}
	rows.rs = resultSet{}
	// rows.mc.affectedRows and rows.mc.insertIds accumulate on each call to
	// nextResultSet.
	resLen, _, err := rows.mc.resultUnchanged().readResultSetHeaderPacket()
	if err != nil {
		// Clean up about multi-results flag
		rows.rs.done = true
		rows.mc.status = rows.mc.status & (^statusMoreResultsExists)
	}
	return resLen, err
}

func (rows *mysqlRows) nextNotEmptyResultSet() (int, error) {
	for {
		resLen, err := rows.nextResultSet()
		if err != nil {
			return 0, err
		}

		if resLen > 0 {
			return resLen, nil
		}

		rows.rs.done = true
	}
}

func (rows *binaryRows) NextResultSet() error {
	resLen, err := rows.nextNotEmptyResultSet()
	if err != nil {
		return err
	}

	rows.rs.columns, err = rows.mc.readColumns(resLen, nil)
	return err
}

func (rows *binaryRows) Next(dest []driver.Value) error {
	if mc := rows.mc; mc != nil {
		if err := mc.error(); err != nil {
			return err
		}

		// Server-side cursor: rows are fetched in batches with
		// COM_STMT_FETCH. Each batch contains up to fetchSize rows
		// followed by a terminating EOF/OK packet.
		if rows.cursorOpen {
			if !rows.batchRequested {
				if err := rows.sendFetch(); err != nil {
					return err
				}
				rows.batchRequested = true
				rows.rowInBatch = 0
			} else if rows.rowInBatch == rows.fetchSize {
				// The previous batch's rows are all consumed; read its
				// terminator and, unless the cursor is exhausted, request
				// the following batch.
				lastRow, err := rows.readBatchTerminator()
				if err != nil {
					return err
				}
				if lastRow {
					rows.cursorOpen = false
					rows.rs.done = true
					if !rows.HasNextResultSet() {
						rows.mc = nil
					}
					return io.EOF
				}
				if err := rows.sendFetch(); err != nil {
					return err
				}
				rows.rowInBatch = 0
			}

			// Read the next packet. It is normally a binary row; if it is
			// already the terminator, the batch contained fewer than
			// fetchSize rows and this is the final packet.
			data, err := mc.readPacket()
			if err != nil {
				return err
			}
			if data[0] == iEOF || data[0] == iERR {
				lastRow, err := rows.handleBatchEnd(data)
				if err != nil {
					return err
				}
				if lastRow {
					rows.cursorOpen = false
					rows.rs.done = true
					if !rows.HasNextResultSet() {
						rows.mc = nil
					}
					return io.EOF
				}
				// Defensive: short batch without LAST_ROW_SENT is illegal
				// for MySQL cursors; surface it rather than loop forever.
				return ErrMalformPkt
			}
			if err := rows.parseRow(data, dest); err != nil {
				return err
			}
			rows.rowInBatch++
			rows.convertTinyInt1ToBool(dest)
			return nil
		}

		// Fetch next row from stream
		if err := rows.readRow(dest); err != nil {
			return err
		}
		rows.convertTinyInt1ToBool(dest)
		return nil
	}
	return io.EOF
}

// sendFetch sends COM_STMT_FETCH for the next batch.
func (rows *binaryRows) sendFetch() error {
	return rows.mc.writeStmtFetchPacket(rows.stmtID, rows.fetchSize)
}

// handleBatchEnd interprets a terminating or error packet that arrived
// before fetchSize rows were consumed. It reports whether the cursor is
// exhausted (SERVER_STATUS_LAST_ROW_SENT).
func (rows *binaryRows) handleBatchEnd(data []byte) (bool, error) {
	if data[0] == iERR {
		mc := rows.mc
		rows.cursorOpen = false
		rows.rs.done = true
		rows.mc = nil
		return false, mc.handleErrorPacket(data)
	}

	mc := rows.mc
	mc.status = readResultSetRowStatus(mc, data)
	return mc.status&statusLastRowSent != 0, nil
}

func (rows *textRows) NextResultSet() (err error) {
	resLen, err := rows.nextNotEmptyResultSet()
	if err != nil {
		return err
	}

	rows.rs.columns, err = rows.mc.readColumns(resLen, nil)
	return err
}

func (rows *textRows) Next(dest []driver.Value) error {
	if mc := rows.mc; mc != nil {
		if err := mc.error(); err != nil {
			return err
		}

		// Fetch next row from stream
		if err := rows.readRow(dest); err != nil {
			return err
		}
		rows.convertTinyInt1ToBool(dest)
		return nil
	}
	return io.EOF
}
