## Unreleased

### Improvements

- Add schema-declared inter-table references (foreign keys) checked at
  commit time. References support `restrict` (default) and `cascade` delete
  actions, including transitive chains and cycles with per-row de-duplication.
  `Txn.Commit` now returns an error wrapping `ErrReferenceViolation`; failed
  commits publish no index changes and fire no watch notifications or deferred
  functions. Tables without references are unchanged.

### Changes

### Fixed

### Security
