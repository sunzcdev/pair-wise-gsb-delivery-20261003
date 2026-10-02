// Copyright (c) 2026 Uber Technologies, Inc.
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

package zapcore_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var errBoom = errors.New("boom")

// partialFailObject writes a couple of fields and then fails.
type partialFailObject struct{ err error }

func (o partialFailObject) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddString("name", "alice")
	enc.AddInt("age", 30)
	return o.err
}

// nestedObject embeds another object under the "inner" key. It propagates
// the inner marshaler's error only if propagate is set.
type nestedObject struct {
	inner     zapcore.ObjectMarshaler
	propagate bool
}

func (o nestedObject) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddString("outer", "field")
	if err := enc.AddObject("inner", o.inner); err != nil && o.propagate {
		return err
	}
	enc.AddString("after", "inner")
	return nil
}

// partialFailArray appends a couple of elements and then fails.
type partialFailArray struct{ err error }

func (a partialFailArray) MarshalLogArray(enc zapcore.ArrayEncoder) error {
	enc.AppendString("ok-1")
	enc.AppendInt(42)
	return a.err
}

// objectArray appends each object, propagating errors only if propagate is
// set.
type objectArray struct {
	objs      []zapcore.ObjectMarshaler
	propagate bool
}

func (a objectArray) MarshalLogArray(enc zapcore.ArrayEncoder) error {
	for _, o := range a.objs {
		if err := enc.AppendObject(o); err != nil && a.propagate {
			return err
		}
	}
	return nil
}

// namespaceFailObject opens a namespace, writes into it, and then fails.
type namespaceFailObject struct{ err error }

func (o namespaceFailObject) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.OpenNamespace("inner-ns")
	enc.AddString("x", "y")
	return o.err
}

// bigObject stages more than 16 bytes without failing.
type bigObject struct{}

func (bigObject) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddString("data", strings.Repeat("x", 100))
	return nil
}

func strictEncoderConfig() zapcore.EncoderConfig {
	cfg := testEncoderConfig()
	cfg.StrictFieldIsolation = true
	return cfg
}

func encodeStrict(t *testing.T, cfg zapcore.EncoderConfig, fields []zapcore.Field) string {
	t.Helper()
	enc := zapcore.NewJSONEncoder(cfg)
	buf, err := enc.EncodeEntry(zapcore.Entry{
		Level:   zapcore.InfoLevel,
		Time:    _epoch,
		Message: "hello",
	}, fields)
	require.NoError(t, err)
	defer buf.Free()
	return buf.String()
}

func assertValidJSON(t *testing.T, s string) {
	t.Helper()
	var v interface{}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(s)), &v),
		"output must be valid JSON: %s", s)
}

func TestStrictIsolationFailedObjectReplaced(t *testing.T) {
	out := encodeStrict(t, strictEncoderConfig(), []zapcore.Field{
		zap.String("before", "1"),
		zap.Object("obj", partialFailObject{err: errBoom}),
		zap.String("after", "2"),
	})

	assert.Equal(t,
		`{"level":"info","ts":0,"msg":"hello","before":"1","obj":"<marshal failed: boom>","objError":"boom","after":"2"}`+"\n",
		out)
	assert.NotContains(t, out, "alice", "partial object output must not leak")
	assertValidJSON(t, out)
}

func TestStrictIsolationNestedObjectPropagated(t *testing.T) {
	out := encodeStrict(t, strictEncoderConfig(), []zapcore.Field{
		zap.Object("outer", nestedObject{
			inner:     partialFailObject{err: errBoom},
			propagate: true,
		}),
		zap.String("after", "2"),
	})

	// The inner failure propagates, so the whole outer object is replaced.
	assert.Equal(t,
		`{"level":"info","ts":0,"msg":"hello","outer":"<marshal failed: boom>","outerError":"boom","after":"2"}`+"\n",
		out)
	assert.NotContains(t, out, "alice")
	assertValidJSON(t, out)
}

func TestStrictIsolationNestedObjectContained(t *testing.T) {
	out := encodeStrict(t, strictEncoderConfig(), []zapcore.Field{
		zap.Object("outer", nestedObject{
			inner:     partialFailObject{err: errBoom},
			propagate: false,
		}),
	})

	// The outer marshaler swallows the inner error, so only the inner
	// object is replaced by the placeholder.
	assert.Equal(t,
		`{"level":"info","ts":0,"msg":"hello","outer":{"outer":"field","inner":"<marshal failed: boom>","after":"inner"}}`+"\n",
		out)
	assert.NotContains(t, out, "alice")
	assertValidJSON(t, out)
}

func TestStrictIsolationFailedArrayReplaced(t *testing.T) {
	out := encodeStrict(t, strictEncoderConfig(), []zapcore.Field{
		zap.Array("arr", partialFailArray{err: errBoom}),
		zap.String("after", "2"),
	})

	assert.Equal(t,
		`{"level":"info","ts":0,"msg":"hello","arr":"<marshal failed: boom>","arrError":"boom","after":"2"}`+"\n",
		out)
	assert.NotContains(t, out, "ok-1")
	assertValidJSON(t, out)
}

func TestStrictIsolationArrayElementObject(t *testing.T) {
	out := encodeStrict(t, strictEncoderConfig(), []zapcore.Field{
		zap.Array("arr", objectArray{
			objs: []zapcore.ObjectMarshaler{
				nestedObject{inner: partialFailObject{err: nil}},
				partialFailObject{err: errBoom},
			},
		}),
	})

	// Only the failed element is replaced; the array itself survives.
	assert.Equal(t,
		`{"level":"info","ts":0,"msg":"hello","arr":[{"outer":"field","inner":{"name":"alice","age":30},"after":"inner"},"<marshal failed: boom>"]}`+"\n",
		out)
	assertValidJSON(t, out)
}

