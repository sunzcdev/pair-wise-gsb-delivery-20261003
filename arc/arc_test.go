package arc_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"io"
	"strings"
	"testing"

	"github.com/emersion/go-msgauth/arc"
	"github.com/emersion/go-msgauth/authres"
	"github.com/emersion/go-msgauth/dkim"
)

const originalMail = "From: joe@example.com\r\n" +
	"To: list@relay2.example\r\n" +
	"Subject: hello\r\n" +
	"\r\n" +
	"original body\r\n"

type testKey struct {
	priv ed25519.PrivateKey
}

type keyStore struct {
	keys map[string]ed25519.PublicKey // "selector/domain"
}

func (s *keyStore) LookupARCKey(domain, selector string) (*dkim.PublicKey, error) {
	pub, ok := s.keys[selector+"/"+domain]
	if !ok {
		return nil, nil
	}
	return dkim.NewPublicKey(pub)
}

func newTestKeys() (map[string]*testKey, *keyStore) {
	store := &keyStore{keys: map[string]ed25519.PublicKey{}}
	privs := map[string]*testKey{}
	for _, id := range []string{
		"arc/relay1.example",
		"arc/relay2.example",
	} {
		pub, priv, _ := ed25519.GenerateKey(strings.NewReader(strings.Repeat(id, 8)))
		store.keys[id] = pub
		privs[id] = &testKey{priv: priv}
	}
	return privs, store
}

func seal(t *testing.T, r io.Reader, domain, authserv string, priv ed25519.PrivateKey, resolver arc.KeyResolver, cv arc.ChainValidationValue) string {
	return sealKeys(t, r, domain, authserv, priv, resolver, cv, nil)
}

func sealKeys(t *testing.T, r io.Reader, domain, authserv string, priv ed25519.PrivateKey, resolver arc.KeyResolver, cv arc.ChainValidationValue, headerKeys []string) string {
	t.Helper()
	var b bytes.Buffer
	err := arc.Seal(&b, r, &arc.SealOptions{
		Domain:               domain,
		Selector:             "arc",
		Signer:               priv,
		AuthenticationServID: authserv,
		AuthResults: []authres.Result{
			&authres.DKIMResult{Value: authres.ResultPass, Domain: "example.com"},
		},
		CV:         cv,
		Resolver:   resolver,
		HeaderKeys: headerKeys,
	})
	if err != nil {
		t.Fatalf("Seal(%s): %v", domain, err)
	}
	return b.String()
}

func TestTwoHopChain(t *testing.T) {
	privs, store := newTestKeys()

	// Hop 1: relay1 authenticates and performs the first seal (cv=none).
	hop1 := seal(t, strings.NewReader(originalMail),
		"relay1.example", "relay1.example", privs["arc/relay1.example"].priv, store, arc.ChainValidationNone)

	v1, err := arc.VerifyWithOptions(strings.NewReader(hop1), &arc.VerifyOptions{Resolver: store})
	if err != nil {
		t.Fatalf("verify hop1: %v", err)
	}
	if v1.Result != arc.ChainPass || v1.Instances() != 1 || v1.Err != nil {
		t.Fatalf("hop1: result=%q err=%v", v1.Result, v1.Err)
	}

	// Hop 2: relay2 modifies the body and seals again (cv auto -> pass).
	modified := strings.Replace(hop1, "original body\r\n", "rewritten by mailing list\r\n", 1)
	if modified == hop1 {
		t.Fatal("body not modified")
	}
	hop2 := seal(t, strings.NewReader(modified),
		"relay2.example", "relay2.example", privs["arc/relay2.example"].priv, store, arc.ChainValidationPass)

	v2, err := arc.VerifyWithOptions(strings.NewReader(hop2), &arc.VerifyOptions{Resolver: store})
	if err != nil {
		t.Fatalf("verify hop2: %v", err)
	}
	if v2.Result != arc.ChainPass || v2.Instances() != 2 || v2.Err != nil {
		t.Fatalf("hop2: result=%q err=%v sets=%d", v2.Result, v2.Err, v2.Instances())
	}

	// Old AMS body hash is stale but the chain must still pass.
	if v2.Sets[0].AMSBodyErr == nil {
		t.Errorf("expected stale body diagnostic on old AMS")
	}
	if v2.Sets[0].AMSErr != nil || v2.Sets[1].AMSErr != nil || v2.Sets[1].ASErr != nil {
		t.Errorf("unexpected signature errors: old=%v new=%v seal=%v",
			v2.Sets[0].AMSErr, v2.Sets[1].AMSErr, v2.Sets[1].ASErr)
	}

	// Trust decision is the caller's: a valid chain from an untrusted
	// domain must not be accepted.
	if v2.Trusted([]string{"relay1.example"}) {
		t.Errorf("newest seal is relay2; relay1-only trust must not pass")
	}
	if !v2.Trusted([]string{"relay2.example"}) {
		t.Errorf("expected trusted newest domain relay2")
	}
}

