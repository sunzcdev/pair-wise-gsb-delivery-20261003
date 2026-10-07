package geohash

import (
	"math/rand"
	"reflect"
	"testing"
)

const testAlphabet = "0123456789bcdefghjkmnpqrstuvwxyz"

// allPrefixes returns every prefix of the given length (32^length strings).
func allPrefixes(length int) []string {
	if length == 0 {
		return []string{""}
	}
	var out []string
	for _, parent := range allPrefixes(length - 1) {
		for _, c := range testAlphabet {
			out = append(out, parent+string(c))
		}
	}
	return out
}

func assertPrefixes(t *testing.T, p *PrefixSet, want []string) {
	t.Helper()
	got := p.Prefixes()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("prefixes = %v, want %v", got, want)
	}
}

func TestPrefixSetAddFolds32Children(t *testing.T) {
	p := NewPrefixSet()

	// 31 children are not enough to fold: arity is 32.
	for _, c := range testAlphabet[:31] {
		if err := p.Add(string(c)); err != nil {
			t.Fatal(err)
		}
	}
	if p.Len() != 31 {
		t.Fatalf("len = %d, want 31", p.Len())
	}

	// The 32nd completes the parent (the whole globe) and folds to "".
	if err := p.Add(string(testAlphabet[31])); err != nil {
		t.Fatal(err)
	}
	assertPrefixes(t, p, []string{""})
}

func TestPrefixSetFoldIsNotBinary(t *testing.T) {
	// Fewer than 32 siblings, including the first two, must never fold.
	for _, n := range []int{1, 2, 5, 16, 31} {
		p := NewPrefixSet()
		for _, c := range testAlphabet[:n] {
			if err := p.Add(string(c)); err != nil {
				t.Fatal(err)
			}
		}
		if p.Len() != n {
			t.Fatalf("with %d children, len = %d, want %d (no folding below 32)", n, p.Len(), n)
		}
	}

	// Folding also applies one level down: 32 children of "0" fold to "0".
	p := NewPrefixSet()
	for _, c := range testAlphabet {
		if err := p.Add("0" + string(c)); err != nil {
			t.Fatal(err)
		}
	}
	assertPrefixes(t, p, []string{"0"})
}

func TestPrefixSetAddRemovesRedundantDescendants(t *testing.T) {
	p := NewPrefixSet()
	for _, suffix := range []string{"00", "01", "zz", "bcd"} {
		if err := p.Add(suffix); err != nil {
			t.Fatal(err)
		}
	}

	// Adding "0" absorbs "00" and "01".
	if err := p.Add("0"); err != nil {
		t.Fatal(err)
	}
	assertPrefixes(t, p, []string{"0", "bcd", "zz"})

	// Adding a descendant of an existing prefix is a no-op.
	if err := p.Add("0xyz"); err != nil {
		t.Fatal(err)
	}
	assertPrefixes(t, p, []string{"0", "bcd", "zz"})

	// The empty prefix covers the globe and removes everything else.
	if err := p.Add(""); err != nil {
		t.Fatal(err)
	}
	assertPrefixes(t, p, []string{""})
}

func TestPrefixSetSubtract(t *testing.T) {
	p := NewPrefixSet()
	if err := p.Add(""); err != nil {
		t.Fatal(err)
	}

	// Remove one character-level cell from the globe.
	if err := p.Subtract("0"); err != nil {
		t.Fatal(err)
	}
	if p.Len() != 31 {
		t.Fatalf("len = %d, want 31", p.Len())
	}
	if p.Covered("0") || p.Covered("00") {
		t.Fatal("removed region should not be covered")
	}
	if !p.Covered("1") || !p.Covered("zzzz") {
		t.Fatal("remaining regions should be covered")
	}

	// Adding it back restores the single folded prefix.
	if err := p.Add("0"); err != nil {
		t.Fatal(err)
	}
	assertPrefixes(t, p, []string{""})

	// Removing a deeper cell expands only as deep as needed.
	if err := p.Subtract("00"); err != nil {
		t.Fatal(err)
	}
	if p.Len() != 62 {
		t.Fatalf("len = %d, want 62", p.Len())
	}
	if p.Covered("00") || p.Covered("000") {
		t.Fatal("removed region should not be covered")
	}
	for _, cell := range []string{"01", "0z", "1", "zz"} {
		if !p.Covered(cell) {
			t.Fatalf("%q should be covered", cell)
		}
	}

	// Subtracting an uncovered region is a no-op.
	before := p.Prefixes()
	if err := p.Subtract("00"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.Prefixes(), before) {
		t.Fatal("subtracting an uncovered region changed the set")
	}

	// Subtracting the empty prefix empties the globe.
	if err := p.Subtract(""); err != nil {
		t.Fatal(err)
	}
	if p.Len() != 0 {
		t.Fatalf("len = %d, want 0", p.Len())
	}
}

