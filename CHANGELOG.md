## Unreleased

### Improvements

* Add `TableSchema.ObjectCloner`, an optional per-table object copy policy.
  When set, MemDB clones objects at the input boundary (`Insert`) and the
  output boundary (`First`, `Get`, iterators, snapshots), so callers may
  freely modify objects after insert and may modify any object returned by
  a query without affecting the stored data or indexes.

### Changes

### Fixed

### Security
