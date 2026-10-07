package semver

import (
	"fmt"
	"sort"
	"strings"
)

// ConstraintErrorKind classifies the problems reported by
// NewConstraintWithDiagnostics.
type ConstraintErrorKind int

const (
	// ConstraintErrInvalidSyntax indicates text that is neither a valid
	// constraint nor a valid separator between constraints.
	ConstraintErrInvalidSyntax ConstraintErrorKind = iota + 1

	// ConstraintErrEmptyBranch indicates a || branch that contains no
	// constraint.
	ConstraintErrEmptyBranch

	// ConstraintErrInvalidVersion indicates a version that could not be
	// parsed, for example a number outside the range of a uint64.
	ConstraintErrInvalidVersion

	// ConstraintErrTooLong indicates the input exceeds MaxConstraintLen.
	ConstraintErrTooLong

	// ConstraintErrTooManyGroups indicates the input contains more than
	// MaxConstraintGroups || branches.
	ConstraintErrTooManyGroups
)

// String returns a human readable name for the error kind.
func (k ConstraintErrorKind) String() string {
	switch k {
	case ConstraintErrInvalidSyntax:
		return "invalid syntax"
	case ConstraintErrEmptyBranch:
		return "empty branch"
	case ConstraintErrInvalidVersion:
		return "invalid version"
	case ConstraintErrTooLong:
		return "constraint too long"
	case ConstraintErrTooManyGroups:
		return "too many constraint groups"
	default:
		return "unknown constraint error"
	}
}

// ConstraintDiagnostic describes a single problem found while parsing a
// constraint string. All positions are byte offsets into the original input
// passed to NewConstraintWithDiagnostics, never into any internally rewritten
// form. End is exclusive. When Start == End the diagnostic points at a
// position between two bytes, for example a missing separator.
type ConstraintDiagnostic struct {
	// Kind classifies the problem.
	Kind ConstraintErrorKind

	// Branch is the 0 based index of the || branch the problem was found in.
	// It is -1 for problems that are not specific to a single branch, such as
	// ConstraintErrTooLong.
	Branch int

	// Start is the byte offset of the first byte of the problematic text.
	Start int

	// End is the byte offset one past the last byte of the problematic text.
	End int

	// Message describes the problem.
	Message string
}

// Error implements the error interface.
func (d ConstraintDiagnostic) Error() string {
	loc := fmt.Sprintf("bytes %d-%d", d.Start, d.End)
	if d.Start == d.End {
		loc = fmt.Sprintf("byte %d", d.Start)
	}
	if d.Branch >= 0 {
		return fmt.Sprintf("constraint %s in || branch %d at %s: %s", d.Kind, d.Branch, loc, d.Message)
	}
	return fmt.Sprintf("constraint %s at %s: %s", d.Kind, loc, d.Message)
}

// Is reports whether the diagnostic corresponds to the given sentinel error.
// It matches ErrConstraintTooLong and ErrTooManyConstraintGroups so callers
// can use errors.Is with the same sentinels NewConstraint returns.
func (d ConstraintDiagnostic) Is(target error) bool {
	switch target {
	case ErrConstraintTooLong:
		return d.Kind == ConstraintErrTooLong
	case ErrTooManyConstraintGroups:
		return d.Kind == ConstraintErrTooManyGroups
	}
	return false
}

// ConstraintSpan records where a single parsed comparison lives in the
// original constraint string. All positions are byte offsets into the
// original input. End is exclusive.
type ConstraintSpan struct {
	// Branch is the 0 based index of the || branch.
	Branch int

	// Index is the 0 based index of the comparison within the branch's AND
	// group.
	Index int

	// Start is the byte offset of the first byte of the comparison text.
	Start int

	// End is the byte offset one past the last byte of the comparison text.
	End int
}

// ConstraintDiagnostics carries the result of NewConstraintWithDiagnostics.
type ConstraintDiagnostics struct {
	// Diagnostics lists every problem found, ordered by position in the
	// input. It is empty when parsing succeeded.
	Diagnostics []ConstraintDiagnostic

	// Spans maps each parsed comparison back to the original input.
	// Spans[branch][index] corresponds to the comparison at the same
	// position within the returned Constraints. A hyphen range such as
	// "1.2.3 - 4.5.6" produces two comparisons whose spans both cover the
	// whole range expression. Spans is only populated when there are no
	// diagnostics.
	Spans [][]ConstraintSpan
}

// HasErrors reports whether any problems were found while parsing.
func (d *ConstraintDiagnostics) HasErrors() bool {
	return len(d.Diagnostics) > 0
}

