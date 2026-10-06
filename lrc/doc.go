// Package lrc implements a locally repairable coding (LRC) layout on top of
// Reed-Solomon.
//
// # Layout
//
// The object is split into DataShards (= k) data shards. The data shards are
// partitioned into Groups (= l) equally sized groups. Every group has
// LocalParity (= p) local parity shards computed from that group's data shards
// only, and GlobalParity (= r) global parity shards are computed over all k
// data shards (a flat Reed-Solomon (k, r) stripe). Total shard count is
// k + l*p + r.
//
// Shard indices in the flat shard slice are laid out as:
//
//	[ data shards 0..k-1 | local parity groups 0..l-1 | global parity ]
//
// # Repair
//
// A repair plan is produced in two tiers:
//
//   - Local tier: a group that has lost at most p of its (k/l + p) shards is
//     repaired using only shards from that group. Other groups are never read.
//   - Global tier: if the locally unrepairable groups together with missing
//     global parity can be covered by the flat (k, r) stripe (at most r
//     unresolved data shards plus missing global parity shards), those data
//     shards are reconstructed globally, and every missing parity shard is
//     regenerated from the recovered data.
//
// # Fault tolerance (important)
//
// This layout is NOT a maximum-distance-separable (MDS) code. It must not be
// advertised as providing the same guarantee as the flat (k, r) Reed-Solomon
// MDS layout in the parent package, which tolerates ANY r shard losses.
//
// The LRC trades storage for repair bandwidth: a single data shard loss is
// normally repaired from k/l data-equivalents inside its group instead of k,
// but the worst-case tolerance depends on WHICH shards are lost. Losses
// concentrated in one group can be unrecoverable even when the total number of
// lost shards is below r, and conversely some loss patterns with more than r
// lost shards remain recoverable because local parity covers extra failures in
// other groups. Exhaustively enumerated recoverable and unrecoverable
// combinations for a small (k=6, l=3, p=1, r=2) layout are shown in
// layout_test.go (TestSmallLayoutCombinations).
package lrc
