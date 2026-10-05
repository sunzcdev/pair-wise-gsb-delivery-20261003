package parser

import (
	"fmt"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/text"
	"github.com/yuin/goldmark/v2/util"
)

// DelimiterProcessorNamer is an optional interface that a
// [DelimiterProcessor] may implement to expose a stable diagnostic name.
//
// Processors that do not implement this interface receive a compatibility
// default name derived from their concrete type, such as
// "github.com/yuin/goldmark/v2/parser.emphasisDelimiterProcessor".
type DelimiterProcessorNamer interface {
	DelimiterProcessorName() string
}

// DelimiterProcessorName returns the diagnostic name of the given processor.
//
// If the processor implements [DelimiterProcessorNamer], its name is used.
// Otherwise a compatibility default name derived from the concrete type is
// returned.
func DelimiterProcessorName(p DelimiterProcessor) string {
	if p == nil {
		return ""
	}
	if named, ok := p.(DelimiterProcessorNamer); ok {
		if name := named.DelimiterProcessorName(); name != "" {
			return name
		}
	}
	return defaultProcessorName(p)
}

func defaultProcessorName(p DelimiterProcessor) string {
	t := reflect.TypeOf(p)
	if t == nil {
		return "delimiter-processor"
	}
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if name := t.Name(); name != "" {
		if pkg := t.PkgPath(); pkg != "" {
			return pkg + "." + name
		}
		return name
	}
	return "delimiter-processor"
}

// namedDelimiterProcessor wraps a [DelimiterProcessor] so that it keeps a
// stable registration-provided name even when the underlying processor does
// not implement [DelimiterProcessorNamer].
type namedDelimiterProcessor struct {
	DelimiterProcessor
	name string
}

func (p *namedDelimiterProcessor) DelimiterProcessorName() string {
	return p.name
}

// DelimiterProcessorRegistration describes a [DelimiterProcessor] together
// with the metadata that determines how the parser selects it when several
// inline extensions react to the same delimiter character.
type DelimiterProcessorRegistration struct {
	// name is a stable name used in build-time diagnostics and run-time traces.
	name string

	// priority decides which registration is tried first for a shared trigger
	// character. Lower values have higher precedence, matching
	// [util.Prioritized]. Registrations with equal priority keep their
	// registration order.
	priority int

	// triggers are the delimiter characters handled by this processor.
	triggers []byte

	// minLength and maxLength describe the delimiter-run lengths this processor
	// accepts. A maxLength of 0 means "no upper bound".
	minLength int
	maxLength int

	// processor is the wrapped [DelimiterProcessor].
	processor DelimiterProcessor

	// site and line locate the registration call site for diagnostics.
	site string
	line int
}

// Name returns the diagnostic name of the registration.
func (r *DelimiterProcessorRegistration) Name() string {
	return r.name
}

// Priority returns the registration priority.
func (r *DelimiterProcessorRegistration) Priority() int {
	return r.priority
}

// Triggers returns a copy of the trigger characters of the registration.
func (r *DelimiterProcessorRegistration) Triggers() []byte {
	return append([]byte(nil), r.triggers...)
}

// MinLength returns the minimum accepted delimiter-run length.
func (r *DelimiterProcessorRegistration) MinLength() int {
	return r.minLength
}

// MaxLength returns the maximum accepted delimiter-run length, or 0 when there
// is no upper bound.
func (r *DelimiterProcessorRegistration) MaxLength() int {
	return r.maxLength
}

// Processor returns the wrapped [DelimiterProcessor].
func (r *DelimiterProcessorRegistration) Processor() DelimiterProcessor {
	return r.processor
}

// acceptsLength reports whether a delimiter run of length n falls inside the
// declared [minLength, maxLength] window.
func (r *DelimiterProcessorRegistration) acceptsLength(n int) bool {
	if n < r.minLength {
		return false
	}
	return r.maxLength == 0 || n <= r.maxLength
}

// DelimiterProcessorOption customizes a
// [DelimiterProcessorRegistration] built by
// [NewDelimiterProcessorRegistration].
type DelimiterProcessorOption func(*DelimiterProcessorRegistration)

// WithDelimiterProcessorPriority sets the registration priority.
// Lower values have higher precedence. The default priority is 500.
func WithDelimiterProcessorPriority(priority int) DelimiterProcessorOption {
	return func(r *DelimiterProcessorRegistration) {
		r.priority = priority
	}
}

