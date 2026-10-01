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

	"go.uber.org/multierr"
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

// optionFunc wraps a func so it satisfies the SamplerOption interface.
type optionFunc func(*sampler)

func (f optionFunc) apply(s *sampler) {
	f(s)
}

// SamplerOption configures a Sampler.
type SamplerOption interface {
	apply(*sampler)
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
	return optionFunc(func(s *sampler) {
		s.hook = hook
	})
}

// SamplerKeys configures the sampler to include the values of the named
// structured fields in the sampling key, in addition to the entry's level
// and message.
//
// This is useful when the same message is logged on behalf of many different
// tenants or contexts: keying only on the message lets high-volume tenants
// consume the entire sampling budget of a shared message, starving the rest.
// For example,
//
//	core = NewSamplerWithOptions(core, time.Second, 10, 5,
//	    SamplerKeys("tenant"))
//
// logs the first 10 entries per tenant and message each second, and every
// 5th entry per tenant and message thereafter.
//
// Fields bound to the Core with With and fields passed to the individual
// logging call both contribute to the sampling key. Because the latter are
// not available when the entry is checked, the sampling decision is deferred
// until the entry is written.
//
// Only fields named here are encoded to compute the key, and each of their
// marshalers is evaluated at most once per entry; fields that don't
// participate in the key are never encoded for dropped entries.
//
// Keys share the same fixed-size counter table as message-based sampling, so
// memory usage stays bounded no matter how many distinct keys are observed.
// Distinct keys that happen to hash to the same counter share their sampling
// budget.
func SamplerKeys(keys ...string) SamplerOption {
	return optionFunc(func(s *sampler) {
		s.keyFields = make(map[string]struct{}, len(keys))
		for _, k := range keys {
			s.keyFields[k] = struct{}{}
		}
	})
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
// By default, entries are sampled by their level and message alone. Use the
// SamplerKeys option to additionally key on the values of selected structured
// fields.
//
// Keep in mind that Zap's sampling implementation is optimized for speed over
// absolute precision; under load, each tick may be slightly over- or
// under-sampled.
func NewSamplerWithOptions(core Core, tick time.Duration, first, thereafter int, opts ...SamplerOption) Core {
	s := &sampler{
		Core:       core,
		tick:       tick,
		counts:     newCounters(),
		first:      uint64(first),
		thereafter: uint64(thereafter),
		hook:       nopSamplingHook,
	}
	for _, opt := range opts {
		opt.apply(s)
	}

	return s
}

type sampler struct {
	Core

	counts            *counters
	tick              time.Duration
	first, thereafter uint64
	hook              func(Entry, SamplingDecision)

	// keyFields, when non-empty, names the structured fields whose values
	// participate in the sampling key alongside the entry's message.
	keyFields map[string]struct{}
	// keyPrefix holds the encoded key fragments of matching fields bound
	// with With, so that they're extracted exactly once no matter how many
	// entries the derived Core logs.
	keyPrefix string
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
	ns := &sampler{
		Core:       s.Core.With(fields),
		tick:       s.tick,
		counts:     s.counts,
		first:      s.first,
		thereafter: s.thereafter,
		hook:       s.hook,
		keyFields:  s.keyFields,
		keyPrefix:  s.keyPrefix,
	}
	for i := range fields {
		if _, ok := s.keyFields[fields[i].Key]; ok {
			ns.keyPrefix += keyFragment(fields[i])
		}
	}
	return ns
}

func (s *sampler) Check(ent Entry, ce *CheckedEntry) *CheckedEntry {
	if !s.Enabled(ent.Level) {
		return ce
	}

	if ent.Level >= _minLevel && ent.Level <= _maxLevel {
		if len(s.keyFields) > 0 {
			// Fields passed to the logging call aren't available until
			// Write, so defer the sampling decision to a per-entry gate.
			inner := s.Core.Check(ent, nil)
			if inner == nil {
				return ce
			}
			cores := append([]Core(nil), inner.cores...)
			putCheckedEntry(inner)
			return ce.AddCore(ent, &samplingGate{sampler: s, cores: cores})
		}

		if !s.sample(ent, ent.Message) {
			return ce
		}
	}
	return s.Core.Check(ent, ce)
}

// sample makes a sampling decision for an entry with the given key, reporting
// it to the configured hook.
func (s *sampler) sample(ent Entry, key string) bool {
	counter := s.counts.get(ent.Level, key)
	n := counter.IncCheckReset(ent.Time, s.tick)
	if n > s.first && (s.thereafter == 0 || (n-s.first)%s.thereafter != 0) {
		s.hook(ent, LogDropped)
		return false
	}
	s.hook(ent, LogSampled)
	return true
}

// samplingGate is a per-entry Core that defers the sampling decision until
// Write, when the fields passed to the logging call are available. It holds
// the cores that accepted the entry during Check and forwards sampled entries
// to them.
type samplingGate struct {
	sampler *sampler
	cores   []Core
}

var _ Core = (*samplingGate)(nil)

func (g *samplingGate) Enabled(Level) bool { return true }
func (g *samplingGate) With([]Field) Core  { return g }
func (g *samplingGate) Sync() error        { return g.sampler.Sync() }
func (g *samplingGate) Level() Level       { return LevelOf(g.sampler) }

func (g *samplingGate) Check(ent Entry, ce *CheckedEntry) *CheckedEntry {
	return ce.AddCore(ent, g)
}

func (g *samplingGate) Write(ent Entry, fields []Field) error {
	if !g.sampler.sample(ent, g.sampler.keyFor(ent, fields)) {
		return nil
	}

	var err error
	for i := range g.cores {
		err = multierr.Append(err, g.cores[i].Write(ent, fields))
	}
	return err
}

// keyFor builds the sampling key for an entry from its message, the key
// fragments bound with With, and the matching fields passed to the logging
// call.
func (s *sampler) keyFor(ent Entry, fields []Field) string {
	var sb strings.Builder
	sb.WriteString(ent.Message)
	sb.WriteString(s.keyPrefix)
	for i := range fields {
		if _, ok := s.keyFields[fields[i].Key]; ok {
			sb.WriteString(keyFragment(fields[i]))
		}
	}
	return sb.String()
}

// keyFragment renders a field's value for use in a sampling key. Marshalers
// carried by the field are evaluated exactly once.
func keyFragment(f Field) string {
	if f.Type == StringType {
		return "\xff" + f.String
	}
	enc := NewMapObjectEncoder()
	f.AddTo(enc)
	v, ok := enc.Fields[f.Key]
	if !ok {
		// Fields that don't add themselves under their own key (e.g.
		// inline marshalers) are represented by everything they encoded.
		v = enc.Fields
	}
	return "\xff" + fmt.Sprint(v)
}
