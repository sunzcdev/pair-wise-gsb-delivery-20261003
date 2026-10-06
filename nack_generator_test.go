// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package rtcp

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestNackGenerator(t *testing.T, config NackGeneratorConfig) *NackGenerator {
	t.Helper()

	if config.SenderSSRC == 0 && config.MediaSSRC == 0 {
		config.SenderSSRC = 1
		config.MediaSSRC = 2
	}

	generator, err := NewNackGenerator(config)
	require.NoError(t, err)

	return generator
}

func feedbackSequences(packet *TransportLayerNack) []uint16 {
	sequences := []uint16{}
	if packet == nil {
		return sequences
	}

	for i := range packet.Nacks {
		sequences = append(sequences, packet.Nacks[i].PacketList()...)
	}

	return sequences
}

// TestNackGeneratorFeedbackLifecycle drives a virtual clock through the four
// distinguishable outcomes: first request after the reorder window, second
// request after the retry interval, cancellation by a late packet and final
// expiry after the maximum lifetime.
func TestNackGeneratorFeedbackLifecycle(t *testing.T) {
	const (
		window = 50 * time.Millisecond
		retry  = 100 * time.Millisecond
		maxAge = 400 * time.Millisecond
	)

	now := time.Unix(0, 0)
	generator := newTestNackGenerator(t, NackGeneratorConfig{
		ReorderWindow: window,
		RetryInterval: retry,
		MaxLifetime:   maxAge,
		MaxTracked:    64,
		SenderSSRC:    0x11111111,
		MediaSSRC:     0x22222222,
	})

	result, err := generator.PacketReceived(1000, now)
	require.NoError(t, err)
	assert.Equal(t, ReceptionNew, result.Class)

	// No packet at or before the reorder window: mild reordering is tolerated.
	packet, info := generator.Feedback(now)
	assert.Nil(t, packet)
	assert.Empty(t, info.FirstRequests)

	// Gap 1001 appears when 1002 jumps ahead.
	result, err = generator.PacketReceived(1002, now.Add(10*time.Millisecond))
	require.NoError(t, err)
	assert.Equal(t, ReceptionNew, result.Class)
	assert.Equal(t, 1, generator.Pending())

	packet, info = generator.Feedback(now.Add(10 * time.Millisecond))
	assert.Nil(t, packet)
	assert.Empty(t, info.FirstRequests)

	// First feedback only after the reorder window elapsed.
	packet, info = generator.Feedback(now.Add(60 * time.Millisecond))
	require.NotNil(t, packet)
	assert.Equal(t, []uint16{1001}, info.FirstRequests)
	assert.Empty(t, info.Retries)
	assert.Empty(t, info.Expired)
	assert.Equal(t, []uint16{1001}, feedbackSequences(packet))
	assert.Equal(t, uint32(0x11111111), packet.SenderSSRC)
	assert.Equal(t, uint32(0x22222222), packet.MediaSSRC)

	// Before the retry interval the same gap must not be requested again.
	packet, info = generator.Feedback(now.Add(150 * time.Millisecond))
	assert.Nil(t, packet)
	assert.Empty(t, info.FirstRequests)
	assert.Empty(t, info.Retries)

	// Second request after the retry interval.
	packet, info = generator.Feedback(now.Add(160 * time.Millisecond))
	require.NotNil(t, packet)
	assert.Empty(t, info.FirstRequests)
	assert.Equal(t, []uint16{1001}, info.Retries)
	assert.Empty(t, info.Expired)
	assert.Equal(t, []uint16{1001}, feedbackSequences(packet))

	// The late packet cancels the gap and every pending retransmission.
	result, err = generator.PacketReceived(1001, now.Add(200*time.Millisecond))
	require.NoError(t, err)
	assert.Equal(t, ReceptionLate, result.Class)
	assert.Equal(t, 190*time.Millisecond, result.ReorderDelay)
	assert.Equal(t, 0, generator.Pending())

	packet, info = generator.Feedback(now.Add(250 * time.Millisecond))
	assert.Nil(t, packet)
	assert.Empty(t, info.FirstRequests)
	assert.Empty(t, info.Retries)

	// A new gap opens: request once, then retry, until it finally expires.
	result, err = generator.PacketReceived(1004, now.Add(300*time.Millisecond))
	require.NoError(t, err)
	assert.Equal(t, ReceptionNew, result.Class)

	packet, info = generator.Feedback(now.Add(350 * time.Millisecond))
	require.NotNil(t, packet)
	assert.Equal(t, []uint16{1003}, info.FirstRequests)

	packet, info = generator.Feedback(now.Add(450 * time.Millisecond))
	require.NotNil(t, packet)
	assert.Equal(t, []uint16{1003}, info.Retries)

	// MaxLifetime is reached at T+700; expiry wins over a due retry and the
	// entry disappears without another packet.
	packet, info = generator.Feedback(now.Add(700 * time.Millisecond))
	assert.Nil(t, packet)
	assert.Empty(t, info.FirstRequests)
	assert.Empty(t, info.Retries)
	assert.Equal(t, []uint16{1003}, info.Expired)
	assert.Equal(t, 0, generator.Pending())
}