func TestStrictIsolationInsideNamespace(t *testing.T) {
	out := encodeStrict(t, strictEncoderConfig(), []zapcore.Field{
		zap.Namespace("ns"),
		zap.Object("obj", partialFailObject{err: errBoom}),
		zap.String("s", "v"),
	})

	assert.Equal(t,
		`{"level":"info","ts":0,"msg":"hello","ns":{"obj":"<marshal failed: boom>","objError":"boom","s":"v"}}`+"\n",
		out)
	assertValidJSON(t, out)
}

func TestStrictIsolationNamespaceInsideFailedObject(t *testing.T) {
	out := encodeStrict(t, strictEncoderConfig(), []zapcore.Field{
		zap.Object("obj", namespaceFailObject{err: errBoom}),
		zap.String("after", "2"),
	})

	// The unclosed namespace opened by the failed object must not leak
	// into the rest of the entry.
	assert.Equal(t,
		`{"level":"info","ts":0,"msg":"hello","obj":"<marshal failed: boom>","objError":"boom","after":"2"}`+"\n",
		out)
	assert.NotContains(t, out, "inner-ns")
	assertValidJSON(t, out)
}

func TestStrictIsolationEncoderReuse(t *testing.T) {
	enc := zapcore.NewJSONEncoder(strictEncoderConfig())
	ent := zapcore.Entry{Level: zapcore.InfoLevel, Time: _epoch, Message: "hello"}

	buf, err := enc.EncodeEntry(ent, []zapcore.Field{
		zap.Object("obj", partialFailObject{err: errBoom}),
	})
	require.NoError(t, err)
	first := buf.String()
	buf.Free()
	assert.Contains(t, first, "<marshal failed: boom>")

	// A failed field must not pollute subsequent entries encoded with the
	// same (cloned/pooled) encoder.
	buf, err = enc.EncodeEntry(ent, []zapcore.Field{
		zap.String("clean", "yes"),
	})
	require.NoError(t, err)
	second := buf.String()
	buf.Free()

	assert.Equal(t,
		`{"level":"info","ts":0,"msg":"hello","clean":"yes"}`+"\n",
		second)
	assert.NotContains(t, second, "marshal failed")
	assert.NotContains(t, second, "alice")
	assertValidJSON(t, second)
}

func TestStrictIsolationCoreReuse(t *testing.T) {
	var sink bytes.Buffer
	core := zapcore.NewCore(
		zapcore.NewJSONEncoder(strictEncoderConfig()),
		zapcore.AddSync(&sink),
		zapcore.InfoLevel,
	)
	logger := zap.New(core)

	logger.Info("first", zap.Object("obj", partialFailObject{err: errBoom}))
	logger.Info("second", zap.String("clean", "yes"))

	lines := strings.Split(strings.TrimSpace(sink.String()), "\n")
	require.Len(t, lines, 2)

	assert.Contains(t, lines[0], `"obj":"<marshal failed: boom>"`)
	assertValidJSON(t, lines[0])

	assert.Contains(t, lines[1], `"clean":"yes"`)
	assert.NotContains(t, lines[1], "marshal failed")
	assert.NotContains(t, lines[1], "alice")
	assertValidJSON(t, lines[1])
}

func TestStrictIsolationStagingLimit(t *testing.T) {
	cfg := strictEncoderConfig()
	cfg.MaxStagedFieldBytes = 16

	out := encodeStrict(t, cfg, []zapcore.Field{
		zap.Object("obj", bigObject{}),
		zap.String("after", "2"),
	})

	// Exceeding the staging limit is treated like a marshaling failure.
	assert.Equal(t,
		`{"level":"info","ts":0,"msg":"hello","obj":"<marshal failed: staged field exceeds 16-byte limit>","objError":"staged field exceeds 16-byte limit","after":"2"}`+"\n",
		out)
	assert.NotContains(t, out, strings.Repeat("x", 100))
	assertValidJSON(t, out)
}

func TestStrictIsolationDisabledKeepsPartialOutput(t *testing.T) {
	// Without the opt-in, the historical behavior is preserved: partial
	// output from a failed marshaler is left in place.
	out := encodeStrict(t, testEncoderConfig(), []zapcore.Field{
		zap.Object("obj", partialFailObject{err: errBoom}),
		zap.String("after", "2"),
	})

	assert.Equal(t,
		`{"level":"info","ts":0,"msg":"hello","obj":{"name":"alice","age":30},"objError":"boom","after":"2"}`+"\n",
		out)
	assertValidJSON(t, out)
}

func TestStrictIsolationConsoleEncoder(t *testing.T) {
	enc := zapcore.NewConsoleEncoder(strictEncoderConfig())
	buf, err := enc.EncodeEntry(zapcore.Entry{
		Level:   zapcore.InfoLevel,
		Time:    _epoch,
		Message: "hello",
	}, []zapcore.Field{
		zap.Object("obj", partialFailObject{err: errBoom}),
		zap.String("after", "2"),
	})
	require.NoError(t, err)
	defer buf.Free()

	out := buf.String()
	assert.Contains(t, out, `"obj": "<marshal failed: boom>"`)
	assert.Contains(t, out, `"after": "2"`)
	assert.NotContains(t, out, "alice")
}
