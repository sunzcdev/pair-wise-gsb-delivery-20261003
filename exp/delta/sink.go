// Copyright 2026 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package delta

import (
	"context"
	"log"
	"time"
)

// Sink consumes interval reports, for example by forwarding increments to
// an internal statistics service. Push is called sequentially from
// [Reader.Run], so implementations do not need their own locking unless
// they share state with other goroutines.
type Sink interface {
	Push(ctx context.Context, report *Report) error
}

// SinkFunc adapts a function to the Sink interface.
type SinkFunc func(ctx context.Context, report *Report) error

// Push implements Sink.
func (f SinkFunc) Push(ctx context.Context, report *Report) error {
	return f(ctx, report)
}

// ErrorHandler reacts to Gather or Sink errors encountered by Run. If nil,
// errors are discarded.
type ErrorHandler func(err error)

// LoggingErrorHandler returns an ErrorHandler that logs every error with
// the standard library logger.
func LoggingErrorHandler() ErrorHandler {
	return func(err error) {
		log.Printf("delta reader error: %v", err)
	}
}

// RunOption configures Reader.Run.
type RunOption func(*runConfig)

type runConfig struct {
	onError ErrorHandler
}

// WithErrorHandler installs a callback for Gather and Sink errors.
func WithErrorHandler(h ErrorHandler) RunOption {
	return func(c *runConfig) {
		c.onError = h
	}
}

// Run gathers and pushes interval reports on every tick from ticker until
// ctx is canceled. It performs an immediate Read to establish the baseline
// before waiting for the first tick, so the report delivered after the
// first tick covers exactly one interval.
//
// A failed Gather does not advance the baseline and is forwarded to the
// error handler instead of to the sink. A failed Push does not alter the
// baseline either: the increments of the failed interval have already been
// consumed from it and are not retried, which matches at-most-once
// delivery semantics.
func (r *Reader) Run(ctx context.Context, interval time.Duration, sink Sink, opts ...RunOption) error {
	cfg := runConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}

	handleError := func(err error) {
		if cfg.onError != nil {
			cfg.onError(err)
		}
	}

	if _, err := r.Read(); err != nil {
		handleError(err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			report, err := r.Read()
			if err != nil {
				handleError(err)
				continue
			}
			if err := sink.Push(ctx, report); err != nil {
				handleError(err)
			}
		}
	}
}
