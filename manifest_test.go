package wazero

import (
	"testing"

	"github.com/tetratelabs/wazero/api"
	experimentalapi "github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/internal/testing/binaryencoding"
	"github.com/tetratelabs/wazero/internal/testing/require"
	"github.com/tetratelabs/wazero/internal/wasm"
)

var (
	typeI32I32 = wasm.FunctionType{
		Params:  []wasm.ValueType{wasm.ValueTypeI32},
		Results: []wasm.ValueType{wasm.ValueTypeI32},
	}
	typeVV = wasm.FunctionType{}
)

func tableMax(v uint32) *uint32 { return &v }

// mixedImportModule imports one of each kind, plus a duplicate func name, to
// prove indices are tracked per type and duplicates are preserved.
func mixedImportModule() *wasm.Module {
	return &wasm.Module{
		TypeSection: []wasm.FunctionType{typeI32I32, typeVV},
		ImportSection: []wasm.Import{
			{Type: wasm.ExternTypeFunc, Module: "env", Name: "read", DescFunc: 0},
			{Type: wasm.ExternTypeMemory, Module: "env", Name: "mem",
				DescMem: &wasm.Memory{Min: 1, Cap: 1, Max: 4, IsMaxEncoded: true}},
			{Type: wasm.ExternTypeGlobal, Module: "env", Name: "g",
				DescGlobal: wasm.GlobalType{ValType: wasm.ValueTypeI32, Mutable: true}},
			{Type: wasm.ExternTypeTable, Module: "env", Name: "tbl",
				DescTable: wasm.Table{Min: 2, Max: tableMax(8), Type: wasm.RefTypeFuncref}},
			// Same module and name as the first import, but a different
			// signature: it must keep its own index.
			{Type: wasm.ExternTypeFunc, Module: "env", Name: "read", DescFunc: 1},
		},
	}
}

func TestDecodeModuleManifest(t *testing.T) {
	manifest, err := DecodeModuleManifest(binaryencoding.EncodeModule(mixedImportModule()), api.CoreFeaturesV2)
	require.NoError(t, err)

	expected := []ImportManifest{
		{
			Index: 0, Module: "env", Name: "read", Type: api.ExternTypeFunc,
			Func: &FuncSignature{Params: []api.ValueType{api.ValueTypeI32}, Results: []api.ValueType{api.ValueTypeI32}},
		},
		{
			Index: 0, Module: "env", Name: "mem", Type: api.ExternTypeMemory,
			Memory: &MemoryLimits{MinPages: 1, MaxPages: 4, HasMax: true},
		},
		{
			Index: 0, Module: "env", Name: "g", Type: api.ExternTypeGlobal,
			Global: &GlobalType{ValType: api.ValueTypeI32, Mutable: true},
		},
		{
			Index: 0, Module: "env", Name: "tbl", Type: api.ExternTypeTable,
			Table: &TableLimits{Min: 2, Max: 8, HasMax: true, Type: api.ValueTypeFuncref},
		},
		{
			Index: 1, Module: "env", Name: "read", Type: api.ExternTypeFunc,
			Func: &FuncSignature{Params: []api.ValueType{}, Results: []api.ValueType{}},
		},
	}
	require.Equal(t, expected, manifest.Imports)
}

func TestDecodeModuleManifest_Errors(t *testing.T) {
	tests := []struct {
		name        string
		module      *wasm.Module
		features    api.CoreFeatures
		expectedErr string
	}{
		{
			name:        "unknown feature bits",
			module:      &wasm.Module{},
			features:    api.CoreFeaturesV2 | (1 << 20),
			expectedErr: "manifest: unknown features: 0x100000",
		},
		{
			name: "disabled feature: shared memory without threads",
			module: &wasm.Module{
				ImportSection: []wasm.Import{
					{Type: wasm.ExternTypeMemory, Module: "env", Name: "mem",
						DescMem: &wasm.Memory{Min: 1, Cap: 1, Max: 1, IsMaxEncoded: true, IsShared: true}},
				},
			},
			features:    api.CoreFeaturesV2,
			expectedErr: "shared memory requested but threads feature not enabled",
		},
		{
			name: "disabled feature: multi-value",
			module: &wasm.Module{
				TypeSection: []wasm.FunctionType{{
					Results: []wasm.ValueType{wasm.ValueTypeI32, wasm.ValueTypeI32},
				}},
			},
			features:    api.CoreFeaturesV1,
			expectedErr: "multiple result types invalid as feature \"multi-value\" is disabled",
		},
		{
			name: "invalid type index",
			module: &wasm.Module{
				TypeSection: []wasm.FunctionType{typeVV},
				ImportSection: []wasm.Import{
					{Type: wasm.ExternTypeFunc, Module: "env", Name: "read", DescFunc: 7},
				},
			},
			features:    api.CoreFeaturesV2,
			expectedErr: "manifest: import[0] func[env.read]: invalid type index 7, but only 1 types are defined",
		},
		{
			name: "tag import is not a describable capability",
			module: &wasm.Module{
				TypeSection: []wasm.FunctionType{typeVV},
				ImportSection: []wasm.Import{
					{Type: wasm.ExternTypeTag, Module: "env", Name: "exn", DescTag: 0},
				},
			},
			features:    api.CoreFeaturesV2 | experimentalapi.CoreFeaturesExceptionHandling,
			expectedErr: "manifest: import[0] 0x4[env.exn]: unsupported import kind",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeModuleManifest(binaryencoding.EncodeModule(tt.module), tt.features)
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.expectedErr)
		})
	}
}

