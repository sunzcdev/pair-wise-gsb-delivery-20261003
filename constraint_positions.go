package semver

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ConstraintDiagnosticKind categorizes a problem found while parsing a
// constraint string. Categories let an editor pick an appropriate marker
// without parsing an error message.
type ConstraintDiagnosticKind int

const (
	// DiagSyntax is returned for text that cannot be read as an operator or
	// a version (for example ">= bar" or a stray character).
	DiagSyntax ConstraintDiagnosticKind = iota + 1

	// DiagEmptyBranch is returned when an OR branch is empty or only
	// contains whitespace (for example "1.0.0 ||").
	DiagEmptyBranch

	// DiagInvalidSeparator is returned when two comparisons are not joined
	// by whitespace or an optional comma (for example "1.0.0,2.0.0" or
	// "12.3.34~ 1.2.3"), or when a multi-byte whitespace such as U+3000 is
	// used where ASCII whitespace is required.
	DiagInvalidSeparator

	// DiagNumberOutOfRange is returned when a major, minor, or patch numeric
	// segment cannot be represented as a uint64.
	DiagNumberOutOfRange

	// DiagInvalidVersion is returned when a version is structurally invalid
	// (for example a malformed prerelease) even though it matches the rough
	// shape of a version.
	DiagInvalidVersion

	// DiagTooLong is returned when the entire input exceeds
	// MaxConstraintLen. Recovery is not possible for this case.
	DiagTooLong

	// DiagTooManyGroups is returned when the input contains more than
	// MaxConstraintGroups OR groups. Recovery is not possible for this case.
	DiagTooManyGroups
)

// String returns a stable, machine readable name for a diagnostic kind.
func (k ConstraintDiagnosticKind) String() string {
	switch k {
	case DiagSyntax:
		return "syntax"
	case DiagEmptyBranch:
		return "empty_branch"
	case DiagInvalidSeparator:
		return "invalid_separator"
	case DiagNumberOutOfRange:
		return "number_out_of_range"
	case DiagInvalidVersion:
		return "invalid_version"
	case DiagTooLong:
		return "too_long"
	case DiagTooManyGroups:
		return "too_many_groups"
	default:
		return "unknown"
	}
}

// ConstraintPosition is a byte offset into the original constraint string.
// Offsets always refer to the input passed to NewConstraintWithPositions,
// never to its internally rewritten form.
type ConstraintPosition struct {
	ByteOffset int
}

// Span returns a zero-width byte range located at the position.
func (p ConstraintPosition) Span() ConstraintSpan {
	return ConstraintSpan{Start: p.ByteOffset, End: p.ByteOffset}
}

// Column returns the 1-based byte column of the position relative to
// lineStart, the byte offset where the containing line begins.
func (p ConstraintPosition) Column(lineStart int) int {
	return p.ByteOffset - lineStart + 1
}

// ConstraintSpan is a half-open byte range [Start, End) within the original
// constraint string.
type ConstraintSpan struct {
	Start int
	End   int
}

// Slice returns the original input text covered by the span.
func (s ConstraintSpan) Slice(original string) string {
	if s.Start < 0 || s.End > len(original) || s.Start > s.End {
		return ""
	}
	return original[s.Start:s.End]
}

// Contains reports whether the span covers the given byte offset.
func (s ConstraintSpan) Contains(offset int) bool {
	return s.Start <= offset && offset < s.End
}

// ConstraintDiagnostic describes a single problem in a constraint string,
// including the exact byte range an editor should mark and the OR branch it
// belongs to.
type ConstraintDiagnostic struct {
	// Kind categorizes the problem.
	Kind ConstraintDiagnosticKind

	// Message is a human readable description of the problem.
	Message string

	// Span is the byte range in the original input that caused the problem.
	// A zero-width span (Start == End) points between two bytes.
	Span ConstraintSpan

	// Branch is the index of the OR branch containing the problem, starting
	// at zero. It is -1 for problems that apply to the whole input.
	Branch int

	// Fatal reports that parsing cannot continue reliably. No further
	// diagnostics should be expected after a fatal diagnostic.
	Fatal bool

	// syntheticHyphen marks a hyphen diagnostic created to replace a consumed
	// range operator; it must not be filtered back out.
	syntheticHyphen bool
}

// Error implements the error interface.
func (d ConstraintDiagnostic) Error() string {
	return d.Message
}

// ConstraintParseError wraps the diagnostics returned by
// NewConstraintWithPositions when parsing fails. It supports errors.Is for
// ErrConstraintTooLong and ErrTooManyConstraintGroups.
type ConstraintParseError struct {
	Diagnostics []ConstraintDiagnostic
}

// Error implements the error interface.
func (e *ConstraintParseError) Error() string {
	if len(e.Diagnostics) == 0 {
		return "improper constraint"
	}
	return e.Diagnostics[0].Message
}

// Is reports errors.Is matches for the input level sentinel errors.
func (e *ConstraintParseError) Is(target error) bool {
	switch target {
	case ErrConstraintTooLong:
		for _, d := range e.Diagnostics {
			if d.Kind == DiagTooLong {
				return true
			}
		}
	case ErrTooManyConstraintGroups:
		for _, d := range e.Diagnostics {
			if d.Kind == DiagTooManyGroups {
				return true
			}
		}
	}
	return false
}

// ConstraintComparison captures one parsed comparison together with the byte
// ranges it occupies in the original input.
type ConstraintComparison struct {
	// Item is the full comparison excluding surrounding whitespace, for
	// example ">= 1.2". For a bound derived from a hyphen range it covers
	// the whole range as written, for example "1.2 - 1.4".
	Item ConstraintSpan

	// Operator is the operator text the user wrote, for example ">=". It is
	// nil when no operator was written. It never points at the synthesized
	// ">="/"<=" operators produced internally for hyphen ranges.
	Operator *ConstraintSpan

	// Version is the version operand the user wrote.
	Version ConstraintSpan

	// HyphenRange reports that the comparison is a bound derived from a
	// hyphen range. When true, Item covers the whole range and Operator is
	// nil because the user wrote no comparison operator.
	HyphenRange bool
}

// ConstraintBranch captures one OR group (an AND list of comparisons) and
// the positions of its comparisons.
type ConstraintBranch struct {
	// Span is the byte range of the branch within the original input,
	// excluding the "||" separators but including interior whitespace.
	Span ConstraintSpan

	// Comparisons are the comparisons in the branch, in source order.
	Comparisons []ConstraintComparison
}

// ConstraintPositions is the positional counterpart of Constraints. Branches
// line up one-to-one with the OR groups of Constraints and, within each
// branch, comparisons line up one-to-one with the AND constraints.
type ConstraintPositions struct {
	Branches []ConstraintBranch
}

