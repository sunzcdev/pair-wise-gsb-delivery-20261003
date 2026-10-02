// Package httphead contains utils for parsing HTTP and HTTP-grammar compatible
// text protocols headers.
//
// That is, this package first aim is to bring ability to easily parse
// constructions, described here https://tools.ietf.org/html/rfc2616#section-2
package httphead

import (
	"bytes"
	"strconv"
	"strings"
)

// ScanTokens parses data in this form:
//
// list = 1#token
//
// It returns false if data is malformed.
func ScanTokens(data []byte, it func([]byte) bool) bool {
	lexer := &Scanner{data: data}

	var ok bool
	for lexer.Next() {
		switch lexer.Type() {
		case ItemToken:
			ok = true
			if !it(lexer.Bytes()) {
				return true
			}
		case ItemSeparator:
			if !isComma(lexer.Bytes()) {
				return false
			}
		default:
			return false
		}
	}

	return ok && !lexer.err
}

// ParseOptions parses all header options and appends it to given slice of
// Option. It returns flag of successful (wellformed input) parsing.
//
// Note that appended options are all consist of subslices of data. That is,
// mutation of data will mutate appended options.
//
// For error diagnostics use ParseOptionsDetailed.
func ParseOptions(data []byte, options []Option) ([]Option, bool) {
	var i int
	index := -1
	return options, ScanOptions(data, func(idx int, name, attr, val []byte) Control {
		if idx != index {
			index = idx
			i = len(options)
			options = append(options, Option{Name: name})
		}
		if attr != nil {
			options[i].Parameters.Set(attr, val)
		}
		return ControlContinue
	})
}

// ParseStatus reports the way options parsing ended.
type ParseStatus int

const (
	// ParseComplete reports that all options up to the end of data were
	// parsed successfully.
	ParseComplete ParseStatus = iota
	// ParseStopped reports that parsing was stopped early by the callback
	// returning ControlBreak. Input up to the stopping point was valid.
	ParseStopped
	// ParseMalformed reports that input was corrupted. The accompanying
	// OptionError describes the error.
	ParseMalformed
)

// OptionErrorKind encodes the category of an options parsing error.
type OptionErrorKind int

const (
	// OptionErrorBadToken reports an unexpected or otherwise invalid
	// syntactic element. For example, a bad token right after a comma.
	OptionErrorBadToken OptionErrorKind = iota + 1
	// OptionErrorInvalidByte reports a byte that does not belong to any
	// RFC2616 token or separator.
	OptionErrorInvalidByte
	// OptionErrorMissingValue reports a parameter name followed by "=" with
	// no value.
	OptionErrorMissingValue
	// OptionErrorUnterminatedString reports a quoted-string without a
	// closing quote.
	OptionErrorUnterminatedString
	// OptionErrorUnterminatedComment reports a comment with an unbalanced
	// opening parenthesis.
	OptionErrorUnterminatedComment
)

func (k OptionErrorKind) String() string {
	switch k {
	case OptionErrorBadToken:
		return "bad token"
	case OptionErrorInvalidByte:
		return "invalid byte"
	case OptionErrorMissingValue:
		return "missing parameter value"
	case OptionErrorUnterminatedString:
		return "unterminated quoted-string"
	case OptionErrorUnterminatedComment:
		return "unterminated comment"
	default:
		return "unknown error"
	}
}

// OptionError describes a malformed options input.
type OptionError struct {
	// Kind is the category of the error.
	Kind OptionErrorKind
	// Offset is the byte offset of the offending construct in the original
	// raw input. It is never derived from the length of a de-quoted token
	// value.
	Offset int
	// Option is the zero-based ordinal of the option being parsed when the
	// error occurred.
	Option int
	// Param is the zero-based ordinal of the parameter being parsed when the
	// error occurred within the current option.
	Param int
}

func (e OptionError) Error() string {
	return "httphead: " + e.Kind.String() +
		" at byte offset " + strconv.Itoa(e.Offset) +
		" (option " + strconv.Itoa(e.Option) +
		", parameter " + strconv.Itoa(e.Param) + ")"
}

// ParseOptionsDetailed is like ParseOptions, but reports how parsing ended
// and, when status is ParseMalformed, a structured OptionError.
//
// Note that appended options are all consist of subslices of data. That is,
// mutation of data will mutate appended options.
//
// Parsing is streaming: no syntax tree is allocated beyond the options slice
// requested by the caller.
func ParseOptionsDetailed(data []byte, options []Option) ([]Option, ParseStatus, OptionError) {
	var i int
	index := -1
	status, derr := ScanOptionsDetailed(data, func(idx int, name, attr, val []byte) Control {
		if idx != index {
			index = idx
			i = len(options)
			options = append(options, Option{Name: name})
		}
		if attr != nil {
			options[i].Parameters.Set(attr, val)
		}
		return ControlContinue
	})
	return options, status, derr
}

