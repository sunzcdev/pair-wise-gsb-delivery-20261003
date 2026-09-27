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
	"fmt"

	"go.uber.org/multierr"
)

// WriteReceipt describes the outcome of writing a single Entry to a single
// Core.
type WriteReceipt struct {
	// Core is the Core that produced this receipt.
	Core Core

	// Err is the error returned by the Core's Write method, if any.
	//
	// A nil Err means only that the Core accepted the Entry synchronously.
	// In particular, Cores backed by user-defined asynchronous or buffered
	// sinks may return nil as soon as the Entry is queued, before it
	// reaches its destination: a nil Err is NOT a guarantee that the Entry
	// has been persisted. Call Sync (or the sink's own flush mechanism)
	// and observe its error to confirm durability.
	Err error
}

// WriteWithReceipts writes the entry to all the stored Cores and returns a
// WriteReceipt for each of them, in the order the Cores were added to the
// CheckedEntry. Like Write, it attempts every Core even if some of them
// fail, reports any write errors to the CheckedEntry's ErrorOutput, and
// returns the CheckedEntry to a pool for re-use. It's safe to call on nil
// CheckedEntry references, in which case it returns nil.
//
// This is an opt-in alternative to Write for callers (for example, audit
// logging) that need to know which destinations accepted an entry:
//
//	if ce := logger.Check(zapcore.InfoLevel, "audit"); ce != nil {
//		for _, r := range ce.WriteWithReceipts(fields...) {
//			// inspect r.Core and r.Err
//		}
//	}
//
// The plain Write method is unchanged and remains the cheapest option;
// WriteWithReceipts additionally allocates the receipts slice.
//
// Terminal behavior: any CheckWriteHook registered with After runs after
// every Core has been attempted and all receipts have been collected, just
// like Write. zap.Logger registers panicking and exiting hooks for Panic
// and Fatal entries, so with those defaults WriteWithReceipts still writes
// to every Core but never returns — the process panics or exits first, and
// the caller never observes the receipts. To observe receipts for terminal
// entries, override the logger's hooks with zap.WithPanicHook or
// zap.WithFatalHook and a non-terminating CheckWriteHook.
func (ce *CheckedEntry) WriteWithReceipts(fields ...Field) []WriteReceipt {
	if ce == nil {
		return nil
	}

	if ce.dirty {
		if ce.ErrorOutput != nil {
			// Make a best effort to detect unsafe re-use of this
			// CheckedEntry, exactly as Write does.
			_, _ = fmt.Fprintf(
				ce.ErrorOutput,
				"%v Unsafe CheckedEntry re-use near Entry %+v.\n",
				ce.Time,
				ce.Entry,
			)
			_ = ce.ErrorOutput.Sync() // ignore error
		}
		return nil
	}
	ce.dirty = true

	ent := ce.Entry
	for i := range ce.before {
		ent, fields = ce.before[i](ent, fields)
	}

	receipts := make([]WriteReceipt, len(ce.cores))
	var err error
	for i := range ce.cores {
		receipts[i].Core = ce.cores[i]
		receipts[i].Err = ce.cores[i].Write(ent, fields)
		err = multierr.Append(err, receipts[i].Err)
	}
	if err != nil && ce.ErrorOutput != nil {
		_, _ = fmt.Fprintf(
			ce.ErrorOutput,
			"%v write error: %v\n",
			ce.Time,
			err,
		)
		_ = ce.ErrorOutput.Sync() // ignore error
	}

	hook := ce.after
	if hook != nil {
		hook.OnWrite(ce, fields)
	}
	putCheckedEntry(ce)
	return receipts
}
