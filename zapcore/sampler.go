// Copyright (c) 2016-2022 Uber Technologies, Inc.
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
	"strings"
	"sync/atomic"
	"time"
)

const (
	_numLevels        = _maxLevel - _minLevel + 1
	_countersPerLevel = 4096
)

type counter struct {
	resetAt atomic.Int64
	counter atomic.Uint64
}

type counters [_numLevels][_countersPerLevel]counter

func newCounters() *counters {
	return &counters{}
}

func (cs *counters) get(lvl Level, key string) *counter {
	i := lvl - _minLevel
	j := fnv32a(key) % _countersPerLevel
	return &cs[i][j]
}

// fnv32a, adapted from "hash/fnv", but without a []byte(string) alloc
func fnv32a(s string) uint32 {
	const (
		offset32 = 2166136261
		prime32  = 16777619
	)
	hash := uint32(offset32)
	for i := 0; i < len(s); i++ {
		hash ^= uint32(s[i])
		hash *= prime32
	}
	return hash
}

func (c *counter) IncCheckReset(t time.Time, tick time.Duration) uint64 {
	tn := t.UnixNano()
	resetAfter := c.resetAt.Load()
	if resetAfter > tn {
		return c.counter.Add(1)
	}

	c.counter.Store(1)

	newResetAfter := tn + tick.Nanoseconds()
	if !c.resetAt.CompareAndSwap(resetAfter, newResetAfter) {
		// We raced with another goroutine trying to reset, and it also reset
		// the counter to 1, so we need to reincrement the counter.
		return c.counter.Add(1)
	}

	return 1
}

// SamplingDecision is a decision represented as a bit field made by sampler.
// More decisions may be added in the future.
type SamplingDecision uint32

const (
	// LogDropped indicates that the Sampler dropped a log entry.
	LogDropped SamplingDecision = 1 << iota
	// LogSampled indicates that the Sampler sampled a log entry.
	LogSampled
)

// samplerOptions holds the configuration shared by all samplers.
type samplerOptions struct {
	hook func(Entry, SamplingDecision)
}

// optionFunc wraps a func so it satisfies the SamplerOption interface.
type optionFunc func(*samplerOptions)

func (f optionFunc) apply(o *samplerOptions) {
	f(o)
}

// SamplerOption configures a Sampler.
type SamplerOption interface {
	apply(*samplerOptions)
}

// nopSamplingHook is the default hook used by sampler.
func nopSamplingHook(Entry, SamplingDecision) {}

// SamplerHook registers a function  which will be called when Sampler makes a
// decision.
//
// This hook may be used to get visibility into the performance of the sampler.
// For example, use it to track metrics of dropped versus sampled logs.
//
//	var dropped atomic.Int64
//	zapcore.SamplerHook(func(ent zapcore.Entry, dec zapcore.SamplingDecision) {
//	  if dec&zapcore.LogDropped > 0 {
//	    dropped.Inc()
//	  }
//	})
func SamplerHook(hook func(entry Entry, dec SamplingDecision)) SamplerOption {
	return optionFunc(func(o *samplerOptions) {
		o.hook = hook
	})
}

func newSamplerOptions(opts []SamplerOption) samplerOptions {
	o := samplerOptions{hook: nopSamplingHook}
	for _, opt := range opts {
		opt.apply(&o)
	}
	return o
}

// NewSamplerWithOptions creates a Core that samples incoming entries, which
// caps the CPU and I/O load of logging while attempting to preserve a
// representative subset of your logs.
//
// Zap samples by logging the first N entries with a given level and message
// each tick. If more Entries with the same level and message are seen during
// the same interval, every Mth message is logged and the rest are dropped.
//
// For example,
//
//	core = NewSamplerWithOptions(core, time.Second, 10, 5)
//
// This will log the first 10 log entries with the same level and message
// in a one second interval as-is. Following that, it will allow through
// every 5th log entry with the same level and message in that interval.
//
// If thereafter is zero, the Core will drop all log entries after the first N
// in that interval.
//
// Sampler can be configured to report sampling decisions with the SamplerHook
// option.
//
// Keep in mind that Zap's sampling implementation is optimized for speed over
// absolute precision; under load, each tick may be slightly over- or
// under-sampled.
func NewSamplerWithOptions(core Core, tick time.Duration, first, thereafter int, opts ...SamplerOption) Core {
	o := newSamplerOptions(opts)
	s := &sampler{
		Core:       core,
		tick:       tick,
		counts:     newCounters(),
		first:      uint64(first),
		thereafter: uint64(thereafter),
		hook:       o.hook,
	}

	return s
}

type sampler struct {
	Core

	counts            *counters
	tick              time.Duration
	first, thereafter uint64
	hook              func(Entry, SamplingDecision)
}

var (
	_ Core           = (*sampler)(nil)
	_ leveledEnabler = (*sampler)(nil)
)

// NewSampler creates a Core that samples incoming entries, which
// caps the CPU and I/O load of logging while attempting to preserve a
// representative subset of your logs.
//
// Zap samples by logging the first N entries with a given level and message
// each tick. If more Entries with the same level and message are seen during
// the same interval, every Mth message is logged and the rest are dropped.
//
// Keep in mind that zap's sampling implementation is optimized for speed over
// absolute precision; under load, each tick may be slightly over- or
// under-sampled.
//
// Deprecated: use NewSamplerWithOptions.
func NewSampler(core Core, tick time.Duration, first, thereafter int) Core {
	return NewSamplerWithOptions(core, tick, first, thereafter)
}

