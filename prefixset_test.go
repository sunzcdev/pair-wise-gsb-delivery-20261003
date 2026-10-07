package geohash

import (
	"sort"
	"testing"
)

const testAlphabet = "0123456789bcdefghjkmnpqrstuvwxyz"

// members returns the canonical set members in sorted order.
func members(s *PrefixSet) []string {
	out := s.Prefixes()
	sort.Strings(out)
	return out
}

func expectMembers(t *testing.T, s *PrefixSet, want []string) {
	t.Helper()
	got := members(s)
	if len(got) != len(want) {
		t.Fatalf("members = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("members = %v, want %v", got, want)
		}
	}
}

// children returns the 32 one-character children of parent.
func children(parent string) []string {
	out := make([]string, 0, 32)
	for i := 0; i < len(testAlphabet); i++ {
		out = append(out, parent+string(testAlphabet[i]))
	}
	return out
}

func TestPrefixSetAddRemovesRedundantDescendants(t *testing.T) {
	s := NewPrefixSet()
	for _, p := range []string{"0", "00", "01pb", "zzzz"} {
		if err := s.Add(p); err != nil {
			t.Fatalf("add %q: %v", p, err)
		}
	}
	expectMembers(t, s, []string{"0", "zzzz"})

	// Adding an ancestor absorbs the descendants.
	if err := s.Add("z"); err != nil {
		t.Fatal(err)
	}
	expectMembers(t, s, []string{"0", "z"})
}

func TestPrefixSetAddOrderIndependent(t *testing.T) {
	// The same regions added in several orders must canonicalize identically.
	inputs := []string{"00", "01", "0pb", "zzz", "01bc", "w", "zz", "0p"}

	orders := [][]string{
		inputs,
		{"w", "zz", "zzz", "0p", "0pb", "01", "01bc", "00"},
		{"01bc", "zzz", "00", "w", "0pb", "0p", "zz", "01"},
	}

	var ref []string
	for i, order := range orders {
		s := NewPrefixSet()
		for _, p := range order {
			if err := s.Add(p); err != nil {
				t.Fatalf("order %d: add %q: %v", i, p, err)
			}
		}
		got := members(s)
		if ref == nil {
			ref = got
			continue
		}
		if len(got) != len(ref) {
			t.Fatalf("order %d = %v, want %v", i, got, ref)
		}
		for j := range got {
			if got[j] != ref[j] {
				t.Fatalf("order %d = %v, want %v", i, got, ref)
			}
		}
	}
}

func TestPrefixSetCollapsesAll32Children(t *testing.T) {
	s := NewPrefixSet()
	kids := children("0")

	// With 31 children the parent must not appear.
	for _, k := range kids[:31] {
		if err := s.Add(k); err != nil {
			t.Fatal(err)
		}
	}
	if s.Contains("0") {
		t.Fatalf("parent should not fold until all 32 children present; %v", members(s))
	}
	if s.Len() != 31 {
		t.Fatalf("len = %d, want 31", s.Len())
	}

	// The 32nd child triggers the collapse.
	if err := s.Add(kids[31]); err != nil {
		t.Fatal(err)
	}
	expectMembers(t, s, []string{"0"})
}

func TestPrefixSetNotBinaryCollapse(t *testing.T) {
	// Two children alone must never be treated as a complete level.
	s := NewPrefixSet()
	if err := s.Add("00"); err != nil {
		t.Fatal(err)
	}
	if err := s.Add("01"); err != nil {
		t.Fatal(err)
	}
	expectMembers(t, s, []string{"00", "01"})

	// A handful of children must likewise remain distinct.
	for _, k := range children("p")[:16] {
		if err := s.Add(k); err != nil {
			t.Fatal(err)
		}
	}
	if s.Contains("p") {
		t.Fatalf("16 children must not collapse into parent; %v", members(s))
	}
	if s.Len() != 18 {
		t.Fatalf("len = %d, want 18", s.Len())
	}
}

func TestPrefixSetCascadeCollapse(t *testing.T) {
	// Add 31 of the children of "00", and all of the other 31 children of
	// "0". Completing "00" must cascade up to fold all of "0".
	s := NewPrefixSet()

	grandkids := children("00")
	for _, g := range grandkids[:31] {
		if err := s.Add(g); err != nil {
			t.Fatal(err)
		}
	}

	others := children("0")[1:] // "01" .. "0z"
	for _, k := range others {
		if err := s.Add(k); err != nil {
			t.Fatal(err)
		}
	}
	if s.Contains("0") {
		t.Fatalf("premature cascade; %v", members(s))
	}

	if err := s.Add(grandkids[31]); err != nil {
		t.Fatal(err)
	}
	expectMembers(t, s, []string{"0"})
}

func TestPrefixSetGlobalRoot(t *testing.T) {
	s := NewPrefixSet()
	if err := s.Add(""); err != nil {
		t.Fatal(err)
	}
	expectMembers(t, s, []string{""})

	// Everything is absorbed into the global prefix.
	if err := s.Add("0123"); err != nil {
		t.Fatal(err)
	}
	expectMembers(t, s, []string{""})

	// Subtracting a region expands the root only to that depth.
	if err := s.Subtract("0"); err != nil {
		t.Fatal(err)
	}
	want := children("")[1:] // all top-level cells except "0"
	expectMembers(t, s, want)
}

