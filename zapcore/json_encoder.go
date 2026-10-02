// Copyright (c) 2016 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package zapcore

import (
	"encoding/base64"
	"math"
	"sort"
	"time"
	"unicode/utf8"

	"go.uber.org/zap/buffer"
	"go.uber.org/zap/internal/bufferpool"
	"go.uber.org/zap/internal/pool"
)

// For JSON-escaping; see jsonEncoder.safeAddString below.
const _hex = "0123456789abcdef"

var _jsonPool = pool.New(func() *jsonEncoder {
	return &jsonEncoder{}
})

func putJSONEncoder(enc *jsonEncoder) {
	if enc.reflectBuf != nil {
		enc.reflectBuf.Free()
	}
	enc.EncoderConfig = nil
	enc.buf = nil
	enc.spaced = false
	enc.openNamespaces = 0
	enc.reflectBuf = nil
	enc.reflectEnc = nil
	enc.duplicateKeys = nil
	enc.pendingDeletions = nil
	enc.contextCopied = false
	_jsonPool.Put(enc)
}

type jsonEncoder struct {
	*EncoderConfig
	buf            *buffer.Buffer
	spaced         bool // include spaces after colons and commas
	openNamespaces int

	// for encoding generic values by reflection
	reflectBuf *buffer.Buffer
	reflectEnc ReflectedEncoder

	duplicateKeys    *duplicateKeyTracker
	pendingDeletions []keyRecord
	contextCopied    bool
}

// NewJSONEncoder creates a fast, low-allocation JSON encoder. The encoder
// appropriately escapes all field keys and values.
//
// By default, duplicate keys are written in encounter order. Set
// EncoderConfig.DuplicateKeyPolicy to keep either the first or last value,
// or to report the conflict.
func NewJSONEncoder(cfg EncoderConfig) Encoder {
	return newJSONEncoder(cfg, false)
}

func newJSONEncoder(cfg EncoderConfig, spaced bool) *jsonEncoder {
	if cfg.SkipLineEnding {
		cfg.LineEnding = ""
	} else if cfg.LineEnding == "" {
		cfg.LineEnding = DefaultLineEnding
	}

	// If no EncoderConfig.NewReflectedEncoder is provided by the user, then use default
	if cfg.NewReflectedEncoder == nil {
		cfg.NewReflectedEncoder = defaultReflectedEncoder
	}

	return &jsonEncoder{
		EncoderConfig: &cfg,
		buf:           bufferpool.Get(),
		spaced:        spaced,
	}
}

func (enc *jsonEncoder) reportDuplicateKey(info DuplicateKeyInfo) {
	if enc.OnDuplicateKey != nil {
		enc.OnDuplicateKey(info)
	}
}

func (enc *jsonEncoder) duplicateKeyPolicy() DuplicateKeyPolicy {
	if enc.EncoderConfig == nil {
		return ""
	}
	return enc.DuplicateKeyPolicy
}

func (enc *jsonEncoder) addKey(key string) bool {
	if tracker := enc.duplicateKeys; tracker != nil {
		if policy := enc.duplicateKeyPolicy(); policy != "" {
			if !tracker.prepare(key, enc.buf.Len(), policy, enc.reportDuplicateKey) {
				return false
			}
		} else {
			enc.writeKey(key)
			return true
		}
	} else if policy := enc.duplicateKeyPolicy(); policy != "" {
		enc.duplicateKeys = newDuplicateKeyTracker()
		if !enc.duplicateKeys.prepare(key, enc.buf.Len(), policy, enc.reportDuplicateKey) {
			return false
		}
	} else {
		enc.writeKey(key)
		return true
	}
	enc.writeKey(key)
	return true
}

func (enc *jsonEncoder) writeKey(key string) {
	enc.addElementSeparator()
	enc.buf.AppendByte('"')
	enc.safeAddString(key)
	enc.buf.AppendByte('"')
	enc.buf.AppendByte(':')
	if enc.spaced {
		enc.buf.AppendByte(' ')
	}
}

func (enc *jsonEncoder) finishKey() {
	if tracker := enc.duplicateKeys; tracker != nil && enc.DuplicateKeyPolicy != "" {
		tracker.finish(enc.buf.Len())
	}
}

