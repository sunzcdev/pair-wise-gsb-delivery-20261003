package vm_test

import (
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/internal/testify/require"
	"github.com/expr-lang/expr/vm"
)

// resign mutates the payload of an artifact and recomputes its checksum, so
// the resulting bytes pass integrity verification and reach the deeper
// structural checks.
func resign(t *testing.T, data []byte, mutate func(payload []byte)) []byte {
	t.Helper()
	start, end := vm.ArtifactPayloadRange(data)
	out := append([]byte(nil), data...)
	mutate(out[start:end])
	sum := sha256.Sum256(out[start:end])
	copy(out[end:], sum[:])
	return out
}

func tamperProgram(t *testing.T, code string) []byte {
	t.Helper()
	program, err := expr.Compile(code,
		expr.Env(artifactEnv{}),
		expr.Function("double", artifactHosts()["double"].Fn, new(func(int) int)))
	require.NoError(t, err)
	data, err := expr.SaveProgram(program)
	require.NoError(t, err)
	return data
}

func TestArtifactIntegrityRejected(t *testing.T) {
	good := tamperProgram(t, "double(Foo) + 1")

	t.Run("bad magic", func(t *testing.T) {
		bad := append([]byte(nil), good...)
		bad[0] = 'X'
		_, err := expr.LoadProgram(bad, artifactHosts(), artifactEnv{})
		require.ErrorIs(t, err, vm.ErrArtifactFormat)
	})

	t.Run("truncated container", func(t *testing.T) {
		_, err := expr.LoadProgram(good[:len(good)-40], artifactHosts(), artifactEnv{})
		require.ErrorIs(t, err, vm.ErrArtifactTruncated)
	})

	t.Run("trailing garbage", func(t *testing.T) {
		bad := append(append([]byte(nil), good...), 0, 0, 0, 0)
		_, err := expr.LoadProgram(bad, artifactHosts(), artifactEnv{})
		require.ErrorIs(t, err, vm.ErrArtifactTruncated)
	})

	t.Run("flipped byte without re-signing", func(t *testing.T) {
		bad := append([]byte(nil), good...)
		start, _ := vm.ArtifactPayloadRange(bad)
		bad[start+5] ^= 0x01
		_, err := expr.LoadProgram(bad, artifactHosts(), artifactEnv{})
		require.ErrorIs(t, err, vm.ErrArtifactChecksum)
	})

	t.Run("unsupported format version", func(t *testing.T) {
		bad := append([]byte(nil), good...)
		bad[8] = 99 // format version field
		_, err := expr.LoadProgram(bad, artifactHosts(), artifactEnv{})
		require.ErrorIs(t, err, vm.ErrArtifactFormat)
	})

	t.Run("incompatible bytecode revision", func(t *testing.T) {
		bad := append([]byte(nil), good...)
		bad[12] ^= 0x01 // bytecode version field (checksum covers payload only)
		_, err := expr.LoadProgram(bad, artifactHosts(), artifactEnv{})
		require.ErrorIs(t, err, vm.ErrArtifactBytecode)
	})
}

func TestArtifactBadInstructionRejected(t *testing.T) {
	good := tamperProgram(t, "double(Foo) + 1")

	t.Run("unknown opcode after re-signing", func(t *testing.T) {
		bad := resign(t, good, func(payload []byte) {
			off, _ := vm.ArtifactBytecodeOffset(payload)
			payload[off] = 0xFE // no such opcode
		})
		_, err := expr.LoadProgram(bad, artifactHosts(), artifactEnv{})
		require.ErrorIs(t, err, vm.ErrArtifactInstruction)
	})

	t.Run("jump out of range after re-signing", func(t *testing.T) {
		jumpy := tamperProgram(t, "Foo > 1 ? double(Foo) : 0")
		bad := resign(t, jumpy, func(payload []byte) {
			off, count := vm.ArtifactBytecodeOffset(payload)
			for i := 0; i < count; i++ {
				ip := vm.ArtifactInstructionOffset(payload, off, i)
				if payload[ip] == byte(vm.OpJumpIfFalse) {
					// Argument is a small uvarint in the next byte; inflate
					// it to 127 so the target leaves the program.
					payload[ip+1] = 0x7F
					return
				}
			}
			t.Fatal("no OpJumpIfFalse found")
		})
		_, err := expr.LoadProgram(bad, artifactHosts(), artifactEnv{})
		require.ErrorIs(t, err, vm.ErrArtifactJump)
	})
}

func TestArtifactTruncatedConstantsRejected(t *testing.T) {
	// Shrink the declared payload length so the constants section is cut
	// short; the checksum still has to match, which it cannot unless the
	// data is untouched — instead re-sign after deleting trailing constant
	// bytes via the declared length header.
	good := tamperProgram(t, "double(Foo) + 1")
	bad := append([]byte(nil), good...)

	// Declare a payload that ends in the middle of the constants section.
	start, end := vm.ArtifactPayloadRange(good)
	newEnd := start + (end-start)/2
	n := uint32(newEnd - start)
	bad[16] = byte(n >> 24)
	bad[17] = byte(n >> 16)
	bad[18] = byte(n >> 8)
	bad[19] = byte(n)

	// Move the checksum to the new payload end so the container is
	// structurally consistent but its payload is genuinely truncated.
	sum := sha256.Sum256(bad[start:newEnd])
	copy(bad[newEnd:], sum[:])

	_, err := expr.LoadProgram(bad, artifactHosts(), artifactEnv{})
	require.True(t, errors.Is(err, vm.ErrArtifactTruncated) || errors.Is(err, vm.ErrArtifactProgram) || errors.Is(err, vm.ErrArtifactInstruction),
		"expected truncation/structural error, got %v", err)
}

func TestArtifactHostSignatureMismatchRejected(t *testing.T) {
	good := tamperProgram(t, "double(Foo)")

	t.Run("missing host function", func(t *testing.T) {
		_, err := expr.LoadProgram(good, expr.Hosts{}, artifactEnv{})
		require.ErrorIs(t, err, vm.ErrArtifactBinding)
	})

	t.Run("declared signature differs", func(t *testing.T) {
		hosts := expr.Hosts{"double": {
			Fn:        artifactHosts()["double"].Fn,
			Signature: "func(string) string",
		}}
		_, err := expr.LoadProgram(good, hosts, artifactEnv{})
		require.ErrorIs(t, err, vm.ErrArtifactBinding)
	})

	t.Run("nil host implementation", func(t *testing.T) {
		_, err := expr.LoadProgram(good, expr.Hosts{"double": {}}, artifactEnv{})
		require.ErrorIs(t, err, vm.ErrArtifactBinding)
	})
}

func TestArtifactStackUnderflowRejected(t *testing.T) {
	// Replace the body of a valid program with two pops and no pushes after
	// re-signing, forcing a provable stack underflow.
	good := tamperProgram(t, "double(Foo)")
	bad := resign(t, good, func(payload []byte) {
		off, count := vm.ArtifactBytecodeOffset(payload)
		// Rewrite the first two instructions to OpPop (opcode 3) with zero
		// arguments; keep the rest so the declared count still parses.
		pos := off
		for i := 0; i < count && i < 2; i++ {
			payload[pos] = byte(vm.OpPop)
			pos++
			if payload[pos]&0x80 != 0 {
				payload[pos] = 0
				pos++
			}
			payload[pos] = 0 // argument becomes 0
		}
	})
	_, err := expr.LoadProgram(bad, artifactHosts(), artifactEnv{})
	require.ErrorIs(t, err, vm.ErrArtifactStack)
}
