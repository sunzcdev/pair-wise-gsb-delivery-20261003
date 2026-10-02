package vm

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"

	"github.com/expr-lang/expr/file"
	"github.com/expr-lang/expr/vm/runtime"
)

// LoadArtifact parses and verifies a serialized artifact and binds host
// functions by stable symbol name. It never returns a runnable program
// unless the checksum, format/bytecode versions, every instruction, every
// jump target, static stack usage and all host bindings have been verified.
// env, when non-nil, is additionally used to verify struct field and method
// indexes recorded at compile time.
func LoadArtifact(data []byte, hosts Hosts, env any) (*Program, error) {
	if len(data) < len(artifactMagic)+4+4+4+sha256.Size {
		return nil, fmt.Errorf("%w: header too short (%d bytes)", ErrArtifactTruncated, len(data))
	}
	p := 0
	if string(data[p:p+len(artifactMagic)]) != string(artifactMagic[:]) {
		return nil, fmt.Errorf("%w: bad magic signature", ErrArtifactFormat)
	}
	p += len(artifactMagic)

	formatVersion := binary.BigEndian.Uint32(data[p:])
	p += 4
	if formatVersion != ArtifactFormatVersion {
		return nil, fmt.Errorf("%w: version %d (this binary supports %d)", ErrArtifactFormat, formatVersion, ArtifactFormatVersion)
	}
	bytecodeVersion := binary.BigEndian.Uint32(data[p:])
	p += 4
	if bytecodeVersion != BytecodeVersion {
		return nil, fmt.Errorf("%w: artifact bytecode revision %d, runtime revision %d", ErrArtifactBytecode, bytecodeVersion, BytecodeVersion)
	}
	payloadLen := int(binary.BigEndian.Uint32(data[p:]))
	p += 4

	end := p + payloadLen
	if end > len(data)-sha256.Size {
		return nil, fmt.Errorf("%w: declared payload is %d bytes but only %d remain", ErrArtifactTruncated, payloadLen, len(data)-p-sha256.Size)
	}
	payload := data[p:end]
	storedSum := data[end : end+sha256.Size]
	actualSum := sha256.Sum256(payload)
	if string(storedSum) != string(actualSum[:]) {
		return nil, fmt.Errorf("%w: payload digest does not match stored digest", ErrArtifactChecksum)
	}
	if end+sha256.Size != len(data) {
		return nil, fmt.Errorf("%w: %d trailing bytes after checksum", ErrArtifactTruncated, len(data)-end-sha256.Size)
	}

	r := &codeReader{buf: payload}
	program, err := r.program()
	if err != nil {
		return nil, err
	}
	if !r.exhausted() {
		return nil, fmt.Errorf("%w: %d unconsumed payload bytes", ErrArtifactTruncated, r.remaining())
	}

	if err := verifyProgram(program); err != nil {
		return nil, err
	}
	if err := bindHosts(program, hosts, env); err != nil {
		return nil, err
	}
	return program, nil
}

type codeReader struct {
	buf []byte
	pos int
	err error
}

func (r *codeReader) exhausted() bool { return r.pos == len(r.buf) }
func (r *codeReader) remaining() int  { return len(r.buf) - r.pos }

func (r *codeReader) need(n int) bool {
	if r.err != nil {
		return false
	}
	if n < 0 || r.pos+n > len(r.buf) {
		r.err = fmt.Errorf("%w: section ended at offset %d needing %d more bytes", ErrArtifactTruncated, r.pos, n)
		return false
	}
	return true
}

func (r *codeReader) byteV() byte {
	if !r.need(1) {
		return 0
	}
	b := r.buf[r.pos]
	r.pos++
	return b
}

func (r *codeReader) u32() uint32 {
	if !r.need(4) {
		return 0
	}
	v := binary.BigEndian.Uint32(r.buf[r.pos:])
	r.pos += 4
	return v
}

