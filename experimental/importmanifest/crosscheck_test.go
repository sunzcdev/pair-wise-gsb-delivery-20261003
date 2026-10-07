package importmanifest

import (
	"testing"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/internal/wasm"
	"github.com/tetratelabs/wazero/internal/wasm/binary"
)

// TestCrossCheck_AgainstCoreDecoder builds mixed binaries and confirms the
// read-only manifest reports the same imports wazero's full decoder derives.
func TestCrossCheck_AgainstCoreDecoder(t *testing.T) {
	features := api.CoreFeaturesV2 | experimental.CoreFeaturesThreads

	b := newBuilder()
	tAbort := b.functype([]byte{valI32, valI64}, []byte{valF32})
	tVoid := b.functype(nil, []byte{valI64})
	b.importFunc("env", "abort", tAbort)
	b.importGlobal("env", "g", valF64, false)
	b.importMemory("env", "mem", 3, ptrU32(9), true)
	b.importFunc("env", "noop", tVoid)
	b.importTable("env", "tab", valFuncref, 1, ptrU32(2))
	bin := b.build()

	core, err := binary.DecodeModule(bin, features, wasm.MemoryLimitPages, false, false, false)
	if err != nil {
		t.Fatalf("core decoder rejected fixture: %v", err)
	}

	m, err := Read(bin, features)
	if err != nil {
		t.Fatalf("manifest Read: %v", err)
	}

	if len(m.Imports) != len(core.ImportSection) {
		t.Fatalf("import count manifest=%d core=%d", len(m.Imports), len(core.ImportSection))
	}
	for i := range core.ImportSection {
		want := &core.ImportSection[i]
		got := &m.Imports[i]
		if got.Module != want.Module || got.Name != want.Name || got.Kind != want.Type {
			t.Fatalf("import[%d] identity manifest=(%s.%s %s) core=(%s.%s %s)",
				i, got.Module, got.Name, api.ExternTypeName(got.Kind),
				want.Module, want.Name, api.ExternTypeName(want.Type))
		}
		switch got.Kind {
		case KindFunc:
			coreType := &core.TypeSection[want.DescFunc]
			if !kindsEqual(got.Signature.Params, coreType.Params) ||
				!kindsEqual(got.Signature.Results, coreType.Results) {
				t.Fatalf("import[%d] signature mismatch", i)
			}
		case KindMemory:
			if got.Memory.Min != want.DescMem.Min {
				t.Fatalf("import[%d] min %d != %d", i, got.Memory.Min, want.DescMem.Min)
			}
			if (got.Memory.Max == nil) != (!want.DescMem.IsMaxEncoded) {
				t.Fatalf("import[%d] max presence differs", i)
			}
			if got.Memory.Shared != want.DescMem.IsShared {
				t.Fatalf("import[%d] shared differs", i)
			}
		case KindTable:
			if got.Table.Min != want.DescTable.Min {
				t.Fatalf("import[%d] table min differs", i)
			}
		case KindGlobal:
			if got.GlobalType.Mutable != want.DescGlobal.Mutable {
				t.Fatalf("import[%d] global mutability differs", i)
			}
		}
	}
}

func kindsEqual(got []api.ValueType, want []wasm.ValueType) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i].Kind() {
			return false
		}
	}
	return true
}

// TestRead_EmptyAndImportless verifies modules with no imports produce an empty
// (but non-nil, usable) manifest rather than an error.
func TestRead_EmptyAndImportless(t *testing.T) {
	header := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	for name, bin := range map[string][]byte{
		"header only":  header,
		"type section": append(append([]byte{}, header...), sectionIDType, 0x01, 0x00),
	} {
		m, err := Read(bin, api.CoreFeaturesV2)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(m.Imports) != 0 || len(m.PerModule) != 0 {
			t.Fatalf("%s: expected empty manifest, got %+v", name, m)
		}
		if f := m.Check(nil); !f.Allowed() {
			t.Fatalf("%s: empty module should pass empty allowlist: %v", name, f)
		}
	}
}