// NewConstraintWithPositions is a position-aware variant of NewConstraint.
//
// On success it returns the same Constraints value NewConstraint would
// produce along with a ConstraintPositions value describing where every
// comparison, operator, and version lives in the original input.
//
// On failure it returns a *ConstraintParseError carrying one or more
// ConstraintDiagnostic values. Diagnostics reference byte ranges of the
// original input, include an error kind, and identify the OR branch they
// belong to. Multiple problems in one branch are reported together when they
// can be located independently. A fatal diagnostic means further positions
// could not be recovered reliably and parsing stopped.
func NewConstraintWithPositions(c string) (*Constraints, *ConstraintPositions, error) {
	if len(c) > MaxConstraintLen {
		return nil, nil, &ConstraintParseError{Diagnostics: []ConstraintDiagnostic{{
			Kind:    DiagTooLong,
			Message: ErrConstraintTooLong.Error(),
			Span:    ConstraintSpan{Start: 0, End: len(c)},
			Branch:  -1,
			Fatal:   true,
		}}}
	}

	rawBranches := strings.Split(c, "||")
	if len(rawBranches) > MaxConstraintGroups {
		return nil, nil, &ConstraintParseError{Diagnostics: []ConstraintDiagnostic{{
			Kind:    DiagTooManyGroups,
			Message: ErrTooManyConstraintGroups.Error(),
			Span:    ConstraintSpan{Start: 0, End: len(c)},
			Branch:  -1,
			Fatal:   true,
		}}}
	}

	var (
		diagnostics []ConstraintDiagnostic
		positions   ConstraintPositions
		groups      [][]*constraint
		containsPre []bool
	)

	offset := 0
	for branchIndex, raw := range rawBranches {
		_ = branchIndex
		offset += len(raw) + len("||")
	}

	// Scan the whole input so hyphen ranges that cross "||" boundaries can be
	// detected precisely, then bucket results by branch.
	scanned := scanAllBranches(c)
	diagnostics = append(diagnostics, scanned.diagnostics...)

	for branchIndex, raw := range rawBranches {
		start := scanned.branchStart[branchIndex]
		end := start + len(raw)
		var comparisons []ConstraintComparison
		var constraints []*constraint
		hasPre := false
		for _, pc := range scanned.byBranch[branchIndex] {
			constraints = append(constraints, pc.pc)
			comparisons = append(comparisons, pc.span)
			if pc.pc.con.pre != "" {
				hasPre = true
			}
		}
		positions.Branches = append(positions.Branches, ConstraintBranch{
			Span:        ConstraintSpan{Start: start, End: end},
			Comparisons: comparisons,
		})
		groups = append(groups, constraints)
		containsPre = append(containsPre, hasPre)
	}

	if len(diagnostics) > 0 {
		sort.SliceStable(diagnostics, func(i, j int) bool {
			if diagnostics[i].Branch != diagnostics[j].Branch {
				return diagnostics[i].Branch < diagnostics[j].Branch
			}
			if diagnostics[i].Span.Start != diagnostics[j].Span.Start {
				return diagnostics[i].Span.Start < diagnostics[j].Span.Start
			}
			return diagnostics[i].Span.End < diagnostics[j].Span.End
		})
		return nil, nil, &ConstraintParseError{Diagnostics: diagnostics}
	}

	return &Constraints{constraints: groups, containsPre: containsPre}, &positions, nil
}

var (
	// locatedItemRegexp matches a single optional operator plus version. It
	// mirrors findConstraintRegex but uses named capture groups for the
	// operator and version sub-parts.
	locatedItemRegexp *locatedRegexp

	// locatedRangeRegexp matches a hyphen range. It mirrors
	// constraintRangeRegex but captures the hyphen separator as well.
	locatedRangeRegexp *locatedRegexp

	// malformedRangeRegexp locates "version -" prefixes where the upper
	// version of a hyphen range is missing or invalid. It mirrors the valid
	// range expression up to and including the hyphen separator.
	malformedRangeRegexp *regexp.Regexp

	// malformedRangeLocated is the named-group view used to extract subspans.
	malformedRangeLocated *locatedRegexp
)

const (
	locatedOpsPattern = `=||!=|>|<|>=|=>|<=|=<|~|~>|\^`
	locatedVerPattern = `v?(?P<ver_major>[0-9|x|X|\*]+)(?P<ver_minor>\.[0-9|x|X|\*]+)?` +
		`(?P<ver_patch>\.[0-9|x|X|\*]+)?` +
		`(?:-(?P<ver_pre>[0-9A-Za-z\-]+(?:\.[0-9A-Za-z\-]+)*))?` +
		`(?:\+(?P<ver_meta>[0-9A-Za-z\-]+(?:\.[0-9A-Za-z\-]+)*))?`
)

// locatedRegexp pairs a regexp with a name-to-index lookup for its named
// subexpressions.
type locatedRegexp struct {
	re    *regexp.Regexp
	index map[string]int
}

func compileLocated(expr string) *locatedRegexp {
	re := regexp.MustCompile(expr)
	index := make(map[string]int)
	for _, name := range re.SubexpNames() {
		if name != "" {
			index[name] = re.SubexpIndex(name)
		}
	}
	return &locatedRegexp{re: re, index: index}
}

func (l *locatedRegexp) sub(match []int, name string) (int, int, bool) {
	i, ok := l.index[name]
	if !ok || i*2+1 >= len(match) {
		return 0, 0, false
	}
	start, end := match[2*i], match[2*i+1]
	if start < 0 || end < 0 {
		return 0, 0, false
	}
	return start, end, true
}

func init() {
	locatedItemRegexp = compileLocated(fmt.Sprintf(
		`(?P<lead>\s*)(?P<op>%s)?\s*(?P<ver>%s)`,
		locatedOpsPattern, locatedVerPattern))

	locatedRangeRegexp = compileLocated(fmt.Sprintf(
		`(?P<lead>\s*)(?P<low>%s)(?P<hyphen>\s+-\s+)(?P<high>%s)(?P<trail>[ \t\r\n\v\f]*)`,
		locatedVerPattern, locatedVerPattern))

	malformedRangeRegexp = regexp.MustCompile(fmt.Sprintf(
		`\s*(?P<m_low>%s)(?P<m_sep>\s+-\s*)(?P<m_up>.*)$`, locatedVerPattern))
	malformedRangeLocated = compileLocated(malformedRangeRegexp.String())
}

type scannedBranch struct {
	constraints []*constraint
	spans       []ConstraintComparison
	elems       []scannedElem
	diagnostics []ConstraintDiagnostic
	containsPre bool
	cleanCross  map[int]string
}

type scannedInput struct {
	byBranch    [][]parsedComparisons
	branchStart []int
	diagnostics []ConstraintDiagnostic
}

type scannedBranchResult struct {
	constraints []*constraint
	comparisons []ConstraintComparison
	diagnostics []ConstraintDiagnostic
	containsPre bool
}

type parsedComparisons struct {
	pc   *constraint
	span ConstraintComparison
}

type scannedElemKind int

const (
	elemItem scannedElemKind = iota
	elemRange
)

// scannedElem is one comparison or hyphen range, ordered by source position.
// Offsets are relative to the branch text.
type scannedElem struct {
	kind       scannedElemKind
	start      int
	end        int
	itemMatch  []int
	rangeMatch []int
	group      int
	// matchBase is the branch-relative offset of the regexp match. The match
	// may include leading whitespace; start/end describe the element itself.
	matchBase int
	// matchEnd is the branch-relative end of the regexp match, including any
	// trailing/leading separator whitespace consumed by the match.
	matchEnd int
}