func (r *codeReader) uvarintV() int {
	if r.err != nil {
		return 0
	}
	v, n := binary.Uvarint(r.buf[r.pos:])
	if n <= 0 {
		r.err = fmt.Errorf("%w: corrupt varint at offset %d", ErrArtifactTruncated, r.pos)
		return 0
	}
	if v > math.MaxInt32 {
		r.err = fmt.Errorf("%w: length/argument %d exceeds int32 range", ErrArtifactTruncated, v)
		return 0
	}
	r.pos += n
	return int(v)
}

func (r *codeReader) stringV() string {
	n := r.uvarintV()
	if r.err != nil {
		return ""
	}
	if !r.need(n) {
		return ""
	}
	s := string(r.buf[r.pos : r.pos+n])
	r.pos += n
	return s
}

func (r *codeReader) fixed8() []byte {
	if !r.need(8) {
		return nil
	}
	b := r.buf[r.pos : r.pos+8]
	r.pos += 8
	return b
}

func (r *codeReader) program() (*Program, error) {
	source := file.NewSource(r.stringV())

	n := int(r.u32())
	if r.err == nil && n > maxArtifactLen {
		return nil, fmt.Errorf("%w: bytecode length %d exceeds limit", ErrArtifactTruncated, n)
	}
	if r.err == nil && !r.need(n) { // opcode byte plus at least one varint per instruction
		return nil, r.err
	}
	bytecode := make([]Opcode, n)
	arguments := make([]int, n)
	for i := 0; i < n; i++ {
		if r.err != nil {
			return nil, r.err
		}
		bytecode[i] = Opcode(r.byteV())
		arguments[i] = r.uvarintV()
	}
	if r.err != nil {
		return nil, r.err
	}

	locN := int(r.u32())
	if locN != 0 && locN != n {
		return nil, fmt.Errorf("%w: %d locations for %d instructions", ErrArtifactProgram, locN, n)
	}
	locations := make([]file.Location, locN)
	for i := 0; i < locN; i++ {
		locations[i] = file.Location{From: r.uvarintV(), To: r.uvarintV()}
	}

	variables := r.uvarintV()

	constN := int(r.u32())
	if constN > maxArtifactConstants {
		return nil, fmt.Errorf("%w: constant count %d exceeds limit", ErrArtifactProgram, constN)
	}
	constants := make([]any, constN)
	for i := 0; i < constN; i++ {
		c, err := r.value(0)
		if err != nil {
			return nil, fmt.Errorf("constant #%d: %w", i, err)
		}
		constants[i] = c
	}

	fnN := int(r.u32())
	if fnN > maxArtifactConstants {
		return nil, fmt.Errorf("%w: function count %d exceeds limit", ErrArtifactProgram, fnN)
	}
	names := make([]string, fnN)
	sigs := make([]string, fnN)
	kinds := make([]byte, fnN)
	for i := 0; i < fnN; i++ {
		names[i] = r.stringV()
		sigs[i] = r.stringV()
		kinds[i] = r.byteV()
		if r.err == nil && kinds[i] != FunctionKindHost && kinds[i] != FunctionKindBuiltin {
			return nil, fmt.Errorf("%w: function slot %d (%s) has unknown origin kind %d",
				ErrArtifactProgram, i, names[i], kinds[i])
		}
		if r.err != nil {
			return nil, r.err
		}
	}

	dbgN := int(r.u32())
	debugInfo := make(map[string]string, dbgN)
	for i := 0; i < dbgN; i++ {
		k := r.stringV()
		v := r.stringV()
		debugInfo[k] = v
	}
	if r.err != nil {
		return nil, r.err
	}

	prog := NewProgramFromArtifact(source, locations, variables, constants, bytecode, arguments, make([]Function, fnN), names, sigs, kinds, debugInfo)
	return prog, nil
}

