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
	"context"
	"io"
	"sync"
	"testing"

	//revive:disable:dot-imports
	. "go.uber.org/zap/zapcore"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubSink is a WriteSyncer that records every write and fails with a
// configurable error.
type stubSink struct {
	mu     sync.Mutex
	writes [][]byte
	n      int // bytes to report written on failure
	err    error
}

func (s *stubSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.writes = append(s.writes, append([]byte(nil), p...))
	s.mu.Unlock()
	if s.err != nil {
		return s.n, s.err
	}
	return len(p), nil
}

func (s *stubSink) Sync() error { return nil }

func (s *stubSink) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.writes)
}

func TestWriteWithReceiptsAttribution(t *testing.T) {
	var (
		okSink       = &stubSink{}
		shortSink    = &stubSink{n: 1, err: io.ErrShortWrite}
		canceledSink = &stubSink{err: context.Canceled}
	)

	cfg := testEncoderConfig()
	cfg.TimeKey = ""
	newCore := func(ws WriteSyncer) Core {
		return NewCore(NewJSONEncoder(cfg), ws, DebugLevel)
	}
	var (
		okCore       = newCore(okSink)
		shortCore    = newCore(shortSink)
		canceledCore = newCore(canceledSink)
	)
	core := NewTee(okCore, shortCore, canceledCore)

	ent := Entry{Level: InfoLevel, Message: "audit"}
	ce := core.Check(ent, nil)
	require.NotNil(t, ce, "Expected entry to be checked.")

	receipts := ce.WriteWithReceipts(makeInt64Field("k", 42))
	require.Len(t, receipts, 3, "Expected one receipt per core.")

	// Receipts are attributed to their cores in order.
	assert.Same(t, okCore, receipts[0].Core)
	assert.NoError(t, receipts[0].Err, "Expected the healthy sink to succeed.")

	assert.Same(t, shortCore, receipts[1].Core)
	assert.ErrorIs(t, receipts[1].Err, io.ErrShortWrite,
		"Expected the short-writing sink's error on its own receipt.")

	assert.Same(t, canceledCore, receipts[2].Core)
	assert.ErrorIs(t, receipts[2].Err, context.Canceled,
		"Expected the canceled sink's error on its own receipt.")

	// Every destination is attempted, even after failures.
	assert.Equal(t, 1, okSink.writeCount(), "Expected the healthy sink to be written.")
	assert.Equal(t, 1, shortSink.writeCount(), "Expected the short-writing sink to be attempted.")
	assert.Equal(t, 1, canceledSink.writeCount(), "Expected the canceled sink to be attempted.")
}

func TestWriteWithReceiptsNil(t *testing.T) {
	var ce *CheckedEntry
	assert.Nil(t, ce.WriteWithReceipts(),
		"Expected nil receipts from a nil CheckedEntry.")
}

// queueSink accepts writes into an in-memory queue without persisting them,
// mimicking a user-defined asynchronous sink.
type queueSink struct {
	mu        sync.Mutex
	queued    int
	persisted int
}

func (s *queueSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.queued++
	s.mu.Unlock()
	return len(p), nil
}

func (s *queueSink) Sync() error { return nil }

func (s *queueSink) counts() (queued, persisted int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queued, s.persisted
}

func TestWriteWithReceiptsQueuedIsNotPersisted(t *testing.T) {
	sink := &queueSink{}
	core := NewCore(NewJSONEncoder(testEncoderConfig()), sink, DebugLevel)

	ce := core.Check(Entry{Level: InfoLevel, Message: "audit"}, nil)
	require.NotNil(t, ce, "Expected entry to be checked.")

	receipts := ce.WriteWithReceipts()
	require.Len(t, receipts, 1, "Expected one receipt.")
	assert.NoError(t, receipts[0].Err,
		"A nil receipt error means the core accepted the entry synchronously.")

	queued, persisted := sink.counts()
	assert.Equal(t, 1, queued, "Expected the entry to be queued.")
	assert.Equal(t, 0, persisted,
		"A clean receipt must not imply persistence: nothing was flushed yet.")
}

func TestWriteWithReceiptsTerminalHook(t *testing.T) {
	var (
		okSink    = &stubSink{}
		shortSink = &stubSink{n: 1, err: io.ErrShortWrite}
	)
	cfg := testEncoderConfig()
	cfg.TimeKey = ""
	core := NewTee(
		NewCore(NewJSONEncoder(cfg), okSink, DebugLevel),
		NewCore(NewJSONEncoder(cfg), shortSink, DebugLevel),
	)

	ent := Entry{Level: PanicLevel, Message: "boom"}
	ce := core.Check(ent, nil)
	require.NotNil(t, ce, "Expected entry to be checked.")
	ce = ce.After(ent, WriteThenPanic)

	returned := false
	func() {
		defer func() {
			assert.Equal(t, "boom", recover(),
				"Expected the terminal hook to panic after writing.")
		}()
		ce.WriteWithReceipts()
		returned = true
	}()

	assert.False(t, returned,
		"Expected WriteWithReceipts not to return once the terminal hook fires.")
	assert.Equal(t, 1, okSink.writeCount(),
		"Expected all destinations to be attempted before the hook fires.")
	assert.Equal(t, 1, shortSink.writeCount(),
		"Expected all destinations to be attempted before the hook fires.")
}