func (enc *jsonEncoder) replaceKey(key string) bool {
	tracker := enc.duplicateKeys
	if tracker == nil || enc.DuplicateKeyPolicy != DuplicateKeyKeepLast {
		return false
	}
	record, ok := tracker.take(key)
	if !ok {
		return false
	}
	if record.inherited {
		if !enc.contextCopied {
			enc.pendingDeletions = append(enc.pendingDeletions, record)
		} else {
			start, end := tracker.remove(record, enc.buf.Bytes())
			enc.buf.Delete(start, end)
		}
		return true
	}
	start, end := tracker.remove(record, enc.buf.Bytes())
	enc.buf.Delete(start, end)
	return true
}

func (enc *jsonEncoder) addMemberKey(key string) bool {
	enc.replaceKey(key)
	return enc.addKey(key)
}

func (enc *jsonEncoder) AddArray(key string, arr ArrayMarshaler) error {
	if !enc.addMemberKey(key) {
		return nil
	}
	err := enc.AppendArray(arr)
	enc.finishKey()
	return err
}

func (enc *jsonEncoder) AddObject(key string, obj ObjectMarshaler) error {
	if !enc.addMemberKey(key) {
		return nil
	}
	err := enc.AppendObject(obj)
	enc.finishKey()
	return err
}

func (enc *jsonEncoder) AddBinary(key string, val []byte) {
	enc.AddString(key, base64.StdEncoding.EncodeToString(val))
}

func (enc *jsonEncoder) AddByteString(key string, val []byte) {
	if enc.addMemberKey(key) {
		enc.AppendByteString(val)
		enc.finishKey()
	}
}

func (enc *jsonEncoder) AddBool(key string, val bool) {
	if enc.addMemberKey(key) {
		enc.AppendBool(val)
		enc.finishKey()
	}
}

func (enc *jsonEncoder) AddComplex128(key string, val complex128) {
	if enc.addMemberKey(key) {
		enc.AppendComplex128(val)
		enc.finishKey()
	}
}

func (enc *jsonEncoder) AddComplex64(key string, val complex64) {
	if enc.addMemberKey(key) {
		enc.AppendComplex64(val)
		enc.finishKey()
	}
}

func (enc *jsonEncoder) AddDuration(key string, val time.Duration) {
	if enc.addMemberKey(key) {
		enc.AppendDuration(val)
		enc.finishKey()
	}
}

func (enc *jsonEncoder) AddFloat64(key string, val float64) {
	if enc.addMemberKey(key) {
		enc.AppendFloat64(val)
		enc.finishKey()
	}
}

func (enc *jsonEncoder) AddFloat32(key string, val float32) {
	if enc.addMemberKey(key) {
		enc.AppendFloat32(val)
		enc.finishKey()
	}
}

func (enc *jsonEncoder) AddInt64(key string, val int64) {
	if enc.addMemberKey(key) {
		enc.AppendInt64(val)
		enc.finishKey()
	}
}

func (enc *jsonEncoder) resetReflectBuf() {
	if enc.reflectBuf == nil {
		enc.reflectBuf = bufferpool.Get()
		enc.reflectEnc = enc.NewReflectedEncoder(enc.reflectBuf)
	} else {
		enc.reflectBuf.Reset()
	}
}

var nullLiteralBytes = []byte("null")

// Only invoke the standard JSON encoder if there is actually something to
// encode; otherwise write JSON null literal directly.
func (enc *jsonEncoder) encodeReflected(obj interface{}) ([]byte, error) {
	if obj == nil {
		return nullLiteralBytes, nil
	}
	enc.resetReflectBuf()
	if err := enc.reflectEnc.Encode(obj); err != nil {
		return nil, err
	}
	enc.reflectBuf.TrimNewline()
	return enc.reflectBuf.Bytes(), nil
}

func (enc *jsonEncoder) AddReflected(key string, obj interface{}) error {
	valueBytes, err := enc.encodeReflected(obj)
	if err != nil {
		return err
	}
	if !enc.addMemberKey(key) {
		return nil
	}
	_, err = enc.buf.Write(valueBytes)
	enc.finishKey()
	return err
}

func (enc *jsonEncoder) OpenNamespace(key string) {
	if !enc.addMemberKey(key) {
		return
	}
	enc.buf.AppendByte('{')
	enc.openNamespaces++
	if enc.duplicateKeys != nil && enc.DuplicateKeyPolicy != "" {
		enc.duplicateKeys.openNamespace()
	}
}