func buildTwoHopChain(t *testing.T) (string, *keyStore) {
	privs, store := newTestKeys()
	incoming := strings.Replace(originalMail, "Subject: hello\r\n",
		"Subject: hello\r\nX-Hop1-Only: seen at first hop\r\n", 1)
	hop1 := seal(t, strings.NewReader(incoming),
		"relay1.example", "relay1.example", privs["arc/relay1.example"].priv, store, arc.ChainValidationNone)
	modified := strings.Replace(hop1, "original body\r\n", "rewritten\r\n", 1)
	// relay2 chooses not to cover X-Hop1-Only in its own AMS.
	hop2 := sealKeys(t, strings.NewReader(modified),
		"relay2.example", "relay2.example", privs["arc/relay2.example"].priv, store,
		arc.ChainValidationPass, []string{"From", "To", "Subject"})
	return hop2, store
}

func verify(t *testing.T, msg string, store *keyStore) *arc.Verification {
	t.Helper()
	v, err := arc.VerifyWithOptions(strings.NewReader(msg), &arc.VerifyOptions{Resolver: store})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	return v
}

func TestTamperOldSeal(t *testing.T) {
	msg, store := buildTwoHopChain(t)

	// Add an unknown tag to the old ARC-Seal (i=1). It is structurally
	// harmless, so the break is detected one step later when AS(2) is
	// checked.
	tampered := strings.Replace(msg, "cv=none;", "x=evil; cv=none;", 1)
	if tampered == msg {
		t.Fatal("could not locate AS(1)")
	}

	v := verify(t, tampered, store)
	if v.Result != arc.ChainFail {
		t.Fatalf("expected fail, got %q (%v)", v.Result, v.Err)
	}
	if !strings.Contains(v.Err.Error(), "ARC-Seal (i=2)") ||
		!strings.Contains(v.Err.Error(), "did not verify") {
		t.Fatalf("expected break at AS(2) verification, got: %v", v.Err)
	}
}

func TestDeleteSignedHeader(t *testing.T) {
	msg, store := buildTwoHopChain(t)

	// Remove a header covered only by the old AMS(1) (relay2 chose not to
	// sign it in AMS(2)). The newest AMS still verifies, but AMS(1) does
	// not, so AS(2)'s claim cv=pass is inconsistent.
	tampered := strings.Replace(msg, "X-Hop1-Only: seen at first hop\r\n", "", 1)

	v := verify(t, tampered, store)
	if v.Result != arc.ChainFail {
		t.Fatalf("expected fail, got %q (%v)", v.Result, v.Err)
	}
	if !strings.Contains(v.Err.Error(), "cv mismatch") {
		t.Fatalf("expected break at AS(2) cv check, got: %v", v.Err)
	}
	if v.Sets[0].AMSErr == nil {
		t.Errorf("expected AMS(1) header signature failure")
	}
}

