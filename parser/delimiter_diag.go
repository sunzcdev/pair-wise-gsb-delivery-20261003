package parser

import (
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/yuin/goldmark/v2/util"
)

// DelimiterRunLengths is an optional interface implemented by an [InlineParser]
// that drives a [DelimiterProcessor]. It declares the delimiter run lengths
// (the number of consecutive trigger characters) that the processor accepts.
//
// A run length is accepted when minimum <= n <= maximum. Returning maximum < 0
// (or maximum < minimum) means that there is no upper bound, so every
// n >= minimum is accepted.
//
// Parsers that do not implement this interface are treated as accepting every
// run length (minimum 1, no upper bound). Two processors that share a trigger
// character but accept non-overlapping or partially overlapping run lengths can
// therefore coexist; only configurations in which one processor is completely
// shadowed on every (trigger, length) pair are reported.
type DelimiterRunLengths interface {
	DelimiterRunLengths() (minimum, maximum int)
}

// DelimiterProcessorSource is an optional interface implemented by an
// [InlineParser] that parses its delimiters through a [DelimiterProcessor].
// Build-time diagnostics and run-time traces use the returned processor to
// attribute trigger characters and decisions to a named handler.
type DelimiterProcessorSource interface {
	DelimiterProcessor() DelimiterProcessor
}

// DelimiterProcessorNamer is an optional interface that assigns a stable,
// human-readable name to a [DelimiterProcessor] or the [InlineParser] that
// drives it. Parsers and processors without a name receive a compatibility
// default name derived from their Go type.
type DelimiterProcessorNamer interface {
	DelimiterProcessorName() string
}

// DelimiterProcessorInfo describes a registered delimiter handler.
type DelimiterProcessorInfo struct {
	// Name is the registered name of the handler, either provided explicitly
	// via [DelimiterProcessorNamer] or derived from the Go type for
	// compatibility.
	Name string

	// Priority is the priority of the [InlineParser] that drives the handler.
	// Handlers with a smaller priority value are tried first.
	Priority int

	// Order is the zero-based registration order among handlers with the same
	// priority and acts as the tie-breaker: the first registered handler wins.
	Order int

	// Triggers are the trigger characters that reach the handler, sorted.
	Triggers []byte

	// MinimumLength and MaximumLength describe the delimiter run lengths
	// accepted by the handler. MaximumLength < 0 means no upper bound.
	MinimumLength int
	MaximumLength int
}

// coversLengths reports whether every run length accepted by other is also
// accepted by i.
func (i DelimiterProcessorInfo) coversLengths(other DelimiterProcessorInfo) bool {
	if other.MinimumLength < i.MinimumLength {
		return false
	}
	if i.MaximumLength < 0 {
		return true
	}
	if other.MaximumLength < 0 {
		return false
	}
	return other.MaximumLength <= i.MaximumLength
}

// DelimiterDiagnostic describes a build-time delimiter configuration problem:
// a handler that can never be selected because another earlier handler already
// handles every (trigger character, run length) combination it owns.
type DelimiterDiagnostic struct {
	// Handler is the fully shadowed handler.
	Handler DelimiterProcessorInfo

	// ShadowedBy is the earlier handler that wins on every combination.
	ShadowedBy DelimiterProcessorInfo

	// Triggers are the shared trigger characters that make the shadowing
	// observable, sorted.
	Triggers []byte
}

// Error implements the error interface.
func (d DelimiterDiagnostic) Error() string {
	tie := ""
	if d.Handler.Priority == d.ShadowedBy.Priority {
		tie = fmt.Sprintf(
			" (equal priority %d; handler %q registered first wins the tie-break)",
			d.Handler.Priority, d.ShadowedBy.Name)
	}
	return fmt.Sprintf(
		"delimiter processor %q (priority %d) is fully shadowed by %q (priority %d) on trigger %s; "+
			"it can never be selected%s",
		d.Handler.Name, d.Handler.Priority,
		d.ShadowedBy.Name, d.ShadowedBy.Priority,
		formatTriggerBytes(d.Triggers), tie)
}

func formatTriggerBytes(triggers []byte) string {
	if len(triggers) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(triggers))
	for _, c := range triggers {
		parts = append(parts, strconv.Quote(string([]byte{c})))
	}
	return strings.Join(parts, ",")
}

// delimiterRegistry is built once, when the parser is initialized, from the
// sorted inline parsers. It is read-only afterwards.
type delimiterRegistry struct {
	entries        []DelimiterProcessorInfo
	byInlineParser map[InlineParser]int
	byProcessor    map[DelimiterProcessor]int
	byTrigger      [256][]int
}

