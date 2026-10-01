// Go MySQL Driver - A MySQL-Driver for Go's database/sql package
//
// Copyright 2026 The Go-MySQL-Driver Authors. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

package mysql

import (
	"net"
	"sync"
	"time"
)

// pipeConn is one endpoint of an in-memory, fully buffered byte pipe. Writes
// to one endpoint are immediately available to reads on the other, so a
// server goroutine can queue a whole multi-packet response without waiting.
type pipeConn struct {
	peer *pipeConn

	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	closed bool
}

func netPipe() (*pipeConn, *pipeConn) {
	a := &pipeConn{}
	b := &pipeConn{}
	a.peer = b
	b.peer = a
	a.cond = sync.NewCond(&a.mu)
	b.cond = sync.NewCond(&b.mu)
	return a, b
}

func (c *pipeConn) Read(b []byte) (int, error) {
	c.mu.Lock()
	for len(c.buf) == 0 && !c.closed {
		c.cond.Wait()
	}
	if len(c.buf) > 0 {
		n := copy(b, c.buf)
		c.buf = c.buf[n:]
		c.mu.Unlock()
		return n, nil
	}
	c.mu.Unlock()
	return 0, net.ErrClosed
}

func (c *pipeConn) Write(b []byte) (int, error) {
	p := c.peer
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return 0, net.ErrClosed
	}
	p.buf = append(p.buf, b...)
	n := len(b)
	p.cond.Broadcast()
	p.mu.Unlock()
	return n, nil
}

func (c *pipeConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.cond.Broadcast()
	c.mu.Unlock()

	p := c.peer
	p.mu.Lock()
	p.closed = true
	p.cond.Broadcast()
	p.mu.Unlock()
	return nil
}

func (c *pipeConn) LocalAddr() net.Addr              { return dummyAddr{} }
func (c *pipeConn) RemoteAddr() net.Addr             { return dummyAddr{} }
func (c *pipeConn) SetDeadline(time.Time) error      { return nil }
func (c *pipeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *pipeConn) SetWriteDeadline(time.Time) error { return nil }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "pipe" }
func (dummyAddr) String() string  { return "pipe" }
