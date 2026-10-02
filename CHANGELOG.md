## Unreleased

### Improvements

- Added `WeightedCache` (and `simplelru.WeightedLRU`), an LRU variant bounded
  by a caller-provided total weight budget instead of by entry count. Each
  `Add` takes a non-negative weight; oversized single entries and over-budget
  replacements are rejected while preserving the old value and order, and
  ordinary over-budget adds evict entries oldest-first. Eviction callbacks
  fire outside the cache lock in actual eviction order and may re-enter the
  cache. The existing count-based `Cache` is unchanged.

### Changes

### Fixed

### Security