// scanBranch performs the full per-branch analysis on one "||"-delimited
// segment. base is the byte offset of branch in the original input.
// preDiags are whole-input diagnostics (cross-boundary ranges) already known
// to apply to this branch.
func scanBranch(full, branch string, base, branchIndex int, preDiags []ConstraintDiagnostic, cleanCross map[int]string) scannedBranchResult {
	out := scannedBranch{diagnostics: append([]ConstraintDiagnostic(nil), preDiags...)}
	out.cleanCross = cleanCross

	scanSegment(branch, 0, 0, &out)

	sort.SliceStable(out.elems, func(i, j int) bool { return out.elems[i].start < out.elems[j].start })

	for _, e := range out.elems {
		switch e.kind {
		case elemItem:
			parseItemElem(branch, e, base, branchIndex, &out)
		case elemRange:
			parseRangeElem(branch, e, base, branchIndex, &out)
		}
	}

	validateSeparators(full, branch, out.elems, base, branchIndex, &out)
	suppressMalformedRangeHyphens(branch, base, branchIndex, &out)

	var crossedBare *constraint
	for off, prefix := range cleanCross {
		if branchIndex == 0 && base+off >= base && base+off <= base+len(branch) {
			tokenStart := off - base - len(prefix)
			token := prefix
			if token == "" {
				token = strings.TrimSpace(branch[:off-base])
			}
			found := false
			for i := range out.constraints {
				if i < len(out.spans) &&
					out.spans[i].Version.Start == base+tokenStart &&
					out.spans[i].Version.End == off {
					out.constraints[i].origfunc = ">="
					found = true
					break
				}
			}
			if !found {
				if pc, err := parseConstraint(">= " + token); err == nil {
					crossedBare = pc
				}
			}
		}
		_ = prefix
		if branchIndex == 1 && len(out.constraints) > 0 {
			// The crossed range lives in the branch after the boundary; its
			// low bound is emitted bare by the legacy rewrite.
			out.constraints[0].origfunc = ""
		}
	}
	if crossedBare != nil {
		out.constraints = append([]*constraint{crossedBare}, out.constraints...)
	}

	if len(out.elems) == 0 && strings.TrimSpace(branch) == "" {
		mark := base + len(branch)
		out.diagnostics = append(out.diagnostics, ConstraintDiagnostic{
			Kind:    DiagEmptyBranch,
			Message: fmt.Sprintf("empty constraint branch at index %d", branchIndex),
			Span:    ConstraintSpan{Start: mark, End: mark},
			Branch:  branchIndex,
		})
	}

	return scannedBranchResult{
		constraints: out.constraints,
		comparisons: out.spans,
		diagnostics: out.diagnostics,
		containsPre: out.containsPre,
	}
}

// scanAllBranches scans the entire constraint input, locating elements,
// parsing comparisons, and validating separators and empty branches. It
// performs the whole-input analysis that NewConstraint needs for positions
// while preserving branch identity.
func scanAllBranches(c string) scannedInput {
	rawBranches := strings.Split(c, "||")
	branchStart := make([]int, len(rawBranches))
	pos := 0
	for i, raw := range rawBranches {
		branchStart[i] = pos
		pos += len(raw) + 2
	}

	result := scannedInput{
		branchStart: branchStart,
		byBranch:    make([][]parsedComparisons, len(rawBranches)),
	}

	var diagnostics []ConstraintDiagnostic

	// First, detect hyphen ranges the legacy rewrite would stretch across an
	// "||" boundary. These are whole-input problems attached to the branch
	// whose text they corrupt.
	crossDiags := crossBoundaryRangeDiagnostics(c, branchStart)
	cleanCross := cleanLowCrossRanges(c)

	for gi, raw := range rawBranches {
		b := scanBranch(c, raw, branchStart[gi], gi, crossDiags[gi], cleanCross)
		diagnostics = append(diagnostics, b.diagnostics...)
		for i := range b.comparisons {
			result.byBranch[gi] = append(result.byBranch[gi], parsedComparisons{
				pc:   b.constraints[i],
				span: b.comparisons[i],
			})
		}
	}
	result.diagnostics = diagnostics

	sort.SliceStable(result.diagnostics, func(i, j int) bool {
		if result.diagnostics[i].Branch != result.diagnostics[j].Branch {
			return result.diagnostics[i].Branch < result.diagnostics[j].Branch
		}
		return result.diagnostics[i].Span.Start < result.diagnostics[j].Span.Start
	})

	return result
}

// crossBoundaryRangeDiagnostics reproduces the legacy range rewrite's lack of
// "||" awareness by re-running the global range regexp over the whole input.
// When a hyphen range reaches across an "||", the preceding branch becomes
// invalid after rewriting. The returned slice has one entry per branch.
func crossBoundaryRangeDiagnostics(c string, branchStart []int) [][]ConstraintDiagnostic {
	out := make([][]ConstraintDiagnostic, len(branchStart))
	if !strings.Contains(c, "||") {
		return out
	}

	branchIndexAt := func(off int) int {
		for i := len(branchStart) - 1; i >= 0; i-- {
			if off >= branchStart[i] {
				return i
			}
		}
		return 0
	}

	for _, rm := range locatedRangeRegexp.re.FindAllStringSubmatchIndex(c, -1) {
		lowStart, lowEnd, _ := locatedRangeRegexp.sub(rm, "low")
		highStart, highEnd, _ := locatedRangeRegexp.sub(rm, "high")
		low := c[lowStart:lowEnd]
		high := c[highStart:highEnd]

		// The legacy rewrite is not "||"-aware. A range that starts before a
		// boundary and whose match reaches past it corrupts one of the two
		// branches. When the low bound contains "||" it is only corruption if
		// the prefix before the boundary is a non-empty, non-version item
		// (e.g. "~1.2"): a bare version ("1||1.0.0 - ...") rewrites cleanly.
		barrel := -1
		if strings.Contains(low, "||") {
			barRel := strings.Index(low, "||")
			if _, clean := classifyCleanLowCross(c, lowStart, lowStart+barRel, low[:barRel]); !clean {
				barrel = lowStart + barRel
			}
		} else if bar := strings.Index(high, "||"); bar >= 0 {
			// The boundary may fall at the end of the high bound
			// ("2.0.0||=<v1"): the next branch is independent and parses on
			// its own after the rewrite.
			if bar == len(high)-2 {
				rest2 := c[highEnd:]
				if rewriteRangeValid(rest2) {
					continue
				}
				barrel = highStart + bar
			}
			// Decide using the exact legacy behavior: the text starting at
			// the boundary must rewrite into a valid branch on its own.
			// "X< 23.0" gets a separating space (valid); "3" + ".0" would
			// become "3 .0" (invalid).
			nextBranch := c[highStart+bar+2:]
			token := high[bar+2:]
			tail := nextBranch[len(token):]
			// Reproduce the exact rewritten next branch. The consumed token
			// is removed from the match position; the replacement
			// ">= low, <= high " supplies one space, and the existing
			// whitespace in the tail supplies the rest:
			//   "X" + "< 23"      -> "X < 23"
			//   "3" + ".0"        -> "3 .0" (invalid)
			//   "1" + "  1.0 - 2" -> "1  >= 1.0, <= 2 "
			simulated := token + " " + tail
			// Rewrite inner ranges first, then prepend the consumed token.
			simulated = token + " " + rewriteRange(strings.TrimLeft(tail, " \t\r\n\v\f"))
			if strings.TrimSpace(token) == "" {
				simulated = " " + tail
			}
			clean := isValidRangeBound(high[:bar]) &&
				(validConstraintRegex.MatchString(simulated) ||
					// A token was not actually consumed when the high match
					// ends right at the boundary; validate the next branch
					// independently.
					(token == "" && rewriteRangeValid(nextBranch)))
			if clean {
				continue
			}
			barrel = highStart + bar
		}
		if barrel < 0 {
			continue
		}
		bi := branchIndexAt(barrel)
		out[bi] = append(out[bi], ConstraintDiagnostic{
			Kind:    DiagSyntax,
			Message: "hyphen range crosses an || branch boundary",
			Span:    ConstraintSpan{Start: barrel, End: barrel + 2},
			Branch:  bi,
		})
	}
	return out
}

