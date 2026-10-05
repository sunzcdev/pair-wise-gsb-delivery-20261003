package httphead

import "fmt"

// Option-level states, mirror the states used by ScanOptions.
const (
	osKey = iota
	osParamBeforeName
	osParamName
	osParamBeforeValue
	osParamValue
)

// Lexer modes that may be interrupted at the end of a chunk.
const (
	// omNone means that the next byte starts a new lexeme.
	omNone = iota
	// omToken means that a token continues; pending holds its already seen
	// bytes.
	omToken
	// omString means that a quoted-string continues; pending holds the raw
	// bytes after the opening quote, and escaped reports whether the last
	// pending byte is a dangling backslash.
	omString
	// omComment means that a comment continues; pending holds the raw bytes
	// after the opening "(" seen so far.
	omComment
	// omCR means that a lone CR was seen and the next byte must be LF.
	omCR
	// omCRLF means that CRLF was seen and the next byte must be SP.
	omCRLF
	// omSkip mirrors SkipEscaped(',') after ControlSkip; escaped reports
	// whether the last byte was a backslash.
	omSkip
)

// SyntaxError reports that the scanned header value is malformed.
//
// The Offset field is the absolute byte offset (counting from the first byte
// ever passed to the scanner) at which the malformed lexeme ends. All bytes
// before Offset were successfully consumed. An Offset of -1 reports that no
// option was present in the header value.
type SyntaxError struct {
	Offset int
}

// Error implements error.
func (e *SyntaxError) Error() string {
	return fmt.Sprintf("httphead: malformed header value at offset %d", e.Offset)
}

// LimitError reports that one of the configured size limits was exceeded.
//
// The Offset field is the absolute byte offset (counting from the first byte
// ever passed to the scanner) of the first byte that made the scanned value
// exceed the limit. All bytes before Offset were successfully consumed.
type LimitError struct {
	Offset int

	// Input is true when the total amount of fed data exceeded MaxInputSize.
	// Otherwise it reports that a single option exceeded MaxOptionSize.
	Input bool
}

// Error implements error.
func (e *LimitError) Error() string {
	if e.Input {
		return fmt.Sprintf("httphead: total input size limit exceeded at offset %d", e.Offset)
	}
	return fmt.Sprintf("httphead: option size limit exceeded at offset %d", e.Offset)
}

// ErrStopped is returned when Write is called after the callback requested
// ControlBreak. Call Reset before feeding new data.
var ErrStopped = fmt.Errorf("httphead: option scanner already stopped by callback")

// OptionEventFunc is the callback invoked by an OptionScanner for every
// parsed option and its parameters.
//
// It receives the same arguments as the callback of ScanOptions: the option
// index, the option name, an optional parameter name and an optional
// parameter value.
//
// The option, attribute and value slices are borrowed from the scanner and
// are only valid until the scanner returns from its current Write or Finish
// call. If the caller needs to retain the data longer it must copy it (see
// Option.Copy). When a token or a quoted string is split across multiple fed
// chunks the scanner assembles it in its own bounded buffer; such assembled
// slices have exactly the same lifetime.
//
// The callback must return one of ControlContinue, ControlSkip or
// ControlBreak, with the same meaning as in ScanOptions.
type OptionEventFunc func(index int, option, attribute, value []byte) Control

// OptionScanner incrementally parses header values of the form:
//
//	values = 1#value
//	value = token *( ";" param )
//	param = token [ "=" (token | quoted-string) ]
//
// Unlike ScanOptions it does not require the whole header value to be
// available at once. The caller feeds successive chunks with Write; tokens,
// quoted strings (with backslash escaping) and separators may be split
// arbitrarily between chunks. Structures that are not closed yet (an open
// quote, an open comment, a dangling backslash or a dangling CR of an LWS
// sequence) are carried across Write calls instead of being reported as
// malformed. Only Finish, which marks the end of the header value, reports an
// unterminated structure.
//
// The scanner never accumulates the whole input. Its internal storage is
// bounded by the configured maxOption: only the bytes of the option that is
// currently being parsed may be retained between calls.
type OptionScanner struct {
	fn        OptionEventFunc
	maxInput  int
	maxOption int

	// total is the number of bytes accepted from all Write calls so far.
	total int
	// consumed is the number of those bytes already processed by the state
	// machine. Bytes in [consumed:total] belong to a structure that may
	// continue in the next chunk (pending).
	consumed int

	state    int
	index    int
	key      []byte
	param    []byte
	value    []byte
	mustCall bool
	seen     bool

	// keyChunk, paramChunk and valueChunk report whether the corresponding
	// slice aliases the bytes of the most recently fed chunk.
	keyChunk   bool
	paramChunk bool
	valueChunk bool

	// arena holds scanner-owned copies of key, param and value bytes that
	// must outlive the current chunk. It is reset whenever a new option name
	// starts, so it never grows beyond a single option.
	arena []byte

	mode int

	// pending carries the bytes of the interrupted lexeme.
	pending []byte
	// escaped carries the dangling-backslash state for omString and omSkip.
	escaped bool
	// pendOffset is the absolute offset of pending[0].
	pendOffset int

	// optionStart is the absolute offset of the first byte of the option
	// currently being parsed, or -1 when no option has begun.
	optionStart int

	// lexical reports that the last error was a low-level lexer error
	// (illegal octet, unterminated string or comment), mirroring lexer.err.
	// In that case ScanOptions still flushes the pending key at the end.
	lexical bool

	err     error
	stopped bool
}