func TestDeleteAARSet(t *testing.T) {
	msg, store := buildTwoHopChain(t)

	// Remove the whole AAR of instance 1: instance numbers are no longer
	// complete.
	tampered := removeHeader(msg, "ARC-Authentication-Results: i=1;")

	v := verify(t, tampered, store)
	if v.Result != arc.ChainFail || !strings.Contains(v.Err.Error(), "instance 1 is missing ARC-Authentication-Results") {
		t.Fatalf("expected structural failure at AAR(1), got: %v", v.Err)
	}
}

func TestDuplicateInstance(t *testing.T) {
	msg, store := buildTwoHopChain(t)

	// Duplicate the ARC-Seal with instance number 1.
	dup := duplicateHeader(msg, "ARC-Seal: a=ed25519-sha256; cv=none;")

	v := verify(t, dup, store)
	if v.Result != arc.ChainFail ||
		!strings.Contains(v.Err.Error(), "duplicate ARC-Seal with instance number 1") {
		t.Fatalf("expected duplicate-instance failure, got: %v", v.Err)
	}
}

// removeHeader removes one raw header field, including any folded
// continuation lines. prefix is the start of the field line (name + value).
func removeHeader(msg, prefix string) string {
	return alterHeader(msg, prefix, false)
}

// duplicateHeader inserts a second copy of the matching raw header field
// immediately before the original.
func duplicateHeader(msg, prefix string) string {
	return alterHeader(msg, prefix, true)
}

func alterHeader(msg, prefix string, duplicate bool) string {
	start := strings.Index(msg, prefix)
	if start < 0 {
		return msg
	}
	end := strings.Index(msg[start:], "\r\n")
	if end < 0 {
		return msg
	}
	end = start + end + len("\r\n")
	field := msg[start:end]
	if duplicate {
		return msg[:start] + field + msg[start:]
	}
	return msg[:start] + msg[end:]
}

func TestNoARC(t *testing.T) {
	store := &keyStore{keys: map[string]ed25519.PublicKey{}}
	v, err := arc.VerifyWithOptions(strings.NewReader(originalMail), &arc.VerifyOptions{Resolver: store})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if v.Result != arc.ChainNone {
		t.Fatalf("expected none, got %q", v.Result)
	}
}

func TestRSATwoHopChain(t *testing.T) {
	type rsaKey struct {
		priv *rsa.PrivateKey
	}
	priv1, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	priv2, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	store := &rsaStore{keys: map[string]*rsa.PublicKey{
		"relay1.example": &priv1.PublicKey,
		"relay2.example": &priv2.PublicKey,
	}}
	_ = rsaKey{}

	sealRSA := func(domain string, priv *rsa.PrivateKey, in string, cv arc.ChainValidationValue) string {
		var b bytes.Buffer
		if err := arc.Seal(&b, strings.NewReader(in), &arc.SealOptions{
			Domain: domain, Selector: "arc", Signer: priv,
			AuthenticationServID: domain,
			AuthResults:          []authres.Result{&authres.SPFResult{Value: authres.ResultPass, From: "example.com"}},
			CV:                   cv,
			Resolver:             store,
		}); err != nil {
			t.Fatalf("seal: %v", err)
		}
		return b.String()
	}

	hop1 := sealRSA("relay1.example", priv1, originalMail, arc.ChainValidationNone)
	modified := strings.Replace(hop1, "original body\r\n", "rewritten body\r\n", 1)
	hop2 := sealRSA("relay2.example", priv2, modified, arc.ChainValidationPass)

	v, err := arc.VerifyWithOptions(strings.NewReader(hop2), &arc.VerifyOptions{Resolver: store})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if v.Result != arc.ChainPass || v.Err != nil {
		t.Fatalf("expected pass, got %q: %v", v.Result, v.Err)
	}
}

type rsaStore struct {
	keys map[string]*rsa.PublicKey
}

func (s *rsaStore) LookupARCKey(domain, selector string) (*dkim.PublicKey, error) {
	if pub, ok := s.keys[domain]; ok {
		return dkim.NewPublicKey(pub)
	}
	return nil, nil
}
