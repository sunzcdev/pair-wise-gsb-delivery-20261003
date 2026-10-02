package vm

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"sort"
	"time"

	"github.com/expr-lang/expr/vm/runtime"
)

const (
	// maxArtifactConstants bounds how many constants a single program may
	// carry, so a corrupt count cannot force a huge allocation while reading.
	maxArtifactConstants = 1_000_000
	maxArtifactDepth     = 64
	maxArtifactLen       = 1 << 30 // 1 GiB per length-prefixed section/string
)

// SaveArtifact serializes the stable, pointer-free parts of program into a
// self-describing container. Host function pointers, the AST node and
// profiling spans are never written; functions are recorded as stable symbols
// (name plus declared signature). Programs compiled with profiling enabled
// cannot be saved, because their bytecode refers to in-process Span pointers.
func SaveArtifact(program *Program) ([]byte, error) {
	if program == nil {
		return nil, fmt.Errorf("%w: program is nil", ErrArtifactProgram)
	}
	if program.span != nil {
		return nil, fmt.Errorf("%w: programs compiled with profiling cannot be saved", ErrArtifactProgram)
	}

	payload, err := encodePayload(program)
	if err != nil {
		return nil, err
	}

	var out []byte
	out = append(out, artifactMagic[:]...)
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], ArtifactFormatVersion)
	out = append(out, hdr[:]...)
	binary.BigEndian.PutUint32(hdr[:], BytecodeVersion)
	out = append(out, hdr[:]...)
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	out = append(out, hdr[:]...)
	out = append(out, payload...)
	sum := sha256.Sum256(payload)
	out = append(out, sum[:]...)
	return out, nil
}

func encodePayload(program *Program) ([]byte, error) {
	var w codeWriter

	if len(program.Bytecode) != len(program.Arguments) {
		return nil, fmt.Errorf("%w: bytecode and arguments length mismatch (%d vs %d)",
			ErrArtifactProgram, len(program.Bytecode), len(program.Arguments))
	}
	if len(program.locations) != 0 && len(program.locations) != len(program.Bytecode) {
		return nil, fmt.Errorf("%w: locations length mismatch (%d vs %d bytecode)",
			ErrArtifactProgram, len(program.locations), len(program.Bytecode))
	}

	w.string(program.source.String())
	w.u32(uint32(len(program.Bytecode)))
	for i, op := range program.Bytecode {
		w.byte(byte(op))
		if err := w.uvarint(program.Arguments[i]); err != nil {
			return nil, err
		}
	}
	w.u32(uint32(len(program.locations)))
	for _, loc := range program.locations {
		if err := w.uvarint(loc.From); err != nil {
			return nil, err
		}
		if err := w.uvarint(loc.To); err != nil {
			return nil, err
		}
	}
	if err := w.uvarint(program.variables); err != nil {
		return nil, err
	}

	if len(program.Constants) > maxArtifactConstants {
		return nil, fmt.Errorf("%w: too many constants (%d)", ErrArtifactProgram, len(program.Constants))
	}
	w.u32(uint32(len(program.Constants)))
	for i, c := range program.Constants {
		if err := w.value(c, 0); err != nil {
			return nil, fmt.Errorf("%w: constant #%d: %v", ErrArtifactProgram, i, err)
		}
	}

	names, sigs, kinds := program.functionNames, program.functionSigs, program.functionKinds
	if len(names) != len(program.functions) || len(sigs) != len(program.functions) || len(kinds) != len(program.functions) {
		return nil, fmt.Errorf("%w: program has %d function slots but %d symbol names and %d signatures (it was not compiled with symbol metadata)",
			ErrArtifactProgram, len(program.functions), len(names), len(sigs))
	}
	w.u32(uint32(len(names)))
	for i := range names {
		if kinds[i] != FunctionKindHost && kinds[i] != FunctionKindBuiltin {
			return nil, fmt.Errorf("%w: function slot %d (%s) has unknown origin kind %d",
				ErrArtifactProgram, i, names[i], kinds[i])
		}
		w.string(names[i])
		w.string(sigs[i])
		w.byte(kinds[i])
	}

	w.u32(uint32(len(program.debugInfo)))
	// Debug keys carry positional indices ("var_3"); sort to keep the artifact
	// deterministic regardless of map iteration order.
	var keys []string
	for k := range program.debugInfo {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		w.string(k)
		w.string(program.debugInfo[k])
	}

	return w.buf, nil
}

type codeWriter struct {
	buf []byte
}

func (w *codeWriter) byte(b byte) { w.buf = append(w.buf, b) }

func (w *codeWriter) bytes(b []byte) { w.buf = append(w.buf, b...) }

func (w *codeWriter) u32(v uint32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	w.buf = append(w.buf, b[:]...)
}

