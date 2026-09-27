## Unreleased

### Improvements

* Add `WatchRange`, a range query over an index with an inclusive lower and
  exclusive upper bound whose returned `RangeIterator` can be watched for
  committed mutations to any key within the range. Subscriptions can be
  released with `RangeIterator.Stop`.

### Changes

### Fixed

### Security
