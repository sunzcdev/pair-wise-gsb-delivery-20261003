// Go MySQL Driver - A MySQL-Driver for Go's database/sql package
//
// Copyright 2026 The Go-MySQL-Driver Authors. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

package mysql

// cursorType selects the cursor strategy used for prepared statement result
// sets.
//
// MySQL only supports read-only, non-scrollable cursors. When enabled, the
// driver opens a server-side cursor with COM_STMT_EXECUTE and pulls rows in
// batches with COM_STMT_FETCH instead of receiving the whole result set at
// once. See the "cursors" and "fetchSize" DSN options.
type cursorType byte

const (
	// cursorTypeNone streams the complete result set without a server-side
	// cursor. This is the default.
	cursorTypeNone cursorType = iota

	// cursorTypeReadOnly opens a read-only server-side cursor.
	cursorTypeReadOnly
)

// ReadOnlyCursor enables a read-only server-side cursor for prepared
// statements. With a cursor, rows are fetched from the server in batches
// (see FetchSize) while iterating Rows.Next instead of transferring the
// whole result set up front.
func ReadOnlyCursor(yes bool) Option {
	return func(cfg *Config) error {
		if yes {
			cfg.cursors = cursorTypeReadOnly
		} else {
			cfg.cursors = cursorTypeNone
		}
		return nil
	}
}

// FetchSize sets the number of rows the driver requests from the server in
// each COM_STMT_FETCH batch when a read-only cursor is enabled. Values smaller
// than 1 leave the default batch size (128) in place. It has no effect unless
// ReadOnlyCursor is enabled.
func FetchSize(size int) Option {
	return func(cfg *Config) error {
		cfg.fetchSize = size
		return nil
	}
}
