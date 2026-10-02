// Command load is the rule service's "serve" process: it reads marshaled
// programs from disk, validates and re-binds them against the host
// environment, and only then executes them. Anything that fails
// validation is rejected before a single instruction runs.
//
// Usage:
//
//	go run ./test/serialize/load [flags] [file]
//
// Flags:
//
//	-tamper string
//		Simulate a bad input before loading: "instruction" corrupts an
//		opcode, "constants" truncates the constants section, "signature"
//		re-binds against a host with a changed function signature.
package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/test/serialize/host"
	"github.com/expr-lang/expr/vm"
)

func main() {
	tamper := flag.String("tamper", "", "tamper mode: instruction|constants|signature")
	flag.Parse()

	file := "rules.exbc"
	if flag.NArg() > 0 {
		file = flag.Arg(0)
	}

	blobs, err := readRules(file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", file, err)
		os.Exit(1)
	}

	functions := host.Functions()
	if *tamper == "signature" {
		fmt.Println("tamper: rebinding against a host with a changed riskScore signature")
		functions = host.FunctionsV2()
	}

	env := host.Env{
		Request: host.Request{IP: "10.1.2.3", Method: "GET", Path: "/api/v2/users"},
		User:    host.User{Role: "auditor"},
	}

	rejected := false
	for i, blob := range blobs {
		data := blob
		switch *tamper {
		case "instruction":
			data = corruptInstruction(blob)
		case "constants":
			data = truncateConstants(blob)
		}

		program, err := vm.Unmarshal(data, vm.WithFunctions(functions))
		if err != nil {
			rejected = true
			var ferr *vm.FormatError
			if errors.As(err, &ferr) {
				fmt.Printf("rule #%d REJECTED before execution [kind=%s]: %v\n", i, ferr.Kind, err)
			} else {
				fmt.Printf("rule #%d REJECTED before execution: %v\n", i, err)
			}
			continue
		}

		out, err := expr.Run(program, env)
		if err != nil {
			fmt.Printf("rule #%d runtime error: %v\n", i, err)
			continue
		}
		fmt.Printf("rule #%d OK: %v\n", i, out)
	}

	if rejected {
		os.Exit(1)
	}
}

// readRules reads the framed rule file produced by the save process.
func readRules(file string) ([][]byte, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	off := 0
	uvarint := func() (uint64, error) {
		v, n := binary.Uvarint(data[off:])
		if n <= 0 {
			return 0, fmt.Errorf("corrupt rule file")
		}
		off += n
		return v, nil
	}
	count, err := uvarint()
	if err != nil {
		return nil, err
	}
	blobs := make([][]byte, 0, count)
	for i := uint64(0); i < count; i++ {
		n, err := uvarint()
		if err != nil {
			return nil, err
		}
		if off+int(n) > len(data) {
			return nil, fmt.Errorf("corrupt rule file")
		}
		blobs = append(blobs, data[off:off+int(n)])
		off += int(n)
	}
	return blobs, nil
}

// sections locates the bytecode and constants sections inside a marshaled
// program, so the demo can tamper with them deterministically.
func sections(data []byte) (bytecodeOff, constantsOff int, err error) {
	off := 6 // magic (4) + version (2)
	uvarint := func() (uint64, error) {
		v, n := binary.Uvarint(data[off:])
		if n <= 0 {
			return 0, fmt.Errorf("corrupt program")
		}
		off += n
		return v, nil
	}
	if _, err = uvarint(); err != nil { // variables
		return 0, 0, err
	}
	bytecodeLen, err := uvarint()
	if err != nil {
		return 0, 0, err
	}
	bytecodeOff = off
	off += int(bytecodeLen)
	argumentsLen, err := uvarint()
	if err != nil {
		return 0, 0, err
	}
	for i := uint64(0); i < argumentsLen; i++ {
		_, n := binary.Varint(data[off:])
		if n <= 0 {
			return 0, 0, fmt.Errorf("corrupt program")
		}
		off += n
	}
	constantsOff = off
	return bytecodeOff, constantsOff, nil
}

// corruptInstruction replaces the first opcode with an invalid one.
func corruptInstruction(data []byte) []byte {
	out := append([]byte(nil), data...)
	bytecodeOff, _, err := sections(out)
	if err != nil {
		return out
	}
	out[bytecodeOff] = 0xFF
	return out
}

// truncateConstants cuts the program off right after the first byte of
// the constants section.
func truncateConstants(data []byte) []byte {
	_, constantsOff, err := sections(data)
	if err != nil || constantsOff+1 > len(data) {
		return data[:len(data)/2]
	}
	return data[:constantsOff+1]
}
