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
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func dedupConfig(policy zapcore.DuplicateKeyPolicy, onDup zapcore.DuplicateKeyFunc) zapcore.EncoderConfig {
	return zapcore.EncoderConfig{
		MessageKey:     "msg",
		LevelKey:       "",
		TimeKey:        "",
		NameKey:        "",
		CallerKey:      "",
		EncodeTime:     zapcore.EpochTimeEncoder,
		EncodeDuration: zapcore.SecondsDurationEncoder,
		DuplicateKeys:  policy,
		OnDuplicateKey: onDup,
	}
}

func encodeWith(t *testing.T, cfg zapcore.EncoderConfig, context, fields []zapcore.Field) string {
	t.Helper()
	enc := zapcore.NewJSONEncoder(cfg)
	ent := zapcore.Entry{Level: zapcore.InfoLevel, Message: "m"}
	for _, f := range context {
		f.AddTo(enc)
	}
	out, err := enc.EncodeEntry(ent, fields)
	require.NoError(t, err)
	defer out.Free()
	return strings.TrimSpace(out.String())
}

func TestDuplicateKeyPolicies(t *testing.T) {
	t.Run("allow is historical behavior", func(t *testing.T) {
		out := encodeWith(t, dedupConfig(zapcore.AllowDuplicateKeys, nil),
			[]zapcore.Field{zap.String("request_id", "from-with")},
			[]zapcore.Field{zap.String("request_id", "from-call")})
		assert.Equal(t,
			`{"msg":"m","request_id":"from-with","request_id":"from-call"}`, out)
	})

	t.Run("keep first wins from With", func(t *testing.T) {
		out := encodeWith(t, dedupConfig(zapcore.KeepFirst, nil),
			[]zapcore.Field{zap.String("request_id", "from-with")},
			[]zapcore.Field{zap.String("request_id", "from-call")})
		assertValidJSON(t, out)
		assert.Equal(t,
			`{"msg":"m","request_id":"from-with"}`, out)
	})

	t.Run("keep last wins from call", func(t *testing.T) {
		out := encodeWith(t, dedupConfig(zapcore.KeepLast, nil),
			[]zapcore.Field{zap.String("request_id", "from-with")},
			[]zapcore.Field{zap.String("request_id", "from-call")})
		assertValidJSON(t, out)
		// Surviving value stays at the first occurrence's position, before the
		// later call-site field.
		assert.Equal(t,
			`{"msg":"m","request_id":"from-call"}`, out)
	})

	t.Run("report keeps both and records one conflict", func(t *testing.T) {
		var conflicts []zapcore.DuplicateKeyConflict
		out := encodeWith(t, dedupConfig(zapcore.ReportConflict,
			func(c zapcore.DuplicateKeyConflict) { conflicts = append(conflicts, c) }),
			[]zapcore.Field{zap.String("request_id", "from-with")},
			[]zapcore.Field{zap.String("request_id", "from-call")})
		assert.Equal(t,
			`{"msg":"m","request_id":"from-with","request_id":"from-call"}`, out)
		require.Len(t, conflicts, 1)
		assert.Equal(t, zapcore.DuplicateKeyConflict{
			Path:          "",
			Key:           "request_id",
			ExistingValue: `"from-with"`,
			NewValue:      `"from-call"`,
		}, conflicts[0])
	})
}

// Same-named keys in different namespaces/objects must never be merged.
func TestDuplicateKeyScopedPerObject(t *testing.T) {
	fields := []zapcore.Field{
		zap.String("request_id", "root"),
		zap.Namespace("ns1"),
		zap.String("request_id", "one"),
		zap.Namespace("ns2"),
		zap.String("request_id", "two"),
	}

	for _, policy := range []zapcore.DuplicateKeyPolicy{
		zapcore.KeepFirst,
		zapcore.KeepLast,
	} {
		out := encodeWith(t, dedupConfig(policy, nil), fields, nil)
		assertValidJSON(t, out)
		var got map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(out), &got))
		assert.Equal(t, "root", got["request_id"])
		ns1 := got["ns1"].(map[string]interface{})
		assert.Equal(t, "one", ns1["request_id"])
		ns2 := ns1["ns2"].(map[string]interface{})
		assert.Equal(t, "two", ns2["request_id"])
	}
}

