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

package zapcore

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The spaced JSON layout (used by the console encoder for its structured
// context) must also produce valid output under the duplicate-key policies.
func TestDuplicateKeySpacedLayout(t *testing.T) {
	enc := newJSONEncoder(EncoderConfig{
		MessageKey:    "msg",
		DuplicateKeys: KeepFirst,
	}, true)
	enc.AddString("k", "a")
	enc.AddString("k", "b")
	out, err := enc.EncodeEntry(Entry{Message: "m"}, nil)
	require.NoError(t, err)
	s := strings.TrimSpace(out.String())
	out.Free()
	require.True(t, json.Valid([]byte(s)), "invalid JSON: %s", s)
	assert.Contains(t, s, `"k": "a"`)
	assert.NotContains(t, s, `"k": "b"`)
}

// Clone must preserve an independently-mutable copy of the dedup
// bookkeeping; siblings of a cloned encoder must not interfere.
func TestDuplicateKeyCloneIsolation(t *testing.T) {
	enc := newJSONEncoder(EncoderConfig{
		MessageKey:    "msg",
		DuplicateKeys: KeepFirst,
	}, false)
	enc.AddString("k", "one")

	clone := enc.Clone()
	clone.AddString("k", "two") // suppressed in clone only
	enc.AddString("k", "three") // suppressed in original only

	a, err := enc.EncodeEntry(Entry{Message: "m"}, nil)
	require.NoError(t, err)
	b, err := clone.EncodeEntry(Entry{Message: "m"}, nil)
	require.NoError(t, err)
	assert.Equal(t, `{"msg":"m","k":"one"}`+"\n", a.String())
	assert.Equal(t, `{"msg":"m","k":"one"}`+"\n", b.String())
	a.Free()
	b.Free()
}

// EncodeEntry must be repeatable and each output self-contained; resolving
// duplicates on one entry must not mutate the reusable base encoder.
func TestDuplicateKeyEncodeEntryRepeated(t *testing.T) {
	enc := newJSONEncoder(EncoderConfig{
		MessageKey:    "msg",
		DuplicateKeys: KeepLast,
	}, false)
	enc.AddString("k", "ctx")

	for i := 0; i < 3; i++ {
		out, err := enc.EncodeEntry(Entry{Message: "m"},
			[]Field{{Key: "k", Type: StringType, String: "call"}})
		require.NoError(t, err)
		assert.Equal(t, `{"msg":"m","k":"call"}`+"\n", out.String())
		require.True(t, json.Valid(bytes.TrimSpace(out.Bytes())))
		out.Free()
	}
}

// Duplicates among call-site fields only (no accumulated context) must be
// handled for both policies.
func TestDuplicateKeyEntryFieldsOnly(t *testing.T) {
	fields := []Field{
		{Key: "k", Type: StringType, String: "a"},
		{Key: "k", Type: StringType, String: "b"},
		{Key: "k", Type: StringType, String: "c"},
	}
	t.Run("keep first", func(t *testing.T) {
		enc := newJSONEncoder(EncoderConfig{MessageKey: "msg", DuplicateKeys: KeepFirst}, false)
		out, err := enc.EncodeEntry(Entry{Message: "m"}, fields)
		require.NoError(t, err)
		assert.Equal(t, `{"msg":"m","k":"a"}`+"\n", out.String())
		out.Free()
	})
	t.Run("keep last", func(t *testing.T) {
		enc := newJSONEncoder(EncoderConfig{MessageKey: "msg", DuplicateKeys: KeepLast}, false)
		out, err := enc.EncodeEntry(Entry{Message: "m"}, fields)
		require.NoError(t, err)
		assert.Equal(t, `{"msg":"m","k":"c"}`+"\n", out.String())
		out.Free()
	})
}

// KeepLast across the header boundary drops the header value and keeps the
// later one.
func TestKeepLastHeaderCollision(t *testing.T) {
	enc := newJSONEncoder(EncoderConfig{MessageKey: "msg", DuplicateKeys: KeepLast}, false)
	out, err := enc.EncodeEntry(Entry{Message: "real"},
		[]Field{{Key: "msg", Type: StringType, String: "override"}})
	require.NoError(t, err)
	assert.Equal(t, `{"msg":"override"}`+"\n", out.String())
	require.True(t, json.Valid(bytes.TrimSpace(out.Bytes())))
	out.Free()
}

// KeepLast in spaced mode must consume the ", " separator correctly.
func TestKeepLastSpacedLayout(t *testing.T) {
	enc := newJSONEncoder(EncoderConfig{MessageKey: "msg", DuplicateKeys: KeepLast}, true)
	enc.AddString("k", "a")
	out, err := enc.EncodeEntry(Entry{Message: "m"},
		[]Field{{Key: "k", Type: StringType, String: "b"}})
	require.NoError(t, err)
	s := strings.TrimSpace(out.String())
	out.Free()
	require.True(t, json.Valid([]byte(s)), "invalid JSON: %s", s)
	assert.Equal(t, `{"msg": "m", "k": "b"}`, s)
}
