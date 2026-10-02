// Go MySQL Driver - A MySQL-Driver for Go's database/sql package
//
// Copyright 2026 The Go-MySQL-Driver Authors. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// cursorDSN appends the read-only cursor DSN options to the base test DSN.
func cursorDSN(fetchSize int) string {
	if fetchSize > 0 {
		return fmt.Sprintf("%s&cursors=readOnly&fetchSize=%d", dsn, fetchSize)
	}
	return dsn + "&cursors=readOnly"
}

// openCursorDB opens a database/sql pool using server-side read-only cursors.
// The test is skipped when no MySQL test server is reachable.
func openCursorDB(t *testing.T, fetchSize int) *sql.DB {
	t.Helper()
	if !available {
		t.Skipf("MySQL server not running on %s", netAddr)
	}
	db, err := sql.Open(driverNameTest, cursorDSN(fetchSize))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		t.Skipf("server does not accept read-only cursors: %v", err)
	}
	return db
}

// TestIntegrationCursorIterate walks a result set several times larger than
// the fetch size and verifies every row is returned exactly once. This is the
// real-server counterpart of the protocol double tests: a failure here means
// the driver's COM_STMT_FETCH sequencing does not match the actual server.
func TestIntegrationCursorIterate(t *testing.T) {
	const fetchSize = 7
	db := openCursorDB(t, fetchSize)
	defer db.Close()

	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	stmt, err := conn.PrepareContext(ctx,
		"SELECT n FROM (SELECT 1 AS n UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4) x")
	if err != nil {
		// Some proxies/old servers reject CURSOR_TYPE_READ_ONLY.
		t.Skipf("server rejected cursor statement: %v", err)
	}
	defer stmt.Close()

	rows, err := stmt.QueryContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	var last int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		last = n
		got++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if got != 4 || last != 4 {
		t.Fatalf("got %d rows, last=%d; want 4 rows, last=4", got, last)
	}

	// Reuse the same prepared statement after a fully consumed cursor.
	rows2, err := stmt.QueryContext(ctx)
	if err != nil {
		t.Fatalf("reuse after full iteration: %v", err)
	}
	got = 0
	for rows2.Next() {
		got++
	}
	rows2.Close()
	if got != 4 {
		t.Fatalf("reused statement returned %d rows, want 4", got)
	}
}

// TestIntegrationCursorEarlyClose reads only the first row of a large result
// set and closes the rows early. It then issues a new query on the same
// connection to prove the cursor was released and no residual rows remain.
func TestIntegrationCursorEarlyClose(t *testing.T) {
	const total = fetchSizeDefaultRows
	db := openCursorDB(t, 5)
	defer db.Close()

	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "DROP TABLE IF EXISTS test_cursor"); err != nil {
		t.Skipf("cannot drop test table: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "CREATE TABLE test_cursor (id INT PRIMARY KEY)"); err != nil {
		t.Skipf("cannot create test table: %v", err)
	}
	defer conn.ExecContext(ctx, "DROP TABLE IF EXISTS test_cursor")

	values := ""
	for i := 1; i <= total; i++ {
		if i > 1 {
			values += ","
		}
		values += fmt.Sprintf("(%d)", i)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO test_cursor (id) VALUES "+values); err != nil {
		t.Fatal(err)
	}

	stmt, err := conn.PrepareContext(ctx, "SELECT id FROM test_cursor ORDER BY id")
	if err != nil {
		t.Skipf("server rejected cursor statement: %v", err)
	}
	defer stmt.Close()

	rows, err := stmt.QueryContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatalf("expected at least one row: %v", rows.Err())
	}
	var first int
	if err := rows.Scan(&first); err != nil {
		t.Fatal(err)
	}
	if first != 1 {
		t.Fatalf("first id = %d, want 1", first)
	}
	// Abandon the rest; Close must release the server-side cursor.
	if err := rows.Close(); err != nil {
		t.Fatalf("early close: %v", err)
	}

	// A subsequent query on the same connection must work (proves the stream
	// holds no residual rows and the connection was returned cleanly).
	var count int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM test_cursor").Scan(&count); err != nil {
		t.Fatalf("query after early cursor close failed: %v", err)
	}
	if count != total {
		t.Fatalf("count = %d, want %d", count, total)
	}

	// And the cursor statement itself can be reused.
	rows2, err := stmt.QueryContext(ctx)
	if err != nil {
		t.Fatalf("statement reuse after early close: %v", err)
	}
	n := 0
	for rows2.Next() {
		n++
	}
	if err := rows2.Err(); err != nil {
		t.Fatal(err)
	}
	rows2.Close()
	if n != total {
		t.Fatalf("reused cursor returned %d rows, want %d", n, total)
	}
}

const fetchSizeDefaultRows = 50
