package arc_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/emersion/go-msgauth/arc"
	"github.com/emersion/go-msgauth/dkim"
)

func TestSimpleCanonicalizationChain(t *testing.T) {
	privs, store := newTestKeys()

	sealCan := func(domain string, priv ed25519.PrivateKey, in string, cv arc.ChainValidationValue) string {
		var b bytes.Buffer
		err := arc.Seal(&b, strings.NewReader(in), &arc.SealOptions{
			Domain: domain, Selector: "arc", Signer: priv,
			AuthenticationServID:   domain,
			HeaderCanonicalization: dkim.CanonicalizationSimple,
			BodyCanonicalization:   dkim.CanonicalizationSimple,
			CV:                     cv, Resolver: store,
		})
		if err != nil {
			t.Fatalf("seal: %v", err)
		}
		return b.String()
	}

	hop1 := sealCan("relay1.example", privs["arc/relay1.example"].priv, originalMail, arc.ChainValidationNone)
	hop2 := sealCan("relay2.example", privs["arc/relay2.example"].priv, hop1, arc.ChainValidationPass)

	v := verify(t, hop2, store)
	if v.Result != arc.ChainPass || v.Err != nil {
		t.Fatalf("expected pass, got %q: %v", v.Result, v.Err)
	}
}

func TestTamperNewestSealCV(t *testing.T) {
	msg, store := buildTwoHopChain(t)

	// Flip AS(2)'s claim from pass to fail. The signature covers cv, so the
	// seal itself stops verifying.
	tampered := strings.Replace(msg,
		"ARC-Seal: a=ed25519-sha256; cv=pass;",
		"ARC-Seal: a=ed25519-sha256; cv=fail;", 1)
	if tampered == msg {
		t.Fatal("could not locate AS(2) cv")
	}

	v := verify(t, tampered, store)
	if v.Result != arc.ChainFail ||
		!strings.Contains(v.Err.Error(), "ARC-Seal (i=2) signature did not verify") {
		t.Fatalf("expected AS(2) verification failure, got: %v", v.Err)
	}
}

func TestTamperNewestBody(t *testing.T) {
	msg, store := buildTwoHopChain(t)

	tampered := strings.Replace(msg, "rewritten\r\n", "tampered body after sealing\r\n", 1)
	v := verify(t, tampered, store)
	if v.Result != arc.ChainFail ||
		!strings.Contains(v.Err.Error(), "body hash") {
		t.Fatalf("expected newest AMS body hash failure, got: %v", v.Err)
	}
}

func TestSealRejectsMalformedExistingChain(t *testing.T) {
	msg, store := buildTwoHopChain(t)

	// A second instance without instance 3 is a broken chain; sealing on
	// top must be refused rather than papered over.
	gap := removeHeader(msg, "ARC-Authentication-Results: i=2;")

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	err = arc.Seal(&b, strings.NewReader(gap), &arc.SealOptions{
		Domain: "relay3.example", Selector: "arc", Signer: priv,
		AuthenticationServID: "relay3.example",
		CV:                   arc.ChainValidationFail,
		Resolver:             store,
	})
	if err == nil {
		t.Fatal("expected sealing on a gapped chain to fail")
	}
}