// A repeated key inside a marshaled object only affects that object.
func TestDuplicateKeyNestedObject(t *testing.T) {
	marshaler := zapcore.ObjectMarshalerFunc(func(enc zapcore.ObjectEncoder) error {
		enc.AddString("k", "first")
		enc.AddString("k", "second")
		enc.AddString("other", "x")
		return nil
	})

	t.Run("keep first in object", func(t *testing.T) {
		var conflicts []zapcore.DuplicateKeyConflict
		cfg := dedupConfig(zapcore.KeepFirst,
			func(c zapcore.DuplicateKeyConflict) { conflicts = append(conflicts, c) })
		enc := zapcore.NewJSONEncoder(cfg)
		out, err := enc.EncodeEntry(
			zapcore.Entry{Message: "m"},
			[]zapcore.Field{zap.Object("obj", marshaler), zap.String("k", "rootk")})
		require.NoError(t, err)
		defer out.Free()
		s := strings.TrimSpace(out.String())
		assertValidJSON(t, s)
		assert.Contains(t, s, `"obj":{"k":"first","other":"x"}`)
		assert.Contains(t, s, `"k":"rootk"`)
		assert.Empty(t, conflicts)
	})

	t.Run("keep last in object reports nested path", func(t *testing.T) {
		var conflicts []zapcore.DuplicateKeyConflict
		cfg := dedupConfig(zapcore.ReportConflict,
			func(c zapcore.DuplicateKeyConflict) { conflicts = append(conflicts, c) })
		enc := zapcore.NewJSONEncoder(cfg)
		out, err := enc.EncodeEntry(
			zapcore.Entry{Message: "m"},
			[]zapcore.Field{zap.Object("obj", marshaler)})
		require.NoError(t, err)
		defer out.Free()
		s := strings.TrimSpace(out.String())
		assert.Contains(t, s, `"obj":{"k":"first","k":"second","other":"x"}`)
		require.Len(t, conflicts, 1)
		assert.Equal(t, "obj", conflicts[0].Path)
		assert.Equal(t, "k", conflicts[0].Key)
		assert.Equal(t, `"first"`, conflicts[0].ExistingValue)
		assert.Equal(t, `"second"`, conflicts[0].NewValue)
	})
}

// Custom fields (ObjectMarshaler/ArrayMarshaler/Stringer) must participate,
// and field order must remain stable.
func TestDuplicateKeyCustomFieldsAndOrder(t *testing.T) {
	calls := make([]string, 0)
	custom := zapcore.ObjectMarshalerFunc(func(enc zapcore.ObjectEncoder) error {
		calls = append(calls, "marshaled")
		enc.AddString("deep", "v")
		return nil
	})

	context := []zapcore.Field{
		zap.String("a", "1"),
		zap.Object("obj", custom),
		zap.String("z", "2"),
	}
	fields := []zapcore.Field{
		zap.String("a", "2"),
		zap.String("m", "3"),
	}

	t.Run("keep first keeps order and skips evaluation of dropped fields", func(t *testing.T) {
		calls = calls[:0]
		out := encodeWith(t, dedupConfig(zapcore.KeepFirst, nil), context, fields)
		assertValidJSON(t, out)
		assert.Equal(t,
			`{"msg":"m","a":"1","obj":{"deep":"v"},"z":"2","m":"3"}`, out)
		assert.Equal(t, []string{"marshaled"}, calls)
	})

	t.Run("keep last moves surviving value to last occurrence position", func(t *testing.T) {
		calls = calls[:0]
		out := encodeWith(t, dedupConfig(zapcore.KeepLast, nil), context, fields)
		assertValidJSON(t, out)
		// The surviving "a" occupies the position where it last appeared
		// (call site), after the surviving context fields; order is stable.
		assert.Equal(t,
			`{"msg":"m","obj":{"deep":"v"},"z":"2","a":"2","m":"3"}`, out)
		// KeepLast evaluates every field.
		assert.Equal(t, []string{"marshaled"}, calls)
	})
}

// A dropped KeepFirst lazy field must not be evaluated at all.
func TestKeepFirstDoesNotEvaluateDroppedLazyField(t *testing.T) {
	evaluated := false
	lazy := zapcore.ObjectMarshalerFunc(func(enc zapcore.ObjectEncoder) error {
		evaluated = true
		return nil
	})
	out := encodeWith(t, dedupConfig(zapcore.KeepFirst, nil),
		[]zapcore.Field{zap.String("k", "first")},
		[]zapcore.Field{zap.Object("k", lazy)})
	assert.False(t, evaluated, "dropped object field must not be marshaled")
	assert.NotContains(t, out, `"k":{}`)
	assertValidJSON(t, out)
}

