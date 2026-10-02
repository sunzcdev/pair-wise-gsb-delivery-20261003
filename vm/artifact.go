// Package vm artifact serialization.
//
// A compiled Program contains both stable data (bytecode, arguments,
// constants, variable layout and function symbols) and Go pointers that only
// make sense inside the process that compiled the expression (host functions,
// the AST node, profiling spans).
//
// SaveArtifact writes only the stable data into a self-describing, checksummed
// container. LoadArtifact reads the container, re-binds host functions by
// stable symbol name and runs a full static verification pass. A program is
// never returned (and therefore cannot be executed) when any of the container
// checksum, format version, bytecode version, instructions, jump targets,
// stack usage or host bindings fails validation: mere successful
// deserialization is never enough.
package vm

import "errors"

// ArtifactFormatVersion identifies the on-disk container format.
// It changes only when the layout of the container itself changes.
const ArtifactFormatVersion uint32 = 1

var artifactMagic = [8]byte{'E', 'X', 'P', 'R', 'A', 'R', 'T', '1'}

// Sentinel errors. Use errors.Is to distinguish rejection reasons.
var (
	// ErrArtifactTruncated means the container was cut short or had trailing
	// garbage: some section declared by the header could not be read in full.
	ErrArtifactTruncated = errors.New("expr: artifact is truncated or malformed")

	// ErrArtifactChecksum means the SHA-256 digest of the payload did not
	// match the digest stored at the end of the container (e.g. a byte was
	// flipped or a section was shortened without updating the header).
	ErrArtifactChecksum = errors.New("expr: artifact checksum mismatch")

	// ErrArtifactFormat means the container magic or format version is not
	// supported by this binary.
	ErrArtifactFormat = errors.New("expr: unsupported artifact format version")

	// ErrArtifactBytecode means the bytecode/opcode revision embedded in the
	// artifact does not match this binary.
	ErrArtifactBytecode = errors.New("expr: incompatible bytecode version")

	// ErrArtifactProgram means a section decoded but violates a structural
	// invariant (bad constant kind, inconsistent section lengths, ...).
	ErrArtifactProgram = errors.New("expr: artifact program is invalid")

	// ErrArtifactInstruction means an opcode is unknown, carries an out of
	// range argument, or references an unexpected constant/function.
	ErrArtifactInstruction = errors.New("expr: artifact contains an invalid instruction")

	// ErrArtifactJump means a jump target leaves the program or does not land on
	// a valid instruction boundary (always the start of an opcode in this
	// encoding, so this mainly covers out of range targets).
	ErrArtifactJump = errors.New("expr: artifact contains an invalid jump target")

	// ErrArtifactStack means static stack analysis found an underflow, an
	// inconsistent stack depth at a jump landing site, or a non-scalar final
	// stack.
	ErrArtifactStack = errors.New("expr: artifact fails static stack verification")

	// ErrArtifactBinding means a host function symbol is missing, or the
	// provided host function has a signature that conflicts with the one
	// recorded when the expression was compiled.
	ErrArtifactBinding = errors.New("expr: host function binding rejected")
)

// HostFunc binds a stable function symbol (see expr.Function) to its
// implementation in the loading process. Fn must have the standard calling
// convention func(...any) (any, error); Signature, when non-empty, must match
// the declared compile-time signature (e.g. "func(int, int) int").
type HostFunc struct {
	Fn        Function
	Signature string
}

// Hosts maps stable function symbol names to their host implementations.
type Hosts map[string]HostFunc
