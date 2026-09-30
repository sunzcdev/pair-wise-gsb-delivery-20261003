/*
 * SPDX-FileCopyrightText: © 2017-2025 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package ristretto

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// feedTrace pushes a deliberately skewed access pattern: a few hot keys are
// touched many times, some warm keys a few times, and the long tail of cold
// keys only once. It runs entirely on the policy (under its lock) so the
// resulting TinyLFU state is deterministic.
func feedTrace(p *defaultPolicy[int], hot, warm, cold []uint64, hotN, warmN int) {
	p.Lock()
	defer p.Unlock()
	for _, k := range hot {
		for i := 0; i < hotN; i++ {
			p.admit.Increment(k)
		}
	}
	for _, k := range warm {
		for i := 0; i < warmN; i++ {
			p.admit.Increment(k)
		}
	}
	for _, k := range cold {
		p.admit.Increment(k)
	}
}

// decisions records everything Add can observably return, in order.
type decisions struct {
	added   []bool
	victims [][]uint64
}

func (d *decisions) record(added bool, victims []*Item[int]) {
	d.added = append(d.added, added)
	keys := make([]uint64, 0, len(victims))
	for _, v := range victims {
		keys = append(keys, v.Key)
	}
	d.victims = append(d.victims, keys)
}

// TestAdmissionExportRestoreParity is the core scenario: feed a hot/cold
// trace, export, rebuild the policy in a fresh instance, then continue the
// same requests and require identical admission choices.
func TestAdmissionExportRestoreParity(t *testing.T) {
	// A wide sketch makes collisions negligible so estimates reflect the
	// intended frequencies; maxCost is sized so each admission below needs
	// exactly one eviction, keeping sampled eviction deterministic.
	const numCounters, maxCost = int64(100000), int64(10)
	src := newDefaultPolicy[int](numCounters, maxCost)
	dst := newDefaultPolicy[int](numCounters, maxCost)
	defer src.Close()
	defer dst.Close()

	hot := []uint64{1, 2}
	warm := []uint64{3, 4}
	cold := []uint64{101, 102, 103, 104}
	// 2*120 + 2*20 + 4 = 284 increments, well below resetAt (100000).
	feedTrace(src, hot, warm, cold, 120, 20)

	// Give both policies the same resident set with distinct per-key costs
	// (and distinct frequencies) so sampled eviction has no tie-breaking.
	seedResident := func(p *defaultPolicy[int]) {
		p.Lock()
		defer p.Unlock()
		for k, freq := range map[uint64]int{11: 1, 12: 3, 13: 7, 14: 15} {
			for i := 0; i < freq; i++ {
				p.admit.Increment(k)
			}
		}
		for k, cost := range map[uint64]int64{11: 1, 12: 2, 13: 3, 14: 4} {
			p.evict.add(k, cost)
		}
	}
	seedResident(src)
	seedResident(dst)

	// Sanity: before restore the fresh policy does not know the hot keys.
	src.Lock()
	dst.Lock()
	require.Less(t, dst.admit.Estimate(1), src.admit.Estimate(1))
	dst.Unlock()
	src.Unlock()

	require.NoError(t, dst.RestoreAdmissionPolicyState(src.ExportAdmissionPolicyState()))

	// After restore, estimates (doorkeeper + sketch) must match.
	src.Lock()
	dst.Lock()
	for _, k := range append(append(append([]uint64{}, hot...), warm...), cold...) {
		require.Equal(t, src.admit.Estimate(k), dst.admit.Estimate(k), "key %d", k)
	}
	dst.Unlock()
	src.Unlock()

	// Continue the same requests against both policies and compare choices.
	requests := []struct {
		key  uint64
		cost int64
	}{
		{1, 1},   // very hot, evicts the coldest resident 11
		{2, 1},   // very hot, evicts resident 12
		{101, 5}, // one-hit-wonder, rejected against the sampled minimum
		{102, 1}, // cold key, admitted exactly into remaining room
		{3, 1},   // warm key, evicts the cold new-comer 102
	}

	var got, want decisions
	for _, r := range requests {
		v, a := dst.Add(r.key, r.cost)
		got.record(a, v)
		v, a = src.Add(r.key, r.cost)
		want.record(a, v)
	}
	require.Equal(t, want.added, got.added)
	require.Equal(t, want.victims, got.victims)

	// Bookkeeping must converge as well.
	require.Equal(t, src.Cap(), dst.Cap())
	require.Equal(t, src.Cost(1), dst.Cost(1))
}

// TestAdmissionExportNoEntriesNoCost proves the snapshot carries no key costs
// or resident entries: restoring into an empty policy leaves its sampled-LFU
// state empty and its capacity untouched.
func TestAdmissionExportNoEntriesNoCost(t *testing.T) {
	src := newDefaultPolicy[int](1000, 100)
	dst := newDefaultPolicy[int](1000, 100)
	defer src.Close()
	defer dst.Close()

	src.Add(7, 10)
	src.Add(8, 20)
	src.Lock()
	for i := 0; i < 50; i++ {
		src.admit.Increment(7)
	}
	src.Unlock()

	beforeCap := dst.Cap()
	require.NoError(t, dst.RestoreAdmissionPolicyState(src.ExportAdmissionPolicyState()))

	// No entries materialized, no cost consumed.
	require.False(t, dst.Has(7))
	require.False(t, dst.Has(8))
	require.Equal(t, beforeCap, dst.Cap())
	require.Equal(t, int64(-1), dst.Cost(7))
	// But the heat memory is present.
	require.Equal(t, src.ExportAdmissionPolicyState().Incrs,
		dst.ExportAdmissionPolicyState().Incrs)
}

// TestAdmissionStateJSONRoundTrip ensures the snapshot is serializable and
// the restored policy behaves identically after a JSON round trip.
func TestAdmissionStateJSONRoundTrip(t *testing.T) {
	src := newDefaultPolicy[int](2048, 50)
	dst := newDefaultPolicy[int](2048, 50)
	defer src.Close()
	defer dst.Close()

	feedTrace(src, []uint64{5, 6}, []uint64{7}, []uint64{900, 901, 902}, 60, 10)

	state := src.ExportAdmissionPolicyState()
	raw, err := json.Marshal(state)
	require.NoError(t, err)

	var decoded AdmissionPolicyState
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.NoError(t, dst.RestoreAdmissionPolicyState(&decoded))

	src.Lock()
	dst.Lock()
	for k := uint64(0); k < 1000; k++ {
		require.Equal(t, src.admit.Estimate(k), dst.admit.Estimate(k), "key %d", k)
	}
	dst.Unlock()
	src.Unlock()
	require.Equal(t, src.ExportAdmissionPolicyState(), dst.ExportAdmissionPolicyState())
}

// TestAdmissionRestoreRejectMismatches covers version, size, geometry and
// hash-identity incompatibilities; each rejection leaves the target unchanged.
func TestAdmissionRestoreRejectMismatches(t *testing.T) {
	t.Run("version", func(t *testing.T) {
		src := newDefaultPolicy[int](1000, 10)
		dst := newDefaultPolicy[int](1000, 10)
		defer src.Close()
		defer dst.Close()
		feedTrace(src, []uint64{1}, nil, []uint64{2}, 5, 0)

		before := dst.ExportAdmissionPolicyState()
		state := src.ExportAdmissionPolicyState()
		state.Version = 999
		require.ErrorIs(t, dst.RestoreAdmissionPolicyState(state), ErrAdmissionStateVersion)
		require.Equal(t, before, dst.ExportAdmissionPolicyState())
	})

	t.Run("num counters", func(t *testing.T) {
		src := newDefaultPolicy[int](1000, 10)
		dst := newDefaultPolicy[int](2000, 10)
		defer src.Close()
		defer dst.Close()
		require.ErrorIs(t, dst.RestoreAdmissionPolicyState(src.ExportAdmissionPolicyState()),
			ErrAdmissionStateSize)
	})

	t.Run("malformed row", func(t *testing.T) {
		src := newDefaultPolicy[int](1000, 10)
		dst := newDefaultPolicy[int](1000, 10)
		defer src.Close()
		defer dst.Close()
		feedTrace(src, []uint64{1}, nil, nil, 3, 0)

		before := dst.ExportAdmissionPolicyState()
		state := src.ExportAdmissionPolicyState()
		state.Rows[0] = state.Rows[0][:len(state.Rows[0])-1]
		require.ErrorIs(t, dst.RestoreAdmissionPolicyState(state), ErrAdmissionStateInvalid)
		require.Equal(t, before, dst.ExportAdmissionPolicyState())
	})

	t.Run("bloom geometry", func(t *testing.T) {
		src := newDefaultPolicy[int](1000, 10)
		dst := newDefaultPolicy[int](1000, 10)
		defer src.Close()
		defer dst.Close()
		state := src.ExportAdmissionPolicyState()
		state.BloomSetLocs = 0
		require.ErrorIs(t, dst.RestoreAdmissionPolicyState(state), ErrAdmissionStateInvalid)
	})

	t.Run("hash identity", func(t *testing.T) {
		src := newDefaultPolicy[int](1000, 10)
		dst := newDefaultPolicy[int](1000, 10)
		defer src.Close()
		defer dst.Close()
		src.keyHashID = 42
		dst.keyHashID = 7
		require.ErrorIs(t, dst.RestoreAdmissionPolicyState(src.ExportAdmissionPolicyState()),
			ErrAdmissionStateHash)
	})

	t.Run("nil state", func(t *testing.T) {
		dst := newDefaultPolicy[int](1000, 10)
		defer dst.Close()
		require.ErrorIs(t, dst.RestoreAdmissionPolicyState(nil), ErrAdmissionStateInvalid)
	})
}

// TestAdmissionCacheRestoreNoEntries exercises the public Cache API: the
// restored cache stays empty and free, yet admits keys like the source.
func TestAdmissionCacheRestoreNoEntries(t *testing.T) {
	newCache := func(t *testing.T) *Cache[uint64, int] {
		c, err := NewCache[uint64, int](&Config[uint64, int]{
			NumCounters:        100000,
			MaxCost:            10,
			BufferItems:        4,
			IgnoreInternalCost: true,
		})
		require.NoError(t, err)
		return c
	}

	src := newCache(t)
	defer src.Close()

	// BufferItems=4: every 4 Gets drain one stripe deterministically. Yield
	// between drains so the policy channel is never full (which would drop).
	drainGets := func(key uint64, flushes int) {
		for f := 0; f < flushes; f++ {
			for i := 0; i < 4; i++ {
				src.Get(key)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	drainGets(1, 10) // hot: 10 admissions to the sketch
	for k := uint64(900); k < 910; k++ {
		drainGets(k, 2) // fillers: 2 each
	}
	drainGets(501, 1) // one-hit-wonder: 1
	src.Wait()

	state := src.ExportAdmissionPolicyState()
	require.Equal(t, uint64(0), state.KeyHashID)

	dst := newCache(t)
	defer dst.Close()
	require.NoError(t, dst.RestoreAdmissionPolicyState(state))

	// Restore must not invent entries or consume cost.
	require.Equal(t, int64(10), dst.RemainingCost())
	_, found := dst.Get(1)
	require.False(t, found)

	// Fill the destination, then send the one-hit-wonder through the same
	// admission pipeline as the source would see.
	for k := uint64(900); k < 910; k++ {
		require.True(t, dst.Set(k, 0, 1))
	}
	dst.Set(501, 0, 1)
	dst.Wait()
	for k := uint64(900); k < 910; k++ {
		_, ok := dst.Get(k)
		require.True(t, ok, "filler %d must remain resident", k)
	}
	_, found = dst.Get(501)
	require.False(t, found, "cold one-hit-wonder should have been rejected")

	// The hot key restored from the snapshot is admitted, evicting a filler.
	dst.Set(1, 0, 1)
	dst.Wait()
	_, found = dst.Get(1)
	require.True(t, found, "hot key should be admitted")

	// A fresh cache without the snapshot admits the same one-hit-wonder: at
	// estimate zero the incoming key ties the sampled minimum and is let in.
	fresh := newCache(t)
	defer fresh.Close()
	for k := uint64(900); k < 910; k++ {
		fresh.Set(k, 0, 1)
	}
	fresh.Set(501, 0, 1)
	fresh.Wait()
	_, found = fresh.Get(501)
	require.True(t, found, "without restored heat the cold key ties and is admitted")
}

// TestAdmissionExportConcurrent exercises exports racing counter updates.
// Run with -race; every exported snapshot must be internally coherent, never
// torn. Approximation (an increment may or may not appear) is allowed.
func TestAdmissionExportConcurrent(t *testing.T) {
	p := newDefaultPolicy[int](1<<16, 1<<14)
	defer p.Close()

	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		var i uint64
		for {
			select {
			case <-stop:
				return
			default:
				p.Lock()
				p.admit.Increment(i % 1024)
				p.Unlock()
				i++
			}
		}
	}()

	for i := 0; i < 1000; i++ {
		state := p.ExportAdmissionPolicyState()
		require.NoError(t, state.validate())

		// The deep copy must be independent of later mutations.
		row := append([]byte(nil), state.Rows[0]...)
		time.Sleep(time.Microsecond)
		require.Equal(t, row, state.Rows[0])

		// Any coherent snapshot must restore cleanly into an equal-size policy.
		twin := newDefaultPolicy[int](1<<16, 1<<14)
		require.NoError(t, twin.RestoreAdmissionPolicyState(state))
		twin.Close()
	}

	close(stop)
	wg.Wait()
}