func TestPrefixSetCollapseToGlobalRoot(t *testing.T) {
	s := NewPrefixSet()
	for _, k := range children("") {
		if err := s.Add(k); err != nil {
			t.Fatal(err)
		}
	}
	expectMembers(t, s, []string{""})
}

func TestPrefixSetRejectsInvalid(t *testing.T) {
	bad := []string{
		"a",             // 'a' is not in the geohash alphabet
		"o",             // 'o' is not in the geohash alphabet
		"01i",           // 'i' is not in the geohash alphabet
		"0123l",         // whitespace/uppercase excluded too; 'l' invalid
		"0000000000000", // 13 chars exceeds the maximum
	}
	for _, p := range bad {
		s := NewPrefixSet()
		before := members(s)
		if err := s.Add(p); err == nil {
			t.Errorf("add %q: expected error", p)
			continue
		}
		if got := members(s); len(got) != len(before) {
			t.Errorf("add %q mutated set on error: %v", p, got)
		}

		s2 := NewPrefixSet()
		if err := s2.Add("0"); err != nil {
			t.Fatal(err)
		}
		if err := s2.Subtract(p); err == nil {
			t.Errorf("subtract %q: expected error", p)
		}
		expectMembers(t, s2, []string{"0"})
	}
}

func TestPrefixSetNormalizeIdempotent(t *testing.T) {
	s := NewPrefixSet()
	inputs := []string{"00", "01", "0p", "w", "zzz"}
	for _, p := range inputs {
		if err := s.Add(p); err != nil {
			t.Fatal(err)
		}
	}
	want := members(s)

	// Re-adding the same members and descendants of them must not change the
	// canonical form.
	for _, p := range append(append([]string{}, want...), "00pb", "wzz") {
		if err := s.Add(p); err != nil {
			t.Fatal(err)
		}
	}
	expectMembers(t, s, want)
}

func TestPrefixSetSubtractExpandsBounded(t *testing.T) {
	// From "0", subtract the deeper region "01bc". Expansion stops at depth 4.
	s := NewPrefixSet()
	if err := s.Add("0"); err != nil {
		t.Fatal(err)
	}
	if err := s.Subtract("01bc"); err != nil {
		t.Fatal(err)
	}

	// Retained siblings at each depth along the path to "01bc".
	target := "01bc"
	want := []string{}
	for depth := 1; depth < len(target); depth++ {
		parent := target[:depth]
		branch := target[depth]
		for _, k := range children(parent) {
			if k[depth] == branch {
				continue
			}
			want = append(want, k)
		}
	}
	sort.Strings(want)
	expectMembers(t, s, want)
	if s.Len() != 3*31 {
		t.Fatalf("len = %d, want %d", s.Len(), 3*31)
	}

	// No member is deeper than the subtracted prefix.
	for _, p := range s.Prefixes() {
		if len(p) > len("01bc") {
			t.Fatalf("member %q deeper than the subtraction precision", p)
		}
	}

	// The removed cell is no longer covered, but a sibling cell still is.
	if s.Covers("01bcde") {
		t.Fatal("subtracted region still covered")
	}
	if !s.Covers("01bd") {
		t.Fatal("sibling region should still be covered")
	}
	if !s.Covers("00zz") {
		t.Fatal("unrelated branch should still be covered")
	}
}

func TestPrefixSetSubtractThenAddRestores(t *testing.T) {
	s := NewPrefixSet()
	if err := s.Add("0"); err != nil {
		t.Fatal(err)
	}
	original := members(s)

	target := "01bc"
	if err := s.Subtract(target); err != nil {
		t.Fatal(err)
	}
	if s.Covers(target) {
		t.Fatal("target still covered after subtraction")
	}

	// Adding the removed region back restores the exact original coverage.
	if err := s.Add(target); err != nil {
		t.Fatal(err)
	}
	expectMembers(t, s, original)

	// Restoring from an even deeper subtraction works as well.
	deep := "0pbcdefghjkm"
	if err := s.Subtract(deep); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(deep); err != nil {
		t.Fatal(err)
	}
	expectMembers(t, s, original)
}

func TestPrefixSetSubtractNotContained(t *testing.T) {
	s := NewPrefixSet()
	if err := s.Add("0"); err != nil {
		t.Fatal(err)
	}
	// Subtracting outside any covered region is a no-op.
	if err := s.Subtract("w"); err != nil {
		t.Fatal(err)
	}
	expectMembers(t, s, []string{"0"})
}

func TestPrefixSetCovers(t *testing.T) {
	s := NewPrefixSet()
	for _, p := range []string{"0", "wxy"} {
		if err := s.Add(p); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]bool{
		"0":      true,
		"00":     true,
		"0zzzzz": true,
		"wxy":    true,
		"wxyz":   true,
		"w":      false,
		"ww":     false,
		"wx":     false,
		"1":      false,
	}
	for hash, want := range cases {
		if got := s.Covers(hash); got != want {
			t.Errorf("Covers(%q) = %v, want %v", hash, got, want)
		}
	}
}
