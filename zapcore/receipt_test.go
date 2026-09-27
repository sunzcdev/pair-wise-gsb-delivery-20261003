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
	"testing"
	"time"

	//revive:disable:dot-imports
	. "go.uber.org/zap/zapcore"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingSink is a WriteSyncer that records how many entries it accepted.
type countingSink struct {
	writes *int
}

func (s countingSink) Write(p []byte) (int, error) {
	*s.writes++
	return len(p), nil
}

func (countingSink) Sync() error { return nil }

// shortWriteSink is a WriteSyncer that always fails with io.ErrShortWrite.
type shortWriteSink struct {
	writes *int
}

func (s shortWriteSink) Write(p []byte) (int, error) {
	*s.writes++
	return 0, io.ErrShortWrite
}

func (shortWriteSink) Sync() error { return nil }

// ctxSink is a WriteSyncer that fails once its context is canceled,
// standing in for a remote sink whose connection was torn down.
type ctxSink struct {
	ctx    context.Context
	writes *int
}

func (s ctxSink) Write(p []byte) (int, error) {
	*s.writes++
	if err := s.ctx.Err(); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (ctxSink) Sync() error { return nil }

func TestCheckedEntryWriteWithReceipt(t *testing.T) {
	var okWrites, shortWrites, cancelledWrites int

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the remote destination is already gone

	enc := NewJSONEncoder(testEncoderConfig())
	okCore := NewCore(enc, countingSink{writes: &okWrites}, DebugLevel)
	shortCore := NewCore(enc, shortWriteSink{writes: &shortWrites}, DebugLevel)
	cancelledCore := NewCore(enc, ctxSink{ctx: ctx, writes: &cancelledWrites}, DebugLevel)

	tee := NewTee(okCore, shortCore, cancelledCore)

	ent := Entry{Level: InfoLevel, Message: "audit", Time: time.Now()}
	ce := tee.Check(ent, nil)
	require.NotNil(t, ce, "expected entry to be accepted")

	receipts := ce.WriteWithReceipt(makeInt64Field("k", 1))
	require.Len(t, receipts, 3, "expected one receipt per core")

	// Every destination was attempted, even though one of them failed.
	assert.Equal(t, 1, okWrites, "expected write to healthy sink")
	assert.Equal(t, 1, shortWrites, "expected write to short-writing sink")
	assert.Equal(t, 1, cancelledWrites, "expected write to cancelled sink")

	// Each receipt is attributed to the Core that produced it, in the order
	// the cores were added to the tee.
	assert.True(t, receipts[0].Core == okCore, "receipt 0 attributed to the wrong core")
	assert.NoError(t, receipts[0].Err)

	assert.True(t, receipts[1].Core == shortCore, "receipt 1 attributed to the wrong core")
	assert.ErrorIs(t, receipts[1].Err, io.ErrShortWrite)

	assert.True(t, receipts[2].Core == cancelledCore, "receipt 2 attributed to the wrong core")
	assert.ErrorIs(t, receipts[2].Err, context.Canceled)
}

func TestCheckedEntryWriteWithReceiptNil(t *testing.T) {
	var ce *CheckedEntry
	assert.Nil(t, ce.WriteWithReceipt(), "expected nil receipts from a nil CheckedEntry")
}

type hookFunc func(*CheckedEntry, []Field)

func (f hookFunc) OnWrite(ce *CheckedEntry, fields []Field) { f(ce, fields) }

func TestCheckedEntryWriteWithReceiptHook(t *testing.T) {
	var writes int
	core := NewCore(
		NewJSONEncoder(testEncoderConfig()),
		countingSink{writes: &writes},
		DebugLevel,
	)

	ent := Entry{Level: InfoLevel, Message: "audit", Time: time.Now()}

	// A non-terminating hook observes the write after all cores were
	// written, and WriteWithReceipt still returns the receipts.
	var hookSawWrites int
	ce := core.Check(ent, nil).After(ent, hookFunc(func(*CheckedEntry, []Field) {
		hookSawWrites = writes
	}))

	receipts := ce.WriteWithReceipt()
	require.Len(t, receipts, 1)
	assert.NoError(t, receipts[0].Err)
	assert.Equal(t, 1, hookSawWrites, "expected hook to run after the core was written")
}

func TestCheckedEntryWriteWithReceiptTerminalHook(t *testing.T) {
	var writes int
	core := NewCore(
		NewJSONEncoder(testEncoderConfig()),
		countingSink{writes: &writes},
		DebugLevel,
	)

	// With a terminating hook (as installed for Panic and Fatal entries),
	// the entry is still written to every core, but WriteWithReceipt does
	// not return: the hook runs after the receipts are collected.
	ent := Entry{Level: PanicLevel, Message: "boom", Time: time.Now()}
	ce := core.Check(ent, nil).After(ent, WriteThenPanic)

	assert.Panics(t, func() {
		ce.WriteWithReceipt()
	})
	assert.Equal(t, 1, writes, "expected the core to be written before the hook ran")
}