func (enc *jsonEncoder) AddString(key, val string) {
	if enc.addMemberKey(key) {
		enc.AppendString(val)
		enc.finishKey()
	}
}

func (enc *jsonEncoder) AddTime(key string, val time.Time) {
	if enc.addMemberKey(key) {
		enc.AppendTime(val)
		enc.finishKey()
	}
}

func (enc *jsonEncoder) AddUint64(key string, val uint64) {
	if enc.addMemberKey(key) {
		enc.AppendUint64(val)
		enc.finishKey()
	}
}

func (enc *jsonEncoder) AppendArray(arr ArrayMarshaler) error {
	enc.addElementSeparator()
	enc.buf.AppendByte('[')
	if enc.duplicateKeys != nil && enc.DuplicateKeyPolicy != "" {
		enc.duplicateKeys.openArray()
	}
	err := arr.MarshalLogArray(enc)
	enc.buf.AppendByte(']')
	if enc.duplicateKeys != nil && enc.DuplicateKeyPolicy != "" {
		enc.duplicateKeys.closeArray(enc.buf.Len())
	}
	return err
}

func (enc *jsonEncoder) AppendObject(obj ObjectMarshaler) error {
	// Close ONLY new openNamespaces that are created during
	// AppendObject().
	old := enc.openNamespaces
	enc.openNamespaces = 0
	enc.addElementSeparator()
	enc.buf.AppendByte('{')
	if enc.duplicateKeys != nil && enc.DuplicateKeyPolicy != "" {
		if scopes := enc.duplicateKeys.scopes; len(scopes) > 0 && scopes[len(scopes)-1].array {
			enc.duplicateKeys.arrayElement()
		}
		enc.duplicateKeys.openObject()
	}
	err := obj.MarshalLogObject(enc)
	enc.buf.AppendByte('}')
	if enc.duplicateKeys != nil && enc.DuplicateKeyPolicy != "" {
		enc.duplicateKeys.closeObject(enc.buf.Len())
	}
	enc.closeOpenNamespaces()
	enc.openNamespaces = old
	return err
}

func (enc *jsonEncoder) AppendBool(val bool) {
	enc.addElementSeparator()
	enc.buf.AppendBool(val)
}

func (enc *jsonEncoder) AppendByteString(val []byte) {
	enc.addElementSeparator()
	enc.buf.AppendByte('"')
	enc.safeAddByteString(val)
	enc.buf.AppendByte('"')
}

// appendComplex appends the encoded form of the provided complex128 value.
// precision specifies the encoding precision for the real and imaginary
// components of the complex number.
func (enc *jsonEncoder) appendComplex(val complex128, precision int) {
	enc.addElementSeparator()
	// Cast to a platform-independent, fixed-size type.
	r, i := float64(real(val)), float64(imag(val))
	enc.buf.AppendByte('"')
	// Because we're always in a quoted string, we can use strconv without
	// special-casing NaN and +/-Inf.
	enc.buf.AppendFloat(r, precision)
	// If imaginary part is less than 0, minus (-) sign is added by default
	// by AppendFloat.
	if i >= 0 {
		enc.buf.AppendByte('+')
	}
	enc.buf.AppendFloat(i, precision)
	enc.buf.AppendByte('i')
	enc.buf.AppendByte('"')
}

func (enc *jsonEncoder) AppendDuration(val time.Duration) {
	cur := enc.buf.Len()
	if e := enc.EncodeDuration; e != nil {
		e(val, enc)
	}
	if cur == enc.buf.Len() {
		// User-supplied EncodeDuration is a no-op. Fall back to nanoseconds to keep
		// JSON valid.
		enc.AppendInt64(int64(val))
	}
}

func (enc *jsonEncoder) AppendInt64(val int64) {
	enc.addElementSeparator()
	enc.buf.AppendInt(val)
}

func (enc *jsonEncoder) AppendReflected(val interface{}) error {
	valueBytes, err := enc.encodeReflected(val)
	if err != nil {
		return err
	}
	enc.addElementSeparator()
	_, err = enc.buf.Write(valueBytes)
	return err
}