func (r *codeReader) value(depth int) (any, error) {
	if depth > maxArtifactDepth {
		return nil, fmt.Errorf("%w: nesting too deep", ErrArtifactProgram)
	}
	kind := r.byteV()
	if r.err != nil {
		return nil, r.err
	}
	switch kind {
	case kindNil:
		return nil, nil
	case kindBool:
		b := r.byteV()
		if b > 1 {
			return nil, fmt.Errorf("%w: invalid bool byte %d", ErrArtifactProgram, b)
		}
		return b == 1, nil
	case kindInt:
		b := r.fixed8()
		return int(int64(binary.BigEndian.Uint64(b))), nil
	case kindInt64:
		b := r.fixed8()
		return int64(binary.BigEndian.Uint64(b)), nil
	case kindUint64:
		b := r.fixed8()
		return binary.BigEndian.Uint64(b), nil
	case kindFloat64:
		b := r.fixed8()
		return math.Float64frombits(binary.BigEndian.Uint64(b)), nil
	case kindString:
		return r.stringV(), r.err
	case kindBytes:
		n := r.uvarintV()
		if r.err != nil {
			return nil, r.err
		}
		if !r.need(n) {
			return nil, r.err
		}
		b := make([]byte, n)
		copy(b, r.buf[r.pos:r.pos+n])
		r.pos += n
		return b, nil
	case kindRegexp:
		pattern := r.stringV()
		if r.err != nil {
			return nil, r.err
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid embedded regexp %q: %v", ErrArtifactProgram, pattern, err)
		}
		return re, nil
	case kindField:
		idx, err := r.readIntSlice()
		if err != nil {
			return nil, err
		}
		path, err := r.readStringSlice()
		if err != nil {
			return nil, err
		}
		if len(idx) == 0 || len(path) == 0 {
			return nil, fmt.Errorf("%w: field constant needs non-empty index and path", ErrArtifactProgram)
		}
		return &runtime.Field{Index: idx, Path: path}, nil
	case kindMethod:
		idx := r.uvarintV()
		name := r.stringV()
		if r.err != nil {
			return nil, r.err
		}
		if name == "" {
			return nil, fmt.Errorf("%w: method constant has empty name", ErrArtifactProgram)
		}
		return &runtime.Method{Index: idx, Name: name}, nil
	case kindError:
		msg := r.stringV()
		if r.err != nil {
			return nil, r.err
		}
		return errors.New(msg), nil
	case kindTime:
		sec := binary.BigEndian.Uint64(r.fixed8())
		nsec := binary.BigEndian.Uint64(r.fixed8())
		if r.err != nil {
			return nil, r.err
		}
		if nsec >= 1e9 {
			return nil, fmt.Errorf("%w: time nanosecond field %d out of range", ErrArtifactProgram, nsec)
		}
		return time.Unix(int64(sec), int64(nsec)).UTC(), nil
	case kindDuration:
		b := r.fixed8()
		return time.Duration(int64(binary.BigEndian.Uint64(b))), nil
	case kindAnySlice:
		n := r.uvarintV()
		if r.err != nil {
			return nil, r.err
		}
		s := make([]any, n)
		for i := range s {
			v, err := r.value(depth + 1)
			if err != nil {
				return nil, err
			}
			s[i] = v
		}
		return s, nil
	case kindStringSlice:
		return r.readStringSlice()
	case kindIntSlice:
		return r.readIntSlice()
	case kindMapStringAny:
		n := r.uvarintV()
		if r.err != nil {
			return nil, r.err
		}
		m := make(map[string]any, n)
		for i := 0; i < n; i++ {
			k := r.stringV()
			v, err := r.value(depth + 1)
			if err != nil {
				return nil, err
			}
			if _, dup := m[k]; dup {
				return nil, fmt.Errorf("%w: duplicate map key %q", ErrArtifactProgram, k)
			}
			m[k] = v
		}
		return m, nil
	default:
		return nil, fmt.Errorf("%w: unknown constant kind %d at offset", ErrArtifactProgram, kind)
	}
}

func (r *codeReader) readStringSlice() ([]string, error) {
	n := r.uvarintV()
	if r.err != nil {
		return nil, r.err
	}
	s := make([]string, n)
	for i := range s {
		s[i] = r.stringV()
		if r.err != nil {
			return nil, r.err
		}
	}
	return s, nil
}

func (r *codeReader) readIntSlice() ([]int, error) {
	n := r.uvarintV()
	if r.err != nil {
		return nil, r.err
	}
	s := make([]int, n)
	for i := range s {
		s[i] = r.uvarintV()
		if r.err != nil {
			return nil, r.err
		}
	}
	return s, nil
}
