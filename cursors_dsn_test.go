// Go MySQL Driver - A MySQL-Driver for Go's database/sql package
//
// Copyright 2026 The Go-MySQL-Driver Authors. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

package mysql

import (
	"strings"
	"testing"
)

func TestParseDSNReadOnlyCursor(t *testing.T) {
	cfg, err := ParseDSN("/db?cursors=readOnly")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.cursors != cursorTypeReadOnly {
		t.Fatalf("cursors = %d, want readOnly", cfg.cursors)
	}
	if cfg.fetchSize != defaultFetchSize {
		t.Fatalf("fetchSize = %d, want default %d", cfg.fetchSize, defaultFetchSize)
	}
}

func TestParseDSNReadOnlyCursorAliases(t *testing.T) {
	for _, in := range []string{"/db?cursors=1", "/db?cursors=TRUE", "/db?cursors=yes", "/db?cursors=read_only"} {
		cfg, err := ParseDSN(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if cfg.cursors != cursorTypeReadOnly {
			t.Fatalf("%s: cursors = %d, want readOnly", in, cfg.cursors)
		}
	}
}

func TestParseDSNNoCursor(t *testing.T) {
	for _, in := range []string{"/db", "/db?cursors=0", "/db?cursors=false", "/db?cursors=NONE"} {
		cfg, err := ParseDSN(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if cfg.cursors != cursorTypeNone {
			t.Fatalf("%s: cursors = %d, want none", in, cfg.cursors)
		}
	}
}

func TestParseDSNInvalidCursor(t *testing.T) {
	if _, err := ParseDSN("/db?cursors=scrollable"); err == nil {
		t.Fatal("expected error for invalid cursors value")
	}
}

func TestParseDSNFetchSize(t *testing.T) {
	cfg, err := ParseDSN("/db?cursors=readOnly&fetchSize=500")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.fetchSize != 500 {
		t.Fatalf("fetchSize = %d, want 500", cfg.fetchSize)
	}
}

func TestParseDSNInvalidFetchSize(t *testing.T) {
	for _, in := range []string{"/db?fetchSize=0", "/db?fetchSize=-3", "/db?fetchSize=abc"} {
		if _, err := ParseDSN(in); err == nil {
			t.Fatalf("%s: expected error", in)
		}
	}
}

func TestFormatDSNCursorRoundTrip(t *testing.T) {
	in := "/db?cursors=readOnly&fetchSize=256"
	cfg, err := ParseDSN(in)
	if err != nil {
		t.Fatal(err)
	}
	out := cfg.FormatDSN()
	cfg2, err := ParseDSN(out)
	if err != nil {
		t.Fatalf("reparse %q: %v", out, err)
	}
	if cfg2.cursors != cursorTypeReadOnly || cfg2.fetchSize != 256 {
		t.Fatalf("round trip lost cursor settings: cursors=%d fetchSize=%d", cfg2.cursors, cfg2.fetchSize)
	}
}

func TestFormatDSNFetchSizeWithoutCursorIsOmitted(t *testing.T) {
	// fetchSize only matters with a cursor; a directly-set value should not be
	// serialized when cursors are disabled.
	cfg := NewConfig()
	cfg.fetchSize = 256
	out := cfg.FormatDSN()
	if strings.Contains(out, "fetchSize") {
		t.Fatalf("FormatDSN emitted fetchSize without cursors: %s", out)
	}
}

func TestCursorOptions(t *testing.T) {
	cfg := NewConfig()
	if err := cfg.Apply(ReadOnlyCursor(true), FetchSize(64)); err != nil {
		t.Fatal(err)
	}
	if cfg.cursors != cursorTypeReadOnly || cfg.fetchSize != 64 {
		t.Fatalf("cursors=%d fetchSize=%d", cfg.cursors, cfg.fetchSize)
	}

	if err := cfg.Apply(ReadOnlyCursor(false)); err != nil {
		t.Fatal(err)
	}
	if cfg.cursors != cursorTypeNone {
		t.Fatalf("cursors = %d, want none", cfg.cursors)
	}
}
