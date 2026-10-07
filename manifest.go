package wazero

import (
	"fmt"
	"slices"
	"strings"

	"github.com/tetratelabs/wazero/api"
	experimentalapi "github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/internal/wasm"
	binaryformat "github.com/tetratelabs/wazero/internal/wasm/binary"
)

// knownFeatures is every feature bit wazero implements, including the
// experimental ones. DecodeModuleManifest rejects any bit outside this mask.
const knownFeatures = api.CoreFeaturesV2 |
	experimentalapi.CoreFeaturesThreads |
	experimentalapi.CoreFeaturesTailCall |
	experimentalapi.CoreFeaturesExtendedConst |
	experimentalapi.CoreFeaturesExceptionHandling |
	experimentalapi.CoreFeaturesTypedFunctionReferences

// FuncSignature describes the parameter and result value types of an imported
// or exported function.
type FuncSignature struct {
	// Params are the value types of the function parameters.
	Params []api.ValueType
	// Results are the value types of the function results.
	Results []api.ValueType
}

// String implements fmt.Stringer, e.g. "(i32, i64) -> (f32)".
func (s *FuncSignature) String() string {
	var b strings.Builder
	b.WriteByte('(')
	writeValueTypes(&b, s.Params)
	b.WriteString(") -> (")
	writeValueTypes(&b, s.Results)
	b.WriteByte(')')
	return b.String()
}

func writeValueTypes(b *strings.Builder, types []api.ValueType) {
	for i, t := range types {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(api.ValueTypeName(t))
	}
}

// MemoryLimits describes the limits of a memory, in pages of 64KB.
type MemoryLimits struct {
	// MinPages is the minimum memory size in pages.
	MinPages uint32
	// MaxPages is the maximum memory size in pages. Only meaningful when
	// HasMax is true.
	MaxPages uint32
	// HasMax is true when a maximum was explicitly encoded.
	HasMax bool
	// Shared is true when the memory is shared between agents (threads).
	Shared bool
}

// TableLimits describes the limits and element type of a table.
type TableLimits struct {
	// Min is the minimum number of elements.
	Min uint32
	// Max is the maximum number of elements. Only meaningful when HasMax is
	// true.
	Max uint32
	// HasMax is true when a maximum was explicitly encoded.
	HasMax bool
	// Type is the element reference type, e.g. api.ValueTypeFuncref.
	Type api.ValueType
}

// GlobalType describes the value type and mutability of a global.
type GlobalType struct {
	// ValType is the value type of the global, e.g. api.ValueTypeI32.
	ValType api.ValueType
	// Mutable is true when the global can be overwritten.
	Mutable bool
}

// ImportManifest is a read-only description of one host capability a
// WebAssembly module requests via its import section.
//
// Exactly one of Func, Memory, Table or Global is non-nil, according to Type.
type ImportManifest struct {
	// Index is the index of this import within its own Type. For example,
	// the second imported function has Index 1, regardless of how many
	// memories or globals are imported in between. Imports sharing the same
	// module and name keep their distinct indices.
	Index uint32
	// Module is the primary namespace of this import, e.g. "env".
	Module string
	// Name is the secondary namespace of this import, e.g. "getpid".
	Name string
	// Type is the kind of this import, e.g. api.ExternTypeFunc.
	Type api.ExternType
	// Func is the requested signature when Type is api.ExternTypeFunc.
	Func *FuncSignature
	// Memory is the requested limits when Type is api.ExternTypeMemory.
	Memory *MemoryLimits
	// Table is the requested limits when Type is api.ExternTypeTable.
	Table *TableLimits
	// Global is the requested type when Type is api.ExternTypeGlobal.
	Global *GlobalType
}

// String implements fmt.Stringer.
func (i ImportManifest) String() string {
	prefix := fmt.Sprintf("%s[%s.%s]", api.ExternTypeName(i.Type), i.Module, i.Name)
	switch i.Type {
	case api.ExternTypeFunc:
		return fmt.Sprintf("%s %s", prefix, i.Func)
	case api.ExternTypeMemory:
		return fmt.Sprintf("%s %s", prefix, i.Memory.limitsString("pages"))
	case api.ExternTypeTable:
		return fmt.Sprintf("%s %s type=%s", prefix, i.Table.limitsString("elements"), api.ValueTypeName(i.Table.Type))
	case api.ExternTypeGlobal:
		mut := "const"
		if i.Global.Mutable {
			mut = "mut"
		}
		return fmt.Sprintf("%s %s %s", prefix, mut, api.ValueTypeName(i.Global.ValType))
	}
	return prefix
}

