package ics

import (
	"errors"
	"fmt"
)

// MalformedError reports a parse failure with optional line and character context.
type MalformedError struct {
	Err       error
	Line      int
	Character int
	HasLine   bool
	HasChar   bool
}

func (e *MalformedError) Error() string {
	switch {
	case e == nil:
		return "malformed error"
	case e.HasLine && e.HasChar:
		return fmt.Sprintf("line %d character %d: %v", e.Line, e.Character, e.Err)
	case e.HasLine:
		return fmt.Sprintf("line %d: %v", e.Line, e.Err)
	case e.Err != nil:
		return e.Err.Error()
	default:
		return "malformed error"
	}
}

func (e *MalformedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// NewMalformedError wraps err with line and character context when err is non-nil.
func NewMalformedError(line, character int, err error) error {
	if err == nil {
		return nil
	}
	return &MalformedError{
		Err:       err,
		Line:      line,
		Character: character,
		HasLine:   line > 0,
		HasChar:   character > 0,
	}
}

var (
	// ErrUnexpectedParamValueLength reports a truncated parameter value.
	ErrUnexpectedParamValueLength = errors.New("unexpected end of param value")

	// ErrExpectedVCalendar reports a VCALENDAR token appearing where another token was expected.
	ErrExpectedVCalendar = errors.New("expected a vcalendar")
	// ErrExpectedBegin reports a missing BEGIN token.
	ErrExpectedBegin = errors.New("expected begin")
	// ErrExpectedEnd reports a missing END token.
	ErrExpectedEnd = errors.New("expected a end")
	// ErrExpectedBeginOrEnd reports a token where either BEGIN or END was expected.
	ErrExpectedBeginOrEnd = errors.New("expected begin or end")
	// ErrUnexpectedCalendarEnd reports an END token that appears after the calendar has already ended.
	ErrUnexpectedCalendarEnd = errors.New("unexpected end")
	// ErrBadCalendarState reports an invalid parser state.
	ErrBadCalendarState = errors.New("bad state")
	// ErrVCalendarNotWhereExpected reports a nested VCALENDAR token.
	ErrVCalendarNotWhereExpected = errors.New("vcalendar not where expected")

	// ErrMissingPropertyParamOperator reports a property parameter missing its `=` separator.
	ErrMissingPropertyParamOperator = errors.New("missing property param operator")
	// ErrUnexpectedEndOfProperty reports a property line that ends before the value is complete.
	ErrUnexpectedEndOfProperty = errors.New("unexpected end of property")

	// ErrStartOrEndNotYetDefined reports a missing start or end time.
	ErrStartOrEndNotYetDefined = errors.New("start or end not yet defined")
	// ErrPropertyNotFound reports that a requested property was not set.
	ErrPropertyNotFound = errors.New("property not found")
	// ErrExpectedOneTZID reports an invalid number of TZID parameters.
	ErrExpectedOneTZID = errors.New("expected one TZID")

	// ErrTimeValueNotMatched reports a timestamp that does not match the parser's supported formats.
	ErrTimeValueNotMatched = errors.New("time value not matched")
	// ErrTimeValueMatchedButUnsupportedAllDayTimeStamp reports an all-day timestamp shape that is not supported.
	ErrTimeValueMatchedButUnsupportedAllDayTimeStamp = errors.New("time value matched but unsupported all-day timestamp")
	// ErrTimeValueMatchedButNotSupported reports a recognized timestamp shape that still cannot be parsed.
	ErrTimeValueMatchedButNotSupported = errors.New("time value matched but not supported")

	// ErrParsingComponentProperty reports a failure while parsing a component property.
	ErrParsingComponentProperty = errors.New("parsing component property")
	// ErrParsingComponentLine reports a failure while parsing a component line.
	ErrParsingComponentLine = errors.New("parsing component line")
	// ErrParsingLine reports a failure while parsing a calendar content line.
	ErrParsingLine = errors.New("parsing line")
	// ErrParsingCalendarLine reports a failure while parsing a calendar line.
	ErrParsingCalendarLine = errors.New("parsing calendar line")
	// ErrParsingProperty reports a failure while parsing a property.
	ErrParsingProperty = errors.New("parsing property")
	// ErrParse reports a generic parse failure.
	ErrParse = errors.New("parse error")

	// ErrMissingPropertyValue reports a property parameter missing its value.
	ErrMissingPropertyValue = errors.New("missing property value")

	// ErrUnexpectedASCIIChar reports a disallowed ASCII control character.
	ErrUnexpectedASCIIChar = errors.New("unexpected char ascii")
	// ErrUnexpectedDoubleQuoteInPropertyParamValue reports an invalid quote in a parameter value.
	ErrUnexpectedDoubleQuoteInPropertyParamValue = errors.New("unexpected double quote in property param value")

	// ErrUnbalancedEnd reports a closing token that does not match the current component.
	ErrUnbalancedEnd = errors.New("unbalanced end")
	// ErrOutOfLines reports that the parser ran out of content lines.
	ErrOutOfLines = errors.New("ran out of lines")
	// ErrComponentNestingTooDeep reports that nested components exceeded the maximum allowed depth.
	ErrComponentNestingTooDeep = errors.New("component nesting too deep")
	// ErrorPropertyNotFound reports that a requested property was not found.
	ErrorPropertyNotFound = errors.New("property not found")

	// ErrInvalidOpArg marks an invalid variadic option argument.
	ErrInvalidOpArg = errors.New("invalid option argument")
	// ErrNilStartLine reports that a parser was called without a starting line.
	ErrNilStartLine = errors.New("nil start line")

	// ErrPropertySkipped marks a parser decision to skip a malformed line and continue.
	ErrPropertySkipped = errors.New("property skipped")

	// ErrVTimezoneMissingTZID reports a VTIMEZONE component without a TZID property.
	ErrVTimezoneMissingTZID = errors.New("VTIMEZONE missing TZID")
	// ErrVTimezoneMissingObservance reports a VTIMEZONE component that defines no STANDARD or DAYLIGHT.
	ErrVTimezoneMissingObservance = errors.New("VTIMEZONE has no STANDARD or DAYLIGHT observance")
	// ErrVTimezoneMissingDTSTART reports an observance without a DTSTART property.
	ErrVTimezoneMissingDTSTART = errors.New("VTIMEZONE observance missing DTSTART")
	// ErrVTimezoneMissingOffset reports an observance missing TZOFFSETFROM or TZOFFSETTO.
	ErrVTimezoneMissingOffset = errors.New("VTIMEZONE observance missing TZOFFSETFROM/TZOFFSETTO")
	// ErrVTimezoneInvalidOffset reports a TZOFFSETFROM/TZOFFSETTO value that cannot be parsed.
	ErrVTimezoneInvalidOffset = errors.New("invalid VTIMEZONE UTC offset")
	// ErrVTimezoneInvalidRDATE reports an unparseable RDATE in an observance.
	ErrVTimezoneInvalidRDATE = errors.New("invalid VTIMEZONE RDATE")
	// ErrVTimezoneUnsupportedRDATE reports an RDATE value type other than DATE-TIME or DATE.
	ErrVTimezoneUnsupportedRDATE = errors.New("unsupported VTIMEZONE RDATE (only DATE/DATE-TIME)")
	// ErrVTimezoneUnsupportedRRULE reports an RRULE that cannot be evaluated inside a VTIMEZONE.
	ErrVTimezoneUnsupportedRRULE = errors.New("unsupported VTIMEZONE RRULE")
	// ErrVTimezoneBeforeCoverage reports a local time earlier than the earliest defined observance onset.
	ErrVTimezoneBeforeCoverage = errors.New("local time precedes VTIMEZONE coverage")
	// ErrVTimezoneContradiction reports an internally inconsistent set of observances.
	ErrVTimezoneContradiction = errors.New("contradictory VTIMEZONE rules")
)
