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
	"fmt"
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
	mc             *mysqlConn
	rs             resultSet
	resultSetIndex int
	// columnCache[0] is the cache of the first result set, which is the one
	// stored on the prepared statement (mysqlStmt.columns) and shared across
	// repeated executions. Subsequent entries cache later result sets of a
	// multi-result statement.
	columnCache [][]mysqlField
	finish      func()
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

func (rows *mysqlRows) HasNextResultSet() (b bool) {
	if rows.mc == nil {
		return false
	}
	return rows.mc.status&statusMoreResultsExists != 0
}

func (rows *mysqlRows) nextResultSet() (int, bool, error) {
	if rows.mc == nil {
		return 0, false, io.EOF
	}
	if err := rows.mc.error(); err != nil {
		return 0, false, err
	}

	// Remove unread packets from stream
	if !rows.rs.done {
		if err := rows.mc.skipRows(); err != nil {
			return 0, false, err
		}
		rows.rs.done = true
	}

	if !rows.HasNextResultSet() {
		rows.mc = nil
		return 0, false, io.EOF
	}
	rows.rs = resultSet{}
	rows.resultSetIndex++
	// rows.mc.affectedRows and rows.mc.insertIds accumulate on each call to
	// nextResultSet.
	resLen, metadataFollows, err := rows.mc.resultUnchanged().readResultSetHeaderPacket()
	if err != nil {
		// Clean up about multi-results flag
		rows.rs.done = true
		rows.mc.status = rows.mc.status & (^statusMoreResultsExists)
	}
	return resLen, metadataFollows, err
}

func (rows *mysqlRows) nextNotEmptyResultSet() (int, bool, error) {
	for {
		resLen, metadataFollows, err := rows.nextResultSet()
		if err != nil {
			return 0, false, err
		}

		if resLen > 0 {
			return resLen, metadataFollows, nil
		}

		rows.rs.done = true
	}
}

func (rows *binaryRows) NextResultSet() error {
	resLen, metadataFollows, err := rows.nextNotEmptyResultSet()
	if err != nil {
		return err
	}

	return rows.readResultColumns(resLen, metadataFollows)
}

func (rows *binaryRows) Next(dest []driver.Value) error {
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

func (rows *textRows) NextResultSet() (err error) {
	resLen, metadataFollows, err := rows.nextNotEmptyResultSet()
	if err != nil {
		return err
	}

	return rows.readResultColumns(resLen, metadataFollows)
}

// readResultColumns resolves the column definitions for the current result
// set. When the server omits them because the statement result shape is
// unchanged, the definitions cached for this result-set position are reused.
func (rows *mysqlRows) readResultColumns(count int, metadataFollows bool) error {
	old := rows.cachedResultColumns()
	if metadataFollows {
		columns, err := rows.mc.readColumns(count, old)
		if err != nil {
			return err
		}
		rows.setCachedResultColumns(columns)
		rows.rs.columns = columns
		return nil
	}

	if err := rows.mc.readMetadataEOF(); err != nil {
		return err
	}
	if len(old) != count {
		return fmt.Errorf("%w: cached columns=%d, server result columns=%d",
			ErrNoCachedMetadata, len(old), count)
	}
	rows.rs.columns = old
	return nil
}

// cachedResultColumns returns the definitions cached for the result set at
// the current position.
func (rows *mysqlRows) cachedResultColumns() []mysqlField {
	idx := rows.resultSetIndex
	if idx < len(rows.columnCache) {
		return rows.columnCache[idx]
	}
	return nil
}

// setCachedResultColumns stores the received definitions for the result set
// at the current position.
func (rows *mysqlRows) setCachedResultColumns(columns []mysqlField) {
	for len(rows.columnCache) <= rows.resultSetIndex {
		rows.columnCache = append(rows.columnCache, nil)
	}
	rows.columnCache[rows.resultSetIndex] = columns
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