func (enc *jsonEncoder) AppendString(val string) {
	enc.addElementSeparator()
	enc.buf.AppendByte('"')
	enc.safeAddString(val)
	enc.buf.AppendByte('"')
}

func (enc *jsonEncoder) AppendTimeLayout(time time.Time, layout string) {
	enc.addElementSeparator()
	enc.buf.AppendByte('"')
	enc.buf.AppendTime(time, layout)
	enc.buf.AppendByte('"')
}

func (enc *jsonEncoder) AppendTime(val time.Time) {
	cur := enc.buf.Len()
	if e := enc.EncodeTime; e != nil {
		e(val, enc)
	}
	if cur == enc.buf.Len() {
		// User-supplied EncodeTime is a no-op. Fall back to nanos since epoch to keep
		// output JSON valid.
		enc.AppendInt64(val.UnixNano())
	}
}

func (enc *jsonEncoder) AppendUint64(val uint64) {
	enc.addElementSeparator()
	enc.buf.AppendUint(val)
}

func (enc *jsonEncoder) AddInt(k string, v int)         { enc.AddInt64(k, int64(v)) }
func (enc *jsonEncoder) AddInt32(k string, v int32)     { enc.AddInt64(k, int64(v)) }
func (enc *jsonEncoder) AddInt16(k string, v int16)     { enc.AddInt64(k, int64(v)) }
func (enc *jsonEncoder) AddInt8(k string, v int8)       { enc.AddInt64(k, int64(v)) }
func (enc *jsonEncoder) AddUint(k string, v uint)       { enc.AddUint64(k, uint64(v)) }
func (enc *jsonEncoder) AddUint32(k string, v uint32)   { enc.AddUint64(k, uint64(v)) }
func (enc *jsonEncoder) AddUint16(k string, v uint16)   { enc.AddUint64(k, uint64(v)) }
func (enc *jsonEncoder) AddUint8(k string, v uint8)     { enc.AddUint64(k, uint64(v)) }
func (enc *jsonEncoder) AddUintptr(k string, v uintptr) { enc.AddUint64(k, uint64(v)) }
func (enc *jsonEncoder) AppendComplex64(v complex64)    { enc.appendComplex(complex128(v), 32) }
func (enc *jsonEncoder) AppendComplex128(v complex128)  { enc.appendComplex(complex128(v), 64) }
func (enc *jsonEncoder) AppendFloat64(v float64)        { enc.appendFloat(v, 64) }
func (enc *jsonEncoder) AppendFloat32(v float32)        { enc.appendFloat(float64(v), 32) }
func (enc *jsonEncoder) AppendInt(v int)                { enc.AppendInt64(int64(v)) }
func (enc *jsonEncoder) AppendInt32(v int32)            { enc.AppendInt64(int64(v)) }
func (enc *jsonEncoder) AppendInt16(v int16)            { enc.AppendInt64(int64(v)) }
func (enc *jsonEncoder) AppendInt8(v int8)              { enc.AppendInt64(int64(v)) }
func (enc *jsonEncoder) AppendUint(v uint)              { enc.AppendUint64(uint64(v)) }
func (enc *jsonEncoder) AppendUint32(v uint32)          { enc.AppendUint64(uint64(v)) }
func (enc *jsonEncoder) AppendUint16(v uint16)          { enc.AppendUint64(uint64(v)) }
func (enc *jsonEncoder) AppendUint8(v uint8)            { enc.AppendUint64(uint64(v)) }
func (enc *jsonEncoder) AppendUintptr(v uintptr)        { enc.AppendUint64(uint64(v)) }

func (enc *jsonEncoder) Clone() Encoder {
	clone := enc.clone()
	clone.buf.Write(enc.buf.Bytes())
	return clone
}

func (enc *jsonEncoder) clone() *jsonEncoder {
	clone := _jsonPool.Get()
	clone.EncoderConfig = enc.EncoderConfig
	clone.spaced = enc.spaced
	clone.openNamespaces = enc.openNamespaces
	clone.buf = bufferpool.Get()
	clone.duplicateKeys = enc.duplicateKeys.clone()
	clone.pendingDeletions = append([]keyRecord(nil), enc.pendingDeletions...)
	return clone
}

func (enc *jsonEncoder) addEntryKey(key string, appendValue func()) {
	if !enc.addMemberKey(key) {
		return
	}
	appendValue()
	enc.finishKey()
}

