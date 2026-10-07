// Copyright 2014-2025 Ulrich Kunitz. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package xz

import (
	"bytes"
	"os/exec"
)

// pythonPath locates a Python interpreter whose lzma module wraps
// liblzma, the reference implementation of the xz filter
// algorithms. Returns "" when no suitable interpreter is available.
func pythonPath() string {
	for _, name := range []string{"python", "python3", "py"} {
		p, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		cmd := exec.Command(p, "-c", "import lzma")
		if cmd.Run() == nil {
			return p
		}
	}
	return ""
}

// verifyWithLiblzma decodes the given file with the reference
// implementation and compares it against codeBody.
func verifyWithLiblzma(python, path string) error {
	script := `
import lzma, sys
want = open("testdata/code.bin","rb").read()
got = lzma.decompress(open(sys.argv[1],"rb").read())
if got != want:
	raise SystemExit("mismatch: %d vs %d bytes" % (len(got), len(want)))
`
	cmd := exec.Command(python, "-c", script, path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return &liblzmaError{err: err, detail: stderr.String()}
	}
	return nil
}

type liblzmaError struct {
	err    error
	detail string
}

func (e *liblzmaError) Error() string {
	return "liblzma verification failed: " + e.err.Error() + " " + e.detail
}