// isValidRangeBound reports whether text parses as a single range bound,
// mirroring parseConstraint(">= "+text).
func isValidRangeBound(text string) bool {
	_, err := parseConstraint(">= " + text)
	return err == nil
}

// cleanLowCrossRanges describes "||" boundaries where a global hyphen range
// reaches across but the legacy rewrite still yields valid branches. The
// returned map keys are boundary offsets; each value is the text of the low
// token that gets the synthesized ">=" attached in the preceding branch.
func cleanLowCrossRanges(c string) map[int]string {
	out := map[int]string{}
	for _, rm := range locatedRangeRegexp.re.FindAllStringSubmatchIndex(c, -1) {
		lowStart, lowEnd, _ := locatedRangeRegexp.sub(rm, "low")
		low := c[lowStart:lowEnd]
		barRel := strings.Index(low, "||")
		if barRel < 0 {
			continue
		}
		if token, clean := classifyCleanLowCross(c, lowStart, lowStart+barRel, low[:barRel]); clean {
			out[lowStart+barRel] = token
		}
	}
	return out
}

// classifyCleanLowCross determines whether a hyphen range crossing an "||" at
// boundaryOff (with the low token prefix ending at tokenEnd) rewrites to
// valid branches. It returns the token the synthesized ">=" attaches to.
func classifyCleanLowCross(c string, lowStart, boundaryOff int, prefix string) (string, bool) {
	// Any valid bare version token at the start of the input ("1", "v1",
	// "x", "v*", ...) rewrites cleanly to ">= <token>" in the preceding
	// branch. Leading ASCII whitespace is allowed ("\tX||...").
	leading := c[:lowStart]
	if lowStart >= 0 && isValidRangeBound(prefix) && onlyASCIISpace(leading) {
		return prefix, true
	}

	// A wildcard token ("*", "x", "X") preceded by a comma rewrites to
	// "...,>= *" in the preceding branch.
	if (prefix == "*" || prefix == "x" || prefix == "X") && lowStart > 0 {
		before := c[:lowStart]
		comma := strings.LastIndex(before, ",")
		if comma < 0 {
			return "", false
		}
		// Everything between the comma and the token must be whitespace.
		if strings.TrimSpace(before[comma+1:]) == "" {
			return prefix, true
		}
	}
	return "", false
}

// suppressMalformedRangeHyphens removes the syntax diagnostic placed on a
// leading "-" when that "-" is the separator of a "lower -" range whose upper
// version is invalid.
func suppressMalformedRangeHyphens(branch string, base, branchIndex int, out *scannedBranch) {
	lr := malformedRangeLocated
	matches := lr.re.FindAllStringSubmatchIndex(branch, -1)
	if len(matches) == 0 {
		return
	}

	type hyphenSpan struct{ start, end int }
	var (
		malformed  [][2]int // whole-match spans of "lower -" with empty upper
		badHyphens []hyphenSpan
	)
	for _, m := range matches {
		lowS, lowE, _ := lr.sub(m, "m_low")
		sepS, sepE, _ := lr.sub(m, "m_sep")
		upS, upE, _ := lr.sub(m, "m_up")

		isValid := false
		for _, rm := range locatedRangeRegexp.re.FindAllStringSubmatchIndex(branch, -1) {
			if lowS >= rm[0] && lowE <= rm[1] {
				isValid = true
				break
			}
		}
		if isValid {
			continue
		}

		upper := strings.TrimSpace(branch[upS:upE])
		hyphenAt := strings.Index(branch[sepS:sepE], "-")
		if hyphenAt < 0 {
			continue
		}

		badHyphens = append(badHyphens, hyphenSpan{
			start: base + sepS + hyphenAt,
			end:   base + sepS + hyphenAt + 1,
		})

		// When the hyphen is consumed as a negative-sign prefix of the next
		// item (e.g. "1.0\n-1"), the standalone hyphen diagnostic is absent
		// but the branch is still invalid. Point at the hyphen.
		if upper != "" {
			upperHasDiag := upperHasDiagnostic(out.diagnostics, branchIndex, base+upS, base+upE)
			// Mark an existing standalone "-" diagnostic so it is kept only
			// when the upper text itself produced no diagnostic; otherwise
			// the hyphen is the range operator and the upper text is the
			// actionable problem.
			for i := range out.diagnostics {
				d := &out.diagnostics[i]
				if d.Span.Start == base+sepS+hyphenAt && d.Span.End == base+sepS+hyphenAt+1 {
					d.syntheticHyphen = upperHasDiag
				}
			}
			// If the upper text parsed as an item ("-1") but the hyphen is a
			// malformed range operator rather than a valid range, the branch
			// is still invalid; add a non-synthetic hyphen diagnostic.
			if !upperHasDiag && !hyphenAlreadyMarked(out.diagnostics, base+sepS+hyphenAt) {
				out.diagnostics = append(out.diagnostics, ConstraintDiagnostic{
					Kind:    DiagSyntax,
					Message: "improper constraint: \"-\"",
					Span: ConstraintSpan{
						Start: base + sepS + hyphenAt,
						End:   base + sepS + hyphenAt + 1,
					},
					Branch: branchIndex,
				})
			}
		}

		if upper == "" {
			malformed = append(malformed, [2]int{m[0], m[1]})
		}
	}

	if len(malformed) == 0 && len(badHyphens) == 0 {
		// Still fall through: a malformed range with a parseable upper value
		// may need an explicit hyphen diagnostic added below.
	}

	// A "lower -" with no upper version is an error on its own even when no
	// separate item diagnostic was produced (e.g. "* -").
	for _, ic := range malformed {
		hasDiag := false
		for _, d := range out.diagnostics {
			// The standalone "-" that the malformed range suppresses does not
			// count: it is about to be filtered out below.
			text := branchReslice(branch, base, d.Span)
			if d.Kind == DiagSyntax && text == "-" {
				continue
			}
			if d.Branch == branchIndex &&
				d.Span.Start >= base+ic[0] && d.Span.Start <= base+ic[1] {
				hasDiag = true
				break
			}
		}
		if !hasDiag {
			out.diagnostics = append(out.diagnostics, ConstraintDiagnostic{
				Kind:    DiagSyntax,
				Message: "hyphen range is missing an upper version",
				Span:    ConstraintSpan{Start: base + ic[0], End: base + ic[1]},
				Branch:  branchIndex,
			})
		}
	}

	filtered := make([]ConstraintDiagnostic, 0, len(out.diagnostics))
	for _, d := range out.diagnostics {
		drop := false
		text := branchReslice(branch, base, d.Span)
		if d.Kind == DiagSyntax && text == "-" && d.Message == "improper constraint: \"-\"" {
			drop = shouldDropRangeHyphen(d, matches, lr, branch, base, out.diagnostics)
		}
		if !drop {
			filtered = append(filtered, d)
		}
	}
	out.diagnostics = filtered
}