func TestNackGeneratorDuplicate(t *testing.T) {
	now := time.Unix(0, 0)
	generator := newTestNackGenerator(t, NackGeneratorConfig{
		ReorderWindow: time.Millisecond,
		RetryInterval: time.Millisecond,
		MaxLifetime:   time.Second,
		MaxTracked:    8,
	})

	_, err := generator.PacketReceived(5, now)
	require.NoError(t, err)

	result, err := generator.PacketReceived(5, now)
	require.NoError(t, err)
	assert.Equal(t, ReceptionDuplicate, result.Class)
	assert.Zero(t, result.ReorderDelay)
	assert.Equal(t, 0, generator.Pending())
}

func TestNackGeneratorWrapAround(t *testing.T) {
	now := time.Unix(0, 0)
	generator := newTestNackGenerator(t, NackGeneratorConfig{
		ReorderWindow: time.Millisecond,
		RetryInterval: 2 * time.Millisecond,
		MaxLifetime:   100 * time.Millisecond,
		MaxTracked:    8,
	})

	for _, sequence := range []uint16{65534, 65535, 1} {
		_, err := generator.PacketReceived(sequence, now)
		require.NoError(t, err)
	}

	assert.Equal(t, 1, generator.Pending())

	packet, info := generator.Feedback(now.Add(2 * time.Millisecond))
	require.NotNil(t, packet)
	assert.Equal(t, []uint16{0}, info.FirstRequests)
	assert.Equal(t, []uint16{0}, feedbackSequences(packet))

	// Late arrival across the wrap closes the same missing entry.
	result, err := generator.PacketReceived(0, now.Add(3*time.Millisecond))
	require.NoError(t, err)
	assert.Equal(t, ReceptionLate, result.Class)
	assert.Equal(t, 0, generator.Pending())

	// The stream continues after the wrap; 2 is a normal forward packet.
	result, err = generator.PacketReceived(2, now.Add(4*time.Millisecond))
	require.NoError(t, err)
	assert.Equal(t, ReceptionNew, result.Class)

	// A very old sequence number from before the first observed packet is a
	// duplicate, not a new restart.
	result, err = generator.PacketReceived(60000, now.Add(5*time.Millisecond))
	require.NoError(t, err)
	assert.Equal(t, ReceptionDuplicate, result.Class)
}

func TestNackGeneratorRestartRequiresConfirmation(t *testing.T) {
	now := time.Unix(0, 0)
	generator := newTestNackGenerator(t, NackGeneratorConfig{
		ReorderWindow: time.Millisecond,
		RetryInterval: time.Millisecond,
		MaxLifetime:   100 * time.Millisecond,
		MaxTracked:    4,
	})

	_, err := generator.PacketReceived(100, now)
	require.NoError(t, err)
	_, err = generator.PacketReceived(102, now)
	require.NoError(t, err)
	assert.Equal(t, 1, generator.Pending())

	// A jump larger than MaxTracked is an obvious restart, but state is kept
	// until the caller confirms it.
	result, err := generator.PacketReceived(200, now)
	assert.ErrorIs(t, err, ErrNackStreamRestart)
	assert.Equal(t, ReceptionRestart, result.Class)

	packet, _ := generator.Feedback(now.Add(2 * time.Millisecond))
	require.NotNil(t, packet)
	assert.Equal(t, []uint16{101}, feedbackSequences(packet))

	// Confirming the restart rebuilds the stream around the new sequence.
	generator.ConfirmRestart(200)
	assert.Equal(t, 0, generator.Pending())

	result, err = generator.PacketReceived(201, now.Add(3*time.Millisecond))
	require.NoError(t, err)
	assert.Equal(t, ReceptionNew, result.Class)

	packet, _ = generator.Feedback(now.Add(4 * time.Millisecond))
	assert.Nil(t, packet)
}

