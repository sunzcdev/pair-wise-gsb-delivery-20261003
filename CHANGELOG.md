# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added

- `NewIntegerErr`, `NewRealErr`, `NewOIDErr`, and `NewRelativeOIDErr`:
  error-returning variants of the existing constructors that report invalid
  input instead of panicking or returning `nil`.
- `NewBigInteger` / `NewBigIntegerErr`: construct an `INTEGER` from a
  `*math/big.Int`, encoded as the minimal two's complement form (negative
  values in two's complement, a leading `0x00` sign octet when the top bit is
  set, zero as a single `0x00`). The constructor copies its input, so the
  packet never shares mutable storage with the caller's `big.Int`.
- `DecodePacketErrWithOptions` and `ReadPacketWithOptions`, with the
  `WithIntegerMode` (`IntegerModeInt64` / `IntegerModeBig` / `IntegerModeAuto`)
  and `WithMaxIntegerBytes` decode options. Big mode yields `*big.Int` for
  every integer; auto mode yields `int64` when the value fits and `*big.Int`
  otherwise. The integer-byte budget (default
  `DefaultMaxIntegerBytes`, 4096 octets) bounds redundant sign-extension
  octets, including in the default int64-compatible mode. Default decoding is
  unchanged.

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