// SelectFlag encodes way of options selection.
type SelectFlag byte

// String represetns flag as string.
func (f SelectFlag) String() string {
	var flags [2]string
	var n int
	if f&SelectCopy != 0 {
		flags[n] = "copy"
		n++
	}
	if f&SelectUnique != 0 {
		flags[n] = "unique"
		n++
	}
	return "[" + strings.Join(flags[:n], "|") + "]"
}

const (
	// SelectCopy causes selector to copy selected option before appending it
	// to resulting slice.
	// If SelectCopy flag is not passed to selector, then appended options will
	// contain sub-slices of the initial data.
	SelectCopy SelectFlag = 1 << iota

	// SelectUnique causes selector to append only not yet existing option to
	// resulting slice. Unique is checked by comparing option names.
	SelectUnique
)

// OptionSelector contains configuration for selecting Options from header value.
type OptionSelector struct {
	// Check is a filter function that applied to every Option that possibly
	// could be selected.
	// If Check is nil all options will be selected.
	Check func(Option) bool

	// Flags contains flags for options selection.
	Flags SelectFlag

	// Alloc used to allocate slice of bytes when selector is configured with
	// SelectCopy flag. It will be called with number of bytes needed for copy
	// of single Option.
	// If Alloc is nil make is used.
	Alloc func(n int) []byte
}

// Select parses header data and appends it to given slice of Option.
// It also returns flag of successful (wellformed input) parsing.
func (s OptionSelector) Select(data []byte, options []Option) ([]Option, bool) {
	var current Option
	var has bool
	index := -1

	alloc := s.Alloc
	if alloc == nil {
		alloc = defaultAlloc
	}
	check := s.Check
	if check == nil {
		check = defaultCheck
	}

	ok := ScanOptions(data, func(idx int, name, attr, val []byte) Control {
		if idx != index {
			if has && check(current) {
				if s.Flags&SelectCopy != 0 {
					current = current.Copy(alloc(current.Size()))
				}
				options = append(options, current)
				has = false
			}
			if s.Flags&SelectUnique != 0 {
				for i := len(options) - 1; i >= 0; i-- {
					if bytes.Equal(options[i].Name, name) {
						return ControlSkip
					}
				}
			}
			index = idx
			current = Option{Name: name}
			has = true
		}
		if attr != nil {
			current.Parameters.Set(attr, val)
		}

		return ControlContinue
	})
	if has && check(current) {
		if s.Flags&SelectCopy != 0 {
			current = current.Copy(alloc(current.Size()))
		}
		options = append(options, current)
	}

	return options, ok
}

func defaultAlloc(n int) []byte { return make([]byte, n) }
func defaultCheck(Option) bool  { return true }

// Control represents operation that scanner should perform.
type Control byte

const (
	// ControlContinue causes scanner to continue scan tokens.
	ControlContinue Control = iota
	// ControlBreak causes scanner to stop scan tokens.
	ControlBreak
	// ControlSkip causes scanner to skip current entity.
	ControlSkip
)

// ScanOptions parses data in this form:
//
// values = 1#value
// value = token *( ";" param )
// param = token [ "=" (token | quoted-string) ]
//
// It calls given callback with the index of the option, option itself and its
// parameter (attribute and its value, both could be nil). Index is useful when
// header contains multiple choises for the same named option.
//
// Given callback should return one of the defined Control* values.
// ControlSkip means that passed key is not in caller's interest. That is, all
// parameters of that key will be skipped.
// ControlBreak means that no more keys and parameters should be parsed. That
// is, it must break parsing immediately.
// ControlContinue means that caller want to receive next parameter and its
// value or the next key.
//
// It returns false if data is malformed. For structured diagnostics use
// ScanOptionsDetailed.
func ScanOptions(data []byte, it func(index int, option, attribute, value []byte) Control) bool {
	status, _, found := scanOptions(data, it, false)
	return found && status != ParseMalformed
}

// ScanOptionsDetailed is like ScanOptions, but instead of a boolean success
// flag it reports how parsing ended and a structured error when status is
// ParseMalformed.
//
// It does not allocate any syntax tree: the OptionError is built on the
// scanner stack at the point of failure only.
func ScanOptionsDetailed(data []byte, it func(index int, option, attribute, value []byte) Control) (ParseStatus, OptionError) {
	status, err, _ := scanOptions(data, it, true)
	return status, err
}

