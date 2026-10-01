package imap

// SortKey is a criterion used to sort messages.
type SortKey string

const (
	SortKeyArrival SortKey = "ARRIVAL"
	SortKeyCc      SortKey = "CC"
	SortKeyDate    SortKey = "DATE"
	SortKeyFrom    SortKey = "FROM"
	SortKeySize    SortKey = "SIZE"
	SortKeySubject SortKey = "SUBJECT"
	SortKeyTo      SortKey = "TO"
)

// SortCriterion is a sort key, optionally reversed.
type SortCriterion struct {
	Key     SortKey
	Reverse bool
}

// SortOptions contains options for the SORT command.
type SortOptions struct {
	SearchCriteria *SearchCriteria
	SortCriteria   []SortCriterion
}

// SortData is the data returned by a SORT command.
type SortData struct {
	// Nums contains sequence numbers or UIDs of the matched messages, in
	// sorted order.
	Nums []uint32
}
