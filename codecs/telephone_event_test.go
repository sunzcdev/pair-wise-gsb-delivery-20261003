// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package codecs

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTelephoneEventMarshalUnmarshal(t *testing.T) {
	event := TelephoneEvent{Event: 11, End: true, Volume: 63, Duration: 0xABCD}

	raw := event.Marshal()
	require.Len(t, raw, TelephoneEventPayloadSize)
	assert.Equal(t, []byte{0x0B, 0xBF, 0xAB, 0xCD}, raw)

	var parsed TelephoneEvent
	require.NoError(t, parsed.Unmarshal(raw))
	assert.Equal(t, event, parsed)

	// The reserved bit must be ignored and the volume masked to 6 bits.
	var noEnd TelephoneEvent
	require.NoError(t, noEnd.Unmarshal([]byte{0x01, 0x40 | 0x0A, 0x00, 0xA0}))
	assert.Equal(t, TelephoneEvent{Event: 1, End: false, Volume: 10, Duration: 160}, noEnd)
}

func TestTelephoneEventUnmarshalErrors(t *testing.T) {
	var event TelephoneEvent
	assert.ErrorIs(t, event.Unmarshal(nil), errNilPacket)
	assert.ErrorIs(t, event.Unmarshal([]byte{0x01, 0x02, 0x03}), errShortPacket)
}

func TestTelephoneEventPayloader(t *testing.T) {
	payloader := &TelephoneEventPayloader{}

	assert.Equal(t, [][]byte{}, payloader.Payload(1200, nil))

	payload := (&TelephoneEvent{Event: 1, Duration: 160}).Marshal()
	out := payloader.Payload(1200, payload)
	require.Len(t, out, 1)
	assert.Equal(t, payload, out[0])
}

func TestTelephoneEventPacket(t *testing.T) {
	packet := &TelephoneEventPacket{}

	raw := (&TelephoneEvent{Event: 15, End: true, Volume: 20, Duration: 320}).Marshal()
	payload, err := packet.Unmarshal(raw)
	require.NoError(t, err)
	assert.Equal(t, raw, payload)
	assert.Equal(t, TelephoneEvent{Event: 15, End: true, Volume: 20, Duration: 320}, packet.Event)

	_, err = packet.Unmarshal(nil)
	assert.ErrorIs(t, err, errNilPacket)

	assert.True(t, packet.IsPartitionHead(raw))
	assert.True(t, packet.IsPartitionTail(false, raw))
}
