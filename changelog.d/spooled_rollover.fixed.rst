Fixed ``SpooledTemporaryFile`` exposing a partially written disk stream to concurrent
readers during rollover. Rollovers are now serialized behind an explicit handoff
protocol (``SpooledTemporaryFile.RolloverPhase``): concurrent reads and writes wait
until the handoff completes or rolls back, a failed or cancelled handoff discards the
incomplete disk file and keeps the in-memory buffer authoritative, and the original
cursor position is preserved on success
