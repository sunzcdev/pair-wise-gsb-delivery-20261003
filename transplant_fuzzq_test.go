// Copyright 2015-2019 Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package etree

import (
	"errors"
	"math/rand"
	"sort"
	"testing"
)

func TestTransplantRandomizedQNames(t *testing.T) {
	qna := NewQNameAttrs().Register("urn:meta", "refs")
	uris := []string{"urn:A", "urn:B", "urn:C"}
	prefixNames := []string{"a", "b", "p"}
	for seed := int64(0); seed < 40; seed++ {
		rng := rand.New(rand.NewSource(seed + 100))
		for iter := 0; iter < 100; iter++ {
			srcXML, refsXML, decls := qnameSource(rng, uris, prefixNames)
			dstXML := qnameTarget(rng, uris, prefixNames)
			srcDoc := mustDoc(t, srcXML)
			dstDoc := mustDoc(t, dstXML)
			node := srcDoc.Root().SelectElement("node")
			targets := collectSlots(dstDoc.Root())
			if node == nil || len(targets) == 0 {
				continue
			}
			target := targets[int(rng.Int63())%len(targets)]

			got, err := Transplant(target, node, &TransplantSettings{QNameAttrs: qna})
			if err != nil {
				if !errors.Is(err, ErrUndefinedPrefix) {
					t.Fatalf("seed %d: unexpected error %v", seed, err)
				}
				continue
			}

			after := expandedQNameValues(got, qna)
			sort.Strings(after)
			want := make([]string, len(decls))
			for i, d := range decls {
				want[i] = d.uri + "|" + refsXML[i].local
			}
			sort.Strings(want)
			if len(after) != len(want) {
				t.Fatalf("seed %d iter %d: got %d values, want %d\nsrc=%s", seed, iter, len(after), len(want), srcXML)
			}
			for i := range want {
				if after[i] != want[i] {
					t.Fatalf("seed %d iter %d:\nwant=%v\ngot =%v\nsrc=%s", seed, iter, want, after, srcXML)
				}
			}
		}
	}
}

type qnameDecl struct {
	prefix string
	uri    string
}

type qnameToken struct {
	prefix string
	local  string
}

// qnameSource builds a subtree whose node attribute is a QName list, with
// the referenced prefixes declared at random depths around the tokens.
func qnameSource(rng *rand.Rand, uris, pre []string) (xml string, refs []qnameToken, decls []qnameDecl) {
	n := int(1 + rng.Int63n(4))
	chosen := make([]qnameDecl, n)
	for i := 0; i < n; i++ {
		chosen[i] = qnameDecl{
			prefix: pre[rng.Int63n(int64(len(pre)))],
			uri:    uris[rng.Int63n(int64(len(uris)))],
		}
	}

	open := `<src xmlns:m="urn:meta">`
	closeTags := "</src>"

	// Declare roughly half the prefixes on the outer src element and the
	// rest on wrappers; duplicate prefixes shadow each other.
	outer := map[string]string{}
	wrapperOpen := ""
	wrapperClose := ""
	for i, d := range chosen {
		tok := qnameToken{prefix: d.prefix, local: "t" + itoa(i)}
		refs = append(refs, tok)
		if rng.Int63n(2) == 0 {
			outer[d.prefix] = d.uri
		} else {
			wrapperOpen += `<w xmlns:` + d.prefix + `="` + d.uri + `">`
			wrapperClose = `</w>` + wrapperClose
		}
		decls = append(decls, d)
	}

	var value string
	for i, t := range refs {
		if i > 0 {
			value += "  "
		}
		value += t.prefix + ":" + t.local
	}

	for p, u := range outer {
		open += `<srcdecl xmlns:` + p + `="` + u + `">`
		closeTags = `</srcdecl>` + closeTags
	}

	xml = open + wrapperOpen +
		`<node m:refs="` + value + `"/>` +
		wrapperClose + closeTags
	return xml, refs, decls
}

// qnameTarget builds a target tree with random conflicting bindings.
func qnameTarget(rng *rand.Rand, uris, pre []string) string {
	b := `<dst>`
	open := 0
	for i := int(rng.Int63n(3)); i > 0; i-- {
		p := pre[rng.Int63n(int64(len(pre)))]
		u := uris[rng.Int63n(int64(len(uris)))]
		b += `<s` + itoa(i) + ` xmlns:` + p + `="` + u + `">`
		open++
	}
	b += `<slot/>`
	for i := 1; i <= open; i++ {
		b += `</s` + itoa(i) + `>`
	}
	b += `</dst>`
	return b
}
