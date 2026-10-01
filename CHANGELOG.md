## Unreleased

### Improvements

* Added `MaxSessionWindowSize`, an optional session level bound on total
  committed receive memory. It counts buffered data plus receive credit
  advertised to the peer that has not yet been consumed by reads. The limit
  is enforced when opening streams and growing windows rather than by
  discarding valid data; credit is returned on read, FIN, close, and reset.
  A zero value (the default) keeps the per-stream-only behavior.

### Changes

### Fixed

### Security
