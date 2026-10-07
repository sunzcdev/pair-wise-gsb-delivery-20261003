package etree

import (
	"errors"
	"math/rand"
	"testing"
)

func TestTransplantRandomizedMulti(t *testing.T) {
	uris := []string{"urn:A", "urn:B", "urn:C", "urn:D", "urn:E"}
	prefixNames := []string{"a", "b", "p", "q", "x"}
	for seed := int64(0); seed < 40; seed++ {
		rng := rand.New(rand.NewSource(seed))
		for iter := 0; iter < 200; iter++ {
			srcXML, pickTag := randomTree(rng, uris, prefixNames, "src")
			dstXML, _ := randomTree(rng, uris, prefixNames, "dst")
			srcDoc := mustDoc(t, srcXML)
			dstDoc := mustDoc(t, dstXML)
			srcEl := findElementByTag(srcDoc.Root(), pickTag)
			if srcEl == nil {
				continue
			}
			targets := collectSlots(dstDoc.Root())
			if len(targets) == 0 {
				continue
			}
			target := targets[int(rng.Int63())%len(targets)]
			before := subtreeSignature(srcEl, nil)
			got, err := Transplant(target, srcEl, nil)
			if err != nil {
				if !errors.Is(err, ErrUndefinedPrefix) {
					t.Fatalf("seed %d iter %d: unexpected error %v", seed, iter, err)
				}
				continue
			}
			after := subtreeSignature(got, nil)
			if len(before) != len(after) {
				t.Fatalf("seed %d iter %d len mismatch:\nbefore=%v\nafter=%v\nsrc=%s\ndst=%s", seed, iter, before, after, srcXML, dstXML)
			}
			for i := range before {
				if before[i] != after[i] {
					t.Fatalf("seed %d iter %d mismatch:\nbefore=%v\nafter=%v\nsrc=%s\ndst=%s", seed, iter, before, after, srcXML, dstXML)
				}
			}
			out, _ := dstDoc.WriteToString()
			if err := roundTripOK(out); err != nil {
				t.Fatalf("seed %d iter %d malformed: %v\n%s", seed, iter, err, out)
			}
		}
	}
}
