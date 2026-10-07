// Copyright 2026 Matthew Holt
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package acme

import (
	"context"
	"sync"
)

// accountGate is a context-aware, writer-preferring reader/writer gate.
// It defines the boundary a coordinated key rollover crosses:
//
//   - Read leases represent requests signing with the current account
//     key. They may overlap freely.
//   - A writer (the keyChange) waits for every active reader to release,
//     so requests that started with the old key drain before the CA
//     switches it.
//   - Once a writer is queued, new readers block, so the boundary
//     between old-key and new-key requests is sharp rather than
//     starved indefinitely by issuance traffic.
//
// Unlike sync.RWMutex, waits honor context cancellation.
type accountGate struct {
	mu      sync.Mutex
	cond    *sync.Cond
	readers int

	// writer is true while the keyChange (and its reconciliation) is
	// in progress and the gate is closed to readers.
	writer bool

	// waitingWriters is the number of rollovers queued to cross the
	// boundary; any non-zero value parks new readers.
	waitingWriters int
}

func (g *accountGate) init() {
	g.cond = sync.NewCond(&g.mu)
}

// beginRead acquires a read lease. It blocks while a rollover is
// queued or in progress, and returns early with ctx's error if the
// context ends while waiting.
func (g *accountGate) beginRead(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	for g.writer || g.waitingWriters > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		stop := context.AfterFunc(ctx, g.cond.Broadcast)
		g.cond.Wait()
		stop()
	}
	g.readers++
	return nil
}

// endRead releases a read lease. When the last lease drains, waiting
// writers are woken.
func (g *accountGate) endRead() {
	g.mu.Lock()
	g.readers--
	if g.readers < 0 {
		panic("acme: account gate read lease released without being held")
	}
	if g.readers == 0 {
		g.cond.Broadcast()
	}
	g.mu.Unlock()
}

// beginWrite queues a writer and blocks until all read leases have
// drained. The returned function releases the writer and lets readers
// (and queued writers) proceed. A cancelled context abandons the queue
// position instead of deadlocking the rollover.
func (g *accountGate) beginWrite(ctx context.Context) (func(), error) {
	g.mu.Lock()

	g.waitingWriters++
	for g.readers > 0 || g.writer {
		if err := ctx.Err(); err != nil {
			g.abandonWriter()
			return nil, err
		}
		stop := context.AfterFunc(ctx, g.cond.Broadcast)
		g.cond.Wait()
		stop()
		if err := ctx.Err(); err != nil {
			g.abandonWriter()
			return nil, err
		}
	}
	g.waitingWriters--
	g.writer = true
	g.mu.Unlock()

	return g.endWrite, nil
}

// abandonWriter must be called with g.mu held when a queued writer
// gives up due to context cancellation.
func (g *accountGate) abandonWriter() {
	g.waitingWriters--
	if g.waitingWriters == 0 {
		// readers were parked behind us; let them run again
		g.cond.Broadcast()
	}
	g.mu.Unlock()
}

func (g *accountGate) endWrite() {
	g.mu.Lock()
	g.writer = false
	g.cond.Broadcast()
	g.mu.Unlock()
}

// writerQueued reports whether a rollover is waiting to cross or is
// currently crossing the boundary. Test helper.
func (g *accountGate) writerQueued() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.writer || g.waitingWriters > 0
}