func branchReslice(branch string, base int, s ConstraintSpan) string {
	start := s.Start - base
	end := s.End - base
	if start < 0 || end > len(branch) || start > end {
		return ""
	}
	return branch[start:end]
}

// upperHasDiagnostic reports whether any diagnostic other than the hyphen
// itself covers the upper-version range [upStart, upEnd).
func upperHasDiagnostic(diags []ConstraintDiagnostic, branchIndex, upStart, upEnd int) bool {
	for _, d := range diags {
		if d.Branch != branchIndex {
			continue
		}
		text := ""
		_ = text
		if d.Span.Start >= upStart && d.Span.End <= upEnd &&
			!(d.Kind == DiagSyntax && d.Message == "improper constraint: \"-\"") {
			return true
		}
	}
	return false
}

func hyphenAlreadyMarked(diags []ConstraintDiagnostic, off int) bool {
	for _, d := range diags {
		if d.Span.Start == off && d.Span.End == off+1 &&
			d.Kind == DiagSyntax && d.Message == "improper constraint: \"-\"" {
			return true
		}
	}
	return false
}

// shouldDropRangeHyphen decides whether a standalone "-" diagnostic that
// belongs to a malformed range should be removed. It is removed when the upper
// version has its own diagnostic (the hyphen is just the range operator), but
// kept when the hyphen was consumed as a sign prefix and nothing else covers
// the upper side.
func shouldDropRangeHyphen(d ConstraintDiagnostic, matches [][]int, lr *locatedRegexp, branch string, base int, diags []ConstraintDiagnostic) bool {
	for _, m := range matches {
		sepS, sepE, _ := lr.sub(m, "m_sep")
		hyphenRel := strings.Index(branch[sepS:sepE], "-")
		if hyphenRel < 0 {
			continue
		}
		hStart := base + sepS + hyphenRel
		if d.Span.Start != hStart || d.Span.End != hStart+1 {
			continue
		}
		upS, upE, _ := lr.sub(m, "m_up")
		if upperHasDiagnostic(diags, d.Branch, base+upS, base+upE) {
			return true
		}
		return false
	}
	return false
}

// scanSegment records ordinary comparisons and hyphen ranges found in a
// segment that is known not to contain "||". Elements are parsed later once
// their source order is known.
func scanSegment(segment string, segmentOffset, group int, out *scannedBranch) {
	if segment == "" {
		return
	}

	// Hyphen ranges are located first so they keep their original positions;
	// the gaps between them are scanned for ordinary comparisons.
	ranges := locatedRangeRegexp.re.FindAllStringSubmatchIndex(segment, -1)
	gapStart := 0
	for _, rm := range ranges {
		lowRel, _, _ := locatedRangeRegexp.sub(rm, "low")
		_, highRel, _ := locatedRangeRegexp.sub(rm, "high")
		trailRel, trailEnd, hasTrail := locatedRangeRegexp.sub(rm, "trail")

		scanItemGap(segment[gapStart:lowRel], segmentOffset+gapStart, group, out)

		out.elems = append(out.elems, scannedElem{
			kind:  elemRange,
			start: segmentOffset + lowRel,
			// Content runs through any trailing separator whitespace so an
			// immediately following item needs its own whitespace; the
			// rewrite inserts a space there.
			end:        segmentOffset + highRel,
			rangeMatch: rm,
			matchBase:  segmentOffset + rm[0],
			matchEnd:   segmentOffset + rm[1],
			group:      group,
		})

		gapStart = highRel
		if hasTrail {
			gapStart = trailEnd
		}
		_ = trailRel
	}
	scanItemGap(segment[gapStart:], segmentOffset+gapStart, group, out)
}

// scanItemGap records ordinary comparisons found in text without hyphen
// ranges. gapOffset is the offset of gap within the branch.
func scanItemGap(gap string, gapOffset, group int, out *scannedBranch) {
	if strings.TrimSpace(gap) == "" {
		return
	}
	matches := locatedItemRegexp.re.FindAllStringSubmatchIndex(gap, -1)
	for _, m := range matches {
		// start/end bound the content (operator/version); the regexp match
		// bounds also cover leading whitespace that belongs to the separator
		// consumed by FindAll.
		start := m[0]
		if opS, _, hasOp := locatedItemRegexp.sub(m, "op"); hasOp {
			start = opS
		} else if verS, _, hasVer := locatedItemRegexp.sub(m, "ver"); hasVer {
			start = verS
		}
		_, verEnd, _ := locatedItemRegexp.sub(m, "ver")
		contentEnd := verEnd
		out.elems = append(out.elems, scannedElem{
			kind:      elemItem,
			start:     gapOffset + start,
			end:       gapOffset + contentEnd,
			matchEnd:  gapOffset + m[1],
			itemMatch: m,
			matchBase: gapOffset + m[0],
			group:     group,
		})
	}
}

func parseItemElem(branch string, e scannedElem, base, branchIndex int, out *scannedBranch) {
	itemText := branch[e.start:e.end]
	pc, err := parseConstraint(strings.TrimSpace(itemText))
	if err != nil {
		m := normalizedMatch(e.itemMatch)
		matchLen := e.itemMatch[1] - e.itemMatch[0]
		matchText := branch[e.matchBase : e.matchBase+matchLen]
		out.diagnostics = append(out.diagnostics,
			diagnoseItemVersion(m, matchText, base+e.matchBase, branchIndex))
		return
	}
	if pc.con.pre != "" {
		out.containsPre = true
	}
	out.constraints = append(out.constraints, pc)
	out.spans = append(out.spans, buildItemSpan(normalizedMatch(e.itemMatch), base+e.matchBase))
}

// normalizedMatch returns a copy of match whose pair offsets are relative to
// the start of the matched text instead of the gap it was found in.
func normalizedMatch(match []int) []int {
	out := append([]int(nil), match...)
	base := match[0]
	for i := 0; i+1 < len(out); i += 2 {
		if out[i] >= 0 {
			out[i] -= base
			out[i+1] -= base
		}
	}
	return out
}

