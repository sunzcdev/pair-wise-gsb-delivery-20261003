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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.uber.org/zap"
	. "go.uber.org/zap/zapcore"
)

func duplicateEncoderConfig() EncoderConfig {
	return EncoderConfig{
		MessageKey:     "msg",
		LineEnding:     "\n",
		EncodeLevel:    LowercaseLevelEncoder,
		EncodeTime:     EpochTimeEncoder,
		EncodeCaller:   ShortCallerEncoder,
		EncodeDuration: SecondsDurationEncoder,
	}
}

func encodeDuplicateLog(t *testing.T, policy DuplicateKeyPolicy, context []zap.Field, fields []zap.Field, conflicts *[]DuplicateKeyInfo) string {
	t.Helper()

	cfg := duplicateEncoderConfig()
	cfg.DuplicateKeyPolicy = policy
	if conflicts != nil {
		cfg.OnDuplicateKey = func(info DuplicateKeyInfo) {
			*conflicts = append(*conflicts, info)
		}
	}

	var output bytes.Buffer
	core := NewCore(NewJSONEncoder(cfg), AddSync(&output), DebugLevel)
	logger := zap.New(core).With(context...)
	logger.Info("message", fields...)
	return output.String()
}

func TestJSONDuplicateKeyPolicy(t *testing.T) {
	t.Run("keep first keeps field order and skips the second value", func(t *testing.T) {
		output := encodeDuplicateLog(t, DuplicateKeyKeepFirst,
			[]zap.Field{zap.String("request_id", "from-with")},
			[]zap.Field{zap.String("other", "v"), zap.String("request_id", "from-call")},
			nil,
		)
		assert.Equal(t, `{"msg":"message","request_id":"from-with","other":"v"}`+"\n", output)
	})

	t.Run("keep last moves the replacement to the later position", func(t *testing.T) {
		output := encodeDuplicateLog(t, DuplicateKeyKeepLast,
			[]zap.Field{zap.String("request_id", "from-with"), zap.String("first", "v")},
			[]zap.Field{zap.String("middle", "v"), zap.String("request_id", "from-call")},
			nil,
		)
		assert.Equal(t, `{"msg":"message","first":"v","middle":"v","request_id":"from-call"}`+"\n", output)
	})

	t.Run("keep last removes the final field without leaving a comma", func(t *testing.T) {
		output := encodeDuplicateLog(t, DuplicateKeyKeepLast,
			[]zap.Field{zap.String("first", "v"), zap.String("request_id", "from-with")},
			[]zap.Field{zap.String("request_id", "from-call")},
			nil,
		)
		assert.Equal(t, `{"msg":"message","first":"v","request_id":"from-call"}`+"\n", output)
	})

	t.Run("keep last uses an entry field when it duplicates context metadata", func(t *testing.T) {
		output := encodeDuplicateLog(t, DuplicateKeyKeepLast,
			[]zap.Field{zap.String("msg", "from-with")},
			nil,
			nil,
		)
		assert.Equal(t, `{"msg":"message"}`+"\n", output)
	})

	t.Run("report preserves order and records the object path", func(t *testing.T) {
		var conflicts []DuplicateKeyInfo
		output := encodeDuplicateLog(t, DuplicateKeyReport,
			[]zap.Field{zap.String("request_id", "from-with")},
			[]zap.Field{zap.String("request_id", "from-call")},
			&conflicts,
		)
		assert.Equal(t, `{"msg":"message","request_id":"from-with","request_id":"from-call"}`+"\n", output)
		assert.Equal(t, []DuplicateKeyInfo{{Path: "$", Key: "request_id"}}, conflicts)
	})
}

func TestJSONDuplicateKeyPolicyNamespacesAndNestedObjects(t *testing.T) {
	nested := ObjectMarshalerFunc(func(enc ObjectEncoder) error {
		enc.AddString("request_id", "nested-first")
		enc.AddString("request_id", "nested-last")
		return nil
	})

	var conflicts []DuplicateKeyInfo
	output := encodeDuplicateLog(t, DuplicateKeyKeepFirst,
		[]zap.Field{zap.Object("request", ObjectMarshalerFunc(func(enc ObjectEncoder) error {
			enc.AddString("request_id", "namespaced")
			return enc.AddObject("metadata", nested)
		}))},
		[]zap.Field{zap.Object("response", ObjectMarshalerFunc(func(enc ObjectEncoder) error {
			return enc.AddObject("metadata", nested)
		}))},
		&conflicts,
	)

	assert.JSONEq(t, `{
		"msg": "message",
		"request": {
			"request_id": "namespaced",
			"metadata": {"request_id": "nested-first"}
		},
		"response": {"metadata": {"request_id": "nested-first"}}
	}`, output)
	assert.Equal(t, []DuplicateKeyInfo{
		{Path: "$.request.metadata", Key: "request_id"},
		{Path: "$.response.metadata", Key: "request_id"},
	}, conflicts)

	var openNamespaceConflicts []DuplicateKeyInfo
	openNamespaceOutput := encodeDuplicateLog(t, DuplicateKeyReport,
		[]zap.Field{zap.String("request_id", "root"), zap.Namespace("request"), zap.String("request_id", "namespaced")},
		[]zap.Field{zap.String("other", "call-field")},
		&openNamespaceConflicts,
	)
	assert.Equal(t, `{"msg":"message","request_id":"root","request":{"request_id":"namespaced","other":"call-field"}}`+"\n", openNamespaceOutput)
	assert.Empty(t, openNamespaceConflicts)
}

