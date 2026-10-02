// Package ber implements ASN.1 Basic Encoding Rules (BER), the encoding used by
// protocols such as LDAP (RFC 4511).
//
// A decoded document is represented as a tree of [Packet] values. Decode bytes
// with [DecodePacketErr], or stream from an [io.Reader] with [ReadPacket]; build
// packets to encode with the New* constructors and serialize them with
// [Packet.Bytes].
//
// INTEGER values are represented as int64 by default; values that do not fit
// are rejected. For arbitrary-precision integers, build a packet with
// [NewBigInteger] and decode with [DecodePacketErrWithOptions] using
// [WithIntegerMode] ([IntegerModeBig] or [IntegerModeAuto]).
//
// The decoder is written to accept untrusted input: the [MaxPacketLengthBytes]
// and [MaxNestingDepth] package variables bound allocation and recursion. Prefer
// [DecodePacketErr] over [DecodePacket], which reports failure only by returning
// nil.
package ber
