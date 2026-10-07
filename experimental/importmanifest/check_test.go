package importmanifest

import (
	"testing"

	"github.com/tetratelabs/wazero/api"
)

func mixedModule(t *testing.T) (*Manifest, uint32, uint32) {
	t.Helper()
	b := newBuilder()
	abortType := b.functype([]byte{valI32, valI32}, []byte{valI32})
	otherType := b.functype([]byte{valI64}, []byte{valI64})
	_ = otherType
	b.importFunc("env", "abort", abortType)
	b.importGlobal("env", "seed", valI64, true)
	b.importMemory("env", "memory", 2, ptrU32(10), false)
	b.importTable("env", "callbacks", valFuncref, 4, ptrU32(8))
	m, err := Read(b.build(), api.CoreFeaturesV2)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return m, abortType, 0
}

func TestCheck_AllGranted(t *testing.T) {
	m, _, _ := mixedModule(t)
	allow := Allowlist{
		{Module: "env", Name: "abort"}:     AllowFunc([]api.ValueType{i32, i32}, []api.ValueType{i32}),
		{Module: "env", Name: "seed"}:      AllowGlobal(i64, true),
		{Module: "env", Name: "memory"}:    AllowMemory(2, ptrU32(10), false),
		{Module: "env", Name: "callbacks"}: AllowTable(api.ValueType(valFuncref), 4, ptrU32(8)),
	}
	if f := m.Check(allow); !f.Allowed() {
		t.Fatalf("expected allowed, got %v", f)
	}
}

// TestCheck_NameIsNotASignaturePass is the core security guarantee: granting a
// member of the right name but a different function signature must be a
// mismatch, never accepted merely because the name exists.
func TestCheck_NameIsNotASignaturePass(t *testing.T) {
	m, _, _ := mixedModule(t)
	allow := Allowlist{
		// "abort" exists, but with (i64)->i64 rather than (i32,i32)->i32.
		{Module: "env", Name: "abort"}:     AllowFunc([]api.ValueType{i64}, []api.ValueType{i64}),
		{Module: "env", Name: "seed"}:      AllowGlobal(i64, true),
		{Module: "env", Name: "memory"}:    AllowMemory(2, ptrU32(10), false),
		{Module: "env", Name: "callbacks"}: AllowTable(api.ValueType(valFuncref), 4, ptrU32(8)),
	}
	f := m.Check(allow)
	if f.Allowed() {
		t.Fatal("wrong signature must not be allowed")
	}
	mis := f.Mismatches()
	if len(mis) != 1 || mis[0].Name != "abort" || mis[0].Reason != FindingTypeMismatch {
		t.Fatalf("expected one abort signature mismatch, got %v", f)
	}
	if len(f.Missing()) != 0 {
		t.Fatalf("abort is present, must not be missing: %v", f.Missing())
	}
}

// TestCheck_ArityDifference verifies param/result length differences mismatch.
func TestCheck_ArityDifference(t *testing.T) {
	b := newBuilder()
	t0 := b.functype([]byte{valI32, valI32}, []byte{valI32})
	b.importFunc("env", "f", t0)
	m, _ := Read(b.build(), api.CoreFeaturesV2)

	cases := map[string]AllowEntry{
		"fewer params":  AllowFunc([]api.ValueType{i32}, []api.ValueType{i32}),
		"extra result":  AllowFunc([]api.ValueType{i32, i32}, []api.ValueType{i32, i32}),
		"void function": AllowFunc(nil, nil),
	}
	for name, entry := range cases {
		if f := m.Check(Allowlist{{Module: "env", Name: "f"}: entry}); f.Allowed() {
			t.Fatalf("%s: expected mismatch", name)
		}
	}
}