func (m *MemoryLimits) limitsString(unit string) string {
	max := "none"
	if m.HasMax {
		max = fmt.Sprintf("%d", m.MaxPages)
	}
	shared := ""
	if m.Shared {
		shared = " shared"
	}
	return fmt.Sprintf("min=%d max=%s %s%s", m.MinPages, max, unit, shared)
}

func (t *TableLimits) limitsString(unit string) string {
	max := "none"
	if t.HasMax {
		max = fmt.Sprintf("%d", t.Max)
	}
	return fmt.Sprintf("min=%d max=%s %s", t.Min, max, unit)
}

// ModuleManifest is a read-only list of every host capability a WebAssembly
// module imports. It is produced by DecodeModuleManifest without compiling,
// instantiating or executing any part of the module, including its start
// function.
type ModuleManifest struct {
	// Imports are all imports in the order they appear in the import
	// section. Duplicate module/name pairs are preserved as separate
	// entries, each with its own Index.
	Imports []ImportManifest
}

// DecodeModuleManifest decodes the WebAssembly binary (%.wasm) and returns
// the manifest of host capabilities it imports, without compiling or
// instantiating the module, and without executing its start function.
//
// The features parameter controls which WebAssembly proposals are accepted,
// exactly like RuntimeConfig.WithCoreFeatures: a module relying on a disabled
// feature is rejected. Bits outside every feature wazero implements are
// unknown and also rejected.
//
// An error is returned if the binary is malformed, uses disabled or unknown
// features, references an invalid type index, or imports a kind wazero
// cannot describe (e.g. exception-handling tags).
func DecodeModuleManifest(bin []byte, features api.CoreFeatures) (*ModuleManifest, error) {
	if unknown := features &^ knownFeatures; unknown != 0 {
		return nil, fmt.Errorf("manifest: unknown features: %#x", uint64(unknown))
	}
	m, err := binaryformat.DecodeModule(bin, features, wasm.MemoryLimitPages, false, false, false)
	if err != nil {
		return nil, err
	}
	manifest := &ModuleManifest{}
	for idx := range m.ImportSection {
		imp := &m.ImportSection[idx]
		entry := ImportManifest{
			Index:  imp.IndexPerType,
			Module: imp.Module,
			Name:   imp.Name,
			Type:   imp.Type,
		}
		switch imp.Type {
		case wasm.ExternTypeFunc:
			if imp.DescFunc >= uint32(len(m.TypeSection)) {
				return nil, fmt.Errorf("manifest: import[%d] func[%s.%s]: invalid type index %d, but only %d types are defined",
					idx, imp.Module, imp.Name, imp.DescFunc, len(m.TypeSection))
			}
			ft := &m.TypeSection[imp.DescFunc]
			params, err := toAPIValueTypes(ft.Params)
			if err != nil {
				return nil, fmt.Errorf("manifest: import[%d] func[%s.%s]: %w", idx, imp.Module, imp.Name, err)
			}
			results, err := toAPIValueTypes(ft.Results)
			if err != nil {
				return nil, fmt.Errorf("manifest: import[%d] func[%s.%s]: %w", idx, imp.Module, imp.Name, err)
			}
			entry.Func = &FuncSignature{Params: params, Results: results}
		case wasm.ExternTypeMemory:
			entry.Memory = &MemoryLimits{
				MinPages: imp.DescMem.Min,
				MaxPages: imp.DescMem.Max,
				HasMax:   imp.DescMem.IsMaxEncoded,
				Shared:   imp.DescMem.IsShared,
			}
		case wasm.ExternTypeTable:
			if imp.DescTable.Type.IsConcreteRef() {
				return nil, fmt.Errorf("manifest: import[%d] table[%s.%s]: unsupported concrete reference element type",
					idx, imp.Module, imp.Name)
			}
			t := &TableLimits{Min: imp.DescTable.Min, Type: api.ValueType(byte(imp.DescTable.Type))}
			if imp.DescTable.Max != nil {
				t.Max, t.HasMax = *imp.DescTable.Max, true
			}
			entry.Table = t
		case wasm.ExternTypeGlobal:
			if imp.DescGlobal.ValType.IsConcreteRef() {
				return nil, fmt.Errorf("manifest: import[%d] global[%s.%s]: unsupported concrete reference value type",
					idx, imp.Module, imp.Name)
			}
			entry.Global = &GlobalType{
				ValType: api.ValueType(byte(imp.DescGlobal.ValType)),
				Mutable: imp.DescGlobal.Mutable,
			}
		default:
			return nil, fmt.Errorf("manifest: import[%d] %s[%s.%s]: unsupported import kind",
				idx, api.ExternTypeName(imp.Type), imp.Module, imp.Name)
		}
		manifest.Imports = append(manifest.Imports, entry)
	}
	return manifest, nil
}