// NewConstraintWithDiagnostics parses a constraint string like NewConstraint
// but reports problems with positions into the original input instead of
// returning a single error. It accepts exactly the same input language as
// NewConstraint and the returned Constraints behave identically.
//
// On success it returns the Constraints and a ConstraintDiagnostics whose
// Spans map every comparison back to its byte range in the original input.
// On failure it returns nil Constraints and a ConstraintDiagnostics listing
// every problem that could be reliably identified. Parsing recovers after
// most problems within a || branch so multiple issues are reported in one
// call; it stops early only when recovery is not reliable, namely when the
// input exceeds MaxConstraintLen or MaxConstraintGroups.
//
// Unlike NewConstraint, hyphen ranges are parsed in place rather than being
// rewritten to >= and <= comparisons first, so positions always refer to
// text the user actually wrote.
func NewConstraintWithDiagnostics(c string) (*Constraints, *ConstraintDiagnostics) {
	result := &ConstraintDiagnostics{}

	if len(c) > MaxConstraintLen {
		result.Diagnostics = append(result.Diagnostics, ConstraintDiagnostic{
			Kind:    ConstraintErrTooLong,
			Branch:  -1,
			Start:   0,
			End:     len(c),
			Message: ErrConstraintTooLong.Error(),
		})
		return nil, result
	}

	// Split into || branches while tracking byte offsets into the original
	// input.
	type branch struct {
		text  string
		start int
	}
	branches := make([]branch, 0, 4)
	start := 0
	for {
		i := strings.Index(c[start:], "||")
		if i < 0 {
			branches = append(branches, branch{c[start:], start})
			break
		}
		branches = append(branches, branch{c[start : start+i], start})
		start += i + 2
	}

	if len(branches) > MaxConstraintGroups {
		result.Diagnostics = append(result.Diagnostics, ConstraintDiagnostic{
			Kind:    ConstraintErrTooManyGroups,
			Branch:  -1,
			Start:   0,
			End:     len(c),
			Message: ErrTooManyConstraintGroups.Error(),
		})
		return nil, result
	}

	or := make([][]*constraint, len(branches))
	hasPre := make([]bool, len(branches))
	spans := make([][]ConstraintSpan, len(branches))
	for k, b := range branches {
		cons, sp := parseConstraintBranch(b.text, b.start, k, result)
		for _, pc := range cons {
			if pc.con.pre != "" {
				hasPre[k] = true
				break
			}
		}
		or[k] = cons
		spans[k] = sp
	}

	if len(result.Diagnostics) > 0 {
		sort.SliceStable(result.Diagnostics, func(i, j int) bool {
			return result.Diagnostics[i].Start < result.Diagnostics[j].Start
		})
		return nil, result
	}

	result.Spans = spans
	return &Constraints{
		constraints: or,
		containsPre: hasPre,
	}, result
}

// constraintSpaceCutset is the set of bytes matched by \s in Go regular
// expressions. Multi-byte whitespace such as U+00A0 is intentionally not
// included so it is reported as invalid rather than silently trimmed.
const constraintSpaceCutset = " \t\n\f\r"

// trimConstraintSpace returns the offsets of s with leading and trailing
// constraint whitespace removed. It operates on bytes so offsets stay valid
// for multi-byte content.
func trimConstraintSpace(s string) (int, int) {
	start := 0
	for start < len(s) && strings.IndexByte(constraintSpaceCutset, s[start]) >= 0 {
		start++
	}
	end := len(s)
	for end > start && strings.IndexByte(constraintSpaceCutset, s[end-1]) >= 0 {
		end--
	}
	return start, end
}

// validConstraintGap reports whether the text between two comparisons is a
// valid separator. The first comparison in a branch may only be preceded by
// whitespace. Later comparisons must be separated by whitespace or a comma,
// matching the separators validConstraintRegex accepts.
func validConstraintGap(gap string, first bool) bool {
	t := strings.Trim(gap, constraintSpaceCutset)
	if first {
		return t == ""
	}
	return gap != "" && (t == "" || t == ",")
}