func parseRangeElem(branch string, e scannedElem, base, branchIndex int, out *scannedBranch) {
	lowStart, lowEnd, _ := locatedRangeRegexp.sub(e.rangeMatch, "low")
	highStart, highEnd, _ := locatedRangeRegexp.sub(e.rangeMatch, "high")

	if pc, err := parseConstraint(">= " + branch[lowStart:lowEnd]); err != nil {
		out.diagnostics = append(out.diagnostics,
			diagnoseBound(branch[lowStart:lowEnd], base+lowStart, branchIndex))
	} else {
		if pc.con.pre != "" {
			out.containsPre = true
		}
		out.constraints = append(out.constraints, pc)
		out.spans = append(out.spans,
			rangeBoundSpan(base+e.start, base+e.end, base+lowStart, base+lowEnd))
	}

	if pc, err := parseConstraint("<= " + branch[highStart:highEnd]); err != nil {
		out.diagnostics = append(out.diagnostics,
			diagnoseBound(branch[highStart:highEnd], base+highStart, branchIndex))
	} else {
		if pc.con.pre != "" {
			out.containsPre = true
		}
		out.constraints = append(out.constraints, pc)
		out.spans = append(out.spans,
			rangeBoundSpan(base+e.start, base+e.end, base+highStart, base+highEnd))
	}
}

// buildItemSpan maps one located item match to original-input byte ranges.
func buildItemSpan(m []int, gapOffset int) ConstraintComparison {
	opStart, opEnd, hasOp := locatedItemRegexp.sub(m, "op")
	verStart, verEnd, hasVer := locatedItemRegexp.sub(m, "ver")

	cmp := ConstraintComparison{
		Version: ConstraintSpan{Start: gapOffset + verStart, End: gapOffset + verEnd},
	}
	if hasOp {
		cmp.Item = ConstraintSpan{Start: gapOffset + opStart, End: gapOffset + verEnd}
	} else if hasVer {
		cmp.Item = ConstraintSpan{Start: gapOffset + verStart, End: gapOffset + verEnd}
	}
	if hasOp {
		op := ConstraintSpan{Start: gapOffset + opStart, End: gapOffset + opEnd}
		cmp.Operator = &op
	}
	return cmp
}

// rangeBoundSpan builds a span for one bound of a hyphen range. Item covers
// the whole range so the synthesized operator is never reported.
func rangeBoundSpan(itemStart, itemEnd, verStart, verEnd int) ConstraintComparison {
	return ConstraintComparison{
		Item:        ConstraintSpan{Start: itemStart, End: itemEnd},
		Version:     ConstraintSpan{Start: verStart, End: verEnd},
		HyphenRange: true,
	}
}

// validateSeparators checks the text before the first element, between
// adjacent elements, and after the last element. Leading/trailing text may
// only contain ASCII whitespace. Text between elements may contain ASCII
// whitespace and at most one comma. Anything else (including multi-byte
// whitespace such as U+3000) is reported at its exact bytes.
func validateSeparators(full, branch string, elems []scannedElem, base, branchIndex int, out *scannedBranch) {
	if len(elems) == 0 {
		emitEdgeJunk(branch, base, branchIndex, out)
		return
	}

	emitLeadingJunk(branch[:elems[0].start], base, branchIndex, out)

	for i := 1; i < len(elems); i++ {
		prev, cur := elems[i-1], elems[i]
		// Separator text is everything between the previous element's
		// content end and the current element's content start. Regexp matches
		// consume some of this whitespace, so use content bounds explicitly.
		between := branch[prev.end:cur.start]

		// A plain item immediately before a hyphen range has to be joined by
		// a comma: the grammar consumes the space before the range during
		// rewriting, so "1.0.0 2.0.0 - 3.0.0" is not valid whereas
		// "1.0.0, 2.0.0 - 3.0.0" is.
		if prev.kind == elemItem && cur.kind == elemRange {
			// After a global range crossing, the range rewrite supplies the
			// comma equivalent: "1  1.0 - 2.0" in the next branch rewrites to
			// "1 >= 1.0, <= 2.0", which is valid.
			if bareItemStartsBranchAfterRange(full, base, prev) &&
				rangeEndsPreviousBranch(full, base) &&
				onlyASCIISpace(between) {
				continue
			}
			emitPreRangeSeparator(between, prev.end, base, branchIndex, out)
			continue
		}

		// Range rewriting inserts a trailing space, so a range may be glued
		// directly to what follows: an operator or bare item
		// ("...2.0.0^0.1", "...2.0.0v1") or another range whose low version
		// ("...2.0.0v11.0.0 - 2.0.0") doubles as the glued item.
		if prev.kind == elemRange && between == "" {
			if cur.kind == elemItem &&
				(itemStartsWithOperator(cur, branch) || itemIsBareVersion(cur, branch)) {
				continue
			}
			if cur.kind == elemRange && rangeLowIsBareItem(cur, branch) {
				continue
			}
		}

		// A bare item immediately followed by an operator is invalid in
		// general, but range rewriting can inject the required space when the
		// boundary lies right before the bare item: "1.0 - 2.0||X<3" rewrites
		// to "X <3", which is valid.
		//
		// Two bare items glued after such a boundary are likewise separated
		// by the injected space: "1.0 - 2.0||Xv1" rewrites to "X v1".
		//
		// A bare item that follows an operator, or two glued bare items, are
		// both allowed here only because the preceding range's rewrite
		// injects the separating space ("...||Xv1" -> "X v1"). Without that
		// preceding range the glued forms stay invalid.
		if prev.kind == elemItem && cur.kind == elemItem && between == "" &&
			itemIsBareVersion(prev, branch) &&
			bareItemStartsBranchAfterRange(full, base, prev) {
			if itemStartsWithOperator(cur, branch) {
				// A bare item glued to an operator is valid only when the
				// preceding branch ends with a range, whose rewritten space
				// is injected at the boundary ("...2.0||X<3" -> "X <3").
				if rangeEndsPreviousBranch(full, base) {
					continue
				}
			} else if itemIsBareVersion(cur, branch) && rangeEndsPreviousBranch(full, base) {
				continue
			}
		}

		emitBetweenDiagnostics(between, prev.end, cur.start, base, branchIndex, out)
	}

	tail := branch[elems[len(elems)-1].end:]
	emitTrailingJunk(tail, base+elems[len(elems)-1].end, branchIndex, out)
}

// emitPreRangeSeparator validates the separator between a plain comparison
// and a following hyphen range. A comma (with optional surrounding ASCII
// whitespace) is required; whitespace alone is invalid.
func emitPreRangeSeparator(text string, relEnd, base, branchIndex int, out *scannedBranch) {
	trimmed := strings.TrimSpace(text)
	// A multi-byte whitespace rune (e.g. U+3000) is never valid even when a
	// comma is present; point at the rune itself.
	if i := indexNonASCIISeparator(text); i >= 0 {
		_, size := decodeASCIIRune(text[i:])
		out.diagnostics = append(out.diagnostics, ConstraintDiagnostic{
			Kind:    DiagInvalidSeparator,
			Message: fmt.Sprintf("non-ASCII whitespace or character %q is not a valid separator", string(text[i])),
			Span:    ConstraintSpan{Start: base + relEnd + i, End: base + relEnd + i + size},
			Branch:  branchIndex,
		})
		return
	}
	if trimmed == "," {
		return
	}
	if trimmed == "" {
		out.diagnostics = append(out.diagnostics, ConstraintDiagnostic{
			Kind:    DiagInvalidSeparator,
			Message: "expected a comma before a hyphen range",
			Span:    ConstraintSpan{Start: base + relEnd, End: base + relEnd},
			Branch:  branchIndex,
		})
		return
	}
	emitBetweenDiagnostics(text, relEnd, relEnd+len(text), base, branchIndex, out)
}