// toAPIValueTypes converts internal value types to their public
// representation, rejecting concrete reference types which have none.
func toAPIValueTypes(types []wasm.ValueType) ([]api.ValueType, error) {
	result := make([]api.ValueType, len(types))
	for i, t := range types {
		if t.IsConcreteRef() {
			return nil, fmt.Errorf("unsupported concrete reference value type: %s", wasm.ValueTypeName(t))
		}
		result[i] = api.ValueType(byte(t))
	}
	return result, nil
}

// ImportAllowlist is a caller-provided set of host capabilities. Use it with
// ModuleManifest.Check to verify a module only requests capabilities the host
// is willing to provide.
//
// Each entry is keyed by its module and name: an entry is not a wildcard. An
// import matching an entry's name but not its kind, signature or limits is
// reported as a type mismatch, not as allowed.
type ImportAllowlist struct {
	entries map[allowlistKey]ImportManifest
}

type allowlistKey struct {
	module, name string
}

// NewImportAllowlist returns an empty allowlist.
func NewImportAllowlist() *ImportAllowlist {
	return &ImportAllowlist{entries: map[allowlistKey]ImportManifest{}}
}

// AllowFunc allows importing the function module.name with exactly the given
// signature. A different signature on the same name is a type mismatch.
func (l *ImportAllowlist) AllowFunc(module, name string, params, results []api.ValueType) *ImportAllowlist {
	l.entries[allowlistKey{module, name}] = ImportManifest{
		Module: module, Name: name, Type: api.ExternTypeFunc,
		Func: &FuncSignature{Params: slices.Clone(params), Results: slices.Clone(results)},
	}
	return l
}

// AllowMemory allows importing the memory module.name, provided the import's
// requirements fit within the given limits: the import's minimum must not
// exceed the allowed minimum, and any maximum the import declares must be at
// least the allowed maximum. A shared import requires a shared entry.
func (l *ImportAllowlist) AllowMemory(module, name string, limits MemoryLimits) *ImportAllowlist {
	l.entries[allowlistKey{module, name}] = ImportManifest{
		Module: module, Name: name, Type: api.ExternTypeMemory,
		Memory: &limits,
	}
	return l
}

// AllowTable allows importing the table module.name, provided the import's
// element type equals limits.Type and its requirements fit within the given
// limits, following the same rules as AllowMemory.
func (l *ImportAllowlist) AllowTable(module, name string, limits TableLimits) *ImportAllowlist {
	l.entries[allowlistKey{module, name}] = ImportManifest{
		Module: module, Name: name, Type: api.ExternTypeTable,
		Table: &limits,
	}
	return l
}

// AllowGlobal allows importing the global module.name with exactly the given
// value type and mutability.
func (l *ImportAllowlist) AllowGlobal(module, name string, valType api.ValueType, mutable bool) *ImportAllowlist {
	l.entries[allowlistKey{module, name}] = ImportManifest{
		Module: module, Name: name, Type: api.ExternTypeGlobal,
		Global: &GlobalType{ValType: valType, Mutable: mutable},
	}
	return l
}

// ImportTypeMismatch describes an import whose module and name appear in the
// allowlist, but whose kind, signature or limits do not match the entry.
type ImportTypeMismatch struct {
	// Import is the requested capability that failed to match.
	Import ImportManifest
	// Reason describes why the allowlist entry did not satisfy the import.
	Reason string
}

// ManifestCheck is the result of comparing a ModuleManifest against an
// ImportAllowlist. Missing capabilities and type mismatches are reported
// separately.
type ManifestCheck struct {
	// Missing are imports with no allowlist entry for their module and name.
	Missing []ImportManifest
	// Mismatched are imports whose allowlist entry exists but does not
	// satisfy the requested kind, signature or limits.
	Mismatched []ImportTypeMismatch
}

// OK returns true when every import is covered by a compatible allowlist
// entry.
func (c *ManifestCheck) OK() bool {
	return len(c.Missing) == 0 && len(c.Mismatched) == 0
}

