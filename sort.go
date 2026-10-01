package imap

// SortKey is a sort key for the SORT command.
//
// See RFC 5256 section 2.1.
type SortKey string

const (
	// SortKeyArrival sorts messages by their internal date and time.
	SortKeyArrival SortKey = "ARRIVAL"
	// SortKeyCc sorts messages by the first CC address.
	SortKeyCc SortKey = "CC"
	// SortKeyDate sorts messages by the Date header field.
	SortKeyDate SortKey = "DATE"
	// SortKeyFrom sorts messages by the first From address.
	SortKeyFrom SortKey = "FROM"
	// SortKeySize sorts messages by their RFC 822 size.
	SortKeySize SortKey = "SIZE"
	// SortKeySubject sorts messages by the message subject, stripped of
	// leading "Re:" and "Fwd:" prefixes.
	SortKeySubject SortKey = "SUBJECT"
	// SortKeyTo sorts messages by the first To address.
	SortKeyTo SortKey = "TO"
)

// SortCriterion is a single sort criterion for the SORT command.
//
// When Reverse is true, the order for this criterion is reversed.
type SortCriterion struct {
	Key     SortKey
	Reverse bool
}

// SortData is the data returned by a SORT command.
//
// The numbers are sequence numbers for SORT, and UIDs for UID SORT.
type SortData struct {
	// All contains the sorted message numbers.
	All []uint32
}

// AllSeqNums returns All as a slice of sequence numbers.
func (data *SortData) AllSeqNums() []uint32 { return data.All }

// AllUIDs returns All as a slice of UIDs.
func (data *SortData) AllUIDs() []UID { return uidListFromNumList(data.All) }
