// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package rtcp

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testSenderSSRC uint32 = 0x11111111
	testMediaSSRC  uint32 = 0x22222222
)

func testNackConfig() NackFeedbackConfig {
	return NackFeedbackConfig{
		ReorderWindow: 10 * time.Millisecond,
		RetryInterval: 20 * time.Millisecond,
		MaxTTL:        100 * time.Millisecond,
		MaxTracked:    1024,
	}
}

func flattenNackPairs(pairs []NackPair) []uint16 {
	out := make([]uint16, 0)
	for _, pair := range pairs {
		out = append(out, pair.PacketList()...)
	}

	return out
}

// TestNackFeedbackGeneratorLifecycle drives a single gap through its whole
// life with a virtual clock: the first feedback waits out the reorder window,
// a second request waits out the retry interval, a late packet cancels the
// gap, and a later gap is dropped once it exceeds its maximum lifetime.
func TestNackFeedbackGeneratorLifecycle(t *testing.T) {
	t0 := time.Unix(0, 0)
	gen, err := NewNackFeedbackGenerator(testMediaSSRC, testNackConfig())
	require.NoError(t, err)

	// Packets 0 then 2 arrive together: sequence number 1 is skipped.
	gen.Received(0, t0)
	gen.Received(2, t0)
	assert.Equal(t, []uint16{1}, gen.MissingSequenceNumbers())

	// Nothing is reported while the reorder window is still open.
	assert.Nil(t, gen.BuildTransportLayerNack(testSenderSSRC, t0.Add(5*time.Millisecond)))
	assert.Equal(t, []NackPair{}, gen.BuildNackPairs(t0.Add(5*time.Millisecond)))

	// First feedback: exactly when the reorder window elapses.
	first := gen.BuildTransportLayerNack(testSenderSSRC, t0.Add(10*time.Millisecond))
	require.NotNil(t, first)
	assert.Equal(t, testSenderSSRC, first.SenderSSRC)
	assert.Equal(t, testMediaSSRC, first.MediaSSRC)
	assert.Equal(t, []uint16{1}, flattenNackPairs(first.Nacks))

	// The produced packet must be wire-ready and survive marshal/unmarshal.
	raw, err := first.Marshal()
	require.NoError(t, err)
	var decoded TransportLayerNack
	require.NoError(t, decoded.Unmarshal(raw))
	assert.Equal(t, []uint16{1}, flattenNackPairs(decoded.Nacks))
	assert.Equal(t, testMediaSSRC, decoded.MediaSSRC)

	// Second request is suppressed until the retry interval elapses.
	assert.Nil(t, gen.BuildTransportLayerNack(testSenderSSRC, t0.Add(15*time.Millisecond)))
	second := gen.BuildTransportLayerNack(testSenderSSRC, t0.Add(30*time.Millisecond))
	require.NotNil(t, second)
	assert.Equal(t, []uint16{1}, flattenNackPairs(second.Nacks))

	// The missing packet arrives late: its state is cancelled immediately.
	gen.Received(1, t0.Add(35*time.Millisecond))
	assert.Empty(t, gen.MissingSequenceNumbers())
	assert.Nil(t, gen.BuildTransportLayerNack(testSenderSSRC, t0.Add(50*time.Millisecond)))

	// A fresh burst of loss (3..9) is reported, then expires after MaxTTL.
	gen.Received(10, t0.Add(40*time.Millisecond))
	third := gen.BuildTransportLayerNack(testSenderSSRC, t0.Add(50*time.Millisecond))
	require.NotNil(t, third)
	assert.Equal(t, []uint16{3, 4, 5, 6, 7, 8, 9}, flattenNackPairs(third.Nacks))

	expired := gen.BuildTransportLayerNack(testSenderSSRC, t0.Add(140*time.Millisecond))
	assert.Nil(t, expired)
	assert.Empty(t, gen.MissingSequenceNumbers())
}

func TestNackFeedbackGeneratorDuplicate(t *testing.T) {
	t0 := time.Unix(0, 0)
	gen, err := NewNackFeedbackGenerator(testMediaSSRC, testNackConfig())
	require.NoError(t, err)

	gen.Received(0, t0)
	gen.Received(2, t0)

	// Duplicates of already received packets (including the newest one) must
	// neither cancel nor recreate loss state.
	gen.Received(0, t0.Add(3*time.Millisecond))
	gen.Received(2, t0.Add(4*time.Millisecond))
	assert.Equal(t, []uint16{1}, gen.MissingSequenceNumbers())

	first := gen.BuildTransportLayerNack(testSenderSSRC, t0.Add(10*time.Millisecond))
	require.NotNil(t, first)
	assert.Equal(t, []uint16{1}, flattenNackPairs(first.Nacks))

	gen.Received(2, t0.Add(12*time.Millisecond))
	assert.Equal(t, []uint16{1}, gen.MissingSequenceNumbers())
}

