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

	// MaxWriteBuffer is the maximum number of payload bytes that may be
	// queued session-wide before they are actually written to the underlying
	// connection. When the limit is reached, stream writes either wait for
	// space (WriteBlock is true) or return ErrSendBufferFull immediately.
	// Control frames (SYN/FIN/NOP/UPD) keep a separate, small quota and are
	// not counted against this limit.
	MaxWriteBuffer int

	// MaxStreamWriteBuffer is the per-stream limit on payload bytes queued
	// but not yet written to the underlying connection. It may be set smaller
	// than MaxWriteBuffer so that a single stream cannot fill the whole
	// session send budget.
	MaxStreamWriteBuffer int

	// WriteBlock controls the behavior when the session or per-stream send
	// buffer limit is reached. When true (the default), stream writes wait
	// until space becomes available (or the write deadline fires). When false,
	// stream writes fail immediately with ErrSendBufferFull. Control frames
	// always block until their own deadline regardless of this setting.
	WriteBlock bool
}

// DefaultConfig is used to return a default configuration
func DefaultConfig() *Config {
	return &Config{
		Version:              1,
		KeepAliveInterval:    10 * time.Second,
		KeepAliveTimeout:     30 * time.Second,
		MaxFrameSize:         32768,
		MaxReceiveBuffer:     4194304,
		MaxStreamBuffer:      65536,
		MaxWriteBuffer:       4194304,
		MaxStreamWriteBuffer: 65536,
		WriteBlock:           true,
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
	if config.MaxWriteBuffer <= 0 {
		return errors.New("max write buffer must be positive")
	}
	if config.MaxWriteBuffer < config.MaxFrameSize {
		return errors.New("max write buffer must not be smaller than max frame size")
	}
	if config.MaxWriteBuffer > math.MaxInt32 {
		return errors.New("max write buffer cannot be larger than 2147483647")
	}
	if config.MaxStreamWriteBuffer <= 0 {
		return errors.New("max stream write buffer must be positive")
	}
	if config.MaxStreamWriteBuffer < config.MaxFrameSize {
		return errors.New("max stream write buffer must not be smaller than max frame size")
	}
	if config.MaxStreamWriteBuffer > config.MaxWriteBuffer {
		return errors.New("max stream write buffer must not be larger than max write buffer")
	}
	if config.MaxStreamWriteBuffer > math.MaxInt32 {
		return errors.New("max stream write buffer cannot be larger than 2147483647")
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