func (w *codeWriter) uvarint(v int) error {
	if v < 0 {
		return fmt.Errorf("negative integer %d cannot be encoded", v)
	}
	var b [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(b[:], uint64(v))
	w.buf = append(w.buf, b[:n]...)
	return nil
}

func (w *codeWriter) string(s string) {
	_ = w.uvarint(len(s))
	w.bytes([]byte(s))
}

// constant kinds
const (
	kindNil byte = iota
	kindBool
	kindInt
	kindInt64
	kindUint64
	kindFloat64
	kindString
	kindBytes
	kindRegexp
	kindField
	kindMethod
	kindError
	kindTime
	kindDuration
	kindAnySlice
	kindStringSlice
	kindIntSlice
	kindMapStringAny
)

func (w *codeWriter) value(v any, depth int) error {
	if depth > maxArtifactDepth {
		return errors.New("nesting too deep")
	}
	switch x := v.(type) {
	case nil:
		w.byte(kindNil)
	case bool:
		w.byte(kindBool)
		w.byte(boolByte(x))
	case int:
		w.byte(kindInt)
		w.bytes(signed64(int64(x)))
	case int64:
		w.byte(kindInt64)
		w.bytes(signed64(x))
	case int32:
		w.byte(kindInt64)
		w.bytes(signed64(int64(x)))
	case uint:
		w.byte(kindUint64)
		w.bytes(unsigned64(uint64(x)))
	case uint64:
		w.byte(kindUint64)
		w.bytes(unsigned64(x))
	case uint32:
		w.byte(kindUint64)
		w.bytes(unsigned64(uint64(x)))
	case float64:
		w.byte(kindFloat64)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], float64bits(x))
		w.bytes(b[:])
	case float32:
		w.byte(kindFloat64)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], float64bits(float64(x)))
		w.bytes(b[:])
	case string:
		w.byte(kindString)
		w.string(x)
	case []byte:
		w.byte(kindBytes)
		if err := w.uvarint(len(x)); err != nil {
			return err
		}
		w.bytes(x)
	case *regexp.Regexp:
		w.byte(kindRegexp)
		w.string(x.String())
	case *runtime.Field:
		w.byte(kindField)
		if err := writeIntSlice(&w.buf, x.Index); err != nil {
			return err
		}
		if err := writeStringSlice(&w.buf, x.Path); err != nil {
			return err
		}
	case *runtime.Method:
		w.byte(kindMethod)
		if err := w.uvarint(x.Index); err != nil {
			return err
		}
		w.string(x.Name)
	case error:
		w.byte(kindError)
		w.string(x.Error())
	case time.Time:
		w.byte(kindTime)
		w.bytes(unsigned64(uint64(x.Unix())))
		w.bytes(unsigned64(uint64(x.Nanosecond())))
	case time.Duration:
		w.byte(kindDuration)
		w.bytes(signed64(int64(x)))
	case []any:
		w.byte(kindAnySlice)
		if err := w.uvarint(len(x)); err != nil {
			return err
		}
		for i, e := range x {
			if err := w.value(e, depth+1); err != nil {
				return fmt.Errorf("[%d]: %w", i, err)
			}
		}
	case []string:
		w.byte(kindStringSlice)
		if err := writeStringSlice(&w.buf, x); err != nil {
			return err
		}
	case []int:
		w.byte(kindIntSlice)
		if err := writeIntSlice(&w.buf, x); err != nil {
			return err
		}
	case map[string]any:
		w.byte(kindMapStringAny)
		if err := w.uvarint(len(x)); err != nil {
			return err
		}
		var keys []string
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			w.string(k)
			if err := w.value(x[k], depth+1); err != nil {
				return fmt.Errorf("[%q]: %w", k, err)
			}
		}
	default:
		return fmt.Errorf("unsupported constant type %s (%T)", reflect.TypeOf(v), v)
	}
	return nil
}

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}

func signed64(v int64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(v))
	return b[:]
}

func unsigned64(v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return b[:]
}

func float64bits(f float64) uint64 {
	return math.Float64bits(f)
}

func writeStringSlice(buf *[]byte, xs []string) error {
	var cw codeWriter
	cw.buf = *buf
	if err := cw.uvarint(len(xs)); err != nil {
		return err
	}
	for _, x := range xs {
		cw.string(x)
	}
	*buf = cw.buf
	return nil
}

func writeIntSlice(buf *[]byte, xs []int) error {
	var cw codeWriter
	cw.buf = *buf
	if err := cw.uvarint(len(xs)); err != nil {
		return err
	}
	for _, x := range xs {
		if err := cw.uvarint(x); err != nil {
			return err
		}
	}
	*buf = cw.buf
	return nil
}
