// Copyright (c) 2013 Kelsey Hightower. All rights reserved.
// Use of this source code is governed by the MIT License that can be found in
// the LICENSE file.

package envconfig

import (
	"errors"
	"os"
	"testing"
)

type nestedLeaf struct {
	Leaf string
}

type lazySpec struct {
	Optional *nestedLeaf
}

func TestLazyNestedPointerStaysNilWithoutInput(t *testing.T) {
	var s lazySpec
	os.Clearenv()
	if err := Process("env_config", &s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Optional != nil {
		t.Errorf("expected nil nested pointer, got %#v", s.Optional)
	}
}

func TestLazyNestedPointerAllocatedFromEnv(t *testing.T) {
	var s lazySpec
	os.Clearenv()
	os.Setenv("ENV_CONFIG_OPTIONAL_LEAF", "present")
	defer os.Clearenv()
	if err := Process("env_config", &s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Optional == nil {
		t.Fatal("expected nested pointer to be allocated")
	}
	if s.Optional.Leaf != "present" {
		t.Errorf("expected %q, got %q", "present", s.Optional.Leaf)
	}
}

func TestLazyNestedPointerAllocatedFromDefault(t *testing.T) {
	var s struct {
		Optional *struct {
			Leaf string `default:"implicit"`
		}
	}
	os.Clearenv()
	if err := Process("env_config", &s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Optional == nil {
		t.Fatal("expected nested pointer to be allocated from default")
	}
	if s.Optional.Leaf != "implicit" {
		t.Errorf("expected %q, got %q", "implicit", s.Optional.Leaf)
	}
}

type level3 struct {
	Deep    string
	Sibling *struct {
		Other string
	}
}

type level2 struct {
	Next *level3
}

type level1 struct {
	Middle *level2
}

type deepSpec struct {
	Top *level1
}

func TestLazyMultiLevelPointers(t *testing.T) {
	var s deepSpec
	os.Clearenv()
	if err := Process("env_config", &s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Top != nil {
		t.Errorf("expected full chain to stay nil, got %#v", s.Top)
	}

	os.Setenv("ENV_CONFIG_TOP_MIDDLE_NEXT_DEEP", "value")
	defer os.Clearenv()
	if err := Process("env_config", &s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Top == nil || s.Top.Middle == nil || s.Top.Middle.Next == nil {
		t.Fatalf("expected full chain to be allocated, got %#v", s.Top)
	}
	if s.Top.Middle.Next.Deep != "value" {
		t.Errorf("expected %q, got %q", "value", s.Top.Middle.Next.Deep)
	}
	if s.Top.Middle.Next.Sibling != nil {
		t.Errorf("expected untouched sibling subtree to stay nil, got %#v", s.Top.Middle.Next.Sibling)
	}
}

func TestLazySiblingSubtreesStayNil(t *testing.T) {
	var s struct {
		Left  *struct{ L string }
		Right *struct{ R string }
	}
	os.Clearenv()
	os.Setenv("ENV_CONFIG_LEFT_L", "only")
	defer os.Clearenv()
	if err := Process("env_config", &s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Left == nil || s.Left.L != "only" {
		t.Errorf("expected left to be populated, got %#v", s.Left)
	}
	if s.Right != nil {
		t.Errorf("expected right subtree to stay nil, got %#v", s.Right)
	}
}

type LazyEmbedded struct {
	EmbeddedValue string
}

type lazyAnonymousSpec struct {
	*LazyEmbedded
	Plain string `required:"true"`
}

func TestLazyAnonymousPointerEmbedded(t *testing.T) {
	var s lazyAnonymousSpec
	os.Clearenv()
	os.Setenv("ENV_CONFIG_PLAIN", "p")
	if err := Process("env_config", &s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.LazyEmbedded != nil {
		t.Errorf("expected anonymous embedded pointer to stay nil, got %#v", s.LazyEmbedded)
	}

	os.Setenv("ENV_CONFIG_EMBEDDEDVALUE", "e")
	defer os.Clearenv()
	if err := Process("env_config", &s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.LazyEmbedded == nil || s.EmbeddedValue != "e" {
		t.Errorf("expected embedded value %q, got %#v", "e", s.LazyEmbedded)
	}
}

func TestLazyRequiredNestedForcesAncestors(t *testing.T) {
	var s struct {
		Optional *struct {
			Missing string `required:"true"`
		}
	}
	os.Clearenv()
	err := Process("env_config", &s)
	if err == nil {
		t.Fatal("expected required error, got nil")
	}
	if s.Optional == nil {
		t.Errorf("expected required child to force parent allocation, got nil")
	}
}

type pointerDecoder struct {
	seen string
}

func (d *pointerDecoder) Decode(value string) error {
	d.seen = value
	return nil
}

func TestLazyPointerDecoderField(t *testing.T) {
	var s struct {
		Custom *pointerDecoder
	}
	os.Clearenv()
	if err := Process("env_config", &s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Custom != nil {
		t.Errorf("expected decoder pointer to stay nil without input, got %#v", s.Custom)
	}

	os.Setenv("ENV_CONFIG_CUSTOM", "decoded")
	defer os.Clearenv()
	if err := Process("env_config", &s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Custom == nil {
		t.Fatal("expected decoder pointer to be allocated")
	}
	if s.Custom.seen != "decoded" {
		t.Errorf("expected %q, got %q", "decoded", s.Custom.seen)
	}
}

func TestLazyMultiLevelPointerLeaf(t *testing.T) {
	var s struct {
		Value **string
	}
	os.Clearenv()
	if err := Process("env_config", &s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Value != nil {
		t.Errorf("expected nil pointer chain, got %#v", s.Value)
	}

	os.Setenv("ENV_CONFIG_VALUE", "deep")
	defer os.Clearenv()
	if err := Process("env_config", &s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Value == nil || *s.Value == nil || **s.Value != "deep" {
		t.Errorf("expected %q, got %#v", "deep", s.Value)
	}
}

type recursiveSpec struct {
	Next *recursiveSpec
	Name string
}

func TestRecursiveSpecificationRejected(t *testing.T) {
	var s recursiveSpec
	os.Clearenv()
	err := Process("env_config", &s)
	if !errors.Is(err, ErrRecursiveSpecification) {
		t.Fatalf("expected ErrRecursiveSpecification, got %v", err)
	}
}

type pairRecursiveA struct {
	B *pairRecursiveB
}

type pairRecursiveB struct {
	A *pairRecursiveA
}

func TestPairRecursiveSpecificationRejected(t *testing.T) {
	var s struct {
		Start *pairRecursiveA
	}
	os.Clearenv()
	err := Process("env_config", &s)
	if !errors.Is(err, ErrRecursiveSpecification) {
		t.Fatalf("expected ErrRecursiveSpecification, got %v", err)
	}
}

type diamondD struct {
	V string
}

type diamondB struct{ D *diamondD }
type diamondC struct{ D *diamondD }

type repeatInner struct {
	V string
}

type repeatMiddle struct {
	First  *repeatInner
	Second *repeatInner
}

func TestDiamondShapeIsNotRejected(t *testing.T) {
	var s struct {
		B *diamondB
		C *diamondC
	}
	os.Clearenv()
	os.Setenv("ENV_CONFIG_B_D_V", "x")
	defer os.Clearenv()
	if err := Process("env_config", &s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.B == nil || s.B.D == nil || s.B.D.V != "x" {
		t.Errorf("expected shared diamond type to work, got %#v", s.B)
	}
	if s.C != nil {
		t.Errorf("expected unvisited sibling to stay nil, got %#v", s.C)
	}
}

func TestRepeatedSiblingTypeIsNotRejected(t *testing.T) {
	var s struct {
		M *repeatMiddle
	}
	os.Clearenv()
	os.Setenv("ENV_CONFIG_M_FIRST_V", "1")
	os.Setenv("ENV_CONFIG_M_SECOND_V", "2")
	defer os.Clearenv()
	if err := Process("env_config", &s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.M.First == nil || s.M.First.V != "1" {
		t.Errorf("expected first %q, got %#v", "1", s.M.First)
	}
	if s.M.Second == nil || s.M.Second.V != "2" {
		t.Errorf("expected second %q, got %#v", "2", s.M.Second)
	}
}

func TestCheckDisallowedDoesNotMutate(t *testing.T) {
	var s deepSpec
	os.Clearenv()
	os.Setenv("UNRELATED_ENV_VAR", "true")
	if err := CheckDisallowed("env_config", &s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Top != nil {
		t.Errorf("CheckDisallowed allocated nested pointers: %#v", s.Top)
	}
}

func TestGatherInfoDoesNotMutate(t *testing.T) {
	var s deepSpec
	os.Clearenv()
	if _, err := gatherInfo("env_config", &s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Top != nil {
		t.Errorf("gatherInfo allocated nested pointers: %#v", s.Top)
	}
}

func TestExplicitBlankValueAllocatesNested(t *testing.T) {
	var s lazySpec
	os.Clearenv()
	os.Setenv("ENV_CONFIG_OPTIONAL_LEAF", "")
	defer os.Clearenv()
	if err := Process("env_config", &s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Optional == nil {
		t.Fatal("expected nested pointer to be allocated for explicit blank value")
	}
	if s.Optional.Leaf != "" {
		t.Errorf("expected blank string, got %q", s.Optional.Leaf)
	}
}
