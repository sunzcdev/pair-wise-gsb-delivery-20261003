package experimental

import "github.com/tetratelabs/wazero/api"

// MemoryGeneration is an opaque, monotonically increasing value that identifies
// a particular capacity "generation" of a linear memory.
//
// Every successful, non-zero Grow changes the generation and invalidates any
// borrowed views obtained beforehand (see BorrowedMemory.ReadBorrow). The zero
// value is a valid generation for a memory that has never been grown.
//
// Note: The generation only tracks capacity changes (Grow). It does not track
// ordinary writes to existing bytes, so it cannot be used to detect data
// mutation inside the current generation.
type MemoryGeneration uint64

// CopyingMemory is an api.Memory that can hand out owned (copied) bytes in
// addition to the borrowed views returned by api.Memory.Read.
//
// A module's api.Memory implements this interface when wazero supports the
// functionality; obtain it with a type assertion:
//
//	copier, ok := mod.Memory().(experimental.CopyingMemory)
//
// # Shared memory and consistent snapshots
//
// ReadCopy returns a stable byte copy at the instant it is read, but it is not
// an atomic or transactional snapshot. If another Wasm thread (or the guest)
// mutates or grows shared memory concurrently, individual bytes may be torn
// relative to one another. wazero does not pretend that a plain copy provides
// cross-word consistency. Callers that need a consistent snapshot must arrange
// their own synchronization with the producer (for example by only reading
// while the guest is parked, or via a separate caller/guest protocol), or use
// BorrowedMemory together with Grow coordination.
type CopyingMemory interface {
	api.Memory

	// ReadCopy reads byteCount bytes starting at offset into a newly allocated,
	// owned slice and returns it. The returned bytes never alias the live
	// linear memory, so later Wasm writes or Grow do not affect them.
	//
	// As with api.Memory.Read, a zero byteCount read succeeds for any offset up
	// to and including the current memory size, and returns an empty (non-nil)
	// slice. ok is false when offset+byteCount overflows uint32 or exceeds the
	// current memory size.
	ReadCopy(offset, byteCount uint32) (copied []byte, ok bool)
}

// BorrowedMemory is an api.Memory that returns borrowed views together with the
// memory's current MemoryGeneration, allowing callers to detect whether a Grow
// has invalidated a view they are holding.
//
// A module's api.Memory implements this interface when wazero supports the
// functionality; obtain it with a type assertion:
//
//	borrower, ok := mod.Memory().(experimental.BorrowedMemory)
//
// # Borrowing contract
//
// ReadBorrow returns a write-through view exactly like api.Memory.Read; it does
// not copy. The returned gen allows the caller to later check, via GrownSince,
// whether the view has outlived a Grow. When GrownSince returns true the view
// may no longer cover live memory and must be re-read before use.
//
// The generation detects capacity changes (Grow) only. Writes to bytes within
// the current generation are always visible through the view and are not
// signalled. For shared memory, concurrent writes remain governed by the
// shared-memory synchronization contract between caller and guest; generation
// tracking does not make a borrowed view a consistent snapshot.
type BorrowedMemory interface {
	api.Memory

	// ReadBorrow reads byteCount bytes starting at offset without copying them,
	// returning a write-through view and the memory's current generation.
	//
	// The view has the same write-through semantics as api.Memory.Read. Retain
	// gen and pass it to GrownSince before trusting the view after any point
	// where the guest could have grown memory.
	//
	// As with api.Memory.Read, a zero byteCount read succeeds for any offset up
	// to and including the current memory size, and returns an empty (non-nil)
	// view. ok is false when offset+byteCount overflows uint32 or exceeds the
	// current memory size. On failure, the returned gen is the current
	// generation.
	ReadBorrow(offset, byteCount uint32) (view []byte, gen MemoryGeneration, ok bool)

	// GrownSince reports whether memory has had a successful, non-zero Grow
	// since gen was returned by ReadBorrow. When true, any view associated with
	// gen must be refreshed before use.
	GrownSince(gen MemoryGeneration) bool
}
