// Command save is the rule service's "compile" process: it compiles the
// rule set once and writes the marshaled programs to disk, so the service
// does not recompile the same expressions on every restart.
//
// Usage:
//
//	go run ./test/serialize/save [output-file]
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"os"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/test/serialize/host"
	"github.com/expr-lang/expr/vm"
)

var rules = []string{
	`user.role == "admin" or isInternalIP(request.ip)`,
	`riskScore(request.ip) < 50 and request.method in ["GET", "HEAD"]`,
	`request.path startsWith "/api" and request.path matches "^/api/v[0-9]+"`,
	`let score = riskScore(request.ip); user.role == "auditor" and score < 90`,
}

func main() {
	out := "rules.exbc"
	if len(os.Args) > 1 {
		out = os.Args[1]
	}

	buf := new(bytes.Buffer)
	writeUvarint(buf, uint64(len(rules)))

	for i, rule := range rules {
		program, err := expr.Compile(rule, host.Options()...)
		if err != nil {
			log.Fatalf("rule #%d: compile: %v", i, err)
		}
		data, err := vm.Marshal(program)
		if err != nil {
			log.Fatalf("rule #%d: marshal: %v", i, err)
		}
		writeUvarint(buf, uint64(len(data)))
		buf.Write(data)
		fmt.Printf("rule #%d compiled and marshaled (%d bytes): %s\n", i, len(data), rule)
	}

	if err := os.WriteFile(out, buf.Bytes(), 0o644); err != nil {
		log.Fatalf("write %s: %v", out, err)
	}
	fmt.Printf("saved %d rule(s) to %s\n", len(rules), out)
}

func writeUvarint(buf *bytes.Buffer, v uint64) {
	var b [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(b[:], v)
	buf.Write(b[:n])
}