// itemStartsWithOperator reports whether an item element begins with an
// explicit operator (rather than a bare version).
func itemStartsWithOperator(e scannedElem, branch string) bool {
	if e.kind != elemItem {
		return false
	}
	if opS, opE, ok := locatedItemRegexp.sub(e.itemMatch, "op"); ok && opE > opS {
		return true
	}
	return false
}

// itemIsBareVersion reports whether an item has no explicit operator.
func itemIsBareVersion(e scannedElem, branch string) bool {
	if e.kind != elemItem {
		return false
	}
	opS, opE, hasOp := locatedItemRegexp.sub(e.itemMatch, "op")
	if hasOp && opE > opS {
		return false
	}
	return true
}

// rangeLowIsBareItem reports whether a range element can directly follow
// another range without a separator. Range lower bounds never have an
// explicit operator, and the previous range's rewritten trailing space
// provides the separation (e.g. "...2.0.0" + "v11.0.0 - 2.0.0").
func rangeLowIsBareItem(e scannedElem, branch string) bool {
	return e.kind == elemRange
}

// bareItemStartsBranchAfterRange reports whether a bare-version item is the
// first element of its "||" branch whose preceding branch ended with a hyphen
// range. The legacy range rewrite injects a space in precisely that layout.
// bareItemStartsBranchAfterRange is checked against the full input because
// separators are validated per branch while the injected space comes from a
// range in the preceding branch.
func bareItemStartsBranchAfterRange(full string, base int, bare scannedElem) bool {
	// The item must be the first element of its branch, and the preceding
	// branch must end with a hyphen range.
	if bare.start != 0 {
		return false
	}
	barrel := -1
	for i := base - 2; i >= 0; i-- {
		if i+1 < len(full) && full[i] == '|' && full[i+1] == '|' {
			barrel = i
			break
		}
	}
	if barrel < 0 {
		return false
	}
	prefix := full[:barrel]
	lastRange := -1
	for _, rm := range locatedRangeRegexp.re.FindAllStringSubmatchIndex(prefix, -1) {
		lastRange = rm[1]
	}
	if lastRange < 0 {
		return false
	}
	// The injected space is only available to separate two bare items (not an
	// operator): "X v1" needs it, but "1.x>=1.0.0" is invalid regardless. An
	// operator glued to a bare item is handled by the caller and does not
	// require the range to be the final token.
	return true
}

// rangeEndsPreviousBranch reports whether the branch immediately before base
// ends with a hyphen range (so its rewritten trailing space reaches the
// boundary and can separate two bare items in the next branch).
func rangeEndsPreviousBranch(full string, base int) bool {
	if base < 2 {
		return false
	}
	// The injected space only reaches the next branch when there is no
	// whitespace between the range and "||" ("...2.0.0||"), not with an
	// explicit separator ("...2.0.0 ||").
	if full[base-2:base] != "||" {
		return false
	}
	prefix := full[:base-2]
	matches := locatedRangeRegexp.re.FindAllStringSubmatchIndex(prefix, -1)
	if len(matches) == 0 {
		return false
	}
	// The range match includes its trailing whitespace. The injected space
	// reaches the boundary only when there is no explicit whitespace between
	// the range's upper version and "||".
	_, highEnd, _ := locatedRangeRegexp.sub(matches[len(matches)-1], "high")
	return highEnd == len(prefix)
}

// emitEdgeJunk handles a branch with no located elements. A non-ASCII
// whitespace rune is pointed at precisely; other non-whitespace content is
// reported as syntax junk. A fully empty/whitespace branch is handled by the
// caller as DiagEmptyBranch.
func emitEdgeJunk(branch string, base, branchIndex int, out *scannedBranch) {
	for i := 0; i < len(branch); {
		r, size := decodeASCIIRune(branch[i:])
		if isConstraintSpace(r) {
			i += size
			continue
		}
		if r > utf8.RuneSelf {
			out.diagnostics = append(out.diagnostics, ConstraintDiagnostic{
				Kind:    DiagInvalidSeparator,
				Message: fmt.Sprintf("non-ASCII whitespace or character %q is not a valid separator", string(r)),
				Span:    ConstraintSpan{Start: base + i, End: base + i + size},
				Branch:  branchIndex,
			})
			i += size
			continue
		}
		if r == '-' {
			out.diagnostics = append(out.diagnostics, ConstraintDiagnostic{
				Kind:    DiagSyntax,
				Message: "improper constraint: \"-\"",
				Span:    ConstraintSpan{Start: base + i, End: base + i + size},
				Branch:  branchIndex,
			})
			i += size
			continue
		}
		j := i + 1
		for j < len(branch) {
			r2, size2 := decodeASCIIRune(branch[j:])
			if isConstraintSpace(r2) || r2 > utf8.RuneSelf {
				break
			}
			j += size2
		}
		out.diagnostics = append(out.diagnostics, syntaxDiagnostic(
			branch[i:j], base+i, base+j, branchIndex))
		i = j
	}
}

func emitLeadingJunk(text string, base, branchIndex int, out *scannedBranch) {
	if onlyASCIISpace(text) {
		return
	}
	emitEdgeJunk(text, base, branchIndex, out)
}

func emitTrailingJunk(text string, base, branchIndex int, out *scannedBranch) {
	if onlyASCIISpace(text) {
		return
	}
	emitEdgeJunk(text, base, branchIndex, out)
}

// emitBetweenDiagnostics validates the separator between two elements.
// relStart/relEnd are branch-relative offsets of text.
func emitBetweenDiagnostics(text string, relStart, relEnd, base, branchIndex int, out *scannedBranch) {
	trimmed := strings.TrimLeft(text, " \t\r\n\v\f")
	leading := len(text) - len(trimmed)
	trimmed = strings.TrimRight(trimmed, " \t\r\n\v\f")

	if trimmed == "" {
		if len(text) == 0 {
			// Two elements touch. Place a zero-width marker at the boundary.
			out.diagnostics = append(out.diagnostics, ConstraintDiagnostic{
				Kind:    DiagInvalidSeparator,
				Message: "missing whitespace between comparisons",
				Span:    ConstraintSpan{Start: base + relEnd, End: base + relEnd},
				Branch:  branchIndex,
			})
			return
		}
		// Only whitespace was present but a multi-byte rune slipped through
		// the ASCII trim.
		emitNonASCIIOnly(text, relStart, base, branchIndex, out)
		return
	}

	// The core must be whitespace, a single comma, or whitespace wrapped
	// comma (", "). Report precise problems otherwise.
	coreStart := relStart + leading
	for i := 0; i < len(trimmed); {
		r, size := decodeASCIIRune(trimmed[i:])
		switch {
		case isConstraintSpace(r):
			i += size
		case r == ',' && i == 0 && len(trimmed) == 1:
			i += size
		case r == ',':
			out.diagnostics = append(out.diagnostics, ConstraintDiagnostic{
				Kind:    DiagInvalidSeparator,
				Message: "only one comma may separate comparisons",
				Span:    ConstraintSpan{Start: base + coreStart + i, End: base + coreStart + i + size},
				Branch:  branchIndex,
			})
			i += size
		case r > utf8.RuneSelf:
			out.diagnostics = append(out.diagnostics, ConstraintDiagnostic{
				Kind:    DiagInvalidSeparator,
				Message: fmt.Sprintf("non-ASCII whitespace or character %q is not a valid separator", string(r)),
				Span:    ConstraintSpan{Start: base + coreStart + i, End: base + coreStart + i + size},
				Branch:  branchIndex,
			})
			i += size
		default:
			j := i + 1
			for j < len(trimmed) {
				r2, size2 := decodeASCIIRune(trimmed[j:])
				if r2 == ',' || isConstraintSpace(r2) || r2 > utf8.RuneSelf {
					break
				}
				j += size2
			}
			out.diagnostics = append(out.diagnostics, syntaxDiagnostic(
				trimmed[i:j], base+coreStart+i, base+coreStart+j, branchIndex))
			i = j
		}
	}
}

