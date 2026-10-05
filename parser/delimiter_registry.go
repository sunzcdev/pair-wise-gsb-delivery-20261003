package parser

import (
	"fmt"

	"github.com/yuin/goldmark/v2/util"
)

// delimiterRegistry is the build-time table of all delimiter processors that
// participate in inline parsing. Entries are stored in selection order:
// ascending priority, with registration order breaking ties.
type delimiterRegistry struct {
	registrations []*DelimiterProcessorRegistration
}

// buildDelimiterRegistry assembles the registry from the priority-sorted
// inline parser list and returns diagnostics about fully shadowed
// registrations. The sorted list is the authority on selection order:
// ascending priority with registration order breaking ties.
//
// The returned diagnostics are drained by the caller after the parser
// initialization lock is released.
func buildDelimiterRegistry(sorted []util.PrioritizedValue[InlineParser]) []DelimiterDiagnostic {
	var registry delimiterRegistry
	for _, v := range sorted {
		r := registrationOf(v.Value)
		if r == nil {
			continue
		}
		registry.appendOnce(r)
	}
	return registry.diagnostics()
}

func registrationOf(ip InlineParser) *DelimiterProcessorRegistration {
	if adapter, ok := ip.(*delimiterProcessorParser); ok {
		return adapter.registration
	}
	if provider, ok := ip.(DelimiterProcessorProvider); ok {
		return provider.DelimiterProcessorRegistration()
	}
	return nil
}

// appendOnce registers r on its first appearance. The same registration may
// appear multiple times because inline parsers are indexed once per trigger
// character.
func (g *delimiterRegistry) appendOnce(r *DelimiterProcessorRegistration) bool {
	for _, existing := range g.registrations {
		if existing == r {
			return false
		}
	}
	g.registrations = append(g.registrations, r)
	return true
}

// diagnostics reports every (registration, trigger) pair that is fully
// shadowed: an earlier registration in selection order accepts a length window
// that covers the shadowed registration's window for the same trigger.
//
// Registrations that share a trigger but accept different run lengths are not
// shadowed and can coexist.
func (g *delimiterRegistry) diagnostics() []DelimiterDiagnostic {
	var diagnostics []DelimiterDiagnostic
	for _, r := range g.registrations {
		for _, c := range r.triggers {
			if shadower := g.findShadower(r, c); shadower != nil {
				diagnostics = append(diagnostics, DelimiterDiagnostic{
					Severity:     DelimiterDiagnosticWarning,
					Message:      shadowMessage(shadower, r, c),
					Registration: r,
					ShadowedBy:   shadower,
					Trigger:      c,
				})
			}
		}
	}
	return diagnostics
}

// findShadower returns the earliest earlier-in-selection-order registration
// whose accepted length window covers r's window for trigger c.
func (g *delimiterRegistry) findShadower(r *DelimiterProcessorRegistration, c byte) *DelimiterProcessorRegistration {
	for _, candidate := range g.registrations {
		if candidate == r {
			return nil
		}
		if !hasTrigger(candidate, c) {
			continue
		}
		if coversLengthWindow(candidate, r) {
			return candidate
		}
	}
	return nil
}

func hasTrigger(r *DelimiterProcessorRegistration, c byte) bool {
	for _, trigger := range r.triggers {
		if trigger == c {
			return true
		}
	}
	return false
}

// coversLengthWindow reports whether outer's accepted run-length window covers
// inner's window. A maxLength of 0 stands for the unbounded window.
func coversLengthWindow(outer, inner *DelimiterProcessorRegistration) bool {
	if outer.minLength > inner.minLength {
		return false
	}
	if outer.maxLength == 0 {
		return true
	}
	if inner.maxLength == 0 {
		return false
	}
	return outer.maxLength >= inner.maxLength
}

func shadowMessage(shadower, shadowed *DelimiterProcessorRegistration, c byte) string {
	return fmt.Sprintf(
		"delimiter processor %q is fully shadowed for trigger %q by earlier-in-selection processor %q "+
			"(priority %d, shadowed priority %d) whose accepted length window covers it; "+
			"the processor can never be selected",
		shadowed.name, string(c), shadower.name, shadower.priority, shadowed.priority)
}