func TestNackFeedbackGeneratorWrapAround(t *testing.T) {
	t0 := time.Unix(0, 0)
	gen, err := NewNackFeedbackGenerator(testMediaSSRC, testNackConfig())
	require.NoError(t, err)

	gen.Received(65534, t0)
	gen.Received(1, t0) // skips 65535 and 0 across the wrap-around
	assert.Equal(t, []uint16{65535, 0}, gen.MissingSequenceNumbers())

	first := gen.BuildTransportLayerNack(testSenderSSRC, t0.Add(10*time.Millisecond))
	require.NotNil(t, first)
	assert.Equal(t, []uint16{65535, 0}, flattenNackPairs(first.Nacks))

	// Packet 0 arrives late across the wrap-around; only 65535 stays missing.
	gen.Received(0, t0.Add(15*time.Millisecond))
	assert.Equal(t, []uint16{65535}, gen.MissingSequenceNumbers())

	second := gen.BuildTransportLayerNack(testSenderSSRC, t0.Add(30*time.Millisecond))
	require.NotNil(t, second)
	assert.Equal(t, []uint16{65535}, flattenNackPairs(second.Nacks))
}

func TestNackFeedbackGeneratorBoundedState(t *testing.T) {
	t0 := time.Unix(0, 0)
	config := testNackConfig()
	config.MaxTracked = 3
	gen, err := NewNackFeedbackGenerator(testMediaSSRC, config)
	require.NoError(t, err)

	gen.Received(0, t0)
	gen.Received(2, t0)                          // gap 1
	gen.Received(4, t0.Add(1*time.Millisecond))  // gap 3
	gen.Received(6, t0.Add(2*time.Millisecond))  // gap 5
	gen.Received(8, t0.Add(3*time.Millisecond))  // gap 7
	gen.Received(10, t0.Add(4*time.Millisecond)) // gap 9

	// Five gaps were observed but capacity is three: the oldest two are
	// evicted and the newest three remain.
	assert.Equal(t, []uint16{5, 7, 9}, gen.MissingSequenceNumbers())

	// A late copy of an evicted number must not grow the state again.
	gen.Received(1, t0.Add(50*time.Millisecond))
	assert.Equal(t, []uint16{5, 7, 9}, gen.MissingSequenceNumbers())
	assert.LessOrEqual(t, len(gen.MissingSequenceNumbers()), config.MaxTracked)
}

func TestNackFeedbackGeneratorReset(t *testing.T) {
	t0 := time.Unix(0, 0)
	gen, err := NewNackFeedbackGenerator(testMediaSSRC, testNackConfig())
	require.NoError(t, err)

	gen.Received(60000, t0)
	gen.Received(60002, t0)
	require.Equal(t, []uint16{60001}, gen.MissingSequenceNumbers())

	// Caller confirmed a sender restart: rebuild the stream from scratch.
	gen.Reset()
	gen.Received(5, t0.Add(1*time.Millisecond))
	assert.Empty(t, gen.MissingSequenceNumbers())
	assert.Nil(t, gen.BuildTransportLayerNack(testSenderSSRC, t0.Add(20*time.Millisecond)))
}

func TestNackFeedbackGeneratorInvalidConfig(t *testing.T) {
	for _, test := range []struct {
		name   string
		config NackFeedbackConfig
		err    error
	}{
		{"negative reorder window", NackFeedbackConfig{ReorderWindow: -1, RetryInterval: time.Millisecond, MaxTTL: time.Second, MaxTracked: 1}, errNackInvalidReorderWindow},
		{"zero retry interval", NackFeedbackConfig{RetryInterval: 0, MaxTTL: time.Second, MaxTracked: 1}, errNackInvalidRetryInterval},
		{"zero ttl", NackFeedbackConfig{RetryInterval: time.Millisecond, MaxTTL: 0, MaxTracked: 1}, errNackInvalidTTL},
		{"zero max tracked", NackFeedbackConfig{RetryInterval: time.Millisecond, MaxTTL: time.Second, MaxTracked: 0}, errNackInvalidMaxTracked},
	} {
		_, err := NewNackFeedbackGenerator(testMediaSSRC, test.config)
		assert.ErrorIsf(t, err, test.err, "%q", test.name)
	}
}