func emitNonASCIIOnly(text string, relStart, base, branchIndex int, out *scannedBranch) {
	for i := 0; i < len(text); {
		r, size := decodeASCIIRune(text[i:])
		if !isConstraintSpace(r) {
			kind := DiagInvalidSeparator
			if r <= utf8.RuneSelf {
				kind = DiagSyntax
			}
			msg := fmt.Sprintf("unexpected %q in comparison separator", string(r))
			if r > utf8.RuneSelf {
				msg = fmt.Sprintf("non-ASCII whitespace or character %q is not a valid separator", string(r))
			}
			out.diagnostics = append(out.diagnostics, ConstraintDiagnostic{
				Kind:    kind,
				Message: msg,
				Span:    ConstraintSpan{Start: base + relStart + i, End: base + relStart + i + size},
				Branch:  branchIndex,
			})
		}
		i += size
	}
}

func syntaxDiagnostic(text string, start, end, branchIndex int) ConstraintDiagnostic {
	return ConstraintDiagnostic{
		Kind:    DiagSyntax,
		Message: fmt.Sprintf("improper constraint: %q", text),
		Span:    ConstraintSpan{Start: start, End: end},
		Branch:  branchIndex,
	}
}

func onlyASCIISpace(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isASCIISpaceByte(s[i]) {
			return false
		}
	}
	return true
}

// indexNonASCIISeparator returns the byte index of the first non-ASCII rune in
// s, or -1.
func indexNonASCIISeparator(s string) int {
	for i := 0; i < len(s); {
		r, size := decodeASCIIRune(s[i:])
		if r > utf8.RuneSelf {
			return i
		}
		i += size
	}
	return -1
}

func isASCIISpaceByte(b byte) bool {
	switch b {
	case ' ', '\t', '\r', '\n', '\v', '\f':
		return true
	}
	return false
}

// isConstraintSpace reports whether r is one of the ASCII whitespace runes
// the constraint grammar accepts. Multi-byte whitespace is intentionally
// rejected.
func isConstraintSpace(r rune) bool {
	switch r {
	case ' ', '\t', '\r', '\n', '\v', '\f':
		return true
	}
	return false
}

// rewriteRangeValid reports whether a branch remains valid after the legacy
// hyphen range rewrite is applied.
func rewriteRangeValid(text string) bool {
	rewritten := rewriteRange(text)
	return validConstraintRegex.MatchString(rewritten)
}

func decodeASCIIRune(s string) (rune, int) {
	if len(s) == 0 {
		return 0, 0
	}
	if s[0] < utf8.RuneSelf {
		return rune(s[0]), 1
	}
	return utf8.DecodeRuneInString(s)
}

// diagnoseItemVersion inspects an item match whose parseConstraint call
// failed and returns the most precise diagnostic available. match indexes are
// relative to itemText; matchBase is the original-input offset of itemText.
func diagnoseItemVersion(match []int, itemText string, matchBase, branchIndex int) ConstraintDiagnostic {
	if d, ok := diagnoseNumericSegments(match, itemText, matchBase, branchIndex); ok {
		return d
	}
	if d, ok := diagnosePrereleaseOrMeta(match, itemText, matchBase, branchIndex); ok {
		return d
	}

	return ConstraintDiagnostic{
		Kind:    DiagSyntax,
		Message: fmt.Sprintf("improper constraint: %q", strings.TrimSpace(itemText)),
		Span:    ConstraintSpan{Start: matchBase + match[0], End: matchBase + match[1]},
		Branch:  branchIndex,
	}
}

// diagnoseBound diagnoses a hyphen-range bound that failed to parse. The
// bound text is matched with the item expression to locate the failing part.
func diagnoseBound(text string, base, branchIndex int) ConstraintDiagnostic {
	if m := locatedItemRegexp.re.FindStringSubmatchIndex(text); m != nil {
		return diagnoseItemVersion(m, text, base, branchIndex)
	}
	return syntaxDiagnostic(text, base, base+len(text), branchIndex)
}

// diagnoseNumericSegments reports the first major/minor/patch segment that
// cannot be represented as a uint64.
func diagnoseNumericSegments(match []int, text string, base, branchIndex int) (ConstraintDiagnostic, bool) {
	for _, name := range []string{"ver_major", "ver_minor", "ver_patch"} {
		s, e, ok := locatedItemRegexp.sub(match, name)
		if !ok {
			continue
		}
		digits := strings.TrimPrefix(text[s:e], ".")
		if digits == "" || isX(digits) {
			continue
		}
		if _, err := strconv.ParseUint(digits, 10, 64); errors.Is(err, strconv.ErrRange) {
			return ConstraintDiagnostic{
				Kind:    DiagNumberOutOfRange,
				Message: fmt.Sprintf("version segment %q is out of range", digits),
				Span:    ConstraintSpan{Start: base + s, End: base + e},
				Branch:  branchIndex,
			}, true
		}
	}
	return ConstraintDiagnostic{}, false
}

// diagnosePrereleaseOrMeta reports a malformed prerelease or build-metadata
// segment, preferring prerelease when both are present.
func diagnosePrereleaseOrMeta(match []int, text string, base, branchIndex int) (ConstraintDiagnostic, bool) {
	if s, e, ok := locatedItemRegexp.sub(match, "ver_pre"); ok {
		if err := validatePrerelease(text[s:e]); err != nil {
			return ConstraintDiagnostic{
				Kind:    DiagInvalidVersion,
				Message: fmt.Sprintf("invalid prerelease %q: %s", text[s:e], err),
				Span:    ConstraintSpan{Start: base + s, End: base + e},
				Branch:  branchIndex,
			}, true
		}
	}
	if s, e, ok := locatedItemRegexp.sub(match, "ver_meta"); ok {
		if err := validateMetadata(text[s:e]); err != nil {
			return ConstraintDiagnostic{
				Kind:    DiagInvalidVersion,
				Message: fmt.Sprintf("invalid metadata %q: %s", text[s:e], err),
				Span:    ConstraintSpan{Start: base + s, End: base + e},
				Branch:  branchIndex,
			}, true
		}
	}
	return ConstraintDiagnostic{}, false
}
