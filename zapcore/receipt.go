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

// WriteReceipt reports the outcome of writing a single CheckedEntry to a
// single Core.
type WriteReceipt struct {
	// Core is the Core that accepted the entry during Check.
	Core Core

	// Err is the error returned by that Core's Write method, if any.
	//
	// A nil Err means only that the Core accepted the entry without error.
	// For Cores that hand entries off to user-defined asynchronous sinks
	// (in-memory buffers, queues, background flushers), a nil Err means the
	// entry was queued for delivery — it does NOT mean the entry was
	// durably persisted.
	Err error
}

// WriteWithReceipt is an opt-in variant of Write that reports the outcome of
// the write for each Core individually. Like Write, it attempts every Core
// that accepted the entry during Check — a failure in one Core does not
// prevent the entry from being written to the others — and it returns one
// WriteReceipt per attempted Core, in the order the Cores were added.
//
// It's safe to call WriteWithReceipt on a nil CheckedEntry, in which case it
// returns nil.
//
// WriteWithReceipt returns the CheckedEntry to a pool for immediate re-use
// and executes any registered CheckWriteHook, exactly like Write. Receipts
// are collected before the hook runs, so a hook that terminates execution
// (WriteThenPanic, WriteThenFatal, WriteThenGoexit) prevents WriteWithReceipt
// from returning: for Panic and Fatal log statements the receipts are
// observable only if a non-terminating hook is installed (for example, with
// zap.WithPanicHook or zap.WithFatalHook in tests).
//
// Unlike Write, WriteWithReceipt allocates a slice to hold the receipts.
// Callers that don't need per-Core results should keep using Write, which
// retains its original cost and behavior.
func (ce *CheckedEntry) WriteWithReceipt(fields ...Field) []WriteReceipt {
	if ce == nil {
		return nil
	}
	receipts := make([]WriteReceipt, 0, len(ce.cores))
	ce.write(fields, &receipts)
	return receipts
}