func TestJSONDuplicateKeyKeepLastNestedObject(t *testing.T) {
	nested := ObjectMarshalerFunc(func(enc ObjectEncoder) error {
		enc.AddString("first", "v")
		enc.AddString("request_id", "nested-first")
		enc.AddString("request_id", "nested-last")
		return nil
	})

	cfg := duplicateEncoderConfig()
	cfg.DuplicateKeyPolicy = DuplicateKeyKeepLast
	var output bytes.Buffer
	core := NewCore(NewJSONEncoder(cfg), AddSync(&output), DebugLevel)
	zap.New(core).Info("message", zap.Object("context", nested))

	assert.Equal(t, `{"msg":"message","context":{"first":"v","request_id":"nested-last"}}`+"\n", output.String())
}

func TestJSONDuplicateKeyPolicyArrayElementsAreSeparatePaths(t *testing.T) {
	var conflicts []DuplicateKeyInfo
	cfg := duplicateEncoderConfig()
	cfg.DuplicateKeyPolicy = DuplicateKeyReport
	cfg.OnDuplicateKey = func(info DuplicateKeyInfo) {
		conflicts = append(conflicts, info)
	}

	var output bytes.Buffer
	core := NewCore(NewJSONEncoder(cfg), AddSync(&output), DebugLevel)
	zap.New(core).Info("message", zap.Array("requests", ArrayMarshalerFunc(func(arr ArrayEncoder) error {
		for _, value := range []string{"first", "second"} {
			if err := arr.AppendObject(ObjectMarshalerFunc(func(enc ObjectEncoder) error {
				enc.AddString("request_id", value)
				enc.AddString("request_id", value+"-duplicate")
				return nil
			})); err != nil {
				return err
			}
		}
		return nil
	})))

	assert.Equal(t, `{"msg":"message","requests":[{"request_id":"first","request_id":"first-duplicate"},{"request_id":"second","request_id":"second-duplicate"}]}`+"\n", output.String())
	assert.Equal(t, []DuplicateKeyInfo{
		{Path: "$.requests[0]", Key: "request_id"},
		{Path: "$.requests[1]", Key: "request_id"},
	}, conflicts)
}

func TestJSONDuplicateKeyPolicyInlineFields(t *testing.T) {
	cfg := duplicateEncoderConfig()
	cfg.DuplicateKeyPolicy = DuplicateKeyKeepLast

	var output bytes.Buffer
	core := NewCore(NewJSONEncoder(cfg), AddSync(&output), DebugLevel)
	zap.New(core).Info("message",
		zap.Inline(ObjectMarshalerFunc(func(enc ObjectEncoder) error {
			enc.AddString("request_id", "inline-first")
			return nil
		})),
		zap.String("other", "v"),
		zap.Inline(ObjectMarshalerFunc(func(enc ObjectEncoder) error {
			enc.AddString("request_id", "inline-last")
			return nil
		})),
	)

	assert.Equal(t, `{"msg":"message","other":"v","request_id":"inline-last"}`+"\n", output.String())
}

type countingObject struct {
	value string
	count *int
}

func (o countingObject) MarshalLogObject(enc ObjectEncoder) error {
	*o.count++
	enc.AddString("request_id", o.value)
	return nil
}

type lazyStringer struct {
	value string
	count *int
}

func (s lazyStringer) String() string {
	*s.count++
	return s.value
}

func TestJSONDuplicateKeyPolicyWithLazyCustomObject(t *testing.T) {
	cfg := duplicateEncoderConfig()
	cfg.DuplicateKeyPolicy = DuplicateKeyKeepFirst

	var output bytes.Buffer
	core := NewCore(NewJSONEncoder(cfg), AddSync(&output), DebugLevel)

	count := 0
	logger := zap.New(core).WithLazy(zap.Object("context", countingObject{
		value: "lazy-at-log-time",
		count: &count,
	}))
	require.Equal(t, 0, count)

	logger.Info("message", zap.Object("context", countingObject{
		value: "should-not-marshal",
		count: &count,
	}))

	assert.Equal(t, 1, count)
	assert.Equal(t, `{"msg":"message","context":{"request_id":"lazy-at-log-time"}}`+"\n", output.String())
}

func TestJSONDuplicateKeyPolicyWithLazyStringerValue(t *testing.T) {
	cfg := duplicateEncoderConfig()
	cfg.DuplicateKeyPolicy = DuplicateKeyKeepFirst

	var output bytes.Buffer
	core := NewCore(NewJSONEncoder(cfg), AddSync(&output), DebugLevel)
	count := 0
	logger := zap.New(core).WithLazy(zap.Stringer("request_id", lazyStringer{
		value: "lazy-at-log-time",
		count: &count,
	}))
	require.Equal(t, 0, count)

	logger.Info("message", zap.Stringer("request_id", lazyStringer{
		value: "from-call",
		count: &count,
	}))

	assert.Equal(t, 2, count)
	assert.Equal(t, `{"msg":"message","request_id":"lazy-at-log-time"}`+"\n", output.String())
}
