package participle

import (
	"fmt"
	"strings"
)

// String renders the report as a human readable, deterministic build log.
func (r *AnalysisReport) String() string {
	var sb strings.Builder
	sb.WriteString("Grammar analysis\n")
	sb.WriteString("================\n\n")
	sb.WriteString("Productions\n")
	sb.WriteString("-----------\n")
	for _, prod := range r.Productions {
		fmt.Fprintf(&sb, "%s (%s)\n", prod.Name, prod.Kind)
		fmt.Fprintf(&sb, "  nullable: %s\n", renderNullable(prod))
		fmt.Fprintf(&sb, "  first:    %s\n", renderFirst(prod))
		if len(prod.Paths) > 0 && !(len(prod.Paths) == 1 && prod.Paths[0] == "") {
			fmt.Fprintf(&sb, "  paths:    %s\n", strings.Join(prod.Paths, ", "))
		}
	}

	sb.WriteString("\nOrdered branches\n")
	sb.WriteString("----------------\n")
	if len(r.Diagnostics) == 0 {
		sb.WriteString("no findings\n")
	}
	for _, d := range r.Diagnostics {
		fmt.Fprintf(&sb, "[%s] %s: branches %d and %d (%s)\n",
			d.Kind, d.Production, d.Earlier, d.Later, d.Path)
		if len(d.Overlapping) > 0 {
			names := make([]string, 0, len(d.Overlapping))
			for _, token := range d.Overlapping {
				names = append(names, token.Name)
			}
			fmt.Fprintf(&sb, "  overlapping first tokens: %s\n", strings.Join(names, ", "))
		}
		if d.Unknown {
			sb.WriteString("  note: one branch uses custom parsing, so the finding may be incomplete\n")
		}
		fmt.Fprintf(&sb, "  %s\n", d.Message)
	}
	return sb.String()
}

func renderNullable(prod ProductionAnalysis) string {
	if !prod.NullableKnown {
		return "unknown (custom parsing)"
	}
	if prod.Nullable {
		return "yes"
	}
	return "no"
}

func renderFirst(prod ProductionAnalysis) string {
	if !prod.FirstKnown {
		return "unknown (custom parsing)"
	}
	if len(prod.First) == 0 {
		return "empty"
	}
	names := make([]string, 0, len(prod.First))
	for _, token := range prod.First {
		names = append(names, token.Name)
	}
	return strings.Join(names, ", ")
}