// found reports whether the callback was called at least once.
func scanOptions(data []byte, it func(index int, option, attribute, value []byte) Control, strict bool) (status ParseStatus, derr OptionError, found bool) {
	lexer := &Scanner{data: data}

	var state int
	const (
		stateKey = iota
		stateParamBeforeName
		stateParamName
		stateParamBeforeValue
		stateParamValue
	)

	var (
		index             int
		paramOrd          int
		valuePos          int
		afterEquals       bool
		key, param, value []byte
		mustCall          bool
	)

	malformed := func(kind OptionErrorKind, offset int) {
		status, derr = ParseMalformed, OptionError{
			Kind:   kind,
			Offset: offset,
			Option: index,
			Param:  paramOrd,
		}
	}

	for lexer.Next() {
		var (
			call      bool
			growIndex int
		)

		t := lexer.Type()
		v := lexer.Bytes()

		switch t {
		case ItemToken:
			switch state {
			case stateKey, stateParamBeforeName:
				key = v
				state = stateParamBeforeName
				mustCall = true
			case stateParamName:
				param = v
				paramOrd++
				state = stateParamBeforeValue
				mustCall = true
			case stateParamValue:
				value = v
				afterEquals = false
				state = stateParamBeforeName
				call = true
			default:
				malformed(OptionErrorBadToken, lexer.itemPos)
				return
			}

		case ItemString:
			if state != stateParamValue {
				malformed(OptionErrorBadToken, lexer.itemPos)
				return
			}
			value = v
			afterEquals = false
			state = stateParamBeforeName
			call = true

		case ItemSeparator:
			switch {
			case isComma(v) && state == stateKey:
				// Nothing to do.

			case isComma(v) && state == stateParamBeforeName && !afterEquals:
				state = stateKey
				// Make call only if we have not called this key yet.
				call = mustCall
				if !call {
					// If we have already called callback with the key
					// that just ended.
					index++
					paramOrd = 0
				} else {
					// Else grow the index after calling callback.
					growIndex = 1
				}

			case isComma(v) && (state == stateParamBeforeValue || afterEquals):
				if afterEquals {
					malformed(OptionErrorMissingValue, lexer.itemPos)
					return
				}
				state = stateKey
				growIndex = 1
				call = true

			case isSemicolon(v) && state == stateParamBeforeName && !afterEquals:
				state = stateParamName

			case isSemicolon(v) && state == stateParamBeforeValue && !afterEquals:
				state = stateParamName
				call = true

			case isSemicolon(v) && afterEquals:
				// Legacy behavior tolerates a value-less parameter that
				// continues with another parameter.
				state = stateParamName
				call = true

			case isEquality(v) && state == stateParamBeforeValue:
				state = stateParamValue
				afterEquals = true
				valuePos = lexer.pos

			default:
				malformed(OptionErrorBadToken, lexer.itemPos)
				return
			}

		default:
			malformed(OptionErrorBadToken, lexer.itemPos)
			return
		}

		if call {
			switch it(index, key, param, value) {
			case ControlBreak:
				// User want to stop to parsing parameters.
				return ParseStopped, OptionError{}, found

			case ControlSkip:
				// User want to skip current param.
				state = stateKey
				afterEquals = false
				growIndex = 0
				lexer.SkipEscaped(',')

			case ControlContinue:
				// User is interested in rest of parameters.
				// Nothing to do.

			default:
				panic("unexpected control value")
			}
			found = true
			param = nil
			value = nil
			mustCall = false
			index += growIndex
			if growIndex != 0 {
				paramOrd = 0
			}
		}
	}

	if kind, offset := lexer.Err(); kind != ScanErrorNone {
		switch kind {
		case ScanErrorUnterminatedString:
			malformed(OptionErrorUnterminatedString, offset)
		case ScanErrorUnterminatedComment:
			malformed(OptionErrorUnterminatedComment, offset)
		default:
			malformed(OptionErrorInvalidByte, offset)
		}
		return
	}

	if strict && afterEquals {
		// A "=" was seen, but no value followed before end of input.
		// Point at the first byte the missing value should have occupied:
		// the byte right after "=".
		malformed(OptionErrorMissingValue, valuePos)
		return
	}

	if mustCall {
		found = true
		it(index, key, param, value)
	}

	return ParseComplete, OptionError{}, found
}

func isComma(b []byte) bool {
	return len(b) == 1 && b[0] == ','
}
func isSemicolon(b []byte) bool {
	return len(b) == 1 && b[0] == ';'
}
func isEquality(b []byte) bool {
	return len(b) == 1 && b[0] == '='
}