// TestCheck_MissingCapability verifies an ungranted member is reported missing,
// distinct from a type mismatch.
func TestCheck_MissingCapability(t *testing.T) {
	m, _, _ := mixedModule(t)
	// Grant everything except "memory"; also reference a wholly unknown module.
	b := newBuilder()
	t0 := b.functype(nil, nil)
	b.importFunc("other_module", "nope", t0)
	extra, err := Read(b.build(), api.CoreFeaturesV2)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	combined := &Manifest{Imports: append(append([]Import{}, m.Imports...), extra.Imports...)}

	allow := Allowlist{
		{Module: "env", Name: "abort"}:     AllowFunc([]api.ValueType{i32, i32}, []api.ValueType{i32}),
		{Module: "env", Name: "seed"}:      AllowGlobal(i64, true),
		{Module: "env", Name: "callbacks"}: AllowTable(api.ValueType(valFuncref), 4, ptrU32(8)),
	}
	f := combined.Check(allow)
	missing := f.Missing()
	if len(missing) != 2 {
		t.Fatalf("expected 2 missing, got %d: %v", len(missing), f)
	}
	names := map[string]bool{}
	for _, x := range missing {
		names[x.Module+"."+x.Name] = true
	}
	if !names["env.memory"] || !names["other_module.nope"] {
		t.Fatalf("missing set wrong: %v", names)
	}
}

// TestCheck_KindMismatch verifies granting a global where a function is asked
// (same name) is a kind mismatch, not a pass.
func TestCheck_KindMismatch(t *testing.T) {
	m, _, _ := mixedModule(t)
	allow := Allowlist{
		{Module: "env", Name: "abort"}:     AllowGlobal(i32, false),
		{Module: "env", Name: "seed"}:      AllowGlobal(i64, true),
		{Module: "env", Name: "memory"}:    AllowMemory(2, ptrU32(10), false),
		{Module: "env", Name: "callbacks"}: AllowTable(api.ValueType(valFuncref), 4, ptrU32(8)),
	}
	f := m.Check(allow)
	if len(f.Mismatches()) != 1 || f.Mismatches()[0].Detail == "" {
		t.Fatalf("expected one kind mismatch with detail, got %v", f)
	}
}

func TestCheck_GlobalMutabilityAndType(t *testing.T) {
	mk := func(valType byte, mutable bool) *Manifest {
		b := newBuilder()
		b.importGlobal("env", "g", valType, mutable)
		m, err := Read(b.build(), api.CoreFeaturesV2)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		return m
	}
	key := Key{Module: "env", Name: "g"}

	if f := mk(valI64, true).Check(Allowlist{key: AllowGlobal(i64, false)}); f.Allowed() {
		t.Fatal("mutable request must reject immutable grant")
	}
	if f := mk(valI64, false).Check(Allowlist{key: AllowGlobal(i64, true)}); f.Allowed() {
		t.Fatal("immutable request must reject mutable grant")
	}
	if f := mk(valI64, false).Check(Allowlist{key: AllowGlobal(i32, false)}); f.Allowed() {
		t.Fatal("different value type must mismatch")
	}
	if f := mk(valI64, false).Check(Allowlist{key: AllowGlobal(i64, false)}); !f.Allowed() {
		t.Fatalf("matching immutable global must be allowed: %v", f)
	}
}

