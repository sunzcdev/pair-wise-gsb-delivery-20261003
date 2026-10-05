/*-
 * Copyright 2024 go-jose Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package jose

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

const (
	roleHeader    = "x-internal-role"
	versionHeader = "x-internal-version"
)

var errRoleMismatch = errors.New("role mismatch")

func roleExtension(stage CriticalHeaderStage, want string, seen *[]string) CriticalHeaderExtension {
	return CriticalHeaderExtension{
		Name:       roleHeader,
		Parameters: []string{versionHeader},
		Stage:      stage,
		Validate: func(params CriticalHeaderParams) error {
			var role string
			ok, err := params.Unmarshal(roleHeader, &role)
			if err != nil || !ok {
				return fmt.Errorf("missing %s", roleHeader)
			}
			// Parameters declared in Parameters must be delivered raw.
			if _, ok := params.Raw(versionHeader); !ok {
				return fmt.Errorf("missing %s", versionHeader)
			}
			if seen != nil {
				*seen = append(*seen, role)
			}
			if role != want {
				return errRoleMismatch
			}
			return nil
		},
	}
}

func mustNewVerifier(t *testing.T, exts ...CriticalHeaderExtension) *Verifier {
	t.Helper()
	v, err := NewVerifier(exts...)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v
}

func signCriticalJWS(t *testing.T, role string, version int) *JSONWebSignature {
	t.Helper()
	opts := new(SignerOptions).
		WithHeader(HeaderKey(roleHeader), role).
		WithHeader(HeaderKey(versionHeader), version).
		WithCritical(roleHeader, versionHeader)
	signer, err := NewSigner(SigningKey{PS256, rsaTestKey}, opts)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	obj, err := signer.Sign([]byte("critical payload"))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return obj
}

func TestVerifierCriticalJWS(t *testing.T) {
	obj := signCriticalJWS(t, "server", 1)

	// A verifier without an extension for the crit names must reject the
	// object; unknown crit headers are never skipped.
	plain := mustNewVerifier(t)
	if _, err := plain.Verify(obj, &rsaTestKey.PublicKey); !errors.Is(err, ErrUnsupportedCriticalHeader) {
		t.Fatalf("plain verifier: got %v, want ErrUnsupportedCriticalHeader", err)
	}

	// Package-level Verify keeps the default behavior (built-in only).
	if _, err := obj.Verify(&rsaTestKey.PublicKey); !errors.Is(err, ErrUnsupportedCriticalHeader) {
		t.Fatalf("package Verify: got %v, want ErrUnsupportedCriticalHeader", err)
	}

	// A verifier that understands the headers accepts the object.
	var seen []string
	good := mustNewVerifier(t, roleExtension(CriticalHeaderStageAfterVerification, "server", &seen))
	if _, err := good.Verify(obj, &rsaTestKey.PublicKey); err != nil {
		t.Fatalf("good verifier: %v", err)
	}
	if len(seen) != 1 || seen[0] != "server" {
		t.Fatalf("callback saw %v, want [server]", seen)
	}

	// A differently configured verifier rejects based on the value.
	bad := mustNewVerifier(t, roleExtension(CriticalHeaderStageAfterVerification, "client", nil))
	if _, err := bad.Verify(obj, &rsaTestKey.PublicKey); !errors.Is(err, errRoleMismatch) {
		t.Fatalf("bad verifier: got %v, want errRoleMismatch", err)
	}

	// The good verifier must be unaffected by the other verifier.
	if _, err := good.Verify(obj, &rsaTestKey.PublicKey); err != nil {
		t.Fatalf("good verifier after bad verifier: %v", err)
	}
}

func TestVerifierCriticalStage(t *testing.T) {
	obj := signCriticalJWS(t, "server", 1)

	// BeforeVerification must run, and a failing callback rejects the object
	// before the signature check can succeed.
	before := mustNewVerifier(t, roleExtension(CriticalHeaderStageBeforeVerification, "client", nil))
	if _, err := before.Verify(obj, &rsaTestKey.PublicKey); !errors.Is(err, errRoleMismatch) {
		t.Fatalf("before verifier: got %v, want errRoleMismatch", err)
	}

	// An invalid stage is rejected at construction time.
	if _, err := NewVerifier(CriticalHeaderExtension{
		Name:     roleHeader,
		Stage:    CriticalHeaderStage(0),
		Validate: func(CriticalHeaderParams) error { return nil },
	}); err == nil {
		t.Fatal("expected error for zero stage")
	}
}

func TestVerifierIgnoresUnprotectedSameName(t *testing.T) {
	obj := signCriticalJWS(t, "server", 1)

	// Tamper with the unprotected header: inject an attacker-controlled value
	// using the same name as a critical parameter. Because it is not covered
	// by the signature and the extension only reads the protected header, the
	// object must verify and the callback must see the protected "server".
	obj.Signatures[0].header = &rawHeader{}
	if err := obj.Signatures[0].header.set(HeaderKey(roleHeader), "attacker"); err != nil {
		t.Fatal(err)
	}
	if err := obj.Signatures[0].header.set(HeaderKey(versionHeader), 999); err != nil {
		t.Fatal(err)
	}

	var seen []string
	v := mustNewVerifier(t, roleExtension(CriticalHeaderStageAfterVerification, "server", &seen))
	if _, err := v.Verify(obj, &rsaTestKey.PublicKey); err != nil {
		t.Fatalf("verify with spoofed unprotected header: %v", err)
	}
	if len(seen) != 1 || seen[0] != "server" {
		t.Fatalf("callback saw %v, want protected value [server]", seen)
	}

	// Conversely, "crit" itself must never appear unprotected.
	obj.Signatures[0].header = &rawHeader{}
	if err := obj.Signatures[0].header.set(headerCritical, []string{roleHeader}); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(obj, &rsaTestKey.PublicKey); !errors.Is(err, ErrUnsupportedCriticalHeader) {
		t.Fatalf("unprotected crit: got %v, want ErrUnsupportedCriticalHeader", err)
	}
}

func TestVerifierRejectsMissingDeclaredParameter(t *testing.T) {
	obj := signCriticalJWS(t, "server", 1)

	// Remove the protected version header while leaving it listed in crit.
	delete(*obj.Signatures[0].protected, HeaderKey(versionHeader))

	v := mustNewVerifier(t, roleExtension(CriticalHeaderStageAfterVerification, "server", nil))
	if _, err := v.Verify(obj, &rsaTestKey.PublicKey); !errors.Is(err, ErrUnsupportedCriticalHeader) {
		t.Fatalf("missing declared parameter: got %v, want ErrUnsupportedCriticalHeader", err)
	}
}

func TestVerifierRejectsMalformedCritical(t *testing.T) {
	obj := signCriticalJWS(t, "server", 1)

	// Empty crit list is invalid per RFC 7515.
	obj.Signatures[0].protected = &rawHeader{}
	if err := obj.Signatures[0].protected.set(headerCritical, []string{}); err != nil {
		t.Fatal(err)
	}
	v := mustNewVerifier(t)
	if _, err := v.Verify(obj, &rsaTestKey.PublicKey); !errors.Is(err, ErrUnsupportedCriticalHeader) {
		t.Fatalf("empty crit: got %v, want ErrUnsupportedCriticalHeader", err)
	}

	// Duplicate crit entries are invalid.
	obj = signCriticalJWS(t, "server", 1)
	if err := obj.Signatures[0].protected.set(headerCritical, []string{roleHeader, roleHeader}); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(obj, &rsaTestKey.PublicKey); !errors.Is(err, ErrUnsupportedCriticalHeader) {
		t.Fatalf("duplicate crit: got %v, want ErrUnsupportedCriticalHeader", err)
	}
}

func TestVerifierB64StillWorks(t *testing.T) {
	key := []byte("a-secret-key-that-is-long-enough-1234")
	opts := new(SignerOptions).WithBase64(false)
	signer, err := NewSigner(SigningKey{HS256, key}, opts)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := signer.Sign([]byte("$.02"))
	if err != nil {
		t.Fatal(err)
	}

	// Built-in b64 must keep working with both the default and a custom
	// verifier (which carries additional extensions).
	custom := mustNewVerifier(t, roleExtension(CriticalHeaderStageAfterVerification, "unused", nil))

	if _, err := obj.Verify(key); err != nil {
		t.Fatalf("default Verify with b64=false: %v", err)
	}
	if _, err := custom.Verify(obj, key); err != nil {
		t.Fatalf("custom Verifier with b64=false: %v", err)
	}
}

func TestVerifierJWE(t *testing.T) {
	rcpt := Recipient{Algorithm: RSA_OAEP_256, Key: &rsaTestKey.PublicKey}
	opts := new(EncrypterOptions).
		WithHeader(HeaderKey(roleHeader), "server").
		WithHeader(HeaderKey(versionHeader), 1).
		WithHeader(headerCritical, []string{roleHeader, versionHeader})
	enc, err := NewEncrypter(A256GCM, rcpt, opts)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := enc.Encrypt([]byte("critical plaintext"))
	if err != nil {
		t.Fatal(err)
	}
	serialized, err := encrypted.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	parse := func() *JSONWebEncryption {
		obj, err := ParseEncrypted(serialized, []KeyAlgorithm{RSA_OAEP_256}, []ContentEncryption{A256GCM})
		if err != nil {
			t.Fatal(err)
		}
		return obj
	}

	plain := mustNewVerifier(t)
	if _, err := plain.Decrypt(parse(), rsaTestKey); !errors.Is(err, ErrUnsupportedCriticalHeader) {
		t.Fatalf("plain verifier: got %v, want ErrUnsupportedCriticalHeader", err)
	}

	// Package-level Decrypt also rejects unknown critical headers now.
	if _, err := parse().Decrypt(rsaTestKey); !errors.Is(err, ErrUnsupportedCriticalHeader) {
		t.Fatalf("package Decrypt: got %v, want ErrUnsupportedCriticalHeader", err)
	}

	good := mustNewVerifier(t, roleExtension(CriticalHeaderStageAfterVerification, "server", nil))
	if pt, err := good.Decrypt(parse(), rsaTestKey); err != nil || string(pt) != "critical plaintext" {
		t.Fatalf("good verifier: pt=%q err=%v", pt, err)
	}

	before := mustNewVerifier(t, roleExtension(CriticalHeaderStageBeforeVerification, "client", nil))
	if _, err := before.Decrypt(parse(), rsaTestKey); !errors.Is(err, errRoleMismatch) {
		t.Fatalf("before verifier: got %v, want errRoleMismatch", err)
	}
}

func TestTwoVerifiersProcessSameInputInParallel(t *testing.T) {
	obj := signCriticalJWS(t, "server", 1)

	// Two verifiers with different configs must make independent decisions on
	// the same input, repeatedly and concurrently, without leaking extensions
	// through any global registry.
	accept := mustNewVerifier(t, roleExtension(CriticalHeaderStageAfterVerification, "server", nil))
	reject := mustNewVerifier(t, roleExtension(CriticalHeaderStageAfterVerification, "client", nil))

	rcpt := Recipient{Algorithm: RSA_OAEP_256, Key: &rsaTestKey.PublicKey}
	encOpts := new(EncrypterOptions).
		WithHeader(HeaderKey(roleHeader), "server").
		WithHeader(HeaderKey(versionHeader), 1).
		WithHeader(headerCritical, []string{roleHeader, versionHeader})
	encrypter, err := NewEncrypter(A256GCM, rcpt, encOpts)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := encrypter.Encrypt([]byte("parallel plaintext"))
	if err != nil {
		t.Fatal(err)
	}
	jweSerialized, err := encrypted.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	parseJWE := func() *JSONWebEncryption {
		jwe, err := ParseEncrypted(jweSerialized, []KeyAlgorithm{RSA_OAEP_256}, []ContentEncryption{A256GCM})
		if err != nil {
			t.Fatal(err)
		}
		return jwe
	}

	const iterations = 200
	var wg sync.WaitGroup
	wg.Add(4)

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if _, err := accept.Verify(obj, &rsaTestKey.PublicKey); err != nil {
				t.Errorf("accept iter %d: %v", i, err)
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if _, err := reject.Verify(obj, &rsaTestKey.PublicKey); !errors.Is(err, errRoleMismatch) {
				t.Errorf("reject iter %d: got %v", i, err)
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if pt, err := accept.Decrypt(parseJWE(), rsaTestKey); err != nil || string(pt) != "parallel plaintext" {
				t.Errorf("JWE accept iter %d: pt=%q err=%v", i, pt, err)
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if _, err := reject.Decrypt(parseJWE(), rsaTestKey); !errors.Is(err, errRoleMismatch) {
				t.Errorf("JWE reject iter %d: got %v", i, err)
				return
			}
		}
	}()

	wg.Wait()

	// A zero-value Verifier must still apply only built-in extensions.
	var zero Verifier
	if _, err := zero.Verify(obj, &rsaTestKey.PublicKey); !errors.Is(err, ErrUnsupportedCriticalHeader) {
		t.Fatalf("zero verifier: got %v, want ErrUnsupportedCriticalHeader", err)
	}
}
