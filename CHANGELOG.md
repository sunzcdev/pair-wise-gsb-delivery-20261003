## Unreleased

### Improvements

* Add `Txn.GetCompound` to intersect multiple index conditions within a single
  transaction, returning each matching object once and exposing a combined
  watch channel. Iteration is driven by the smallest candidate set.

### Changes

### Fixed

### Security