func (r *delimiterRegistry) infoForInlineParser(ip InlineParser) (int, DelimiterProcessorInfo) {
	if r == nil {
		return -1, DelimiterProcessorInfo{}
	}
	index, ok := r.byInlineParser[ip]
	if !ok {
		return -1, DelimiterProcessorInfo{}
	}
	return index, r.entries[index]
}

// countRunLength returns the number of leading bytes equal to c in line.
func countRunLength(line []byte, c byte) int {
	n := 0
	for n < len(line) && line[n] == c {
		n++
	}
	return n
}

func buildDelimiterRegistry(values util.PrioritizedValues[InlineParser]) *delimiterRegistry {
	registry := &delimiterRegistry{
		byInlineParser: map[InlineParser]int{},
		byProcessor:    map[DelimiterProcessor]int{},
	}
	usedNames := map[string]bool{}
	for _, value := range values {
		ip := value.Value
		processor, ok := ip.(DelimiterProcessorSource)
		if !ok {
			continue
		}
		triggers := append([]byte(nil), ip.Trigger()...)
		slices.Sort(triggers)
		triggers = slices.Compact(triggers)
		minimum, maximum := 1, -1
		if rl, ok := ip.(DelimiterRunLengths); ok {
			minimum, maximum = rl.DelimiterRunLengths()
			if minimum < 1 {
				minimum = 1
			}
			if maximum >= 0 && maximum < minimum {
				maximum = -1
			}
		}
		name := delimiterProcessorName(ip, processor.DelimiterProcessor(), usedNames)
		index := len(registry.entries)
		registry.entries = append(registry.entries, DelimiterProcessorInfo{
			Name:          name,
			Priority:      value.Priority,
			Order:         index,
			Triggers:      triggers,
			MinimumLength: minimum,
			MaximumLength: maximum,
		})
		registry.byInlineParser[ip] = index
		for _, c := range triggers {
			registry.byTrigger[c] = append(registry.byTrigger[c], index)
		}
		registry.byProcessor[processor.DelimiterProcessor()] = index
	}
	return registry
}

func delimiterProcessorName(ip InlineParser, processor DelimiterProcessor, used map[string]bool) string {
	name := ""
	if namer, ok := ip.(DelimiterProcessorNamer); ok {
		name = strings.TrimSpace(namer.DelimiterProcessorName())
	}
	if name == "" {
		if namer, ok := processor.(DelimiterProcessorNamer); ok {
			name = strings.TrimSpace(namer.DelimiterProcessorName())
		}
	}
	if name == "" {
		name = defaultProcessorName(processor)
	}
	base := name
	for suffix := 2; used[name]; suffix++ {
		name = base + "-" + strconv.Itoa(suffix)
	}
	used[name] = true
	return name
}

// defaultProcessorName derives a compatibility name from a processor's Go type.
func defaultProcessorName(processor DelimiterProcessor) string {
	t := reflect.TypeOf(processor)
	switch {
	case t == nil:
		return "delimiter"
	case t.Kind() == reflect.Ptr:
		t = t.Elem()
	}
	if name := t.Name(); name != "" {
		return name
	}
	return "delimiter"
}

// delimiterDiagnostics reports every handler that can never be selected
// because an earlier handler completely covers it.
func delimiterDiagnostics(entries []DelimiterProcessorInfo) []DelimiterDiagnostic {
	var diagnostics []DelimiterDiagnostic
	for i := range entries {
		handler := entries[i]
		for j := 0; j < i; j++ {
			other := entries[j]
			triggers := sharedCoveredTriggers(other, handler)
			if len(triggers) == 0 {
				continue
			}
			if len(triggers) != len(handler.Triggers) || !other.coversLengths(handler) {
				continue
			}
			diagnostics = append(diagnostics, DelimiterDiagnostic{
				Handler:    handler,
				ShadowedBy: other,
				Triggers:   triggers,
			})
			break
		}
	}
	return diagnostics
}

// sharedCoveredTriggers returns the triggers of handler that the earlier
// candidate also triggers. The caller additionally checks that every run
// length accepted by handler is covered by the earlier candidate.
func sharedCoveredTriggers(earlier, handler DelimiterProcessorInfo) []byte {
	var triggers []byte
	for _, c := range handler.Triggers {
		if !slices.Contains(earlier.Triggers, c) {
			continue
		}
		if earlier.coversLengths(handler) {
			triggers = append(triggers, c)
		}
	}
	return triggers
}

// DelimiterTraceStage identifies the selection stage a [DelimiterTraceEvent]
// belongs to.
type DelimiterTraceStage int

