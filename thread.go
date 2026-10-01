package imap

// ThreadAlgorithm is a threading algorithm.
type ThreadAlgorithm string

const (
	ThreadOrderedSubject ThreadAlgorithm = "ORDEREDSUBJECT"
	ThreadReferences     ThreadAlgorithm = "REFERENCES"
)

// ThreadData is a message thread tree returned by the THREAD command.
//
// Chain is a list of message numbers where each message is a direct child of
// the previous one. SubThreads contains the children of the last message in
// Chain.
type ThreadData struct {
	Chain      []uint32
	SubThreads []ThreadData
}
