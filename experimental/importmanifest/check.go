package importmanifest

import (
	"fmt"
	"strings"

	"github.com/tetratelabs/wazero/api"
)

// Check compares every requested import against the caller-supplied allowlist.
//
// A member absent from the allowlist is reported as [FindingMissing]; a member
// that is present but whose kind, signature, mutability or limits do not
// satisfy the request is reported as [FindingTypeMismatch]. The two classes
// are never merged, so a single call distinguishes "capability not offered"
// from "capability offered with the wrong shape".
//
// Each duplicate import is checked independently and keeps its own import
// index; a repeated name can never be used as a wildcard for another
// signature. The returned findings are ordered by import index. A nil result
// means the module requests only granted capabilities.
func (m *Manifest) Check(allow Allowlist) Findings {
	if m == nil {
		return nil
	}
	var findings Findings
	for i := range m.Imports {
		req := &m.Imports[i]
		key := Key{Module: req.Module, Name: req.Name}

		grant, ok := allow[key]
		if !ok {
			findings = append(findings, Finding{
				Index: req.Index, Key: key, Kind: req.Kind, Reason: FindingMissing,
			})
			continue
		}

		if detail := matchImport(req, grant); detail != "" {
			findings = append(findings, Finding{
				Index: req.Index, Key: key, Kind: req.Kind,
				Reason: FindingTypeMismatch, Detail: detail,
			})
		}
	}
	return findings
}

// matchImport returns a non-empty explanation when the granted entry fails to
// satisfy the requested import; an empty string means it is compatible.
func matchImport(req *Import, grant AllowEntry) string {
	if req.Kind != grant.Kind {
		return fmt.Sprintf("requested %s but allowlist grants %s",
			api.ExternTypeName(req.Kind), api.ExternTypeName(grant.Kind))
	}

	switch req.Kind {
	case KindFunc:
		if grant.Signature == nil {
			return "allowlist entry for function has no signature"
		}
		return signatureMismatch(req.Signature, grant.Signature)
	case KindGlobal:
		if grant.GlobalType == nil {
			return "allowlist entry for global has no type"
		}
		return globalMismatch(req.GlobalType, grant.GlobalType)
	case KindMemory:
		if grant.Memory == nil {
			return "allowlist entry for memory has no limits"
		}
		return limitsMismatch("memory", req.Memory, grant.Memory)
	case KindTable:
		if grant.Table == nil {
			return "allowlist entry for table has no type"
		}
		if req.Table.ElementType != grant.Table.ElementType {
			return fmt.Sprintf("element type %s does not match granted %s",
				api.ValueTypeName(req.Table.ElementType), api.ValueTypeName(grant.Table.ElementType))
		}
		return limitsMismatch("table", &req.Table.Limits, &grant.Table.Limits)
	}
	return ""
}

func signatureMismatch(req, grant *FunctionSignature) string {
	if !valueTypesEqual(req.Params, grant.Params) {
		return fmt.Sprintf("params %s do not match granted %s", req.Params, grant.Params)
	}
	if !valueTypesEqual(req.Results, grant.Results) {
		return fmt.Sprintf("results %s do not match granted %s", req.Results, grant.Results)
	}
	return ""
}

func globalMismatch(req, grant *GlobalType) string {
	if req.ValueType != grant.ValueType {
		return fmt.Sprintf("value type %s does not match granted %s",
			api.ValueTypeName(req.ValueType), api.ValueTypeName(grant.ValueType))
	}
	// An immutable import cannot be backed by a mutable global (the module
	// expects a constant), and a mutable import cannot be backed by a constant.
	if req.Mutable != grant.Mutable {
		if req.Mutable {
			return "module requires a mutable global but an immutable one is granted"
		}
		return "module requires an immutable global but a mutable one is granted"
	}
	return ""
}

// limitsMismatch checks that the granted memory/table can satisfy the request:
// the host provides at least the requested minimum, any requested maximum fits
// within the granted maximum, and the shared attribute agrees.
func limitsMismatch(what string, req, grant *Limits) string {
	if req.Shared != grant.Shared {
		return fmt.Sprintf("%s shared=%v does not match granted shared=%v",
			what, req.Shared, grant.Shared)
	}
	if grant.Min < req.Min {
		return fmt.Sprintf("%s minimum %d exceeds granted minimum %d", what, req.Min, grant.Min)
	}
	if req.Max != nil {
		switch {
		case grant.Max == nil:
			return fmt.Sprintf("%s maximum %d exceeds granted unlimited maximum", what, *req.Max)
		case *grant.Max < *req.Max:
			return fmt.Sprintf("%s maximum %d exceeds granted maximum %d", what, *req.Max, *grant.Max)
		}
	}
	return ""
}

func valueTypesEqual(a, b []api.ValueType) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Findings is the ordered list of discrepancies reported by [Manifest.Check].
type Findings []Finding

// Allowed reports whether the check found no problems.
func (f Findings) Allowed() bool { return len(f) == 0 }

// Missing returns only the capability-absence findings.
func (f Findings) Missing() []Finding { return f.filter(FindingMissing) }

// Mismatches returns only the type/limit mismatch findings.
func (f Findings) Mismatches() []Finding { return f.filter(FindingTypeMismatch) }

func (f Findings) filter(kind FindingKind) []Finding {
	var out []Finding
	for _, x := range f {
		if x.Reason == kind {
			out = append(out, x)
		}
	}
	return out
}

func (f Findings) Error() string {
	if len(f) == 0 {
		return ""
	}
	parts := make([]string, len(f))
	for i := range f {
		parts[i] = f[i].Error()
	}
	return strings.Join(parts, "; ")
}

// String renders a signature as "(params...) -> (results...)".
func (s *FunctionSignature) String() string {
	return fmt.Sprintf("(%s) -> (%s)", valueTypeNames(s.Params), valueTypeNames(s.Results))
}

func valueTypeNames(ts []api.ValueType) string {
	names := make([]string, len(ts))
	for i, t := range ts {
		names[i] = api.ValueTypeName(t)
	}
	return strings.Join(names, ", ")
}