func TestCheck_MemoryAndTableLimits(t *testing.T) {
	// Request memory min=2,max=10 and table funcref min=4,max=8.
	m, _, _ := mixedModule(t)

	// A host minimum below the requested minimum cannot satisfy the import.
	if f := m.Check(Allowlist{
		{Module: "env", Name: "abort"}:     AllowFunc([]api.ValueType{i32, i32}, []api.ValueType{i32}),
		{Module: "env", Name: "seed"}:      AllowGlobal(i64, true),
		{Module: "env", Name: "memory"}:    AllowMemory(1, ptrU32(16), false),
		{Module: "env", Name: "callbacks"}: AllowTable(api.ValueType(valFuncref), 4, ptrU32(8)),
	}); f.Allowed() {
		t.Fatal("host minimum below requested minimum must mismatch")
	}

	// A host that supplies at least the requested min and at least the
	// requested max satisfies the import (containment, not exact equality).
	if f := m.Check(Allowlist{
		{Module: "env", Name: "abort"}:     AllowFunc([]api.ValueType{i32, i32}, []api.ValueType{i32}),
		{Module: "env", Name: "seed"}:      AllowGlobal(i64, true),
		{Module: "env", Name: "memory"}:    AllowMemory(2, ptrU32(16), false),
		{Module: "env", Name: "callbacks"}: AllowTable(api.ValueType(valFuncref), 4, ptrU32(16)),
	}); !f.Allowed() {
		t.Fatalf("larger host limits must satisfy the import: %v", f)
	}

	// Requested maximum larger than the granted maximum must mismatch.
	if f := m.Check(Allowlist{
		{Module: "env", Name: "abort"}:     AllowFunc([]api.ValueType{i32, i32}, []api.ValueType{i32}),
		{Module: "env", Name: "seed"}:      AllowGlobal(i64, true),
		{Module: "env", Name: "memory"}:    AllowMemory(2, ptrU32(5), false),
		{Module: "env", Name: "callbacks"}: AllowTable(api.ValueType(valFuncref), 4, ptrU32(8)),
	}); f.Allowed() {
		t.Fatal("host maximum below requested maximum must mismatch")
	}

	// Table element type difference must mismatch.
	if f := m.Check(Allowlist{
		{Module: "env", Name: "abort"}:     AllowFunc([]api.ValueType{i32, i32}, []api.ValueType{i32}),
		{Module: "env", Name: "seed"}:      AllowGlobal(i64, true),
		{Module: "env", Name: "memory"}:    AllowMemory(2, ptrU32(10), false),
		{Module: "env", Name: "callbacks"}: AllowTable(api.ValueTypeExternref, 4, ptrU32(8)),
	}); f.Allowed() {
		t.Fatal("funcref table must not match an externref grant")
	}

	// Shared flag difference must mismatch.
	if f := m.Check(Allowlist{
		{Module: "env", Name: "abort"}:     AllowFunc([]api.ValueType{i32, i32}, []api.ValueType{i32}),
		{Module: "env", Name: "seed"}:      AllowGlobal(i64, true),
		{Module: "env", Name: "memory"}:    AllowMemory(2, ptrU32(10), true),
		{Module: "env", Name: "callbacks"}: AllowTable(api.ValueType(valFuncref), 4, ptrU32(8)),
	}); f.Allowed() {
		t.Fatal("shared flag difference must mismatch")
	}
}

// TestCheck_DuplicatesCheckedIndependently verifies two same-named imports are
// each evaluated: granting one signature cannot cover a second, different one.
func TestCheck_DuplicatesCheckedIndependently(t *testing.T) {
	b := newBuilder()
	t0 := b.functype([]byte{valI32}, nil)
	t1 := b.functype([]byte{valI64}, nil)
	b.importFunc("env", "same", t0)
	b.importFunc("env", "same", t1)
	m, err := Read(b.build(), api.CoreFeaturesV2)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	// A single allowlist entry granting the i32 signature satisfies import 0
	// but must still mismatch import 1 (i64), at its own index.
	f := m.Check(Allowlist{
		{Module: "env", Name: "same"}: AllowFunc([]api.ValueType{i32}, nil),
	})
	mis := f.Mismatches()
	if len(mis) != 1 {
		t.Fatalf("expected exactly one mismatch for the differing duplicate, got %v", f)
	}
	if mis[0].Index != 1 {
		t.Fatalf("mismatch must point at import index 1, got %d", mis[0].Index)
	}

	// Granting both exact signatures (same name) allows the module.
	f = m.Check(Allowlist{
		{Module: "env", Name: "same"}: AllowFunc([]api.ValueType{i64}, nil),
	})
	if len(f.Mismatches()) != 1 || f.Mismatches()[0].Index != 0 {
		t.Fatalf("with i64 granted, index 0 should mismatch: %v", f)
	}
}

func TestFinding_ErrorText(t *testing.T) {
	miss := Finding{Index: 3, Key: Key{Module: "env", Name: "x"}, Kind: KindFunc, Reason: FindingMissing}
	if got := miss.Error(); !contains(got, "not granted") || !contains(got, "env") {
		t.Fatalf("missing text = %q", got)
	}
	bad := Finding{Index: 1, Key: Key{Module: "env", Name: "y"}, Kind: KindMemory,
		Reason: FindingTypeMismatch, Detail: "boom"}
	if got := bad.Error(); !contains(got, "type mismatch") || !contains(got, "boom") {
		t.Fatalf("mismatch text = %q", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
