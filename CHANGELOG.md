## Unreleased

### Improvements

* Add `Txn.Rank` and `Txn.Select` for order-statistics queries on any
  sortable index: find an object's position in the index ordering, or the
  object at a given position, in time proportional to the index depth rather
  than the position itself. Duplicate index values are ordered by primary
  key, giving every object a stable rank, and ranks observed within a read
  transaction never drift as writes commit. The underlying radix tree is now
  vendored as `internal/iradix` with subtree-size tracking.

### Changes

### Fixed

* Fix `DeletePrefix` corrupting the tree size when the target node was
  already modified within the same transaction (in the vendored
  `internal/iradix`).

### Security
