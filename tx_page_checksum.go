package bbolt

import (
	"fmt"

	"go.etcd.io/bbolt/internal/common"
)

// verifyReachablePageChecksums validates the checksum trailer of every
// reachable on-disk block. It only inspects the first page of each block (the
// one carrying the page header); overflow continuation pages are covered by
// that block's checksum. Blocks present in the transaction's dirty cache are
// skipped because they are private, unsealed copies.
func (tx *Tx) verifyReachablePageChecksums(reachable map[common.Pgid]*common.Page, ch chan error) {
	seen := make(map[common.Pgid]struct{}, len(reachable))
	for id, p := range reachable {
		if id < 2 || p == nil {
			continue
		}
		if _, dirty := tx.pages[id]; dirty {
			continue
		}
		// Only verify the block head. Overflow continuations share the same
		// Page pointer in the reachable map and are covered transitively.
		if id != p.Id() {
			continue
		}
		if _, ok := seen[p.Id()]; ok {
			continue
		}
		seen[p.Id()] = struct{}{}

		if !p.IsLeafPage() && !p.IsBranchPage() && !p.IsFreelistPage() {
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					if recovered, ok := r.(error); ok {
						ch <- fmt.Errorf("page %d: %w", id, recovered)
					} else {
						ch <- fmt.Errorf("page %d: %v", id, r)
					}
				}
			}()
			if err := common.VerifyPageChecksum(p, tx.db.pageSize); err != nil {
				ch <- err
			}
		}()
	}
}

// VerifyDirtyPageChecksums seals and verifies the checksums of all dirty
// blocks constructed by this write transaction but not committed yet.
//
// It is meant for testing and diagnostics. After it returns successfully the
// dirty blocks carry valid trailers, which is exactly the state the write path
// produces right before writing to disk. It does not write anything and, on a
// rolled-back transaction, the sealed buffers are simply discarded.
//
// On a read-only transaction, or on a database without page checksumming, it
// is a no-op.
func (tx *Tx) VerifyDirtyPageChecksums() error {
	if !tx.writable || !tx.db.pageChecksum {
		return nil
	}
	for _, p := range tx.pages {
		common.SealPageChecksum(p, tx.db.pageSize)
		if err := common.VerifyPageChecksum(p, tx.db.pageSize); err != nil {
			return err
		}
	}
	return nil
}