// Error implements error, summarizing both categories.
func (c *ManifestCheck) Error() string {
	var b strings.Builder
	if len(c.Missing) > 0 {
		b.WriteString("missing capabilities:")
		for _, imp := range c.Missing {
			b.WriteString("\n\t")
			b.WriteString(imp.String())
		}
	}
	if len(c.Mismatched) > 0 {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("type mismatches:")
		for _, mm := range c.Mismatched {
			b.WriteString("\n\t")
			b.WriteString(mm.Import.String())
			b.WriteString(": ")
			b.WriteString(mm.Reason)
		}
	}
	return b.String()
}

// Check compares every import in this manifest against the allowlist and
// reports capabilities not present separately from entries whose types do
// not match. The manifest and the allowlist are not modified.
func (m *ModuleManifest) Check(allowlist *ImportAllowlist) *ManifestCheck {
	result := &ManifestCheck{}
	for _, imp := range m.Imports {
		entry, ok := allowlist.entries[allowlistKey{imp.Module, imp.Name}]
		if !ok {
			result.Missing = append(result.Missing, imp)
			continue
		}
		if reason := matchImport(imp, entry); reason != "" {
			result.Mismatched = append(result.Mismatched, ImportTypeMismatch{Import: imp, Reason: reason})
		}
	}
	return result
}

// matchImport returns an empty string when the allowlist entry satisfies the
// import, or a human-readable reason otherwise.
func matchImport(imp, entry ImportManifest) string {
	if imp.Type != entry.Type {
		return fmt.Sprintf("kind mismatch: import is %s, but allowlist entry is %s",
			api.ExternTypeName(imp.Type), api.ExternTypeName(entry.Type))
	}
	switch imp.Type {
	case api.ExternTypeFunc:
		want, got := imp.Func, entry.Func
		if !slices.Equal(want.Params, got.Params) || !slices.Equal(want.Results, got.Results) {
			return fmt.Sprintf("signature mismatch: import requires %s, but allowlist entry is %s", want, got)
		}
	case api.ExternTypeMemory:
		return matchLimits(
			limitReq{min: imp.Memory.MinPages, max: imp.Memory.MaxPages, hasMax: imp.Memory.HasMax, shared: imp.Memory.Shared},
			limitReq{min: entry.Memory.MinPages, max: entry.Memory.MaxPages, hasMax: entry.Memory.HasMax, shared: entry.Memory.Shared},
			"pages")
	case api.ExternTypeTable:
		if imp.Table.Type != entry.Table.Type {
			return fmt.Sprintf("table element type mismatch: import requires %s, but allowlist entry is %s",
				api.ValueTypeName(imp.Table.Type), api.ValueTypeName(entry.Table.Type))
		}
		return matchLimits(
			limitReq{min: imp.Table.Min, max: imp.Table.Max, hasMax: imp.Table.HasMax},
			limitReq{min: entry.Table.Min, max: entry.Table.Max, hasMax: entry.Table.HasMax},
			"elements")
	case api.ExternTypeGlobal:
		if imp.Global.ValType != entry.Global.ValType {
			return fmt.Sprintf("global type mismatch: import requires %s, but allowlist entry is %s",
				api.ValueTypeName(imp.Global.ValType), api.ValueTypeName(entry.Global.ValType))
		}
		if imp.Global.Mutable != entry.Global.Mutable {
			return fmt.Sprintf("global mutability mismatch: import requires mutable=%t, but allowlist entry is mutable=%t",
				imp.Global.Mutable, entry.Global.Mutable)
		}
	}
	return ""
}

// limitReq is a normalized view of MemoryLimits or TableLimits requirements.
type limitReq struct {
	min, max uint32
	hasMax   bool
	shared   bool
}

// matchLimits checks that the allowed limits can satisfy the required ones,
// mirroring the import resolution rules used at instantiation time.
func matchLimits(required, allowed limitReq, unit string) string {
	if required.shared && !allowed.shared {
		return "import requires a shared memory, but allowlist entry is not shared"
	}
	if required.min > allowed.min {
		return fmt.Sprintf("minimum size mismatch: import requires at least %d %s, but allowlist entry provides %d",
			required.min, unit, allowed.min)
	}
	if required.hasMax {
		if !allowed.hasMax {
			return fmt.Sprintf("import requires a maximum of %d %s, but allowlist entry has no maximum",
				required.max, unit)
		}
		if allowed.max > required.max {
			return fmt.Sprintf("maximum size mismatch: import requires at most %d %s, but allowlist entry allows up to %d",
				required.max, unit, allowed.max)
		}
	}
	return ""
}