func TestPrefixSetSubtractDoesNotOverExpand(t *testing.T) {
	p := NewPrefixSet()
	if err := p.Add(""); err != nil {
		t.Fatal(err)
	}
	if err := p.Subtract("012345678901"); err != nil {
		t.Fatal(err)
	}

	// 11 levels of 31 siblings plus 31 cells at the final level.
	if p.Len() != 11*31+31 {
		t.Fatalf("len = %d, want %d", p.Len(), 11*31+31)
	}
	for _, prefix := range p.Prefixes() {
		if len(prefix) > 12 {
			t.Fatalf("expanded to length %d, exceeding the subtraction depth", len(prefix))
		}
	}
	if p.Covered("012345678901") {
		t.Fatal("removed region should not be covered")
	}
	if !p.Covered("012345678900") || !p.Covered("012345678902") || !p.Covered("zz") {
		t.Fatal("neighbouring regions should remain covered")
	}
}

func TestPrefixSetInvalidInputRejectedAtomically(t *testing.T) {
	bad := []string{"o", "i", "a!", "0123456789abc"}
	for _, input := range bad {
		p := NewPrefixSet()
		if err := p.Add("0"); err != nil {
			t.Fatal(err)
		}
		if err := p.Add(input); err == nil {
			t.Fatalf("Add(%q) expected error", input)
		}
		assertPrefixes(t, p, []string{"0"})

		if err := p.Subtract(input); err == nil {
			t.Fatalf("Subtract(%q) expected error", input)
		}
		assertPrefixes(t, p, []string{"0"})
	}

	// Empty prefix is legal and means the whole globe.
	if err := Validate(""); err != nil {
		t.Fatalf("empty prefix should be valid: %v", err)
	}
}

// randomPrefix builds a valid prefix up to maxLen characters.
func randomPrefix(r *rand.Rand, maxLen int) string {
	n := r.Intn(maxLen + 1)
	b := make([]byte, n)
	for i := range b {
		b[i] = testAlphabet[r.Intn(32)]
	}
	return string(b)
}

func TestPrefixSetOrderIndependentAndIdempotent(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	cells := allPrefixes(2)

	for iter := 0; iter < 200; iter++ {
		inputs := make([]string, r.Intn(40)+1)
		for i := range inputs {
			inputs[i] = randomPrefix(r, 2)
		}

		// Reference coverage oracle at depth 2.
		reference := func(cell string) bool {
			for _, in := range inputs {
				if len(in) <= len(cell) && cell[:len(in)] == in {
					return true
				}
			}
			return false
		}

		build := func(order []int) *PrefixSet {
			p := NewPrefixSet()
			for _, i := range order {
				if err := p.Add(inputs[i]); err != nil {
					t.Fatal(err)
				}
			}
			return p
		}

		order := r.Perm(len(inputs))
		p := build(order)

		// Coverage matches the reference.
		for _, cell := range cells {
			if p.Covered(cell) != reference(cell) {
				t.Fatalf("coverage mismatch for %q: got %v want %v", cell, p.Covered(cell), reference(cell))
			}
		}

		// Order independence: reverse order produces the same canonical set.
		reversed := make([]int, len(order))
		for i, j := range order {
			reversed[len(order)-1-i] = j
		}
		if !reflect.DeepEqual(build(reversed).Prefixes(), p.Prefixes()) {
			t.Fatal("canonical form depends on insertion order")
		}

		// Normalization is idempotent: rebuild from canonical prefixes, and
		// re-add each existing prefix.
		canonical := p.Prefixes()
		fresh := NewPrefixSet()
		for _, prefix := range canonical {
			if err := fresh.Add(prefix); err != nil {
				t.Fatal(err)
			}
		}
		if !reflect.DeepEqual(fresh.Prefixes(), canonical) {
			t.Fatal("normalization is not idempotent")
		}
		for _, prefix := range canonical {
			if err := p.Add(prefix); err != nil {
				t.Fatal(err)
			}
		}
		if !reflect.DeepEqual(p.Prefixes(), canonical) {
			t.Fatal("re-adding canonical prefixes changed the set")
		}
	}
}

func TestPrefixSetSubtractThenAddRestoresCoverage(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	cells := allPrefixes(2)

	for iter := 0; iter < 100; iter++ {
		removed := make([]string, r.Intn(10)+1)
		for i := range removed {
			removed[i] = randomPrefix(r, 2)
		}

		p := NewPrefixSet()
		if err := p.Add(""); err != nil {
			t.Fatal(err)
		}
		for _, prefix := range removed {
			if err := p.Subtract(prefix); err != nil {
				t.Fatal(err)
			}
		}

		// Reference: covered unless inside a removed region.
		wantCovered := func(cell string) bool {
			for _, rm := range removed {
				if len(rm) <= len(cell) && cell[:len(rm)] == rm {
					return false
				}
			}
			return true
		}
		for _, cell := range cells {
			if p.Covered(cell) != wantCovered(cell) {
				t.Fatalf("after subtraction %q: coverage mismatch for %q", removed, cell)
			}
		}

		// Add the removed regions back (shuffled): full globe must return.
		for _, i := range r.Perm(len(removed)) {
			if err := p.Add(removed[i]); err != nil {
				t.Fatal(err)
			}
		}
		assertPrefixes(t, p, []string{""})
		for _, cell := range cells {
			if !p.Covered(cell) {
				t.Fatalf("coverage not restored for %q", cell)
			}
		}
	}
}
