package vm

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"sort"

	"github.com/expr-lang/expr/builtin"
	"github.com/expr-lang/expr/vm/runtime"
)

// This file implements saving and loading of compiled programs.
//
// A marshaled program records only stable data: the bytecode, its
// arguments, the number of variables, pointer-free constants, and the
// function symbols (names and type signatures) the program was compiled
// against. It never contains Go pointers.
//
// Unmarshal re-binds the program to the host environment: host functions
// are resolved by name and their signatures are verified against the
// recorded ones. Before a program is returned, Unmarshal also validates
// the format version, every instruction, every jump target, and the stack
// and scope usage of the bytecode. A program is never returned — and thus
// never executed — merely because it deserializes.

const (
	// formatMagic identifies a marshaled program: the bytes "EXBC".
	formatMagic = uint32(0x45584243)
	// formatVersion is the current serialization format version.
	formatVersion = uint16(1)
)

// ErrKind classifies the reason a marshaled program was rejected, so
// callers can distinguish corruption, tampering, and host environment
// mismatches from one another.
type ErrKind int

const (
	// ErrKindMagic means the data does not start with the expected magic bytes.
	ErrKindMagic ErrKind = iota + 1
	// ErrKindVersion means the format version is not supported.
	ErrKindVersion
	// ErrKindTruncated means the data ended before a section was complete.
	ErrKindTruncated
	// ErrKindConstant means a constant is malformed or of an unexpected type.
	ErrKindConstant
	// ErrKindOpcode means an instruction is not a valid opcode.
	ErrKindOpcode
	// ErrKindJump means a jump offset or target is out of bounds.
	ErrKindJump
	// ErrKindArgument means an instruction argument is out of range.
	ErrKindArgument
	// ErrKindStack means the bytecode can underflow the evaluation stack.
	ErrKindStack
	// ErrKindScope means iteration scopes (OpBegin/OpEnd) are unbalanced.
	ErrKindScope
	// ErrKindFunction means a required host function is not provided.
	ErrKindFunction
	// ErrKindSignature means a host function's signature differs from the
	// one recorded when the program was compiled.
	ErrKindSignature
)

func (k ErrKind) String() string {
	switch k {
	case ErrKindMagic:
		return "magic"
	case ErrKindVersion:
		return "version"
	case ErrKindTruncated:
		return "truncated"
	case ErrKindConstant:
		return "constant"
	case ErrKindOpcode:
		return "opcode"
	case ErrKindJump:
		return "jump"
	case ErrKindArgument:
		return "argument"
	case ErrKindStack:
		return "stack"
	case ErrKindScope:
		return "scope"
	case ErrKindFunction:
		return "function"
	case ErrKindSignature:
		return "signature"
	}
	return "unknown"
}

// FormatError describes why a marshaled program was rejected. Use
// errors.As to inspect the Kind and distinguish the cause.
type FormatError struct {
	Kind ErrKind
	msg  string
}

func (e *FormatError) Error() string {
	return e.msg
}

func errorf(kind ErrKind, format string, args ...any) *FormatError {
	return &FormatError{Kind: kind, msg: "expr: " + fmt.Sprintf(format, args...)}
}

// functionSymbol is the stable, pointer-free record of a host function.
type functionSymbol struct {
	name      string
	signature string
}

// Marshal serializes a compiled program into a stable, pointer-free byte
// representation. It returns an error if the program contains constants
// that cannot be represented (for example functions or profiling spans).
func Marshal(program *Program) ([]byte, error) {
	if program == nil {
		return nil, fmt.Errorf("expr: cannot marshal a nil program")
	}
	if len(program.Bytecode) != len(program.Arguments) {
		return nil, fmt.Errorf("expr: cannot marshal program: bytecode and arguments length mismatch")
	}

	buf := new(bytes.Buffer)

	var header [4]byte
	binary.LittleEndian.PutUint32(header[:], formatMagic)
	buf.Write(header[:])
	var version [2]byte
	binary.LittleEndian.PutUint16(version[:], formatVersion)
	buf.Write(version[:])

	writeUvarint(buf, uint64(program.variables))

	writeUvarint(buf, uint64(len(program.Bytecode)))
	for _, op := range program.Bytecode {
		buf.WriteByte(byte(op))
	}

	writeUvarint(buf, uint64(len(program.Arguments)))
	for _, arg := range program.Arguments {
		writeVarint(buf, int64(arg))
	}

	writeUvarint(buf, uint64(len(program.Constants)))
	for _, c := range program.Constants {
		if err := encodeConstant(buf, c); err != nil {
			return nil, err
		}
	}

	writeUvarint(buf, uint64(len(program.functions)))
	for i := range program.functions {
		name := program.debugInfo[fmt.Sprintf("func_%d", i)]
		if name == "" {
			return nil, fmt.Errorf("expr: cannot marshal program: function #%d has no recorded symbol", i)
		}
		signature := program.debugInfo[fmt.Sprintf("funcsig_%d", i)]
		if signature == "" {
			return nil, fmt.Errorf("expr: cannot marshal program: function %q has no recorded signature", name)
		}
		writeString(buf, name)
		writeString(buf, signature)
	}

	return buf.Bytes(), nil
}

