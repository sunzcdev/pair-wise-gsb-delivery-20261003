package imap

// StoreOptions contains options for the STORE command.
type StoreOptions struct {
	UnchangedSince uint64 // requires CONDSTORE
}

// StoreResult is the result of a conditional STORE command.
//
// It requires CONDSTORE and is returned by SessionCondStore.Store.
type StoreResult struct {
	// Modified contains the messages which didn't satisfy the
	// UNCHANGEDSINCE condition. It's a UIDSet for a UID STORE command, a
	// SeqSet otherwise.
	Modified NumSet
	// ModSeq is the new HIGHESTMODSEQ value of the mailbox. It's only
	// populated when at least one message has actually been changed.
	ModSeq uint64
}

// StoreFlagsOp is a flag operation: set, add or delete.
type StoreFlagsOp int

const (
	StoreFlagsSet StoreFlagsOp = iota
	StoreFlagsAdd
	StoreFlagsDel
)

// StoreFlags alters message flags.
type StoreFlags struct {
	Op     StoreFlagsOp
	Silent bool
	Flags  []Flag
}