func TestNackGeneratorBoundedState(t *testing.T) {
	now := time.Unix(0, 0)
	const maxTracked = 4

	generator := newTestNackGenerator(t, NackGeneratorConfig{
		ReorderWindow: time.Millisecond,
		RetryInterval: time.Millisecond,
		MaxLifetime:   100 * time.Millisecond,
		MaxTracked:    maxTracked,
	})

	// Jump opening exactly MaxTracked gaps, all of which are tracked.
	_, err := generator.PacketReceived(0, now)
	require.NoError(t, err)

	_, err = generator.PacketReceived(uint16(maxTracked+1), now)
	require.NoError(t, err)
	assert.Equal(t, maxTracked, generator.Pending())

	// Fill one late so one capacity slot frees up.
	result, err := generator.PacketReceived(1, now)
	require.NoError(t, err)
	assert.Equal(t, ReceptionLate, result.Class)

	// A smaller forward jump must never exceed the bound; gaps that do not
	// fit are not registered.
	_, err = generator.PacketReceived(uint16(maxTracked+4), now)
	require.NoError(t, err)
	assert.Equal(t, maxTracked, generator.Pending())

	// The skipped sequence number cannot cancel anything when it arrives.
	result, err = generator.PacketReceived(uint16(maxTracked+3), now)
	require.NoError(t, err)
	assert.Equal(t, ReceptionDuplicate, result.Class)
	assert.Equal(t, maxTracked, generator.Pending())
}

func TestNackGeneratorConfig(t *testing.T) {
	for _, test := range []struct {
		name   string
		config NackGeneratorConfig
	}{
		{
			name: "negative reorder window",
			config: NackGeneratorConfig{
				ReorderWindow: -1,
			},
		},
		{
			name: "reorder window larger than lifetime",
			config: NackGeneratorConfig{
				ReorderWindow: time.Second,
				MaxLifetime:   time.Millisecond,
			},
		},
		{
			name: "negative retry interval",
			config: NackGeneratorConfig{
				RetryInterval: -1,
			},
		},
		{
			name: "negative max tracked",
			config: NackGeneratorConfig{
				MaxTracked: -1,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewNackGenerator(test.config)
			assert.ErrorIs(t, err, ErrNackInvalidConfig)
		})
	}

	generator, err := NewNackGenerator(NackGeneratorConfig{})
	require.NoError(t, err)
	assert.Equal(t, defaultNackReorderWindow, generator.config.ReorderWindow)
	assert.Equal(t, defaultNackRetryInterval, generator.config.RetryInterval)
	assert.Equal(t, defaultNackMaxLifetime, generator.config.MaxLifetime)
	assert.Equal(t, defaultNackMaxTracked, generator.config.MaxTracked)

	// Feedback before the first packet is a no-op rather than an error.
	packet, info := generator.Feedback(time.Unix(0, 0))
	assert.Nil(t, packet)
	assert.Empty(t, info.FirstRequests)
	assert.Empty(t, info.Retries)
	assert.Empty(t, info.Expired)
}

func TestNackPairsCapped(t *testing.T) {
	sequences := make([]uint16, 0, 40)
	for i := 0; i < 40; i++ {
		sequences = append(sequences, uint16(i))
	}

	pairs, included := nackPairsCapped(sequences, 2)
	assert.Len(t, pairs, 2)
	assert.Len(t, included, 2*17)
	assert.Equal(t, sequences[:2*17], included)

	// The un-capped helper must keep the same packing as the existing helper.
	sequences = []uint16{1, 3, 17, 18}
	pairs, included = nackPairsCapped(sequences, len(sequences))
	assert.Equal(t, NackPairsFromSequenceNumbers(sequences), pairs)
	assert.Equal(t, sequences, included)
}
