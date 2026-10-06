// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package codecs

import "errors"

var (
	errShortPacket          = errors.New("packet is not large enough")
	errNilPacket            = errors.New("invalid nil packet")
	errTooManyPDiff         = errors.New("too many PDiff")
	errTooManySpatialLayers = errors.New("too many spatial layers")
	errUnhandledNALUType    = errors.New("NALU Type is unhandled")

	// RED (RFC 2198) errors.
	errREDNoBlocks            = errors.New("RED payload requires at least one block")
	errREDBlockLength         = errors.New("RED redundant block exceeds 10-bit length field")
	errREDPayloadType         = errors.New("RED block payload type exceeds 7 bits")
	errREDTimestampOffset     = errors.New("RED timestamp offset exceeds 30 bits")
	errREDZeroTimestampOffset = errors.New("RED redundant timestamp offset must be non-zero")

	// AV1 Errors.
	errIsKeyframeAndFragment = errors.New(
		"bits Z and N are set. Not possible to have OBU be tail fragment and be keyframe",
	)
)
