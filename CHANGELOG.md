## Unreleased

### Improvements

* `TransmitLimitedQueue` supports a small, fixed set of priority classes (see `PriorityBroadcast`) so time-sensitive membership changes are gossiped ahead of ordinary traffic, with a bounded-bypass anti-starvation policy (`StarvationLimit`) that guarantees lower-priority broadcasts still make progress under sustained churn.

### Changes

### Fixed

### Security
