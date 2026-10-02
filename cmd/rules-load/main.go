// Command rules-load is the independent "rule service" process. It loads the
// previously saved artifacts, re-binds the host environment and runs every
// rule without recompiling. It also demonstrates that corrupted artifacts
// (bad bytecode, truncated constant section, changed host signature, ...) are
// rejected with a specific reason before any instruction can execute.
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/cmd/rulesdemo"
	"github.com/expr-lang/expr/vm"
)

func main() {
	dir := "rules-cache"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}

	env := rulesdemo.Env{UserID: 42, Role: "admin", Score: 95, Tags: []string{"a", "b"}}

	fmt.Println("== normal startup: load cached artifacts, no recompilation ==")
	names := make([]string, 0, len(rulesdemo.Rules))
	for name := range rulesdemo.Rules {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		path := filepath.Join(dir, name+".rule")
		data, err := os.ReadFile(path)
		if err != nil {
			fatal("read %q: %v", path, err)
		}
		program, err := expr.LoadProgram(data, rulesdemo.Hosts(), env)
		if err != nil {
			fatal("load %q: %v", name, err)
		}
		out, err := expr.Run(program, env)
		if err != nil {
			fatal("run %q: %v", name, err)
		}
		fmt.Printf("  %-11s => %v\n", name, out)
	}

	fmt.Println("\n== security: malformed artifacts must be rejected before execution ==")
	good := readRule(filepath.Join(dir, "tagged.rule"))

	demo("unsupported bytecode revision", bumpBytecodeVersion(good), env)
	demo("flipped payload byte (checksum)", flipByte(good), env)
	demo("truncated constant section", truncatePayload(good), env)
	demo("corrupted opcode (re-signed)", mutateBytecode(good), env)
	demo("jump target out of range", mutateJump(good), env)
	demo("stack underflow (re-signed)", underflowBody(good), env)
	demo("host function signature changed", good, rulesdemo.Env{},
		expr.Hosts{"discount": {
			Fn:        rulesdemo.Hosts()["discount"].Fn,
			Signature: "func(string) string",
		}})
	demo("missing host function binding", good, rulesdemo.Env{}, expr.Hosts{})
}

func demo(label string, data []byte, env any, hosts ...expr.Hosts) {
	var h expr.Hosts
	if len(hosts) == 1 {
		h = hosts[0]
	} else {
		h = rulesdemo.Hosts()
	}
	_, err := expr.LoadProgram(data, h, env)
	if err == nil {
		fmt.Printf("  %-42s !!! ACCEPTED (this must never happen)\n", label)
		return
	}
	fmt.Printf("  %-42s rejected: %s\n", label, classify(err))
}

func classify(err error) string {
	for _, sentinel := range []error{
		vm.ErrArtifactTruncated,
		vm.ErrArtifactChecksum,
		vm.ErrArtifactFormat,
		vm.ErrArtifactBytecode,
		vm.ErrArtifactInstruction,
		vm.ErrArtifactJump,
		vm.ErrArtifactStack,
		vm.ErrArtifactBinding,
		vm.ErrArtifactProgram,
	} {
		if errors.Is(err, sentinel) {
			return err.Error()
		}
	}
	return err.Error()
}

func readRule(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		fatal("read %q: %v", path, err)
	}
	return data
}

const headerLen = 8 + 4 + 4 + 4 // magic + format + bytecode + payload length

func payloadRange(data []byte) (int, int) {
	n := int(binary.BigEndian.Uint32(data[16:20]))
	return headerLen, headerLen + n
}

func resign(data []byte, mutate func(payload []byte)) []byte {
	start, end := payloadRange(data)
	out := append([]byte(nil), data...)
	mutate(out[start:end])
	sum := sha256.Sum256(out[start:end])
	copy(out[end:], sum[:])
	return out
}

func flipByte(data []byte) []byte {
	out := append([]byte(nil), data...)
	start, _ := payloadRange(out)
	out[start+7] ^= 0xFF
	return out // intentionally not re-signed
}

func bumpBytecodeVersion(data []byte) []byte {
	out := append([]byte(nil), data...)
	out[12] ^= 0x01 // bytecode revision field; checksum covers payload only
	return out
}

func truncatePayload(data []byte) []byte {
	start, end := payloadRange(data)
	// Declare a payload half as long and place a fresh checksum there, so the
	// only thing wrong is that the constant section is now incomplete.
	newEnd := start + (end-start)/2
	out := append([]byte(nil), data[:newEnd+sha256.Size]...)
	n := uint32(newEnd - start)
	out[16], out[17], out[18], out[19] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
	sum := sha256.Sum256(out[start:newEnd])
	copy(out[newEnd:], sum[:])
	return out
}

func mutateBytecode(data []byte) []byte {
	return resign(data, func(payload []byte) {
		// Skip source string: uvarint length prefix followed by contents.
		i, srcLen := 0, 0
		for shift := uint(0); ; shift += 7 {
			c := payload[i]
			srcLen |= int(c&0x7F) << shift
			i++
			if c&0x80 == 0 {
				break
			}
		}
		i += srcLen
		i += 4            // instruction count
		payload[i] = 0xFE // unknown opcode
	})
}

// mutateJump inflates the argument of the first conditional jump so its
// target leaves the program. The example program contains OpJumpIfFalse
// (short-circuit && / ||).
func mutateJump(data []byte) []byte {
	return resign(data, func(payload []byte) {
		off, count := bytecodeRange(payload)
		pos := off
		for i := 0; i < count; i++ {
			op := payload[pos]
			if op == byte(vm.OpJumpIfFalse) || op == byte(vm.OpJumpIfTrue) ||
				op == byte(vm.OpJumpIfNil) || op == byte(vm.OpJump) {
				payload[pos+1] = 0x7F
				return
			}
			pos++
			for payload[pos]&0x80 != 0 {
				pos++
			}
			pos++
		}
	})
}

// underflowBody rewrites the first two instructions to OpPop without pushes.
func underflowBody(data []byte) []byte {
	return resign(data, func(payload []byte) {
		off, count := bytecodeRange(payload)
		pos := off
		for i := 0; i < count && i < 2; i++ {
			payload[pos] = byte(vm.OpPop)
			pos++
			if payload[pos]&0x80 != 0 {
				payload[pos] = 0
				pos++
			}
			payload[pos] = 0
		}
	})
}

// bytecodeRange returns the payload offset of the code section and its
// instruction count.
func bytecodeRange(payload []byte) (offset, count int) {
	i, srcLen := 0, 0
	for shift := uint(0); ; shift += 7 {
		c := payload[i]
		srcLen |= int(c&0x7F) << shift
		i++
		if c&0x80 == 0 {
			break
		}
	}
	i += srcLen + 4
	count = int(binary.BigEndian.Uint32(payload[i-4:]))
	return i, count
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "rules-load: "+format+"\n", args...)
	os.Exit(1)
}