// LoadOption configures how Unmarshal re-binds a program to the host.
type LoadOption func(*loadConfig)

type loadConfig struct {
	functions map[string]*builtin.Function
}

// WithFunctions provides the host functions a loaded program is re-bound
// to. Every function symbol recorded at compile time must be present here
// (or be a builtin) and must have the same type signature as recorded.
func WithFunctions(functions map[string]*builtin.Function) LoadOption {
	return func(c *loadConfig) {
		c.functions = functions
	}
}

// Unmarshal decodes a program produced by Marshal, validates it, and
// re-binds it to the host environment. The returned program is safe to
// run; any problem with the data or the host environment is reported as a
// *FormatError before any bytecode is executed.
func Unmarshal(data []byte, opts ...LoadOption) (*Program, error) {
	config := loadConfig{}
	for _, opt := range opts {
		opt(&config)
	}

	program, symbols, err := decodeProgram(data)
	if err != nil {
		return nil, err
	}
	if err := validateProgram(program); err != nil {
		return nil, err
	}
	if err := rebindFunctions(program, symbols, config.functions); err != nil {
		return nil, err
	}
	return program, nil
}

// decoder is a strict cursor over the marshaled data. Every read is
// bounds-checked; running out of data is reported as ErrKindTruncated
// naming the section being read.
type decoder struct {
	data    []byte
	off     int
	section string
}

func (d *decoder) read(n int) ([]byte, error) {
	if n < 0 || len(d.data)-d.off < n {
		return nil, errorf(ErrKindTruncated, "unexpected end of data while reading %s", d.section)
	}
	b := d.data[d.off : d.off+n]
	d.off += n
	return b, nil
}