// WithDelimiterProcessorLengths restricts the accepted delimiter-run lengths
// to the inclusive [minLength, maxLength] window.
// A maxLength of 0 means there is no upper bound.
//
// Registrations that share a trigger character but accept different run
// lengths can coexist: the parser skips a processor whose length window does
// not cover the current run and continues with the next candidate.
func WithDelimiterProcessorLengths(minLength, maxLength int) DelimiterProcessorOption {
	return func(r *DelimiterProcessorRegistration) {
		r.minLength = minLength
		r.maxLength = maxLength
	}
}

// WithDelimiterProcessorName overrides the diagnostic name of a registration.
// By default the name provided to [NewDelimiterProcessorRegistration] is used,
// or a name derived from the processor type.
func WithDelimiterProcessorName(name string) DelimiterProcessorOption {
	return func(r *DelimiterProcessorRegistration) {
		r.name = name
	}
}

// WithDelimiterProcessorRegistrationSite overrides the source location
// recorded for the registration. This is mostly useful for built-in
// extensions that register processors from a shared factory.
func WithDelimiterProcessorRegistrationSite(file string, line int) DelimiterProcessorOption {
	return func(r *DelimiterProcessorRegistration) {
		r.site = file
		r.line = line
	}
}

const defaultDelimiterProcessorPriority = 500

// NewDelimiterProcessorRegistration creates a
// [DelimiterProcessorRegistration] for the given processor.
//
// name is used in diagnostics and traces; an empty name falls back to the
// processor's [DelimiterProcessorName]. triggers are the delimiter characters
// handled by the processor.
//
// The call site is captured automatically so that build-time diagnostics can
// point at the exact registration.
func NewDelimiterProcessorRegistration(name string, processor DelimiterProcessor, triggers []byte,
	opts ...DelimiterProcessorOption) *DelimiterProcessorRegistration {
	if processor == nil {
		panic("parser: NewDelimiterProcessorRegistration requires a non-nil processor")
	}
	site, line := callerSite(1)
	r := &DelimiterProcessorRegistration{
		name:      name,
		priority:  defaultDelimiterProcessorPriority,
		minLength: 1,
		maxLength: 0,
		triggers:  append([]byte(nil), triggers...),
		processor: processor,
		site:      site,
		line:      line,
	}
	for _, opt := range opts {
		opt(r)
	}
	if r.name == "" {
		r.name = DelimiterProcessorName(processor)
	}
	if r.minLength < 1 {
		panic(fmt.Sprintf("parser: delimiter processor %q has invalid min length %d", r.name, r.minLength))
	}
	if r.maxLength != 0 && r.maxLength < r.minLength {
		panic(fmt.Sprintf("parser: delimiter processor %q has max length %d below min length %d",
			r.name, r.maxLength, r.minLength))
	}
	r.triggers = uniqueSortedTriggers(r.triggers)
	r.processor = &namedDelimiterProcessor{DelimiterProcessor: processor, name: r.name}
	return r
}

func callerSite(skip int) (string, int) {
	if _, file, line, ok := runtime.Caller(skip + 1); ok {
		return filepath.ToSlash(file), line
	}
	return "", 0
}

func uniqueSortedTriggers(triggers []byte) []byte {
	if len(triggers) == 0 {
		return nil
	}
	seen := make(map[byte]struct{}, len(triggers))
	unique := make([]byte, 0, len(triggers))
	for _, c := range triggers {
		if _, ok := seen[c]; ok {
			continue
		}
		seen[c] = struct{}{}
		unique = append(unique, c)
	}
	for i := 1; i < len(unique); i++ {
		for j := i; j > 0 && unique[j-1] > unique[j]; j-- {
			unique[j-1], unique[j] = unique[j], unique[j-1]
		}
	}
	return unique
}

// DelimiterProcessorProvider is an optional interface implemented by inline
// parsers that own a [DelimiterProcessor]. The parser consults it when it
// builds the delimiter processor registry, so built-in extensions such as
// emphasis and strikethrough participate in diagnostics and traces without
// being re-registered.
type DelimiterProcessorProvider interface {
	DelimiterProcessorRegistration() *DelimiterProcessorRegistration
}