const (
	// DelimiterStageProcessor records which registered handler is tried for a
	// trigger character, in priority order.
	DelimiterStageProcessor DelimiterTraceStage = iota + 1

	// DelimiterStageCandidate records opener/closer candidate pairs examined by
	// [ProcessDelimiters].
	DelimiterStageCandidate
)

// DelimiterTraceResult is the outcome recorded by a [DelimiterTraceEvent].
type DelimiterTraceResult int

const (
	// DelimiterTried marks a candidate that is being evaluated. Every
	// DelimiterTried event is followed by a terminal DelimiterConsumed or
	// DelimiterRejected event for the same step.
	DelimiterTried DelimiterTraceResult = iota + 1

	// DelimiterRejected marks a candidate that was not selected.
	DelimiterRejected

	// DelimiterConsumed marks a candidate that produced an AST node.
	DelimiterConsumed
)

// Reasons explaining why a delimiter candidate was rejected.
const (
	// DelimiterReasonDeclined: the handler returned nil for the trigger.
	DelimiterReasonDeclined = "declined"

	// DelimiterReasonMismatch: opener and closer cannot be paired
	// (DelimiterProcessor.CanOpenCloser returned false).
	DelimiterReasonMismatch = "mismatch"

	// DelimiterReasonRule9: CommonMark Rule 9 (multiple-of-three) forbids the match.
	DelimiterReasonRule9 = "rule9"

	// DelimiterReasonNoOpener: no opener candidate was found for the closer.
	DelimiterReasonNoOpener = "no-opener"
)

// DelimiterTraceEvent describes a single delimiter selection decision.
//
// Events never include the parsed source text: only positions, characters,
// handler names, reasons and lengths are reported.
type DelimiterTraceEvent struct {
	// Sequence is a per-parse, zero-based monotonic event number.
	Sequence uint64

	// Stage is the selection stage.
	Stage DelimiterTraceStage

	// Result is the recorded outcome.
	Result DelimiterTraceResult

	// Handler is the registered name of the handler involved.
	Handler string

	// Char is the trigger or delimiter character.
	Char byte

	// Priority is the handler priority (processor stage only).
	Priority int

	// Position is the absolute source position of the trigger character
	// (processor stage) or the closer (candidate stage).
	Position int

	// OpenerPosition is the absolute source position of the opener candidate
	// (candidate stage only, -1 otherwise).
	OpenerPosition int

	// RunLength is the observed trigger run length (processor stage) or the
	// closer run length (candidate stage).
	RunLength int

	// Consumed is the number of characters taken from each side of a pair
	// (candidate stage) or consumed by a handler from the reader
	// (processor stage).
	Consumed int

	// OpenerRemaining is the opener run length left after a pair was consumed.
	OpenerRemaining int

	// CloserRemaining is the closer run length left after a pair was consumed.
	CloserRemaining int

	// Remaining is the number of unread source bytes at the time of the event.
	Remaining int

	// Reason explains rejections (empty for tried/consumed events).
	Reason string
}

// DelimiterTraceFunc receives delimiter selection events while parsing.
//
// The callback is never invoked while the parser holds an internal lock, so it
// is safe to record events from within the callback.
type DelimiterTraceFunc func(DelimiterTraceEvent)

// delimiterTrace is the per-parse trace state stored in the parse context.
type delimiterTrace struct {
	registry *delimiterRegistry
	sequence atomic.Uint64
	current  func() DelimiterTraceFunc
	active   *atomic.Bool
}

// enabled reports whether tracing is currently active. It is used on parsing
// hot paths to avoid building events (and resolving the callback) when tracing
// is disabled.
func (dt *delimiterTrace) enabled() bool {
	if dt == nil || dt.active == nil {
		return false
	}
	return dt.active.Load()
}

func (dt *delimiterTrace) emit(event DelimiterTraceEvent) {
	if dt == nil {
		return
	}
	fn := dt.fn()
	if fn == nil {
		return
	}
	event.Sequence = dt.sequence.Add(1) - 1
	fn(event)
}

func (dt *delimiterTrace) fn() DelimiterTraceFunc {
	if dt.current != nil {
		return dt.current()
	}
	return nil
}

// processorName resolves a [DelimiterProcessor] to its registered name.
func (dt *delimiterTrace) processorName(processor DelimiterProcessor) string {
	if dt != nil && dt.registry != nil {
		if index, ok := dt.registry.byProcessor[processor]; ok {
			return dt.registry.entries[index].Name
		}
	}
	return defaultProcessorName(processor)
}