func (s *sampler) Level() Level {
	return LevelOf(s.Core)
}

func (s *sampler) With(fields []Field) Core {
	return &sampler{
		Core:       s.Core.With(fields),
		tick:       s.tick,
		counts:     s.counts,
		first:      s.first,
		thereafter: s.thereafter,
		hook:       s.hook,
	}
}

func (s *sampler) Check(ent Entry, ce *CheckedEntry) *CheckedEntry {
	if !s.Enabled(ent.Level) {
		return ce
	}

	if ent.Level >= _minLevel && ent.Level <= _maxLevel {
		counter := s.counts.get(ent.Level, ent.Message)
		n := counter.IncCheckReset(ent.Time, s.tick)
		if n > s.first && (s.thereafter == 0 || (n-s.first)%s.thereafter != 0) {
			s.hook(ent, LogDropped)
			return ce
		}
		s.hook(ent, LogSampled)
	}
	return s.Core.Check(ent, ce)
}

// NewFieldSamplerWithOptions creates a Core that samples incoming entries
// based on the values of the selected fields, which caps the CPU and I/O load
// of logging while attempting to preserve a representative subset of your
// logs.
//
// Unlike NewSamplerWithOptions, which counts entries by their message, this
// sampler counts entries by the values of the fields named in keys. Fields
// bound with Core.With and fields supplied at the log site together determine
// the sampling key, so, for example, keying on a "tenant" field keeps a
// noisy tenant from consuming the sampling budget of quieter tenants that
// log the same message.
//
// For example,
//
//	core = NewFieldSamplerWithOptions(core, time.Second, 10, 5, []string{"tenant"})
//
// This will log the first 10 log entries with the same level and tenant in a
// one second interval as-is. Following that, it will allow through every 5th
// log entry with the same level and tenant in that interval.
//
// If thereafter is zero, the Core will drop all log entries after the first N
// in that interval.
//
// Because fields are not available when Check is called, the sampling
// decision is deferred to Write. Only the selected fields are encoded to
// compute the key, and each marshaler among them is evaluated at most once
// per entry; fields that don't participate in the key are never encoded for
// entries that are dropped.
//
// The sampler counts keys in a fixed-size table (_countersPerLevel counters
// per level), so memory usage is bounded regardless of how many distinct
// field values are observed; distinct keys may share a counter if their
// hashes collide.
//
// Sampler can be configured to report sampling decisions with the
// SamplerHook option.
//
// Keep in mind that Zap's sampling implementation is optimized for speed over
// absolute precision; under load, each tick may be slightly over- or
// under-sampled.
func NewFieldSamplerWithOptions(core Core, tick time.Duration, first, thereafter int, keys []string, opts ...SamplerOption) Core {
	o := newSamplerOptions(opts)
	keySet := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		keySet[k] = struct{}{}
	}
	return &fieldSampler{
		Core:       core,
		tick:       tick,
		counts:     newCounters(),
		first:      uint64(first),
		thereafter: uint64(thereafter),
		keys:       keys,
		keySet:     keySet,
		hook:       o.hook,
	}
}

type fieldSampler struct {
	Core

	counts            *counters
	tick              time.Duration
	first, thereafter uint64
	keys              []string
	keySet            map[string]struct{}
	with              []Field
	hook              func(Entry, SamplingDecision)
}

var (
	_ Core           = (*fieldSampler)(nil)
	_ leveledEnabler = (*fieldSampler)(nil)
)

func (s *fieldSampler) Level() Level {
	return LevelOf(s.Core)
}

func (s *fieldSampler) With(fields []Field) Core {
	with := make([]Field, 0, len(s.with)+len(fields))
	with = append(with, s.with...)
	with = append(with, fields...)
	return &fieldSampler{
		Core:       s.Core.With(fields),
		tick:       s.tick,
		counts:     s.counts,
		first:      s.first,
		thereafter: s.thereafter,
		keys:       s.keys,
		keySet:     s.keySet,
		with:       with,
		hook:       s.hook,
	}
}

func (s *fieldSampler) Check(ent Entry, ce *CheckedEntry) *CheckedEntry {
	if !s.Enabled(ent.Level) {
		return ce
	}
	// The fields supplied at the log site aren't available until Write, so
	// the sampling decision is deferred until then.
	return ce.AddCore(ent, s)
}

func (s *fieldSampler) Write(ent Entry, fields []Field) error {
	if ent.Level >= _minLevel && ent.Level <= _maxLevel {
		counter := s.counts.get(ent.Level, s.key(fields))
		n := counter.IncCheckReset(ent.Time, s.tick)
		if n > s.first && (s.thereafter == 0 || (n-s.first)%s.thereafter != 0) {
			s.hook(ent, LogDropped)
			return nil
		}
		s.hook(ent, LogSampled)
	}
	return s.Core.Write(ent, fields)
}

// key computes the sampling key from the selected fields bound with With and
// those supplied at the log site. Only fields named in keys are encoded, and
// each of them is encoded at most once.
func (s *fieldSampler) key(fields []Field) string {
	enc := NewMapObjectEncoder()
	for _, f := range s.with {
		if _, ok := s.keySet[f.Key]; ok {
			f.AddTo(enc)
		}
	}
	for _, f := range fields {
		if _, ok := s.keySet[f.Key]; ok {
			f.AddTo(enc)
		}
	}

	var b strings.Builder
	for _, k := range s.keys {
		v, ok := enc.Fields[k]
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "%q=%v;", k, v)
	}
	return b.String()
}