// WithLazy defers field evaluation until the first write, and the policy then
// applies to the lazily-added context fields.
func TestDuplicateKeyWithLazy(t *testing.T) {
	evaluated := false
	lazy := zapcore.ObjectMarshalerFunc(func(enc zapcore.ObjectEncoder) error {
		evaluated = true
		enc.AddString("v", "lazy")
		return nil
	})

	var sink strings.Builder
	enc := zapcore.NewJSONEncoder(dedupConfig(zapcore.KeepFirst, nil))
	core := zapcore.NewCore(enc, zapcore.AddSync(&sink), zapcore.InfoLevel)
	lazyCore := zapcore.NewLazyWith(core,
		[]zapcore.Field{
			zap.Object("obj", lazy),
			zap.String("request_id", "from-with"),
		})
	logger := zap.New(lazyCore)

	assert.False(t, evaluated, "WithLazy fields must not be evaluated before logging")

	// A disabled log must not force evaluation either.
	logger.Debug("nope", zap.String("request_id", "from-call"))
	assert.False(t, evaluated, "WithLazy fields must not be evaluated for disabled logs")

	logger.Info("hello", zap.String("request_id", "from-call"))
	assert.True(t, evaluated, "WithLazy fields must be evaluated at log time")

	out := strings.TrimSpace(sink.String())
	assertValidJSON(t, out)
	// KeepFirst: With-supplied request_id wins over the call-site one, and
	// the lazily evaluated object is still present.
	assert.Contains(t, out, `"request_id":"from-with"`)
	assert.NotContains(t, out, `"request_id":"from-call"`)
	assert.Contains(t, out, `"obj":{"v":"lazy"}`)
}

func TestDuplicateKeyPolicyUnmarshal(t *testing.T) {
	for _, tc := range []struct {
		text string
		want zapcore.DuplicateKeyPolicy
	}{
		{"keepFirst", zapcore.KeepFirst},
		{"keepLast", zapcore.KeepLast},
		{"reportConflict", zapcore.ReportConflict},
		{"", zapcore.AllowDuplicateKeys},
		{"bogus", zapcore.AllowDuplicateKeys},
	} {
		var p zapcore.DuplicateKeyPolicy
		require.NoError(t, p.UnmarshalText([]byte(tc.text)))
		assert.Equal(t, tc.want, p)
	}
	assert.Equal(t, "keepFirst", zapcore.KeepFirst.String())
	assert.Equal(t, "keepLast", zapcore.KeepLast.String())
	assert.Equal(t, "reportConflict", zapcore.ReportConflict.String())
	assert.Equal(t, "allowDuplicateKeys", zapcore.AllowDuplicateKeys.String())
}

func assertValidJSON(t *testing.T, s string) {
	t.Helper()
	var v interface{}
	require.Truef(t, json.Valid([]byte(s)), "invalid JSON: %s", s)
	require.NoError(t, json.Unmarshal([]byte(s), &v))
}

// Objects nested inside arrays dedup independently of the enclosing object.
func TestDuplicateKeyObjectInsideArray(t *testing.T) {
	item := zapcore.ObjectMarshalerFunc(func(enc zapcore.ObjectEncoder) error {
		enc.AddString("k", "a")
		enc.AddString("k", "b")
		return nil
	})
	arr := zapcore.ArrayMarshalerFunc(func(enc zapcore.ArrayEncoder) error {
		return enc.AppendObject(item)
	})

	for _, policy := range []zapcore.DuplicateKeyPolicy{zapcore.KeepFirst, zapcore.KeepLast} {
		enc := zapcore.NewJSONEncoder(dedupConfig(policy, nil))
		out, err := enc.EncodeEntry(zapcore.Entry{Message: "m"},
			[]zapcore.Field{zap.Array("items", arr), zap.String("k", "outer")})
		require.NoError(t, err)
		s := strings.TrimSpace(out.String())
		out.Free()
		assertValidJSON(t, s)
		want := `"items":[{"k":"a"}]`
		if policy == zapcore.KeepLast {
			want = `"items":[{"k":"b"}]`
		}
		assert.Contains(t, s, want)
		assert.Contains(t, s, `"k":"outer"`)
	}
}

// InlineMarshaler fields merge into the current object, so their keys
// participate in deduplication there.
func TestDuplicateKeyInlineMarshaler(t *testing.T) {
	inline := zap.Inline(zapcore.ObjectMarshalerFunc(func(enc zapcore.ObjectEncoder) error {
		enc.AddString("shared", "inline")
		return nil
	}))
	out := encodeWith(t, dedupConfig(zapcore.KeepFirst, nil),
		[]zapcore.Field{zap.String("shared", "ctx")},
		[]zapcore.Field{inline, zap.String("call", "x")})
	assertValidJSON(t, out)
	assert.Equal(t, `{"msg":"m","shared":"ctx","call":"x"}`, out)
}

