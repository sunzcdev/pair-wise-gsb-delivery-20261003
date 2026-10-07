// Package importmanifest provides a read-only inventory of the host
// capabilities requested by a WebAssembly module through its imports.
//
// Unlike instantiation, reading a manifest never executes module code: the
// module is not instantiated and its start function is never run. Only the
// portions of the binary required to describe imports (the type and import
// sections) are inspected.
//
// The resulting [Manifest] lists every imported module name, member name,
// external kind and, depending on the kind, the exact function signature or
// the memory/table limits. [Manifest.Check] compares that inventory against
// an [Allowlist] supplied by the caller and reports missing capabilities and
// type or limit mismatches as distinct findings.
package importmanifest

import (
	"fmt"

	"github.com/tetratelabs/wazero/api"
)

// Kind classifies an imported external. It mirrors the WebAssembly import
// description byte (importdesc).
type Kind = api.ExternType

// Re-exported external kinds so callers do not need a second import.
const (
	KindFunc   = api.ExternTypeFunc
	KindTable  = api.ExternTypeTable
	KindMemory = api.ExternTypeMemory
	KindGlobal = api.ExternTypeGlobal
)

// Limits describes the minimum and optional maximum declared by an imported
// memory or table. Max is nil when the binary declares no maximum.
type Limits struct {
	Min uint32
	// Max is nil when no maximum was encoded.
	Max *uint32
	// Shared is true for shared linear memory (the threads proposal).
	Shared bool
}

func (l Limits) String() string {
	if l.Max != nil {
		if l.Shared {
			return fmt.Sprintf("{min=%d max=%d shared}", l.Min, *l.Max)
		}
		return fmt.Sprintf("{min=%d max=%d}", l.Min, *l.Max)
	}
	if l.Shared {
		return fmt.Sprintf("{min=%d shared}", l.Min)
	}
	return fmt.Sprintf("{min=%d}", l.Min)
}

// Import is a single entry in a module's import section.
//
// Index is the position of the import within the import section. Duplicate
// imports that share the same module and member name keep distinct indexes
// and are therefore reported independently.
type Import struct {
	// Module is the importing module name (first import field).
	Module string
	// Name is the imported member name (second import field).
	Name string
	// Kind is the external kind: func, table, memory or global.
	Kind Kind

	// Index is the import-section position, preserving duplicates.
	Index uint32
	// IndexPerKind is the index within this Kind's index space. For example
	// the second imported function has IndexPerKind == 1 even if globals are
	// interleaved before it.
	IndexPerKind uint32

	// FuncType is the index into the module type section for KindFunc. It is
	// meaningless for the other kinds.
	FuncType uint32
	// Signature is populated for KindFunc by resolving FuncType against the
	// type section. It is nil for other kinds.
	Signature *FunctionSignature

	// GlobalType is populated for KindGlobal.
	GlobalType *GlobalType
	// Memory is populated for KindMemory.
	Memory *Limits
	// Table is populated for KindTable.
	Table *Table
}

// FunctionSignature is the resolved (params) -> (results) type of an imported
// function. Two signatures match only when their parameter and result type
// sequences are identical, so a shared member name never doubles as a pass
// for an arbitrary signature.
type FunctionSignature struct {
	Params  []api.ValueType
	Results []api.ValueType
}

// GlobalType describes an imported global: its value type and mutability.
type GlobalType struct {
	ValueType api.ValueType
	Mutable   bool
}

func (g GlobalType) String() string {
	if g.Mutable {
		return "mut " + api.ValueTypeName(g.ValueType)
	}
	return "const " + api.ValueTypeName(g.ValueType)
}

// Table describes an imported table: its element type and limits.
type Table struct {
	ElementType api.ValueType
	Limits
}

func (t Table) String() string {
	return fmt.Sprintf("%s %s", api.ValueTypeName(t.ElementType), t.Limits.String())
}