func (d *decoder) byte() (byte, error) {
	b, err := d.read(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (d *decoder) uvarint() (uint64, error) {
	v, n := binary.Uvarint(d.data[d.off:])
	if n <= 0 {
		return 0, errorf(ErrKindTruncated, "unexpected end of data while reading %s", d.section)
	}
	d.off += n
	return v, nil
}

func (d *decoder) varint() (int64, error) {
	v, n := binary.Varint(d.data[d.off:])
	if n <= 0 {
		return 0, errorf(ErrKindTruncated, "unexpected end of data while reading %s", d.section)
	}
	d.off += n
	return v, nil
}

func (d *decoder) count() (int, error) {
	v, err := d.uvarint()
	if err != nil {
		return 0, err
	}
	// Every element takes at least one byte, so a count larger than the
	// remaining data can never be satisfied.
	if v > uint64(len(d.data)-d.off) {
		return 0, errorf(ErrKindTruncated, "unexpected end of data while reading %s", d.section)
	}
	return int(v), nil
}

func (d *decoder) str() (string, error) {
	n, err := d.count()
	if err != nil {
		return "", err
	}
	b, err := d.read(n)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func decodeProgram(data []byte) (*Program, []functionSymbol, error) {
	d := &decoder{data: data, section: "header"}

	magic, err := d.read(4)
	if err != nil {
		return nil, nil, errorf(ErrKindTruncated, "unexpected end of data while reading header")
	}
	if binary.LittleEndian.Uint32(magic) != formatMagic {
		return nil, nil, errorf(ErrKindMagic, "not a marshaled program: bad magic bytes")
	}
	versionRaw, err := d.read(2)
	if err != nil {
		return nil, nil, err
	}
	if version := binary.LittleEndian.Uint16(versionRaw); version != formatVersion {
		return nil, nil, errorf(ErrKindVersion, "unsupported program format version %d (expected %d)", version, formatVersion)
	}

	program := &Program{
		debugInfo: make(map[string]string),
	}

	d.section = "variables"
	variables, err := d.uvarint()
	if err != nil {
		return nil, nil, err
	}
	program.variables = int(variables)

	d.section = "bytecode"
	bytecodeLen, err := d.count()
	if err != nil {
		return nil, nil, err
	}
	bytecode, err := d.read(bytecodeLen)
	if err != nil {
		return nil, nil, err
	}
	program.Bytecode = make([]Opcode, bytecodeLen)
	for i, b := range bytecode {
		program.Bytecode[i] = Opcode(b)
	}

	d.section = "arguments"
	argumentsLen, err := d.count()
	if err != nil {
		return nil, nil, err
	}
	program.Arguments = make([]int, argumentsLen)
	for i := range program.Arguments {
		arg, err := d.varint()
		if err != nil {
			return nil, nil, err
		}
		program.Arguments[i] = int(arg)
	}

	d.section = "constants"
	constantsLen, err := d.count()
	if err != nil {
		return nil, nil, err
	}
	program.Constants = make([]any, constantsLen)
	for i := range program.Constants {
		c, err := decodeConstant(d)
		if err != nil {
			return nil, nil, err
		}
		program.Constants[i] = c
	}

	d.section = "functions"
	functionsLen, err := d.count()
	if err != nil {
		return nil, nil, err
	}
	symbols := make([]functionSymbol, functionsLen)
	program.functions = make([]Function, functionsLen)
	for i := range symbols {
		name, err := d.str()
		if err != nil {
			return nil, nil, err
		}
		signature, err := d.str()
		if err != nil {
			return nil, nil, err
		}
		symbols[i] = functionSymbol{name: name, signature: signature}
		program.debugInfo[fmt.Sprintf("func_%d", i)] = name
		program.debugInfo[fmt.Sprintf("funcsig_%d", i)] = signature
	}

	if d.off != len(d.data) {
		return nil, nil, errorf(ErrKindTruncated, "%d trailing byte(s) after end of program", len(d.data)-d.off)
	}

	return program, symbols, nil
}

func rebindFunctions(program *Program, symbols []functionSymbol, functions map[string]*builtin.Function) error {
	for i, symbol := range symbols {
		fn := functions[symbol.name]
		if fn == nil {
			if index, ok := builtin.Index[symbol.name]; ok {
				fn = builtin.Builtins[index]
			}
		}
		if fn == nil {
			return errorf(ErrKindFunction, "unknown host function %q: provide it via vm.WithFunctions", symbol.name)
		}
		if actual := fn.Type().String(); actual != symbol.signature {
			return errorf(ErrKindSignature, "host function %q has signature %s, but the program was compiled against %s", symbol.name, actual, symbol.signature)
		}
		program.functions[i] = fn.Func
	}
	return nil
}

// Constant encoding. Only stable, pointer-free representations are
// written; host-specific values are recorded symbolically (fields and
// methods by path/index, regular expressions by pattern) and re-created
// on load.
const (
	tagNil       = 0x00
	tagFalse     = 0x01
	tagTrue      = 0x02
	tagInt       = 0x03
	tagInt64     = 0x04
	tagFloat64   = 0x05
	tagFloat32   = 0x06
	tagString    = 0x07
	tagBytes     = 0x08
	tagField     = 0x09
	tagMethod    = 0x0A
	tagRegexp    = 0x0B
	tagSlice     = 0x0C // []any, elements encoded recursively
	tagMap       = 0x0D // map[string]any, values encoded recursively
	tagSliceInt  = 0x0E
	tagSliceStr  = 0x0F
	tagSliceF64  = 0x10
	tagSetString = 0x11 // map[string]struct{}
	tagSetInt    = 0x12 // map[int]struct{}
)

func writeUvarint(buf *bytes.Buffer, v uint64) {
	var b [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(b[:], v)
	buf.Write(b[:n])
}

func writeVarint(buf *bytes.Buffer, v int64) {
	var b [binary.MaxVarintLen64]byte
	n := binary.PutVarint(b[:], v)
	buf.Write(b[:n])
}

func writeString(buf *bytes.Buffer, s string) {
	writeUvarint(buf, uint64(len(s)))
	buf.WriteString(s)
}

func encodeConstant(buf *bytes.Buffer, c any) error {
	switch c := c.(type) {
	case nil:
		buf.WriteByte(tagNil)
	case bool:
		if c {
			buf.WriteByte(tagTrue)
		} else {
			buf.WriteByte(tagFalse)
		}
	case int:
		buf.WriteByte(tagInt)
		writeVarint(buf, int64(c))
	case int64:
		buf.WriteByte(tagInt64)
		writeVarint(buf, c)
	case float64:
		buf.WriteByte(tagFloat64)
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], math.Float64bits(c))
		buf.Write(b[:])
	case float32:
		buf.WriteByte(tagFloat32)
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], math.Float32bits(c))
		buf.Write(b[:])
	case string:
		buf.WriteByte(tagString)
		writeString(buf, c)
	case []byte:
		buf.WriteByte(tagBytes)
		writeUvarint(buf, uint64(len(c)))
		buf.Write(c)
	case *runtime.Field:
		buf.WriteByte(tagField)
		writeUvarint(buf, uint64(len(c.Path)))
		for _, p := range c.Path {
			writeString(buf, p)
		}
		writeUvarint(buf, uint64(len(c.Index)))
		for _, i := range c.Index {
			writeVarint(buf, int64(i))
		}
	case *runtime.Method:
		buf.WriteByte(tagMethod)
		writeString(buf, c.Name)
		writeVarint(buf, int64(c.Index))
	case *regexp.Regexp:
		buf.WriteByte(tagRegexp)
		writeString(buf, c.String())
	case []any:
		buf.WriteByte(tagSlice)
		writeUvarint(buf, uint64(len(c)))
		for _, e := range c {
			if err := encodeConstant(buf, e); err != nil {
				return err
			}
		}
	case map[string]any:
		buf.WriteByte(tagMap)
		keys := make([]string, 0, len(c))
		for k := range c {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		writeUvarint(buf, uint64(len(keys)))
		for _, k := range keys {
			writeString(buf, k)
			if err := encodeConstant(buf, c[k]); err != nil {
				return err
			}
		}
	case []int:
		buf.WriteByte(tagSliceInt)
		writeUvarint(buf, uint64(len(c)))
		for _, e := range c {
			writeVarint(buf, int64(e))
		}
	case []string:
		buf.WriteByte(tagSliceStr)
		writeUvarint(buf, uint64(len(c)))
		for _, e := range c {
			writeString(buf, e)
		}
	case []float64:
		buf.WriteByte(tagSliceF64)
		writeUvarint(buf, uint64(len(c)))
		for _, e := range c {
			var b [8]byte
			binary.LittleEndian.PutUint64(b[:], math.Float64bits(e))
			buf.Write(b[:])
		}
	case map[string]struct{}:
		buf.WriteByte(tagSetString)
		keys := make([]string, 0, len(c))
		for k := range c {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		writeUvarint(buf, uint64(len(keys)))
		for _, k := range keys {
			writeString(buf, k)
		}
	case map[int]struct{}:
		buf.WriteByte(tagSetInt)
		keys := make([]int, 0, len(c))
		for k := range c {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		writeUvarint(buf, uint64(len(keys)))
		for _, k := range keys {
			writeVarint(buf, int64(k))
		}
	default:
		return encodeConstantReflect(buf, c)
	}
	return nil
}

// encodeConstantReflect handles less common slices and maps by degrading
// them to []any / map[string]any. Anything else (functions, channels,
// profiling spans, arbitrary structs) cannot be marshaled.
func encodeConstantReflect(buf *bytes.Buffer, c any) error {
	v := reflect.ValueOf(c)
	switch v.Kind() {
	case reflect.Slice, reflect.Array:
		buf.WriteByte(tagSlice)
		writeUvarint(buf, uint64(v.Len()))
		for i := 0; i < v.Len(); i++ {
			if err := encodeConstant(buf, v.Index(i).Interface()); err != nil {
				return err
			}
		}
		return nil
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			break
		}
		buf.WriteByte(tagMap)
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		writeUvarint(buf, uint64(len(keys)))
		for _, k := range keys {
			writeString(buf, k.String())
			if err := encodeConstant(buf, v.MapIndex(k).Interface()); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("expr: cannot marshal constant of type %T", c)
}

func decodeConstant(d *decoder) (any, error) {
	tag, err := d.byte()
	if err != nil {
		return nil, err
	}
	switch tag {
	case tagNil:
		return nil, nil
	case tagFalse:
		return false, nil
	case tagTrue:
		return true, nil
	case tagInt:
		v, err := d.varint()
		if err != nil {
			return nil, err
		}
		return int(v), nil
	case tagInt64:
		v, err := d.varint()
		if err != nil {
			return nil, err
		}
		return v, nil
	case tagFloat64:
		b, err := d.read(8)
		if err != nil {
			return nil, err
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(b)), nil
	case tagFloat32:
		b, err := d.read(4)
		if err != nil {
			return nil, err
		}
		return math.Float32frombits(binary.LittleEndian.Uint32(b)), nil
	case tagString:
		return d.str()
	case tagBytes:
		n, err := d.count()
		if err != nil {
			return nil, err
		}
		b, err := d.read(n)
		if err != nil {
			return nil, err
		}
		out := make([]byte, n)
		copy(out, b)
		return out, nil
	case tagField:
		pathLen, err := d.count()
		if err != nil {
			return nil, err
		}
		field := &runtime.Field{
			Path:  make([]string, pathLen),
			Index: make([]int, 0),
		}
		for i := range field.Path {
			field.Path[i], err = d.str()
			if err != nil {
				return nil, err
			}
		}
		indexLen, err := d.count()
		if err != nil {
			return nil, err
		}
		for i := 0; i < indexLen; i++ {
			v, err := d.varint()
			if err != nil {
				return nil, err
			}
			field.Index = append(field.Index, int(v))
		}
		return field, nil
	case tagMethod:
		name, err := d.str()
		if err != nil {
			return nil, err
		}
		index, err := d.varint()
		if err != nil {
			return nil, err
		}
		return &runtime.Method{Name: name, Index: int(index)}, nil
	case tagRegexp:
		pattern, err := d.str()
		if err != nil {
			return nil, err
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, errorf(ErrKindConstant, "invalid regular expression constant %q: %v", pattern, err)
		}
		return re, nil
	case tagSlice:
		n, err := d.count()
		if err != nil {
			return nil, err
		}
		out := make([]any, n)
		for i := range out {
			e, err := decodeConstant(d)
			if err != nil {
				return nil, err
			}
			out[i] = e
		}
		return out, nil
	case tagMap:
		n, err := d.count()
		if err != nil {
			return nil, err
		}
		out := make(map[string]any, n)
		for i := 0; i < n; i++ {
			k, err := d.str()
			if err != nil {
				return nil, err
			}
			v, err := decodeConstant(d)
			if err != nil {
				return nil, err
			}
			out[k] = v
		}
		return out, nil
	case tagSliceInt:
		n, err := d.count()
		if err != nil {
			return nil, err
		}
		out := make([]int, n)
		for i := range out {
			v, err := d.varint()
			if err != nil {
				return nil, err
			}
			out[i] = int(v)
		}
		return out, nil
	case tagSliceStr:
		n, err := d.count()
		if err != nil {
			return nil, err
		}
		out := make([]string, n)
		for i := range out {
			out[i], err = d.str()
			if err != nil {
				return nil, err
			}
		}
		return out, nil
	case tagSliceF64:
		n, err := d.count()
		if err != nil {
			return nil, err
		}
		out := make([]float64, n)
		for i := range out {
			b, err := d.read(8)
			if err != nil {
				return nil, err
			}
			out[i] = math.Float64frombits(binary.LittleEndian.Uint64(b))
		}
		return out, nil
	case tagSetString:
		n, err := d.count()
		if err != nil {
			return nil, err
		}
		out := make(map[string]struct{}, n)
		for i := 0; i < n; i++ {
			k, err := d.str()
			if err != nil {
				return nil, err
			}
			out[k] = struct{}{}
		}
		return out, nil
	case tagSetInt:
		n, err := d.count()
		if err != nil {
			return nil, err
		}
		out := make(map[int]struct{}, n)
		for i := 0; i < n; i++ {
			k, err := d.varint()
			if err != nil {
				return nil, err
			}
			out[int(k)] = struct{}{}
		}
		return out, nil
	}
	return nil, errorf(ErrKindConstant, "unknown constant tag 0x%02x", tag)
}

// validateProgram statically verifies a decoded program before it is
// re-bound or executed: every instruction must be a known opcode with an
// in-range argument, every jump target must stay inside the bytecode, and
// the evaluation stack and iteration scopes must be well-formed on every
// possible execution path.
func validateProgram(program *Program) error {
	if len(program.Bytecode) == 0 {
		return errorf(ErrKindOpcode, "program has no instructions")
	}
	if len(program.Bytecode) != len(program.Arguments) {
		return errorf(ErrKindOpcode, "bytecode length %d does not match arguments length %d",
			len(program.Bytecode), len(program.Arguments))
	}
	for ip := range program.Bytecode {
		if err := validateInstruction(program, ip); err != nil {
			return err
		}
	}
	return validateFlow(program)
}

func validateInstruction(program *Program, ip int) error {
	op := program.Bytecode[ip]
	arg := program.Arguments[ip]

	if op == OpInvalid {
		return errorf(ErrKindOpcode, "invalid opcode OpInvalid at ip %d", ip)
	}
	if op > OpEnd {
		return errorf(ErrKindOpcode, "unknown opcode 0x%02x at ip %d", byte(op), ip)
	}

	constant := func() any {
		if arg < 0 || arg >= len(program.Constants) {
			return nil
		}
		return program.Constants[arg]
	}
	constantOf := func(op Opcode, want string, check func(any) bool) error {
		if arg < 0 || arg >= len(program.Constants) {
			return errorf(ErrKindArgument, "constant index %d out of range (%d constants) at ip %d",
				arg, len(program.Constants), ip)
		}
		if !check(program.Constants[arg]) {
			return errorf(ErrKindConstant, "constant %d has type %T, but opcode %d requires %s at ip %d",
				arg, program.Constants[arg], byte(op), want, ip)
		}
		return nil
	}
	isField := func(c any) bool { _, ok := c.(*runtime.Field); return ok }
	isMethod := func(c any) bool { _, ok := c.(*runtime.Method); return ok }
	isString := func(c any) bool { _, ok := c.(string); return ok }
	isRegexp := func(c any) bool { _, ok := c.(*regexp.Regexp); return ok }

	switch op {
	case OpJump, OpJumpIfTrue, OpJumpIfFalse, OpJumpIfNil, OpJumpIfNotNil, OpJumpIfEnd:
		if arg < 0 {
			return errorf(ErrKindJump, "negative jump offset %d at ip %d", arg, ip)
		}
		if target := ip + 1 + arg; target > len(program.Bytecode) {
			return errorf(ErrKindJump, "jump target %d out of bounds (%d instructions) at ip %d",
				target, len(program.Bytecode), ip)
		}
	case OpJumpBackward:
		if arg < 0 {
			return errorf(ErrKindJump, "negative jump offset %d at ip %d", arg, ip)
		}
		if target := ip + 1 - arg; target < 0 {
			return errorf(ErrKindJump, "backward jump target %d out of bounds at ip %d", target, ip)
		}

	case OpPush, OpLoadConst:
		if constant() == nil && (arg < 0 || arg >= len(program.Constants)) {
			return errorf(ErrKindArgument, "constant index %d out of range (%d constants) at ip %d",
				arg, len(program.Constants), ip)
		}
	case OpLoadField, OpFetchField:
		if err := constantOf(op, "*runtime.Field", isField); err != nil {
			return err
		}
	case OpLoadMethod, OpMethod:
		if err := constantOf(op, "*runtime.Method", isMethod); err != nil {
			return err
		}
	case OpLoadFast:
		if err := constantOf(op, "string", isString); err != nil {
			return err
		}
	case OpMatchesConst:
		if err := constantOf(op, "*regexp.Regexp", isRegexp); err != nil {
			return err
		}
	case OpProfileStart, OpProfileEnd:
		return errorf(ErrKindConstant, "profiling opcode at ip %d cannot appear in a marshaled program", ip)

	case OpLoadFunc, OpCall0, OpCall1, OpCall2, OpCall3:
		if arg < 0 || arg >= len(program.functions) {
			return errorf(ErrKindArgument, "function index %d out of range (%d functions) at ip %d",
				arg, len(program.functions), ip)
		}
	case OpStore, OpLoadVar:
		if arg < 0 || arg >= program.variables {
			return errorf(ErrKindArgument, "variable index %d out of range (%d variables) at ip %d",
				arg, program.variables, ip)
		}
	case OpCallBuiltin1:
		if arg < 0 || arg >= len(builtin.Builtins) {
			return errorf(ErrKindArgument, "builtin index %d out of range at ip %d", arg, ip)
		}
	case OpCallTyped:
		if arg < 0 || arg >= len(FuncTypes) {
			return errorf(ErrKindArgument, "typed function index %d out of range at ip %d", arg, ip)
		}
	case OpCast:
		if arg < 0 || arg > 3 {
			return errorf(ErrKindArgument, "invalid cast argument %d at ip %d", arg, ip)
		}
	case OpCreate:
		if arg != 1 && arg != 2 {
			return errorf(ErrKindArgument, "invalid OpCreate argument %d at ip %d", arg, ip)
		}
	case OpCall, OpCallN, OpCallFast, OpCallSafe:
		if arg < 0 {
			return errorf(ErrKindArgument, "negative argument count %d at ip %d", arg, ip)
		}
	}
	return nil
}

// stackEffect returns how many values the opcode pops and pushes. For
// OpArray and OpMap the effect depends on a runtime value, so dynamic is
// returned instead and the caller resolves it.
func stackEffect(op Opcode, arg int) (pops, pushes int, dynamic bool) {
	switch op {
	case OpPush, OpInt, OpLoadVar, OpLoadConst, OpLoadField, OpLoadFast,
		OpLoadMethod, OpLoadFunc, OpLoadEnv, OpTrue, OpFalse, OpNil,
		OpGetIndex, OpGetCount, OpGetLen, OpGetAcc, OpPointer, OpSort, OpCall0:
		return 0, 1, false
	case OpCreate:
		if arg == 2 {
			return 1, 1, false
		}
		return 0, 1, false
	case OpPop, OpStore, OpSetAcc, OpSetIndex, OpGroupBy, OpSortBy, OpThrow, OpBegin:
		return 1, 0, false
	case OpFetch, OpEqual, OpEqualInt, OpEqualString, OpIn, OpLess, OpMore,
		OpLessOrEqual, OpMoreOrEqual, OpAdd, OpSubtract, OpMultiply, OpDivide,
		OpModulo, OpExponent, OpRange, OpMatches, OpContains, OpStartsWith,
		OpEndsWith, OpAnd, OpOr, OpCall2:
		return 2, 1, false
	case OpSlice, OpCall3:
		return 3, 1, false
	case OpFetchField, OpMethod, OpNegate, OpNot, OpMatchesConst, OpLen, OpCast,
		OpDeref, OpCallTyped, OpCallBuiltin1, OpCall1:
		return 1, 1, false
	case OpCall, OpCallN, OpCallFast, OpCallSafe:
		return arg + 1, 1, false
	case OpArray, OpMap:
		return 0, 0, true
	}
	return 0, 0, false
}

// opNeedsScope reports whether the opcode requires an open iteration scope.
var opNeedsScope = [OpEnd + 1]bool{
	OpJumpIfEnd:      true,
	OpIncrementIndex: true,
	OpDecrementIndex: true,
	OpIncrementCount: true,
	OpGetIndex:       true,
	OpGetCount:       true,
	OpGetLen:         true,
	OpGetAcc:         true,
	OpSetAcc:         true,
	OpSetIndex:       true,
	OpPointer:        true,
	OpGroupBy:        true,
	OpSortBy:         true,
	OpSort:           true,
}

// arraySize statically determines the element count consumed by an OpArray
// or OpMap instruction. The compiler always emits the size push immediately
// before the instruction; if the size cannot be determined statically, the
// second return value is false.
func arraySize(program *Program, ip int) (int, bool, error) {
	if ip == 0 {
		return 0, false, nil
	}
	var size int
	var ok bool
	switch prev := program.Bytecode[ip-1]; prev {
	case OpInt:
		size, ok = program.Arguments[ip-1], true
	case OpPush:
		arg := program.Arguments[ip-1]
		if arg >= 0 && arg < len(program.Constants) {
			if n, isInt := program.Constants[arg].(int); isInt {
				size, ok = n, true
			}
		}
	}
	if ok && size < 0 {
		return 0, false, errorf(ErrKindArgument, "negative collection size %d at ip %d", size, ip)
	}
	return size, ok, nil
}

// flowState is the abstract machine state tracked by validateFlow: the
// minimum evaluation stack depth and the exact iteration scope depth.
type flowState struct {
	stack int
	scope int
}

// validateFlow performs a dataflow analysis over the control flow graph of
// the bytecode. It rejects programs that can underflow the evaluation
// stack, use scope opcodes without an open scope, or leave scopes
// unbalanced on any path.
func validateFlow(program *Program) error {
	n := len(program.Bytecode)
	incoming := make([]flowState, n)
	reached := make([]bool, n)

	reached[0] = true
	queue := []int{0}

	for len(queue) > 0 {
		ip := queue[len(queue)-1]
		queue = queue[:len(queue)-1]

		state := incoming[ip]
		op := program.Bytecode[ip]
		arg := program.Arguments[ip]

		if opNeedsScope[op] && state.scope < 1 {
			return errorf(ErrKindScope, "opcode %d requires an open scope at ip %d", byte(op), ip)
		}

		out := state
		switch op {
		case OpBegin:
			out.scope++
		case OpEnd:
			out.scope--
			if out.scope < 0 {
				return errorf(ErrKindScope, "OpEnd without matching OpBegin at ip %d", ip)
			}
		}

		pops, pushes, dynamic := stackEffect(op, arg)
		if dynamic {
			size, known, err := arraySize(program, ip)
			if err != nil {
				return err
			}
			if known {
				pops = size + 1
				if op == OpMap {
					pops = 2*size + 1
				}
				pushes = 1
				if state.stack < pops {
					return errorf(ErrKindStack, "stack underflow at ip %d: need %d value(s), have %d",
						ip, pops, state.stack)
				}
				out.stack = state.stack - pops + pushes
			} else {
				// The element count is only known at runtime; require at
				// least the size value itself and keep a safe lower bound.
				if state.stack < 1 {
					return errorf(ErrKindStack, "stack underflow at ip %d: need %d value(s), have %d",
						ip, 1, state.stack)
				}
				out.stack = 1
			}
		} else {
			if state.stack < pops {
				return errorf(ErrKindStack, "stack underflow at ip %d: need %d value(s), have %d",
					ip, pops, state.stack)
			}
			out.stack = state.stack - pops + pushes
		}

		terminal := true
		for _, succ := range flowSuccessors(program, ip) {
			terminal = false
			if !reached[succ] {
				reached[succ] = true
				incoming[succ] = out
				queue = append(queue, succ)
				continue
			}
			merged := incoming[succ]
			if merged.scope != out.scope {
				return errorf(ErrKindScope, "unbalanced scopes: ip %d is reachable with scope depth %d and %d",
					succ, merged.scope, out.scope)
			}
			if out.stack < merged.stack {
				incoming[succ].stack = out.stack
				queue = append(queue, succ)
			}
		}
		if terminal && out.scope != 0 {
			return errorf(ErrKindScope, "program ends with %d unclosed scope(s)", out.scope)
		}
	}
	return nil
}

// flowSuccessors returns the instructions that can execute right after ip.
// A jump to one past the last instruction ends the program, as does
// falling off the end or executing OpThrow.
func flowSuccessors(program *Program, ip int) []int {
	n := len(program.Bytecode)
	op := program.Bytecode[ip]
	arg := program.Arguments[ip]

	withFallthrough := func(target int) []int {
		var out []int
		if ip+1 < n {
			out = append(out, ip+1)
		}
		if target < n {
			out = append(out, target)
		}
		return out
	}

	switch op {
	case OpJump:
		if target := ip + 1 + arg; target < n {
			return []int{target}
		}
		return nil
	case OpJumpIfTrue, OpJumpIfFalse, OpJumpIfNil, OpJumpIfNotNil, OpJumpIfEnd:
		return withFallthrough(ip + 1 + arg)
	case OpJumpBackward:
		if target := ip + 1 - arg; target < n {
			return []int{target}
		}
		return nil
	case OpThrow:
		return nil
	default:
		if ip+1 < n {
			return []int{ip + 1}
		}
		return nil
	}
}
