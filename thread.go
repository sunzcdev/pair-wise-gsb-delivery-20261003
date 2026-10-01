package imap

// ThreadAlgorithm is a threading algorithm.
type ThreadAlgorithm string

const (
	ThreadOrderedSubject ThreadAlgorithm = "ORDEREDSUBJECT"
	ThreadReferences     ThreadAlgorithm = "REFERENCES"
)

// ThreadData is a node in a message thread, as returned by the THREAD
// command.
//
// Chain holds a linear sequence of messages in which each message is the
// only child of the previous one. SubThreads holds the children of the last
// message in Chain when it has more than one child.
type ThreadData struct {
	Chain      []uint32
	SubThreads []ThreadData
}