// Conflict values are reported exactly as written for non-string values and
// inside nested objects.
func TestReportConflictValuesAndPaths(t *testing.T) {
	var conflicts []zapcore.DuplicateKeyConflict
	cfg := dedupConfig(zapcore.ReportConflict,
		func(c zapcore.DuplicateKeyConflict) { conflicts = append(conflicts, c) })
	nested := zapcore.ObjectMarshalerFunc(func(enc zapcore.ObjectEncoder) error {
		enc.AddInt64("n", 1)
		enc.AddInt64("n", 2)
		enc.AddBool("b", true)
		enc.AddBool("b", false)
		return nil
	})
	enc := zapcore.NewJSONEncoder(cfg)
	out, err := enc.EncodeEntry(zapcore.Entry{Message: "m"},
		[]zapcore.Field{
			zap.Object("payload", nested),
			zap.Int64("payload_n", 0),
		})
	require.NoError(t, err)
	out.Free()

	require.Len(t, conflicts, 2)
	assert.Equal(t, "payload", conflicts[0].Path)
	assert.Equal(t, "n", conflicts[0].Key)
	assert.Equal(t, "1", conflicts[0].ExistingValue)
	assert.Equal(t, "2", conflicts[0].NewValue)
	assert.Equal(t, "b", conflicts[1].Key)
	assert.Equal(t, "true", conflicts[1].ExistingValue)
	assert.Equal(t, "false", conflicts[1].NewValue)
}

// All primitive field types produce valid JSON under both policies.
func TestDuplicateKeyPrimitiveTypes(t *testing.T) {
	first := []zapcore.Field{
		zap.Int("i", 1),
		zap.Float64("f", 1.5),
		zap.Bool("b", true),
		zap.Duration("d", 1),
	}
	second := []zapcore.Field{
		zap.Int("i", 2),
		zap.Float64("f", 2.5),
		zap.Bool("b", false),
		zap.Duration("d", 2),
	}
	for _, policy := range []zapcore.DuplicateKeyPolicy{zapcore.KeepFirst, zapcore.KeepLast} {
		out := encodeWith(t, dedupConfig(policy, nil), first, second)
		assertValidJSON(t, out)
		if policy == zapcore.KeepFirst {
			assert.Equal(t, `{"msg":"m","i":1,"f":1.5,"b":true,"d":0.000000001}`, out)
		} else {
			assert.Equal(t, `{"msg":"m","i":2,"f":2.5,"b":false,"d":0.000000002}`, out)
		}
	}
}

// Dropping every value in an object leaves a valid empty object.
func TestDuplicateKeyAllDroppedLeavesEmptyObject(t *testing.T) {
	obj := zapcore.ObjectMarshalerFunc(func(enc zapcore.ObjectEncoder) error {
		enc.AddString("x", "1")
		enc.AddString("x", "2")
		return nil
	})
	enc := zapcore.NewJSONEncoder(dedupConfig(zapcore.KeepFirst, nil))
	out, err := enc.EncodeEntry(zapcore.Entry{Message: "m"},
		[]zapcore.Field{zap.Object("o", obj)})
	require.NoError(t, err)
	s := strings.TrimSpace(out.String())
	out.Free()
	assertValidJSON(t, s)
	assert.Contains(t, s, `"o":{"x":"1"}`)
}

// A duplicate of a built-in header key (e.g. message/level) follows the same
// policy, scoped to the root object.
func TestDuplicateKeyHeaderCollision(t *testing.T) {
	cfg := dedupConfig(zapcore.KeepFirst, nil)
	enc := zapcore.NewJSONEncoder(cfg)
	out, err := enc.EncodeEntry(zapcore.Entry{Message: "real"},
		[]zapcore.Field{zap.String("msg", "override")})
	require.NoError(t, err)
	s := strings.TrimSpace(out.String())
	out.Free()
	assertValidJSON(t, s)
	assert.Contains(t, s, `"msg":"real"`)
	assert.NotContains(t, s, "override")
}

// The console encoder reuses the JSON encoder for structured context;
// configuring a policy must not corrupt its output.
func TestDuplicateKeyConsoleEncoderUnaffected(t *testing.T) {
	cfg := dedupConfig(zapcore.KeepFirst, nil)
	cfg.ConsoleSeparator = " | "
	var sink strings.Builder
	core := zapcore.NewCore(zapcore.NewConsoleEncoder(cfg), zapcore.AddSync(&sink), zapcore.InfoLevel).
		With([]zapcore.Field{zap.String("k", "a")})
	require.NoError(t, core.Write(zapcore.Entry{Level: zapcore.InfoLevel, Message: "m"},
		[]zapcore.Field{zap.String("k", "b")}))
	out := sink.String()
	assert.Contains(t, out, "m")
	// Console keeps the historical duplicated-context behavior; the policy
	// applies to the JSON encoder only.
	assert.Contains(t, out, `"k": "a"`)
}
