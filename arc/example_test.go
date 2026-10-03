package arc_test

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"strings"

	"github.com/emersion/go-msgauth/arc"
	"github.com/emersion/go-msgauth/authres"
	"github.com/emersion/go-msgauth/dkim"
)

type exampleResolver struct {
	pub ed25519.PublicKey
}

func (r exampleResolver) LookupARCKey(domain, selector string) (*dkim.PublicKey, error) {
	if domain == "relay.example" {
		return dkim.NewPublicKey(r.pub)
	}
	return nil, nil
}

func ExampleSeal() {
	pub, priv, err := ed25519.GenerateKey(strings.NewReader(
		"deterministic-seed-for-example-ok"))
	if err != nil {
		panic(err)
	}
	resolver := exampleResolver{pub: pub}

	msg := "From: alice@example.com\r\nSubject: hi\r\n\r\nbody\r\n"

	// First hop: authenticate and seal with cv=none.
	var sealed bytes.Buffer
	if err := arc.Seal(&sealed, strings.NewReader(msg), &arc.SealOptions{
		Domain:               "relay.example",
		Selector:             "arc",
		Signer:               priv,
		AuthenticationServID: "relay.example",
		AuthResults: []authres.Result{
			&authres.DKIMResult{Value: authres.ResultPass, Domain: "example.com"},
		},
		CV:       arc.ChainValidationNone,
		Resolver: resolver,
	}); err != nil {
		panic(err)
	}

	v, err := arc.VerifyWithOptions(&sealed, &arc.VerifyOptions{Resolver: resolver})
	if err != nil {
		panic(err)
	}

	fmt.Println("chain:", v.Result, "instances:", v.Instances())

	// The caller decides which sealing domains it trusts. A pass from an
	// unknown domain must not be accepted.
	fmt.Println("trusted:", v.Trusted([]string{"relay.example"}))
	fmt.Println("untrusted:", v.Trusted([]string{"other.example"}))

	// Output:
	// chain: pass instances: 1
	// trusted: true
	// untrusted: false
}