func (enc *jsonEncoder) writeInheritedContext(src []byte) {
	if len(src) == 0 {
		if enc.duplicateKeys != nil {
			enc.duplicateKeys.attachEntryScopes()
		}
		return
	}
	if enc.duplicateKeyPolicy() == "" || enc.duplicateKeys == nil {
		enc.addElementSeparator()
		enc.buf.Write(src)
		return
	}
	deletions := append([]keyRecord(nil), enc.pendingDeletions...)
	sort.Slice(deletions, func(i, j int) bool {
		return deletions[i].start > deletions[j].start
	})
	metadataLen := enc.buf.Len()
	enc.addElementSeparator()
	prefixLen := enc.buf.Len()
	enc.duplicateKeys.shiftInherited(0, prefixLen)
	enc.duplicateKeys.shiftPendingInherited(prefixLen)
	enc.buf.Write(src)
	enc.contextCopied = true
	removed := 0
	for _, record := range deletions {
		from := prefixLen + record.start - removed
		to := prefixLen + record.end - removed
		for from > 0 && enc.buf.Bytes()[from-1] == ' ' {
			from--
		}
		if from > 0 && enc.buf.Bytes()[from-1] == ',' {
			from--
		}
		enc.buf.Delete(from, to)
		enc.duplicateKeys.shiftInherited(to, from-to)
		removed += to - from
	}
	if enc.buf.Len() <= prefixLen {
		enc.buf.Delete(metadataLen, enc.buf.Len())
	}
	enc.duplicateKeys.attachEntryScopes()
}

func (enc *jsonEncoder) EncodeEntry(ent Entry, fields []Field) (*buffer.Buffer, error) {
	final := enc.clone()
	final.buf.AppendByte('{')
	if final.duplicateKeys != nil && final.DuplicateKeyPolicy != "" {
		final.duplicateKeys.beginEntry()
	}

	if final.LevelKey != "" && final.EncodeLevel != nil {
		final.addEntryKey(final.LevelKey, func() {
			cur := final.buf.Len()
			final.EncodeLevel(ent.Level, final)
			if cur == final.buf.Len() {
				// User-supplied EncodeLevel was a no-op. Fall back to strings to keep
				// output JSON valid.
				final.AppendString(ent.Level.String())
			}
		})
	}
	if final.TimeKey != "" && !ent.Time.IsZero() {
		final.AddTime(final.TimeKey, ent.Time)
	}
	if ent.LoggerName != "" && final.NameKey != "" {
		final.addEntryKey(final.NameKey, func() {
			cur := final.buf.Len()
			nameEncoder := final.EncodeName

			// if no name encoder provided, fall back to FullNameEncoder for backwards
			// compatibility
			if nameEncoder == nil {
				nameEncoder = FullNameEncoder
			}

			nameEncoder(ent.LoggerName, final)
			if cur == final.buf.Len() {
				// User-supplied EncodeName was a no-op. Fall back to strings to
				// keep output JSON valid.
				final.AppendString(ent.LoggerName)
			}
		})
	}
	if ent.Caller.Defined {
		if final.CallerKey != "" {
			final.addEntryKey(final.CallerKey, func() {
				cur := final.buf.Len()
				final.EncodeCaller(ent.Caller, final)
				if cur == final.buf.Len() {
					// User-supplied EncodeCaller was a no-op. Fall back to strings to
					// keep output JSON valid.
					final.AppendString(ent.Caller.String())
				}
			})
		}
		if final.FunctionKey != "" {
			final.AddString(final.FunctionKey, ent.Caller.Function)
		}
	}
	if final.MessageKey != "" {
		final.AddString(final.MessageKey, ent.Message)
	}
	final.writeInheritedContext(enc.buf.Bytes())
	addFields(final, fields)
	final.closeOpenNamespaces()
	if ent.Stack != "" && final.StacktraceKey != "" {
		final.AddString(final.StacktraceKey, ent.Stack)
	}
	final.buf.AppendByte('}')
	final.buf.AppendString(final.LineEnding)

	ret := final.buf
	putJSONEncoder(final)
	return ret, nil
}

func (enc *jsonEncoder) truncate() {
	enc.buf.Reset()
}

