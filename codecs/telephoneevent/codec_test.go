// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package telephoneevent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPayloaderAndDepacketizer(t *testing.T) {
	raw, err := Payload{Event: Digit8, Volume: 3, Duration: 42}.Marshal()
	require.NoError(t, err)

	payloads := Payloader{}.Payload(1500, raw)
	require.Len(t, payloads, 1)
	assert.Equal(t, raw, payloads[0])

	payloads[0][0] = byte(Digit9)
	assert.Equal(t, Digit8, Event(raw[0]))

	out, err := Depacketizer{}.Unmarshal(raw)
	require.NoError(t, err)
	assert.Equal(t, raw, out)
	assert.True(t, Depacketizer{}.IsPartitionHead(raw))
	assert.True(t, Depacketizer{}.IsPartitionTail(false, raw))
}