// String renders one import as "index: kind module.name descriptor", e.g.
// "2: func env.abort (i32, i32) -> ()". It is intended for showing plugin
// operators exactly which host capabilities an upload requests.
func (imp Import) String() string {
	descriptor := ""
	switch imp.Kind {
	case KindFunc:
		if imp.Signature != nil {
			descriptor = " " + imp.Signature.String()
		} else {
			descriptor = fmt.Sprintf(" type[%d]", imp.FuncType)
		}
	case KindGlobal:
		if imp.GlobalType != nil {
			descriptor = " " + imp.GlobalType.String()
		}
	case KindMemory:
		if imp.Memory != nil {
			descriptor = " " + imp.Memory.String()
		}
	case KindTable:
		if imp.Table != nil {
			descriptor = " " + imp.Table.String()
		}
	}
	return fmt.Sprintf("[%d] %s %s.%s%s",
		imp.Index, api.ExternTypeName(imp.Kind), imp.Module, imp.Name, descriptor)
}

// Manifest is the read-only inventory of everything a module imports.
type Manifest struct {
	// Imports preserves the order and exact indexes found in the binary.
	Imports []Import

	// PerModule groups imports by module name while retaining their original
	// order within each group.
	PerModule map[string][]Import
}

// Key identifies an imported member by its two-level namespace.
type Key struct {
	Module string
	Name   string
}

// FindingKind classifies a [Finding].
type FindingKind int

const (
	// FindingMissing means the allowlist does not grant the requested member.
	FindingMissing FindingKind = iota + 1
	// FindingTypeMismatch means the member is granted but its declared kind,
	// signature or limits do not satisfy the module's request.
	FindingTypeMismatch
)

// Finding is a single discrepancy between a requested import and the
// allowlist. Capability absence ([FindingMissing]) and type/limit mismatch
// ([FindingTypeMismatch]) are reported separately.
type Finding struct {
	// Index is the import-section index of the offending import.
	Index uint32
	Key
	// Kind is the requested external kind.
	Kind Kind
	// Reason is FindingMissing or FindingTypeMismatch.
	Reason FindingKind
	// Detail is a human-readable explanation.
	Detail string
}

func (f Finding) Error() string {
	switch f.Reason {
	case FindingMissing:
		return fmt.Sprintf("import[%d] %s %q.%q: capability not granted by allowlist",
			f.Index, api.ExternTypeName(f.Kind), f.Module, f.Name)
	default:
		return fmt.Sprintf("import[%d] %s %q.%q: type mismatch: %s",
			f.Index, api.ExternTypeName(f.Kind), f.Module, f.Name, f.Detail)
	}
}

// AllowEntry declares a single host capability the caller is willing to
// provide. Exactly one of Signature, GlobalType, Memory or Table must match
// the granted member's kind; use the constructor helpers ([AllowFunc],
// [AllowGlobal], [AllowMemory], [AllowTable]) for clarity.
type AllowEntry struct {
	Kind Kind

	Signature  *FunctionSignature
	GlobalType *GlobalType
	Memory     *Limits
	Table      *Table
}

// Allowlist maps every granted module/name pair to the exact capability the
// host provides.
type Allowlist map[Key]AllowEntry

// AllowFunc grants a function with the given parameter and result types.
func AllowFunc(params, results []api.ValueType) AllowEntry {
	return AllowEntry{Kind: KindFunc, Signature: &FunctionSignature{Params: params, Results: results}}
}

// AllowGlobal grants a global of the given type and mutability.
func AllowGlobal(valueType api.ValueType, mutable bool) AllowEntry {
	return AllowEntry{Kind: KindGlobal, GlobalType: &GlobalType{ValueType: valueType, Mutable: mutable}}
}

// AllowMemory grants a memory with the given limits.
func AllowMemory(min uint32, max *uint32, shared bool) AllowEntry {
	return AllowEntry{Kind: KindMemory, Memory: &Limits{Min: min, Max: max, Shared: shared}}
}

// AllowTable grants a table of the given element type and limits.
func AllowTable(elementType api.ValueType, min uint32, max *uint32) AllowEntry {
	return AllowEntry{Kind: KindTable, Table: &Table{ElementType: elementType, Limits: Limits{Min: min, Max: max}}}
}
