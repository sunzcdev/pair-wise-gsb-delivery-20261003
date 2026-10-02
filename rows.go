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

	// cursorOpen is true while a server-side cursor is open for this result
	// set. Rows arrive in batches instead of as a continuous stream.
	cursorOpen bool
}

type mysqlRows struct {
	mc     *mysqlConn
	rs     resultSet
	finish func()
}

type binaryRows struct {
	mysqlRows

	// stmtID identifies the prepared statement whose cursor is open. It is
	// only valid when rs.cursorOpen is true.
	stmtID uint32

	// maxPacketRows is the number of rows requested in each COM_STMT_FETCH.
	maxPacketRows uint32

	// batchStarted is false until the first packet of the current
	// COM_STMT_FETCH batch has been read.
	batchStarted bool
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
		err = mc.skipRows()
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

// Close releases a server-side cursor when one is open; otherwise it falls
// back to the streaming implementation.
func (rows *binaryRows) Close() error {
	if !rows.rs.cursorOpen {
		return rows.mysqlRows.Close()
	}

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

	err := rows.closeCursor()

	rows.rs.cursorOpen = false
	rows.mc = nil
	return err
}

func (rows *mysqlRows) HasNextResultSet() (b bool) {
	if rows.mc == nil {
		return false
	}
	return rows.mc.status&statusMoreResultsExists != 0
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
		if err := rows.mc.skipRows(); err != nil {
			return 0, err
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

		if rows.rs.cursorOpen {
			return rows.nextCursorRow(dest)
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

// nextCursorRow serves rows out of server-side cursor batches. It reads raw
// packets so the batch-terminating EOF can be distinguished from a row; when a
// batch is exhausted it issues another COM_STMT_FETCH. The cursor is considered
// finished when a batch EOF carries SERVER_STATUS_LAST_ROW_SENT.
func (rows *binaryRows) nextCursorRow(dest []driver.Value) error {
	mc := rows.mc

	for {
		if rows.rs.done {
			return io.EOF
		}

		// Issue a fetch before the first packet of every batch.
		if !rows.batchStarted {
			if err := rows.writeFetchPacket(); err != nil {
				return err
			}
			rows.batchStarted = true
		}

		data, err := mc.readPacket()
		if err != nil {
			return err
		}

		switch data[0] {
		case iOK:
			if err := rows.decodeRow(data, dest); err != nil {
				return err
			}
			rows.convertTinyInt1ToBool(dest)
			return nil

		case iEOF:
			rows.applyCursorEOF(data)
			rows.batchStarted = false
			if rows.rs.done {
				return io.EOF
			}
			continue

		case iERR:
			rows.rs.done = true
			rows.rs.cursorOpen = false
			rows.mc = nil
			return mc.handleErrorPacket(data)
		}
	}
}

// applyCursorEOF reads the status flags out of a COM_STMT_FETCH terminating EOF
// packet (or 0xFE-headed OK packet) and marks the result set finished when the
// server signals the last row.
func (rows *binaryRows) applyCursorEOF(data []byte) {
	rows.mc.status = readResultEndStatus(rows.mc, data)

	if rows.mc.status&statusLastRowSent != 0 {
		rows.rs.done = true
		rows.rs.cursorOpen = false
	}
}

// closeCursor drains a server-side cursor that still holds rows by fetching
// and discarding the remaining batches. The cursor is closed server-side once
// the final batch EOF carries SERVER_STATUS_LAST_ROW_SENT. It never leaves
// unread packets on the stream.
func (rows *binaryRows) closeCursor() error {
	mc := rows.mc

	for !rows.rs.done {
		for {
			// A batch terminating EOF may already be buffered (it can arrive in
			// the same segment as the last rows). Read it before issuing the
			// next fetch so the shared read/write buffer is not busy.
			if !rows.batchStarted {
				if err := rows.writeFetchPacket(); err != nil {
					return err
				}
				rows.batchStarted = true
			}

			data, err := mc.readPacket()
			if err != nil {
				return err
			}
			switch data[0] {
			case iEOF:
				rows.applyCursorEOF(data)
				rows.batchStarted = false
			case iERR:
				rows.rs.done = true
				rows.rs.cursorOpen = false
				return mc.handleErrorPacket(data)
			}
			if rows.rs.done {
				break
			}
		}
	}

	rows.rs.cursorOpen = false

	handleOk := mc.clearResult()
	return handleOk.discardResults()
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
