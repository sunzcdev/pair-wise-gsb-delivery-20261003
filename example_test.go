// Copyright 2014-2022 Ulrich Kunitz. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package xz_test

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/ulikunitz/xz"
)

func ExampleReader() {
	f, err := os.Open("fox.xz")
	if err != nil {
		log.Fatalf("os.Open(%q) error %s", "fox.xz", err)
	}
	defer f.Close()
	r, err := xz.NewReader(bufio.NewReader(f))
	if err != nil {
		log.Fatalf("xz.NewReader(f) error %s", err)
	}
	if _, err = io.Copy(os.Stdout, r); err != nil {
		log.Fatalf("io.Copy error %s", err)
	}
	// Output:
	// The quick brown fox jumps over the lazy dog.
}

func ExampleWriter() {
	f, err := os.Create("example.xz")
	if err != nil {
		log.Fatalf("os.Open(%q) error %s", "example.xz", err)
	}
	defer f.Close()
	w, err := xz.NewWriter(f)
	if err != nil {
		log.Fatalf("xz.NewWriter(f) error %s", err)
	}
	defer w.Close()
	_, err = fmt.Fprintln(w, "The brown fox jumps over the lazy dog.")
	if err != nil {
		log.Fatalf("fmt.Fprintln error %s", err)
	}
	if err = w.Close(); err != nil {
		log.Fatalf("w.Close() error %s", err)
	}
	// Output:
}

func ExampleNewRandomReader() {
	// A file provides io.ReaderAt together with its size.
	f, err := os.Open("fox.xz")
	if err != nil {
		log.Fatalf("os.Open error %s", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		log.Fatalf("f.Stat error %s", err)
	}

	// The layout is recovered from the stream footers and indexes;
	// only the blocks overlapping the request are decompressed.
	rr, err := xz.NewRandomReader(f, fi.Size())
	if err != nil {
		log.Fatalf("NewRandomReader error %s", err)
	}
	p := make([]byte, 5)
	if _, err = rr.ReadAt(p, 4); err != nil {
		log.Fatalf("ReadAt error %s", err)
	}
	fmt.Printf("%s", p)
	// Output:
	// quick
}
