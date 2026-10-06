// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package telephoneevent

// Payloader sends one RFC 4733 payload per RTP packet.
type Payloader struct{}

// Payload copies an encoded telephone-event payload.
func (Payloader) Payload(_ uint16, payload []byte) [][]byte {
	if payload == nil {
		return nil
	}

	out := make([]byte, len(payload))
	copy(out, payload)

	return [][]byte{out}
}

// Depacketizer parses one RFC 4733 payload.
type Depacketizer struct{}

// Unmarshal parses an encoded telephone-event payload.
func (Depacketizer) Unmarshal(payload []byte) ([]byte, error) {
	if _, err := UnmarshalPayload(payload); err != nil {
		return nil, err
	}

	out := make([]byte, len(payload))
	copy(out, payload)

	return out, nil
}

// IsPartitionHead reports whether the payload starts an RTP partition.
func (Depacketizer) IsPartitionHead(_ []byte) bool {
	return true
}

// IsPartitionTail reports whether the payload ends an RTP partition.
func (Depacketizer) IsPartitionTail(_ bool, _ []byte) bool {
	return true
}
