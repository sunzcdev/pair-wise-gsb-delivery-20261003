package importmanifest

import (
	"errors"
	"testing"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

const (
	i32 = api.ValueTypeI32
	i64 = api.ValueTypeI64
	f32 = api.ValueTypeF32
	f64 = api.ValueTypeF64
)

func ptrU32(v uint32) *uint32 { return &v }

// TestRead_MixedImports verifies a module that mixes function, global, memory
// and table imports: every module/member/kind is listed with the resolved
// signature or limits, and kind-specific indexes advance independently.
func TestRead_MixedImports(t *testing.T) {
	b := newBuilder()
	abortType := b.functype([]byte{valI32, valI32}, []byte{valI32})
	procType := b.functype(nil, nil)

	b.importFunc("env", "abort", abortType) // func index 0
	b.importGlobal("env", "seed", valI64, true)
	b.importMemory("env", "memory", 1, ptrU32(4), false)
	b.importFunc("wasi_snapshot_preview1", "proc_exit", procType) // func index 1
	b.importTable("env", "callbacks", valFuncref, 2, ptrU32(8))

	m, err := Read(b.build(), api.CoreFeaturesV2)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if len(m.Imports) != 5 {
		t.Fatalf("expected 5 imports, got %d", len(m.Imports))
	}

	abort := m.Imports[0]
	if abort.Module != "env" || abort.Name != "abort" || abort.Kind != KindFunc {
		t.Fatalf("unexpected import[0]: %+v", abort)
	}
	if abort.FuncType != abortType || abort.Signature == nil {
		t.Fatalf("abort type not resolved: %+v", abort)
	}
	if got := abort.Signature.String(); got != "(i32, i32) -> (i32)" {
		t.Fatalf("abort signature = %q", got)
	}
	if abort.IndexPerKind != 0 {
		t.Fatalf("abort func index = %d, want 0", abort.IndexPerKind)
	}

	if seed := m.Imports[1]; seed.Kind != KindGlobal || seed.GlobalType.ValueType != i64 ||
		!seed.GlobalType.Mutable || seed.IndexPerKind != 0 {
		t.Fatalf("unexpected global: %+v", seed)
	}

	if mem := m.Imports[2]; mem.Kind != KindMemory || mem.Memory.Min != 1 ||
		mem.Memory.Max == nil || *mem.Memory.Max != 4 || mem.IndexPerKind != 0 {
		t.Fatalf("unexpected memory: %+v", mem)
	}

	if proc := m.Imports[3]; proc.Kind != KindFunc || proc.IndexPerKind != 1 {
		t.Fatalf("second function must have func index 1, got %+v", proc)
	}

	if tab := m.Imports[4]; tab.Kind != KindTable || tab.Table.ElementType != api.ValueType(valFuncref) ||
		tab.Table.Min != 2 || tab.Table.Max == nil || *tab.Table.Max != 8 || tab.IndexPerKind != 0 {
		t.Fatalf("unexpected table: %+v", tab)
	}

	if got := len(m.PerModule["env"]); got != 4 {
		t.Fatalf("env imports = %d, want 4", got)
	}
	if got := len(m.PerModule["wasi_snapshot_preview1"]); got != 1 {
		t.Fatalf("wasi imports = %d, want 1", got)
	}
}

// TestRead_DuplicateImportsKeepIndexes verifies repeated identical module/name
// pairs are retained as separate entries with their own import indexes and
// per-kind indexes.
func TestRead_DuplicateImportsKeepIndexes(t *testing.T) {
	b := newBuilder()
	t0 := b.functype([]byte{valI32}, nil)
	t1 := b.functype([]byte{valI64}, nil)
	b.importFunc("env", "same", t0)
	b.importFunc("env", "same", t1)

	m, err := Read(b.build(), api.CoreFeaturesV2)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(m.Imports) != 2 {
		t.Fatalf("expected both duplicate imports, got %d", len(m.Imports))
	}
	if m.Imports[0].Index != 0 || m.Imports[0].IndexPerKind != 0 {
		t.Fatalf("duplicate[0] indexes wrong: %+v", m.Imports[0])
	}
	if m.Imports[1].Index != 1 || m.Imports[1].IndexPerKind != 1 {
		t.Fatalf("duplicate[1] indexes wrong: %+v", m.Imports[1])
	}
	if m.Imports[0].Signature.Params[0] == m.Imports[1].Signature.Params[0] {
		t.Fatalf("duplicate signatures must remain distinct")
	}
}

// TestRead_BadFunctionTypeIndex ensures an import referencing a type index that
// the type section does not define is rejected rather than silently treated as
// any signature.
func TestRead_BadFunctionTypeIndex(t *testing.T) {
	b := newBuilder()
	b.functype(nil, nil) // only index 0 exists
	b.importFunc("env", "ghost", 7)

	_, err := Read(b.build(), api.CoreFeaturesV2)
	if !errors.Is(err, ErrInvalidBinary) {
		t.Fatalf("expected ErrInvalidBinary for bad type index, got %v", err)
	}
}

// TestRead_DoesNotInstantiate verifies that sections following the import
// section (including start) need not be well formed and are never executed: a
// junk tail after the import section is ignored.
func TestRead_DoesNotInstantiate(t *testing.T) {
	b := newBuilder()
	t0 := b.functype(nil, nil)
	b.importFunc("env", "f", t0)
	bin := append(b.build(), 0xde, 0xad, 0xbe, 0xef) // trailing garbage, not parsed

	m, err := Read(bin, api.CoreFeaturesV2)
	if err != nil {
		t.Fatalf("Read should stop after import section: %v", err)
	}
	if len(m.Imports) != 1 {
		t.Fatalf("expected 1 import, got %d", len(m.Imports))
	}
}

func TestRead_RejectsBadMagicAndTruncated(t *testing.T) {
	if _, err := Read([]byte{0, 0, 0, 0, 1, 0, 0, 0}, api.CoreFeaturesV2); !errors.Is(err, ErrInvalidBinary) {
		t.Fatalf("bad magic: %v", err)
	}
	if _, err := Read([]byte{0, 0x61, 0x73}, api.CoreFeaturesV2); !errors.Is(err, ErrInvalidBinary) {
		t.Fatalf("truncated header: %v", err)
	}
}

// TestRead_UnknownImportKindRejected verifies an unrecognized importdesc kind
// byte is refused rather than skipped.
func TestRead_UnknownImportKindRejected(t *testing.T) {
	b := newBuilder()
	b.rawImport("env", "x", 0x09)
	if _, err := Read(b.build(), api.CoreFeaturesV2); !errors.Is(err, ErrInvalidBinary) {
		t.Fatalf("unknown kind: %v", err)
	}
}

// TestRead_DisabledAndUnknownFeatures verifies that encodings which require a
// proposal fail when that feature is off, and succeed once enabled.
func TestRead_DisabledAndUnknownFeatures(t *testing.T) {
	// Shared memory requires threads.
	shared := newBuilder()
	shared.importMemory("env", "mem", 1, ptrU32(2), true)
	if _, err := Read(shared.build(), api.CoreFeaturesV2); !errors.Is(err, ErrFeatureDisabled) {
		t.Fatalf("shared memory without threads: %v", err)
	}
	if _, err := Read(shared.build(), api.CoreFeaturesV2|experimental.CoreFeaturesThreads); err != nil {
		t.Fatalf("shared memory with threads: %v", err)
	}

	// A v128 parameter requires SIMD.
	simd := newBuilder()
	simd.functype([]byte{valV128}, nil)
	simd.importFunc("env", "v", 0)
	if _, err := Read(simd.build(), api.CoreFeaturesV1); !errors.Is(err, ErrFeatureDisabled) {
		t.Fatalf("v128 without simd: %v", err)
	}

	// externref table requires reference-types.
	ext := newBuilder()
	ext.importTable("env", "t", valExternref, 0, nil)
	if _, err := Read(ext.build(), api.CoreFeaturesV1); !errors.Is(err, ErrFeatureDisabled) {
		t.Fatalf("externref without reference-types: %v", err)
	}

	// An unknown value-type byte is rejected outright.
	unk := newBuilder()
	unk.types = append(unk.types, 0x60)
	unk.types = appendU32(unk.types, 1)
	unk.types = append(unk.types, 0x55) // not a value type
	unk.types = appendU32(unk.types, 0)
	unk.typeCount = 1
	unk.importFunc("env", "f", 0)
	if _, err := Read(unk.build(), api.CoreFeaturesV2); !errors.Is(err, ErrInvalidBinary) {
		t.Fatalf("unknown value type: %v", err)
	}
}

// TestRead_NullableRefDesugaring verifies (ref null func)/(ref null extern)
// reduce to funcref/externref while still gating externref behind the feature.
func TestRead_NullableRefDesugaring(t *testing.T) {
	// externref table encoded with the long (ref null extern) form.
	tb := newBuilder()
	// Build the table import manually: kind 0x01, ref prefix 0x63, heap extern.
	tb.beginImport("env", "t")
	tb.imports = append(tb.imports, KindTable, refNullable)
	tb.imports = appendS33(tb.imports, heapExtern)
	tb.imports = appendLimits(tb.imports, 0, nil, false)
	tb.importCount++

	if _, err := Read(tb.build(), api.CoreFeaturesV1); !errors.Is(err, ErrFeatureDisabled) {
		t.Fatalf("nullable externref without reference-types: %v", err)
	}
	m, err := Read(tb.build(), api.CoreFeaturesV2)
	if err != nil {
		t.Fatalf("nullable externref with reference-types: %v", err)
	}
	if m.Imports[0].Table.ElementType != api.ValueTypeExternref {
		t.Fatalf("expected externref, got %x", m.Imports[0].Table.ElementType)
	}
}