// DelimiterDiagnosticSeverity is the severity of a [DelimiterDiagnostic].
type DelimiterDiagnosticSeverity int

const (
	// DelimiterDiagnosticWarning reports a suspicious but valid configuration.
	DelimiterDiagnosticWarning DelimiterDiagnosticSeverity = iota
)

// String implements fmt.Stringer.
func (s DelimiterDiagnosticSeverity) String() string {
	switch s {
	case DelimiterDiagnosticWarning:
		return "warning"
	default:
		return "diagnostic"
	}
}

// DelimiterDiagnostic is a build-time diagnostic about the delimiter processor
// registry. It never contains source text, only registration metadata and a
// locatable call site.
type DelimiterDiagnostic struct {
	// Severity is the diagnostic severity.
	Severity DelimiterDiagnosticSeverity

	// Message is the human-readable diagnostic message.
	Message string

	// Registration is the registration that the diagnostic is reported for.
	Registration *DelimiterProcessorRegistration

	// ShadowedBy is the registration that makes Registration unreachable, when
	// applicable.
	ShadowedBy *DelimiterProcessorRegistration

	// Trigger is the delimiter character on which the conflict was detected.
	Trigger byte
}

// Location returns a "file:line" pointer to the registration call site, or an
// empty string when the site is unknown.
func (d *DelimiterDiagnostic) Location() string {
	if d.Registration == nil || d.Registration.site == "" {
		return ""
	}
	return d.Registration.site + ":" + strconv.Itoa(d.Registration.line)
}

// DelimiterDiagnosticHandler receives build-time [DelimiterDiagnostic]s.
type DelimiterDiagnosticHandler func(DelimiterDiagnostic)

type withDelimiterProcessors struct {
	registrations []*DelimiterProcessorRegistration
}

func (o *withDelimiterProcessors) setParserOption(c *Config) {
	regs := o.registrations
	for _, r := range regs {
		c.inlineParsers = append(c.inlineParsers,
			util.Prioritized[InlineParser](newDelimiterProcessorParser(r), r.priority))
	}
}

// WithDelimiterProcessors is a functional option that registers
// [DelimiterProcessorRegistration]s with the parser.
//
// Build-time diagnostics about fully shadowed registrations are reported when
// a handler is installed via [WithDelimiterDiagnostics].
func WithDelimiterProcessors(registrations ...*DelimiterProcessorRegistration) Option {
	return &withDelimiterProcessors{registrations: registrations}
}

type withDelimiterDiagnostics struct {
	handler DelimiterDiagnosticHandler
}

func (o *withDelimiterDiagnostics) setParserOption(c *Config) {
	c.delimiterDiagnostics = o.handler
}

// WithDelimiterDiagnostics is a functional option that installs a handler for
// build-time delimiter processor diagnostics. The handler is invoked at most
// once per parser, during the first [Parser.Parse] call and without holding
// the parser's internal initialization lock.
func WithDelimiterDiagnostics(handler DelimiterDiagnosticHandler) Option {
	return &withDelimiterDiagnostics{handler: handler}
}

// delimiterProcessorParser is the inline parser contributed by a
// [DelimiterProcessorRegistration]. It scans a run of one trigger character,
// verifies that the run length falls inside the declared window and otherwise
// declines so that lower-precedence candidates can be tried.
type delimiterProcessorParser struct {
	registration *DelimiterProcessorRegistration
}

func newDelimiterProcessorParser(r *DelimiterProcessorRegistration) *delimiterProcessorParser {
	return &delimiterProcessorParser{registration: r}
}

func (p *delimiterProcessorParser) Trigger() []byte {
	return p.registration.Triggers()
}

func (p *delimiterProcessorParser) Parse(_ ast.Node, block text.Reader, pc Context) ast.Node {
	line, segment := block.PeekLine()
	if len(line) == 0 {
		return nil
	}
	c := line[0]
	if !p.registration.processor.IsDelimiter(c) {
		return nil
	}
	n := 0
	for n < len(line) && line[n] == c {
		n++
	}
	if !p.registration.acceptsLength(n) {
		traceDelimiterRun(pc, DelimiterTraceRejected, p.registration, c, segment.Start, n,
			delimiterRejectLength(p.registration, n))
		return nil
	}
	return ParseDelimiter(block, p.registration.minLength, p.registration.processor, pc)
}