func (enc *jsonEncoder) closeOpenNamespaces() {
	for i := 0; i < enc.openNamespaces; i++ {
		enc.buf.AppendByte('}')
		if enc.duplicateKeys != nil && enc.DuplicateKeyPolicy != "" {
			enc.duplicateKeys.closeNamespace(enc.buf.Len())
		}
	}
	enc.openNamespaces = 0
}

func (enc *jsonEncoder) addElementSeparator() {
	last := enc.buf.Len() - 1
	if last < 0 {
		return
	}
	switch enc.buf.Bytes()[last] {
	case '{', '[', ':', ',', ' ':
		return
	default:
		enc.buf.AppendByte(',')
		if enc.spaced {
			enc.buf.AppendByte(' ')
		}
	}
}

func (enc *jsonEncoder) appendFloat(val float64, bitSize int) {
	enc.addElementSeparator()
	switch {
	case math.IsNaN(val):
		enc.buf.AppendString(`"NaN"`)
	case math.IsInf(val, 1):
		enc.buf.AppendString(`"+Inf"`)
	case math.IsInf(val, -1):
		enc.buf.AppendString(`"-Inf"`)
	default:
		enc.buf.AppendFloat(val, bitSize)
	}
}

// safeAddString JSON-escapes a string and appends it to the internal buffer.
// Unlike the standard library's encoder, it doesn't attempt to protect the
// user from browser vulnerabilities or JSONP-related problems.
func (enc *jsonEncoder) safeAddString(s string) {
	safeAppendStringLike(
		(*buffer.Buffer).AppendString,
		utf8.DecodeRuneInString,
		enc.buf,
		s,
	)
}

// safeAddByteString is no-alloc equivalent of safeAddString(string(s)) for s []byte.
func (enc *jsonEncoder) safeAddByteString(s []byte) {
	safeAppendStringLike(
		(*buffer.Buffer).AppendBytes,
		utf8.DecodeRune,
		enc.buf,
		s,
	)
}

// safeAppendStringLike is a generic implementation of safeAddString and safeAddByteString.
// It appends a string or byte slice to the buffer, escaping all special characters.
func safeAppendStringLike[S []byte | string](
	// appendTo appends this string-like object to the buffer.
	appendTo func(*buffer.Buffer, S),
	// decodeRune decodes the next rune from the string-like object
	// and returns its value and width in bytes.
	decodeRune func(S) (rune, int),
	buf *buffer.Buffer,
	s S,
) {
	// The encoding logic below works by skipping over characters
	// that can be safely copied as-is,
	// until a character is found that needs special handling.
	// At that point, we copy everything we've seen so far,
	// and then handle that special character.
	//
	// last is the index of the last byte that was copied to the buffer.
	last := 0
	for i := 0; i < len(s); {
		if s[i] >= utf8.RuneSelf {
			// Character >= RuneSelf may be part of a multi-byte rune.
			// They need to be decoded before we can decide how to handle them.
			r, size := decodeRune(s[i:])
			if r != utf8.RuneError || size != 1 {
				// No special handling required.
				// Skip over this rune and continue.
				i += size
				continue
			}

			// Invalid UTF-8 sequence.
			// Replace it with the Unicode replacement character.
			appendTo(buf, s[last:i])
			buf.AppendString(`\ufffd`)

			i++
			last = i
		} else {
			// Character < RuneSelf is a single-byte UTF-8 rune.
			if s[i] >= 0x20 && s[i] != '\\' && s[i] != '"' {
				// No escaping necessary.
				// Skip over this character and continue.
				i++
				continue
			}

			// This character needs to be escaped.
			appendTo(buf, s[last:i])
			switch s[i] {
			case '\\', '"':
				buf.AppendByte('\\')
				buf.AppendByte(s[i])
			case '\n':
				buf.AppendByte('\\')
				buf.AppendByte('n')
			case '\r':
				buf.AppendByte('\\')
				buf.AppendByte('r')
			case '\t':
				buf.AppendByte('\\')
				buf.AppendByte('t')
			default:
				// Encode bytes < 0x20, except for the escape sequences above.
				buf.AppendString(`\u00`)
				buf.AppendByte(_hex[s[i]>>4])
				buf.AppendByte(_hex[s[i]&0xF])
			}

			i++
			last = i
		}
	}

	// add remaining
	appendTo(buf, s[last:])
}
