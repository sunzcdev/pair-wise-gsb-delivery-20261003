## Unreleased

### Improvements

- `TwoQueueCache` now supports a configurable promotion threshold via
  `New2QWithConfig` and `TwoQueueConfig.PromotionHits`: a recent entry is
  only promoted to frequent after the configured number of hits.
- `TwoQueueCache` can adapt its target recent/frequent ratio based on
  ghost-queue hits, with configurable lower/upper bounds and a fixed
  adjustment step (`AdaptRatio`, `RecentRatioMin`, `RecentRatioMax`,
  `RecentRatioStep`).
- Eviction to the ghost queue, explicit removal, and `Purge` now clear
  the evicted entry's recent-queue hit counter.

### Changes

### Fixed

### Security
