package vm

import "testing"

func TestMaskBitset(t *testing.T) {
	m := newMask(65, false)
	for i := 0; i < 65; i++ {
		if m.get(i) {
			t.Fatalf("bit %d set in empty mask", i)
		}
	}
	m.set(0)
	m.set(64)
	m.set(33)
	if !m.get(0) || !m.get(64) || !m.get(33) {
		t.Fatal("set bits not visible")
	}
	m.clear(33)
	if m.get(33) || !m.get(64) {
		t.Fatal("clear/set mismatch")
	}

	all := newMask(65, true)
	if !all.get(64) || !all.get(0) {
		t.Fatal("full mask missing edge bits")
	}
	all.andNot(m)
	if all.get(0) || all.get(64) || !all.get(1) {
		t.Fatal("andNot mismatch")
	}
}

func TestMaskCloneIndependent(t *testing.T) {
	a := newMask(10, true)
	c := a.clone()
	c.clear(3)
	if !a.get(3) {
		t.Fatal("clone must not share storage")
	}
}

func TestMaskOr(t *testing.T) {
	a := newMask(70, false)
	a.set(5)
	b := newMask(70, false)
	b.set(69)
	a.or(b)
	if !a.get(5) || !a.get(69) {
		t.Fatal("or merge failed across words")
	}
}