// NewOptionScanner creates an incremental option scanner.
//
// maxInput limits the total number of bytes accepted by all Write calls;
// maxOption limits the size of a single option (its name and parameters).
// A non-positive value disables the corresponding limit.
//
// The fn callback must not be nil; see OptionEventFunc for the lifetime of
// the slices passed to it.
func NewOptionScanner(maxInput, maxOption int, fn OptionEventFunc) *OptionScanner {
	if fn == nil {
		panic("httphead: nil option scanner callback")
	}
	return &OptionScanner{
		fn:          fn,
		maxInput:    maxInput,
		maxOption:   maxOption,
		state:       osKey,
		mode:        omNone,
		optionStart: -1,
	}
}

// Consumed returns the number of bytes already fully processed across all
// Write calls.
//
// When Write stops early (the callback returned ControlBreak, a malformed
// sequence was found or a limit was reached) the difference between the
// total number of fed bytes and Consumed is the unconsumed suffix of the
// last chunk. The caller can resume other parsing from that position.
func (s *OptionScanner) Consumed() int {
	return s.consumed
}

// Stopped reports whether scanning was terminated before Finish by the
// callback returning ControlBreak.
func (s *OptionScanner) Stopped() bool {
	return s.stopped
}

// Err returns the error that terminated scanning, if any.
func (s *OptionScanner) Err() error {
	return s.err
}

// Reset restores the scanner to its initial state, keeping the configured
// limits and callback.
func (s *OptionScanner) Reset() {
	fn, maxInput, maxOption := s.fn, s.maxInput, s.maxOption
	*s = OptionScanner{
		fn:          fn,
		maxInput:    maxInput,
		maxOption:   maxOption,
		state:       osKey,
		mode:        omNone,
		optionStart: -1,
	}
}

// Write feeds the next chunk of header value bytes. It returns the number of
// bytes of p that were fully consumed and any error encountered.
//
// A return value n < len(p) means that parsing stopped before the end of p:
// the callback returned ControlBreak, a malformed sequence was found or a
// size limit was reached. Use Consumed to obtain the absolute unconsumed
// position. The scanner must not be used after an error or ControlBreak;
// call Reset to reuse it.
//
// p may be reused or overwritten by the caller as soon as Write returns,
// even when the scanner retains data from it: any bytes needed across calls
// are copied into the scanner's internal buffer.
func (s *OptionScanner) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if s.stopped {
		return 0, ErrStopped
	}

	start := s.total
	data := p
	if s.maxInput > 0 && s.total+len(p) > s.maxInput {
		data = p[:s.maxInput-s.total]
	}

	n, err := s.feed(data, false)
	if err != nil {
		if s.lexical && s.mustCall {
			s.seen = true
			s.fn(s.index, s.key, s.param, s.value)
			s.mustCall = false
		}
		s.bake()
		s.err = err
		return n, err
	}

	s.bake()

	if len(data) < len(p) {
		err = &LimitError{Offset: start + n, Input: true}
		s.err = err
		return n, err
	}

	return n, nil
}

// Finish marks the end of the header value. It flushes any buffered state and
// returns nil when the value was wellformed, or an error otherwise.
func (s *OptionScanner) Finish() error {
	if s.err != nil {
		return s.err
	}
	if s.stopped {
		return nil
	}

	// Reenter the pending lexeme against an empty tail so that unterminated
	// structures are reported here instead of at a chunk boundary.
	var flushErr error
	if s.mode != omNone {
		if _, err := s.feed(nil, true); err != nil {
			flushErr = err
		}
	}

	if (flushErr == nil || s.lexical) && s.mustCall {
		s.seen = true
		s.fn(s.index, s.key, s.param, s.value)
		s.mustCall = false
	}
	s.bake()

	if flushErr != nil {
		s.err = flushErr
		return flushErr
	}

	if !s.seen {
		s.err = &SyntaxError{Offset: -1}
		return s.err
	}

	return nil
}

// bake copies slices that still alias the caller's last chunk into the
// scanner-owned arena, so the caller is free to reuse the chunk after Write
// returns.
func (s *OptionScanner) bake() {
	if s.keyChunk {
		s.key = s.own(s.key)
		s.keyChunk = false
	}
	if s.paramChunk {
		s.param = s.own(s.param)
		s.paramChunk = false
	}
	if s.valueChunk {
		s.value = s.own(s.value)
		s.valueChunk = false
	}
}

// own appends b to the scanner-owned arena and returns the copy.
func (s *OptionScanner) own(b []byte) []byte {
	at := len(s.arena)
	s.arena = append(s.arena, b...)
	return s.arena[at:]
}
