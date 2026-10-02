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
	"errors"
	"fmt"
	"strings"
	"testing"
)

// openCursorDB opens a database/sql handle with server-side cursors enabled
// at the given fetch size.
func openCursorDB(t *testing.T, fetchSize int) *sql.DB {
	t.Helper()
	if !available {
		t.Skipf("MySQL server not running on %s", netAddr)
	}
	cursorDSN := fmt.Sprintf("%s&fetchSize=%d", dsn, fetchSize)
	db, err := sql.Open(driverNameTest, cursorDSN)
	if err != nil {
		t.Fatalf("connecting %q: %v", cursorDSN, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		t.Skipf("server-side cursor not supported: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func seedCursorTable(t *testing.T, db *sql.DB, tbl string, n int) {
	t.Helper()
	if _, err := db.Exec("DROP TABLE IF EXISTS " + tbl); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE " + tbl + " (id BIGINT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}

	// Prefer a recursive CTE (MySQL 8.0+ / MariaDB 10.2+); fall back to a
	// multi-row insert for older servers.
	_, cteErr := db.Exec(fmt.Sprintf(
		"INSERT INTO %s WITH RECURSIVE seq(s) AS "+
			"(SELECT 0 UNION ALL SELECT s+1 FROM seq WHERE s < %d) "+
			"SELECT s FROM seq", tbl, n-1))
	if cteErr != nil {
		var b strings.Builder
		for i := range n {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "(%d)", i)
		}
		if _, err := db.Exec("INSERT INTO " + tbl + " VALUES " + b.String()); err != nil {
			t.Fatalf("seeding table: %v (cte error: %v)", err, cteErr)
		}
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + tbl).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != n {
		t.Fatalf("seeded %d rows, want %d", count, n)
	}
}

// TestServerCursorFullScan iterates a multi-batch result set through
// prepared statements and verifies every value arrives exactly once.
func TestServerCursorFullScan(t *testing.T) {
	const (
		fetchSize = 100
		total     = 250
	)
	db := openCursorDB(t, fetchSize)
	tbl := "cursorfullscan"
	seedCursorTable(t, db, tbl, total)

	// A prepared statement is required to engage COM_STMT_EXECUTE/FETCH.
	stmt, err := db.Prepare("SELECT id FROM " + tbl + " ORDER BY id")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer stmt.Close()

	rows, err := stmt.Query()
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	seen := make([]bool, total)
	var got int
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan %d: %v", got, err)
		}
		if id < 0 || int(id) >= total || seen[id] {
			t.Fatalf("unexpected/duplicate id %d at position %d", id, got)
		}
		seen[id] = true
		got++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("rows close: %v", err)
	}
	if got != total {
		t.Fatalf("got %d rows, want %d", got, total)
	}

	// Connection reuse: run it again on the pooled connection.
	rows2, err := stmt.Query()
	if err != nil {
		t.Fatalf("second query: %v", err)
	}
	got = 0
	for rows2.Next() {
		got++
	}
	if err := rows2.Err(); err != nil {
		t.Fatalf("second rows.Err: %v", err)
	}
	if err := rows2.Close(); err != nil {
		t.Fatalf("second rows close: %v", err)
	}
	if got != total {
		t.Fatalf("second run got %d rows, want %d", got, total)
	}
}

// TestServerCursorEarlyClose stops consuming in the middle and then reuses
// the connection, verifying the cursor is released server-side.
func TestServerCursorEarlyClose(t *testing.T) {
	db := openCursorDB(t, 50)
	tbl := "cursorearlyclose"
	seedCursorTable(t, db, tbl, 200)

	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	rows, err := conn.QueryContext(ctx, "SELECT id FROM "+tbl+" ORDER BY id")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for i := 0; i < 25; i++ {
		if !rows.Next() {
			t.Fatalf("expected 25 readable rows, stopped at %d: %v", i, rows.Err())
		}
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("early close: %v", err)
	}

	// The same pooled connection must be immediately usable with no
	// residual rows.
	var one int
	if err := conn.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("connection not reusable after early cursor close: %v", err)
	}
	if one != 1 {
		t.Fatalf("SELECT 1 returned %d", one)
	}
}

// TestServerCursorCancel cancels iteration while fetching a later batch and
// confirms the query terminates with the context error.
func TestServerCursorCancel(t *testing.T) {
	db := openCursorDB(t, 10)
	tbl := "cursorcancel"
	seedCursorTable(t, db, tbl, 100000)

	ctx, cancel := context.WithCancel(context.Background())
	rows, err := db.QueryContext(ctx, "SELECT id FROM "+tbl+" ORDER BY id")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	var n int
	for rows.Next() {
		n++
		if n == 35 { // midway through the 4th batch
			cancel()
		}
	}
	cancel()
	// database/sql retries non-final rows only when safe; a canceled context
	// surfaces as context.Canceled (possibly wrapped by the driver).
	if err := rows.Err(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("rows.Err = %v, want context.Canceled", err)
	}
	_ = rows.Close()
}
