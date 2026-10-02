package envconfig

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

type lazyInner struct {
	Value       string
	WithDefault string `default:"inner-default"`
	Optional    string
	Sub         *lazyDeep
}

type lazyRequiredInner struct {
	Required string `required:"true"`
}

type lazyDeep struct {
	Value   string
	DeepSub *lazyDeepest
}

type lazyDeepest struct {
	Value string
}

type lazySpec struct {
	A      *lazyRequiredInner
	B      *lazyInner
	Double **lazyDeep
}

type EmbeddedPtr struct {
	EmbeddedValue string
}

// pointerDecoder is a leaf pointer type that implements Decoder on a pointer
// receiver. A nil value must be allocated lazily before Decode is invoked.
type pointerDecoder string

func (p *pointerDecoder) Decode(value string) error {
	*p = pointerDecoder("decoded:" + value)
	return nil
}

type decoderHolder struct {
	D *pointerDecoder
}

func TestLazyNestedPointerOnlyAllocatesPopulatedSubtree(t *testing.T) {
	var s lazySpec
	os.Clearenv()
	os.Setenv("ENV_CONFIG_A_REQUIRED", "present")
	os.Setenv("ENV_CONFIG_B_VALUE", "hello")

	if err := Process("env_config", &s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if s.A == nil {
		t.Fatal("expected required subtree A to be allocated")
	}
	if s.A.Required != "present" {
		t.Errorf("A.Required = %q, want %q", s.A.Required, "present")
	}

	// B has an env value (and a default-bearing leaf), so it is allocated.
	if s.B == nil {
		t.Fatal("expected populated subtree B to be allocated")
	}
	if s.B.Value != "hello" {
		t.Errorf("B.Value = %q, want %q", s.B.Value, "hello")
	}
	if s.B.WithDefault != "inner-default" {
		t.Errorf("B.WithDefault = %q, want %q", s.B.WithDefault, "inner-default")
	}
	// A deeper optional subtree that received nothing stays nil.
	if s.B.Sub != nil {
		t.Errorf("expected B.Sub to remain nil, got %#v", s.B.Sub)
	}
}

type lazyDeepSpec struct {
	One *lazyDeep
	Two *lazyDeep
}

func TestLazyNestedPointerDeeplyNested(t *testing.T) {
	var s lazyDeepSpec
	os.Clearenv()
	os.Setenv("ENV_CONFIG_ONE_DEEPSUB_VALUE", "deep")

	if err := Process("env_config", &s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if s.One == nil || s.One.DeepSub == nil {
		t.Fatalf("expected deep chain allocated, got %#v", s.One)
	}
	if s.One.DeepSub.Value != "deep" {
		t.Errorf("deep value = %q, want %q", s.One.DeepSub.Value, "deep")
	}
	// The untouched sibling subtree, and the intermediate empty subtree,
	// remain nil.
	if s.Two != nil {
		t.Errorf("expected sibling subtree Two to remain nil, got %#v", s.Two)
	}
	// lazyDeep.Value and lazyDeepest siblings were not set; the chain for
	// Deepsub was created but One.Value stays zero.
}

func TestLazyNestedPointerDoubleIndirection(t *testing.T) {
	var s struct {
		Req    *lazyRequiredInner
		Double **lazyDeep
	}
	os.Clearenv()
	os.Setenv("ENV_CONFIG_REQ_REQUIRED", "present")
	os.Setenv("ENV_CONFIG_DOUBLE_VALUE", "x")

	if err := Process("env_config", &s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if s.Double == nil || *s.Double == nil {
		t.Fatalf("expected both pointer levels allocated, got %#v", s.Double)
	}
	if (*s.Double).Value != "x" {
		t.Errorf("double value = %q, want %q", (*s.Double).Value, "x")
	}
}

func TestLazyNestedPointerDefaultAllocatesAncestors(t *testing.T) {
	var s struct {
		Inner *struct {
			HasDefault string `default:"d"`
			NoDefault  string
		}
	}
	os.Clearenv()

	if err := Process("env_config", &s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Inner == nil {
		t.Fatal("expected ancestor allocated to hold the default")
	}
	if s.Inner.HasDefault != "d" {
		t.Errorf("HasDefault = %q, want %q", s.Inner.HasDefault, "d")
	}
}

func TestLazyNestedPointerRequiredAllocatesAncestors(t *testing.T) {
	var s struct {
		Inner *struct {
			Must string `required:"true"`
		}
	}
	os.Clearenv()

	err := Process("env_config", &s)
	if err == nil {
		t.Fatal("expected required-missing error")
	}
	if !strings.Contains(err.Error(), "required key") {
		t.Fatalf("expected required error, got %v", err)
	}
	// The missing required value must not partially mutate the caller object.
	if s.Inner != nil {
		t.Errorf("expected no allocation on required-missing error, got %#v", s.Inner)
	}

	os.Setenv("ENV_CONFIG_INNER_MUST", "set")
	if err := Process("env_config", &s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Inner == nil {
		t.Fatal("expected ancestor allocated to satisfy required field")
	}
	if s.Inner.Must != "set" {
		t.Errorf("Must = %q, want %q", s.Inner.Must, "set")
	}
}

func TestLazyNestedPointerAnonymousEmbedded(t *testing.T) {
	var s struct {
		*EmbeddedPtr
		Named *lazyInner
	}
	os.Clearenv()
	os.Setenv("ENV_CONFIG_NAMED_REQUIRED", "present")
	os.Setenv("ENV_CONFIG_EMBEDDEDVALUE", "embedded")

	if err := Process("env_config", &s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.EmbeddedPtr == nil {
		t.Fatal("expected anonymous embedded pointer allocated")
	}
	if s.EmbeddedValue != "embedded" {
		t.Errorf("EmbeddedValue = %q, want %q", s.EmbeddedValue, "embedded")
	}

	// Without a value the anonymous embedded pointer remains nil too.
	var s2 struct {
		*EmbeddedPtr
	}
	os.Clearenv()
	if err := Process("env_config", &s2); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s2.EmbeddedPtr != nil {
		t.Errorf("expected anonymous embedded pointer nil, got %#v", s2.EmbeddedPtr)
	}
}

func TestLazyNestedPointerDecoderOnPointerLeaf(t *testing.T) {
	var s decoderHolder
	os.Clearenv()
	os.Setenv("ENV_CONFIG_D", "v")

	if err := Process("env_config", &s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.D == nil {
		t.Fatal("expected decoder pointer allocated")
	}
	if string(*s.D) != "decoded:v" {
		t.Errorf("D = %q, want %q", *s.D, "decoded:v")
	}

	// No env value: pointer stays nil, Decode is never invoked.
	var s2 decoderHolder
	os.Clearenv()
	if err := Process("env_config", &s2); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s2.D != nil {
		t.Errorf("expected decoder pointer nil, got %#v", s2.D)
	}
}

func TestEagerNestedPointerAllocatesAllSubtrees(t *testing.T) {
	var s lazySpec
	os.Clearenv()
	os.Setenv("ENV_CONFIG_A_REQUIRED", "present")

	err := ProcessWith("env_config", &s, Config{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.A == nil || s.B == nil {
		t.Errorf("expected eager allocation of both subtrees, got A=%v B=%v", s.A, s.B)
	}
	if s.B.Sub == nil {
		t.Errorf("expected eager allocation of nested subtree")
	}
}

type recursiveNode struct {
	Value string
	Next  *recursiveNode
}

type recA struct {
	ToB *recB
}

type recB struct {
	ToA *recA
}

func TestRecursivePointerSpecificationRejected(t *testing.T) {
	var s struct {
		Node *recursiveNode
	}
	os.Clearenv()

	err := Process("env_config", &s)
	if !errors.Is(err, ErrRecursiveSpecification) {
		t.Fatalf("expected ErrRecursiveSpecification, got %v", err)
	}

	// Structure-only operations report the same error.
	if err := CheckDisallowed("env_config", &s); !errors.Is(err, ErrRecursiveSpecification) {
		t.Fatalf("CheckDisallowed: expected ErrRecursiveSpecification, got %v", err)
	}
}

func TestMutuallyRecursivePointerSpecificationRejected(t *testing.T) {
	var s struct {
		Start *recA
	}
	os.Clearenv()

	if err := Process("env_config", &s); !errors.Is(err, ErrRecursiveSpecification) {
		t.Fatalf("expected ErrRecursiveSpecification, got %v", err)
	}
}

func TestUsageDoesNotMutateNestedPointers(t *testing.T) {
	var s lazySpec
	os.Clearenv()
	buf := new(bytes.Buffer)
	if err := Usagef("env_config", &s, buf, "{{range .}}{{usage_key .}}\n{{end}}"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.A != nil || s.B != nil || s.Double != nil {
		t.Errorf("Usage allocated nested pointers: A=%v B=%v Double=%v", s.A, s.B, s.Double)
	}
	if !strings.Contains(buf.String(), "ENV_CONFIG_A_REQUIRED") {
		t.Errorf("usage output missing nested key:\n%s", buf.String())
	}
}

func TestCheckDisallowedDoesNotMutateNestedPointers(t *testing.T) {
	var s lazySpec
	os.Clearenv()
	os.Setenv("ENV_CONFIG_A_REQUIRED", "x")
	if err := CheckDisallowed("env_config", &s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.A != nil || s.B != nil {
		t.Errorf("CheckDisallowed allocated nested pointers: A=%v B=%v", s.A, s.B)
	}
}
