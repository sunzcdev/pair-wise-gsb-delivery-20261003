// Copyright 2014-2022 Ulrich Kunitz. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package xz_test

import (
	"bytes"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ulikunitz/xz"
)

// pythonInterop checks whether the reference implementation (CPython's
// lzma module, which links liblzma) is available for the interop
// tests.
func pythonInterop(t *testing.T) (python string, ok bool) {
	t.Helper()
	for _, cand := range []string{"python", "python3"} {
		if p, err := exec.LookPath(cand); err == nil {
			cmd := exec.Command(p, "-c", "import lzma")
			if cmd.Run() == nil {
				return p, true
			}
		}
	}
	t.Skip("python with lzma module not available")
	return "", false
}

// decodeScript is run by Python; it decodes the xz file given as the
// first argument and writes the uncompressed content to stdout.
const decodeScript = `
import lzma, sys
with open(sys.argv[1], "rb") as f:
    sys.stdout.buffer.write(lzma.decompress(f.read()))
`

// TestGoWriterPythonReader verifies that streams encoded by this
// package can be decoded by the reference implementation. It exercises
// several filter chains and writer chunk sizes.
func TestGoWriterPythonReader(t *testing.T) {
	python, ok := pythonInterop(t)
	if !ok {
		return
	}
	payload, err := ioutil.ReadFile(filepath.Join("testdata", "filters",
		"payload.bin"))
	if err != nil {
		t.Fatalf("reading payload: %s", err)
	}

	chains := [][]xz.Filter{
		nil,
		{&xz.X86Filter{}},
		{&xz.DeltaFilter{Distance: 1}},
		{&xz.DeltaFilter{Distance: 7}},
		{&xz.DeltaFilter{Distance: 256}},
		{&xz.DeltaFilter{Distance: 1}, &xz.X86Filter{}},
		{&xz.DeltaFilter{Distance: 7}, &xz.X86Filter{StartOffset: 16}},
	}

	dir := t.TempDir()
	for i, chain := range chains {
		for _, ws := range []int{1, 3, 5, 64, 4096} {
			path := filepath.Join(dir, "out.xz")
			f, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			w, err := xz.WriterConfig{Filters: chain}.NewWriter(f)
			if err != nil {
				t.Fatalf("NewWriter chain %d: %s", i, err)
			}
			for pos := 0; pos < len(payload); {
				n := ws
				if pos+n > len(payload) {
					n = len(payload) - pos
				}
				if _, err = w.Write(payload[pos : pos+n]); err != nil {
					t.Fatalf("Write chain %d: %s", i, err)
				}
				pos += n
			}
			if err = w.Close(); err != nil {
				t.Fatalf("Close chain %d: %s", i, err)
			}
			if err = f.Close(); err != nil {
				t.Fatalf("file close: %s", err)
			}

			cmd := exec.Command(python, "-c", decodeScript, path)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err = cmd.Run(); err != nil {
				t.Fatalf("python decode chain %d ws=%d: %s %s",
					i, ws, err, stderr.String())
			}
			if !bytes.Equal(stdout.Bytes(), payload) {
				t.Fatalf("chain %d ws=%d: decoded mismatch %d vs %d",
					i, ws, stdout.Len(), len(payload))
			}
		}
	}
}

// TestPythonWriterGoReader verifies that streams encoded by the
// reference implementation can be decoded by this package using many
// reader chunk sizes.
func TestPythonWriterGoReader(t *testing.T) {
	_, ok := pythonInterop(t)
	if !ok {
		return
	}
	payload, err := ioutil.ReadFile(filepath.Join("testdata", "filters",
		"payload.bin"))
	if err != nil {
		t.Fatalf("reading payload: %s", err)
	}

	names, err := filepath.Glob(filepath.Join("testdata", "filters",
		"ref_*.xz"))
	if err != nil {
		t.Fatal(err)
	}
	sizes := []int{1, 2, 3, 4, 5, 7, 8, 13, 64, 4096}
	for _, name := range names {
		if filepath.Base(name) == "ref_unsupported_arm_crc64.xz" {
			continue
		}
		src, err := ioutil.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, rs := range sizes {
			for _, osz := range sizes {
				r, err := xz.NewReader(&fixedReader{data: src, n: rs})
				if err != nil {
					t.Fatalf("%s NewReader: %s", filepath.Base(name), err)
				}
				var out bytes.Buffer
				p := make([]byte, osz)
				for {
					n, err := r.Read(p)
					out.Write(p[:n])
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatalf("%s rs=%d os=%d: %s",
							filepath.Base(name), rs, osz, err)
					}
				}
				if !bytes.Equal(out.Bytes(), payload) {
					t.Fatalf("%s rs=%d os=%d: mismatch",
						filepath.Base(name), rs, osz)
				}
			}
		}
	}
}

type fixedReader struct {
	data []byte
	pos  int
	n    int
}

func (r *fixedReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := r.n
	if n > len(p) {
		n = len(p)
	}
	if r.pos+n > len(r.data) {
		n = len(r.data) - r.pos
	}
	copy(p, r.data[r.pos:r.pos+n])
	r.pos += n
	return n, nil
}
