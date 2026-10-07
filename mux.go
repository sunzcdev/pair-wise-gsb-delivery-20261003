// MIT License
//
// Copyright (c) 2016-2017 xtaci
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package smux

import (
	"errors"
	"fmt"
	"io"
	"math"
	"time"
)

// Config is used to tune the Smux session
type Config struct {
	// SMUX Protocol version, support 1,2
	Version int

	// Disabled keepalive
	KeepAliveDisabled bool

	// KeepAliveInterval is how often to send a NOP command to the remote
	KeepAliveInterval time.Duration

	// KeepAliveTimeout is how long the session
	// will be closed if no data has arrived
	KeepAliveTimeout time.Duration

	// MaxFrameSize is used to control the maximum
	// frame size to sent to the remote
	MaxFrameSize int

	// MaxReceiveBuffer is used to control the maximum
	// number of data in the buffer pool
	MaxReceiveBuffer int

	// MaxStreamBuffer is used to control the maximum
	// number of data per stream
	MaxStreamBuffer int

	// MaxPendingBytes is the session-level limit of payload bytes that have
	// been queued for sending but not yet written to the underlying
	// connection. When the limit is reached, writes behave according to
	// PendingPolicy. 0 means unlimited.
	MaxPendingBytes int

	// MaxStreamPendingBytes is the per-stream limit of payload bytes queued
	// for sending but not yet written to the underlying connection, it must
	// not be larger than MaxPendingBytes. 0 means unlimited.
	MaxStreamPendingBytes int

	// PendingPolicy determines how Write behaves when MaxPendingBytes or
	// MaxStreamPendingBytes has been reached.
	PendingPolicy PendingPolicy
}

// PendingPolicy determines how Write behaves when the pending
// (queued but not yet sent) limits are reached.
type PendingPolicy int

const (
	// PendingWait blocks the writer until pending space is freed, the write
	// deadline expires or the stream/session is closed.
	PendingWait PendingPolicy = iota

	// PendingFailFast makes Write return ErrBackpressure immediately when
	// the pending limits are reached.
	PendingFailFast
)

// DefaultConfig is used to return a default configuration
func DefaultConfig() *Config {
	return &Config{
		Version:           1,
		KeepAliveInterval: 10 * time.Second,
		KeepAliveTimeout:  30 * time.Second,
		MaxFrameSize:      32768,
		MaxReceiveBuffer:  4194304,
		MaxStreamBuffer:   65536,
		MaxPendingBytes:   4194304,
	}
}

// VerifyConfig is used to verify the sanity of configuration
func VerifyConfig(config *Config) error {
	if !(config.Version == 1 || config.Version == 2) {
		return errors.New("unsupported protocol version")
	}
	if !config.KeepAliveDisabled {
		if config.KeepAliveInterval == 0 {
			return errors.New("keep-alive interval must be positive")
		}
		if config.KeepAliveTimeout < config.KeepAliveInterval {
			return fmt.Errorf("keep-alive timeout must be larger than keep-alive interval")
		}
	}
	if config.MaxFrameSize <= 0 {
		return errors.New("max frame size must be positive")
	}
	if config.MaxFrameSize > 65535 {
		return errors.New("max frame size must not be larger than 65535")
	}
	if config.MaxReceiveBuffer <= 0 {
		return errors.New("max receive buffer must be positive")
	}
	if config.MaxReceiveBuffer > math.MaxInt32 {
		return errors.New("max receive buffer cannot be larger than 2147483647")
	}
	if config.MaxStreamBuffer <= 0 {
		return errors.New("max stream buffer must be positive")
	}
	if config.MaxStreamBuffer > config.MaxReceiveBuffer {
		return errors.New("max stream buffer must not be larger than max receive buffer")
	}
	if config.MaxStreamBuffer > math.MaxInt32 {
		return errors.New("max stream buffer cannot be larger than 2147483647")
	}
	if config.MaxPendingBytes < 0 {
		return errors.New("max pending bytes must not be negative")
	}
	if config.MaxPendingBytes > math.MaxInt32 {
		return errors.New("max pending bytes cannot be larger than 2147483647")
	}
	if config.MaxStreamPendingBytes < 0 {
		return errors.New("max stream pending bytes must not be negative")
	}
	if config.MaxStreamPendingBytes > math.MaxInt32 {
		return errors.New("max stream pending bytes cannot be larger than 2147483647")
	}
	if config.MaxPendingBytes > 0 && config.MaxStreamPendingBytes > config.MaxPendingBytes {
		return errors.New("max stream pending bytes must not be larger than max pending bytes")
	}
	if config.PendingPolicy != PendingWait && config.PendingPolicy != PendingFailFast {
		return errors.New("invalid pending policy")
	}
	return nil
}

// Server is used to initialize a new server-side connection.
func Server(conn io.ReadWriteCloser, config *Config) (*Session, error) {
	if config == nil {
		config = DefaultConfig()
	}
	if err := VerifyConfig(config); err != nil {
		return nil, err
	}
	return newSession(config, conn, false), nil
}

// Client is used to initialize a new client-side connection.
func Client(conn io.ReadWriteCloser, config *Config) (*Session, error) {
	if config == nil {
		config = DefaultConfig()
	}

	if err := VerifyConfig(config); err != nil {
		return nil, err
	}
	return newSession(config, conn, true), nil
}
