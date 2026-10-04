// Package ber implements ASN.1 Basic Encoding Rules (BER), the encoding used by
// protocols such as LDAP (RFC 4511).
//
// A decoded document is represented as a tree of [Packet] values. Decode bytes
// with [DecodePacketErr], or stream from an [io.Reader] with [ReadPacket]; build
// packets to encode with the New* constructors and serialize them with
// [Packet.Bytes].
//
// The decoder is written to accept untrusted input: the [MaxPacketLengthBytes]
// and [MaxNestingDepth] package variables bound allocation and recursion. Prefer
// [DecodePacketErr] over [DecodePacket], which reports failure only by returning
// nil.
//
// # Custom primitive codecs
//
// Beyond the built-in universal types, applications can register primitive
// codecs selected by the (class, tag type, tag) identifier triple. Build a
// [Registry] with [NewRegistry], [Registry.Register] each codec, and decode
// with [NewDecoder]. Primitives with no matching codec retain their raw content
// octets in [Packet.ByteValue] and [Packet.Data] instead of losing them, and
// independent registries allow the same wire tag to map to different Go types
// in different decoders. See [WithUniversalOverrides] for replacing a built-in
// universal codec, and [Decoder.NewPrimitive] for encoding a custom value back
// through its codec.
package ber