// parseConstraintBranch parses a single || branch. text is the branch
// content, offset its absolute byte offset within the original input and
// branchIdx its 0 based branch index. Found problems are appended to result.
func parseConstraintBranch(text string, offset, branchIdx int, result *ConstraintDiagnostics) ([]*constraint, []ConstraintSpan) {
	// An item is a run of input text that produced one or more comparisons.
	// A plain comparison produces one; a hyphen range produces two.
	type item struct {
		start, end int // absolute byte offsets into the original input
		cons       []*constraint
	}

	var items []item
	var rangeRegions [][2]int // branch-relative regions occupied by ranges

	addDiag := func(kind ConstraintErrorKind, start, end int, format string, args ...interface{}) {
		result.Diagnostics = append(result.Diagnostics, ConstraintDiagnostic{
			Kind:    kind,
			Branch:  branchIdx,
			Start:   start,
			End:     end,
			Message: fmt.Sprintf(format, args...),
		})
	}

	// Hyphen ranges are parsed in place so diagnostics and spans keep
	// pointing at the original text instead of a rewritten >= / <= form.
	for _, m := range constraintRangeRegex.FindAllStringSubmatchIndex(text, -1) {
		loStart, loEnd := m[2], m[3]
		hiStart, hiEnd := m[22], m[23]
		rangeRegions = append(rangeRegions, [2]int{loStart, hiEnd})

		var cons []*constraint
		lo := text[loStart:loEnd]
		hi := text[hiStart:hiEnd]
		if lc, err := constraintFromMatch(constraintRegex.FindStringSubmatch(">= " + lo)); err != nil {
			addDiag(ConstraintErrInvalidVersion, offset+loStart, offset+loEnd, "%s", err)
		} else {
			cons = append(cons, lc)
		}
		if hc, err := constraintFromMatch(constraintRegex.FindStringSubmatch("<= " + hi)); err != nil {
			addDiag(ConstraintErrInvalidVersion, offset+hiStart, offset+hiEnd, "%s", err)
		} else {
			cons = append(cons, hc)
		}
		// The region is recorded as an item even when both endpoints are
		// invalid so the gap validation below treats it as occupied and
		// does not report the range text a second time.
		items = append(items, item{offset + loStart, offset + hiEnd, cons})
	}

	for _, m := range findConstraintRegex.FindAllStringSubmatchIndex(text, -1) {
		inside := false
		for _, r := range rangeRegions {
			if m[0] < r[1] && m[1] > r[0] {
				inside = true
				break
			}
		}
		if inside {
			continue
		}

		seg := text[m[0]:m[1]]
		sm := constraintRegex.FindStringSubmatch(seg)
		if sm == nil {
			// findConstraintRegex matches a subset of constraintRegex so
			// this should never happen.
			addDiag(ConstraintErrInvalidSyntax, offset+m[0], offset+m[1], "improper constraint: %q", seg)
			continue
		}
		cs, err := constraintFromMatch(sm)
		if err != nil {
			// The version submatch holds the text that failed to parse.
			addDiag(ConstraintErrInvalidVersion, offset+m[4], offset+m[5], "%s", err)
			// Record the region as occupied so it is not reported again
			// as unexpected text.
			items = append(items, item{offset + m[0], offset + m[1], nil})
			continue
		}
		items = append(items, item{offset + m[0], offset + m[1], []*constraint{cs}})
	}

	if len(items) == 0 {
		ts, te := trimConstraintSpace(text)
		if ts == te {
			addDiag(ConstraintErrEmptyBranch, offset, offset+len(text), "empty constraint branch")
		} else {
			addDiag(ConstraintErrInvalidSyntax, offset+ts, offset+te, "improper constraint: %q", text[ts:te])
		}
		return nil, nil
	}

	sort.SliceStable(items, func(i, j int) bool { return items[i].start < items[j].start })

	// Validate the separators before, between and after the comparisons.
	// Recovery after an invalid separator is reliable because the position
	// of the next comparison is known, so parsing continues to collect
	// further problems.
	prevEnd := 0 // branch-relative offset
	for i, it := range items {
		gap := text[prevEnd : it.start-offset]
		if !validConstraintGap(gap, i == 0) {
			gs, ge := trimConstraintSpace(gap)
			if gs == ge {
				addDiag(ConstraintErrInvalidSyntax, offset+prevEnd, offset+prevEnd, "missing separator between constraints")
			} else {
				addDiag(ConstraintErrInvalidSyntax, offset+prevEnd+gs, offset+prevEnd+ge, "unexpected text: %q", gap[gs:ge])
			}
		}
		prevEnd = it.end - offset
	}
	if tail := text[prevEnd:]; strings.Trim(tail, constraintSpaceCutset) != "" {
		ts, te := trimConstraintSpace(tail)
		addDiag(ConstraintErrInvalidSyntax, offset+prevEnd+ts, offset+prevEnd+te, "unexpected text: %q", tail[ts:te])
	}

	var cons []*constraint
	var spans []ConstraintSpan
	for _, it := range items {
		for _, pc := range it.cons {
			cons = append(cons, pc)
			spans = append(spans, ConstraintSpan{
				Branch: branchIdx,
				Index:  len(spans),
				Start:  it.start,
				End:    it.end,
			})
		}
	}
	return cons, spans
}
