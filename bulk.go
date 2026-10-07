// Copyright 2020 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package btree

import "errors"

// DuplicatePolicy determines how BulkLoad handles two equal keys encountered
// while streaming the input.
type DuplicatePolicy int

const (
	// RejectDuplicates aborts the bulk load when a duplicate key is found.
	RejectDuplicates DuplicatePolicy = iota
	// KeepFirst keeps the first occurrence of a key and ignores later ones.
	KeepFirst
	// KeepLast keeps the last occurrence of a key, replacing earlier values.
	KeepLast
)

// BulkOptions are options for the BulkLoad methods.
type BulkOptions struct {
	// Duplicates selects the policy applied to equal, adjacent keys.
	// Default is RejectDuplicates.
	Duplicates DuplicatePolicy
}

// BulkStats summarizes a completed (or attempted) bulk load.
type BulkStats struct {
	// Loaded is the number of distinct keys present in the new tree.
	Loaded int
	// Duplicates is the number of repeated key occurrences observed.
	Duplicates int
	// Comparisons is the number of invocations of the key comparator.
	// Ordered, distinct input costs exactly one comparison per item after
	// the first.
	Comparisons int64
	// Nodes is the number of tree nodes allocated for the new tree.
	Nodes int64
	// ItemSlots is the number of item slots allocated across all nodes.
	ItemSlots int64
	// PeakBytes is the peak amount of temporary memory owned by the bulk
	// loader, in bytes. Nothing from the old tree is counted here, and the
	// temporary memory is released when BulkLoad returns.
	PeakBytes uintptr
}

// ErrOutOfOrder is returned when bulk load input is not strictly ascending.
var ErrOutOfOrder = errors.New("btree: bulk load input is out of order")

// ErrDuplicateKey is returned when a duplicate key is found with the
// RejectDuplicates policy.
var ErrDuplicateKey = errors.New("btree: duplicate key in bulk load input")

// ErrNilIterator is returned when the iterator argument is nil.
var ErrNilIterator = errors.New("btree: nil bulk load iterator")
