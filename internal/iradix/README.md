# internal/iradix

This package is a fork of [github.com/hashicorp/go-immutable-radix](https://github.com/hashicorp/go-immutable-radix)
v1.3.1 (MPL-2.0, see LICENSE) that adds order-statistics support:

- Every node tracks the number of leaves in its subtree (`Node.size`),
  maintained incrementally on insert, delete and prefix-delete.
- `Rank(key)` returns the number of keys sorting strictly before `key`.
- `Select(rank)` returns the key/value at a 0-based rank in sorted order.

Both operations run in time proportional to the depth of the tree rather
than the rank itself. Because the tree remains immutable, ranks observed
through an old root never drift as new transactions commit.

The fork also fixes an upstream bug where `Txn.DeletePrefix` miscounted
deletions (corrupting the tree size) when the target node had already been
modified within the same transaction.
