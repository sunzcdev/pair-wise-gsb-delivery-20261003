# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added

- `NewIntegerErr`, `NewRealErr`, `NewOIDErr`, and `NewRelativeOIDErr`:
  error-returning variants of the existing constructors that report invalid
  input instead of panicking or returning `nil`.
- Instance-level primitive codec registry: `Registry`, `NewRegistry`,
  `Registry.Register`/`MustRegister`, `NewDecoder`, and `Decoder` decode
  methods (`ReadPacket`, `DecodePacket`, `DecodePacketErr`) select a custom
  codec by the (class, tag type, tag) identifier triple via `CodecKey` and
  `PrimitiveCodec` (with the `FuncCodec`/`DecodeFunc`/`EncodeFunc` helpers).
  `Decoder.NewPrimitive` encodes a custom value back through its codec.
  Registration conflicts and invalid keys are reported at registration time,
  overriding a built-in universal type requires `WithUniversalOverrides`,
  codec decode errors carry the packet path and the original error chain, and
  decode failures never return a half-filled packet. Primitives without a
  matching codec now always retain their raw content in `ByteValue`.

### Changed

- Require Go 1.22 or newer.
- Constructed packets no longer populate the exported `Data` field after
  decoding or `AppendChild`. Content is now serialized lazily by `Bytes()`;
  read a constructed packet's content via `Bytes()` or by walking `Children`.
  This removes an O(depth x subtree) memory amplification.
- `NewOID` and `NewRelativeOID` now return `nil` instead of panicking on a
  structurally invalid OID string (matching how they already handled other
  invalid strings), and are deprecated in favor of the `*Err` variants.

### Deprecated

- `NewOID` and `NewRelativeOID` — use `NewOIDErr` / `NewRelativeOIDErr`.

### Fixed

- OID and Relative-OID parse errors were silently swallowed, so
  `DecodePacketErr` reported success on a malformed OID; they now propagate.
- Over-long `INTEGER`/`BOOLEAN`/`ENUMERATED` values (more than 8 bytes)
  silently decoded to `0`; they are now rejected.
- `IA5String` incorrectly rejected the valid `0x7F` (DEL) character.
- An 8-byte long-form length with the high bit set decoded to a negative
  length and was mistaken for indefinite length; it is now rejected as
  overflow.
- `DecodePacket` returned a partially-populated packet on a decode error
  despite documenting that it returns `nil`.
- Memory amplification in `AppendChild`, which retained a serialized copy of
  every subtree at each level.