func TestModuleManifest_Check(t *testing.T) {
	manifest, err := DecodeModuleManifest(binaryencoding.EncodeModule(mixedImportModule()), api.CoreFeaturesV2)
	require.NoError(t, err)

	fullAllowlist := func() *ImportAllowlist {
		return NewImportAllowlist().
			AllowFunc("env", "read", []api.ValueType{api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
			AllowMemory("env", "mem", MemoryLimits{MinPages: 1, MaxPages: 4, HasMax: true}).
			AllowGlobal("env", "g", api.ValueTypeI32, true).
			AllowTable("env", "tbl", TableLimits{Min: 2, Max: 8, HasMax: true, Type: api.ValueTypeFuncref})
	}

	t.Run("all capabilities allowed", func(t *testing.T) {
		// Note: the duplicate "env.read" import has signature () -> (), so
		// allow both signatures under the same name is impossible: the
		// allowlist entry must match each import exactly. Replace the second
		// import's requirement by checking a manifest without duplicates here.
		m, err := DecodeModuleManifest(binaryencoding.EncodeModule(&wasm.Module{
			TypeSection: []wasm.FunctionType{typeI32I32},
			ImportSection: []wasm.Import{
				{Type: wasm.ExternTypeFunc, Module: "env", Name: "read", DescFunc: 0},
				{Type: wasm.ExternTypeMemory, Module: "env", Name: "mem",
					DescMem: &wasm.Memory{Min: 1, Cap: 1, Max: 4, IsMaxEncoded: true}},
				{Type: wasm.ExternTypeGlobal, Module: "env", Name: "g",
					DescGlobal: wasm.GlobalType{ValType: wasm.ValueTypeI32, Mutable: true}},
				{Type: wasm.ExternTypeTable, Module: "env", Name: "tbl",
					DescTable: wasm.Table{Min: 2, Max: tableMax(8), Type: wasm.RefTypeFuncref}},
			},
		}), api.CoreFeaturesV2)
		require.NoError(t, err)
		check := m.Check(fullAllowlist())
		require.True(t, check.OK(), check.Error())
		require.Equal(t, 0, len(check.Missing))
		require.Equal(t, 0, len(check.Mismatched))
	})

	t.Run("missing capability reported separately from mismatch", func(t *testing.T) {
		check := manifest.Check(NewImportAllowlist().
			AllowFunc("env", "read", []api.ValueType{api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
			AllowGlobal("env", "g", api.ValueTypeI32, true).
			AllowTable("env", "tbl", TableLimits{Min: 2, Max: 8, HasMax: true, Type: api.ValueTypeFuncref}))
		require.False(t, check.OK())
		// "env.mem" has no entry: missing. The duplicate "env.read" with
		// signature () -> () has an entry, but the wrong type: mismatch.
		require.Equal(t, 1, len(check.Missing))
		require.Equal(t, "mem", check.Missing[0].Name)
		require.Equal(t, 1, len(check.Mismatched))
		require.Equal(t, uint32(1), check.Mismatched[0].Import.Index)
		require.Contains(t, check.Mismatched[0].Reason, "signature mismatch")
		require.Contains(t, check.Error(), "missing capabilities:")
		require.Contains(t, check.Error(), "type mismatches:")
	})

	t.Run("a valid name is not a wildcard for any signature", func(t *testing.T) {
		// Allow "env.read" only as () -> (): the first import, same name but
		// signature (i32) -> (i32), must be a mismatch, not allowed.
		check := manifest.Check(NewImportAllowlist().
			AllowFunc("env", "read", nil, nil).
			AllowMemory("env", "mem", MemoryLimits{MinPages: 1, MaxPages: 4, HasMax: true}).
			AllowGlobal("env", "g", api.ValueTypeI32, true).
			AllowTable("env", "tbl", TableLimits{Min: 2, Max: 8, HasMax: true, Type: api.ValueTypeFuncref}))
		require.False(t, check.OK())
		require.Equal(t, 0, len(check.Missing))
		require.Equal(t, 1, len(check.Mismatched))
		require.Equal(t, uint32(0), check.Mismatched[0].Import.Index)
		require.Contains(t, check.Mismatched[0].Reason,
			"signature mismatch: import requires (i32) -> (i32), but allowlist entry is () -> ()")
	})

	t.Run("kind mismatch", func(t *testing.T) {
		check := manifest.Check(NewImportAllowlist().
			AllowGlobal("env", "read", api.ValueTypeI32, false))
		require.False(t, check.OK())
		require.Equal(t, 2, len(check.Mismatched)) // both "env.read" func imports
		require.Contains(t, check.Mismatched[0].Reason, "kind mismatch: import is func, but allowlist entry is global")
		require.Equal(t, 3, len(check.Missing)) // mem, g, tbl
	})

	t.Run("memory limits", func(t *testing.T) {
		tests := []struct {
			name        string
			limits      MemoryLimits
			expectedErr string
		}{
			{"min too small", MemoryLimits{MinPages: 0, MaxPages: 4, HasMax: true},
				"minimum size mismatch: import requires at least 1 pages, but allowlist entry provides 0"},
			{"no max", MemoryLimits{MinPages: 1},
				"import requires a maximum of 4 pages, but allowlist entry has no maximum"},
			{"max too large", MemoryLimits{MinPages: 1, MaxPages: 8, HasMax: true},
				"maximum size mismatch: import requires at most 4 pages, but allowlist entry allows up to 8"},
			{"not shared", MemoryLimits{MinPages: 1, MaxPages: 4, HasMax: true}, ""},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				check := manifest.Check(NewImportAllowlist().AllowMemory("env", "mem", tt.limits))
				if tt.expectedErr == "" {
					require.Equal(t, 0, len(check.Mismatched))
				} else {
					require.Equal(t, 1, len(check.Mismatched))
					require.Equal(t, tt.expectedErr, check.Mismatched[0].Reason)
				}
			})
		}
	})

	t.Run("shared memory requires shared entry", func(t *testing.T) {
		m, err := DecodeModuleManifest(binaryencoding.EncodeModule(&wasm.Module{
			ImportSection: []wasm.Import{
				{Type: wasm.ExternTypeMemory, Module: "env", Name: "mem",
					DescMem: &wasm.Memory{Min: 1, Cap: 1, Max: 4, IsMaxEncoded: true, IsShared: true}},
			},
		}), api.CoreFeaturesV2|experimentalapi.CoreFeaturesThreads)
		require.NoError(t, err)

		check := m.Check(NewImportAllowlist().
			AllowMemory("env", "mem", MemoryLimits{MinPages: 1, MaxPages: 4, HasMax: true}))
		require.Equal(t, 1, len(check.Mismatched))
		require.Contains(t, check.Mismatched[0].Reason, "shared")

		check = m.Check(NewImportAllowlist().
			AllowMemory("env", "mem", MemoryLimits{MinPages: 1, MaxPages: 4, HasMax: true, Shared: true}))
		require.True(t, check.OK(), check.Error())
	})

	t.Run("table limits and element type", func(t *testing.T) {
		check := manifest.Check(NewImportAllowlist().
			AllowTable("env", "tbl", TableLimits{Min: 2, Max: 8, HasMax: true, Type: api.ValueTypeExternref}))
		require.Equal(t, 1, len(check.Mismatched))
		require.Contains(t, check.Mismatched[0].Reason, "table element type mismatch")

		check = manifest.Check(NewImportAllowlist().
			AllowTable("env", "tbl", TableLimits{Min: 1, Max: 8, HasMax: true, Type: api.ValueTypeFuncref}))
		require.Equal(t, 1, len(check.Mismatched))
		require.Contains(t, check.Mismatched[0].Reason, "minimum size mismatch")
	})

	t.Run("global type and mutability", func(t *testing.T) {
		check := manifest.Check(NewImportAllowlist().AllowGlobal("env", "g", api.ValueTypeI64, true))
		require.Equal(t, 1, len(check.Mismatched))
		require.Contains(t, check.Mismatched[0].Reason, "global type mismatch")

		check = manifest.Check(NewImportAllowlist().AllowGlobal("env", "g", api.ValueTypeI32, false))
		require.Equal(t, 1, len(check.Mismatched))
		require.Contains(t, check.Mismatched[0].Reason, "global mutability mismatch")
	})
}
