## Unreleased

### Improvements

- Add `WeightedCache` (and `simplelru.WeightedLRU`), an LRU variant bounded
  by the sum of caller-provided, non-negative entry weights instead of an
  entry count. Entries heavier than the budget are rejected without mutating
  the cache; otherwise oldest entries are evicted until the budget fits.
  Eviction callbacks run outside the cache lock, in eviction order.

### Changes

### Fixed

### Security
