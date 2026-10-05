/*-
 * Copyright 2014 Square Inc.
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
	stdjson "encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestJWSValidatorCriticalHeaderIsolation(t *testing.T) {
	key := make([]byte, 32)
	signer, err := NewSigner(SigningKey{Algorithm: HS256, Key: key}, (&SignerOptions{}).
		WithHeader(HeaderKey("custom"), "yes").
		WithHeader(HeaderKey("other"), "present").
		WithCritical("custom"))
	if err != nil {
		t.Fatal(err)
	}

	obj, err := signer.Sign([]byte("payload"))
	if err != nil {
		t.Fatal(err)
	}

	accepting, err := NewValidator(CriticalHeaderExtension{
		Name:       HeaderKey("custom"),
		Parameters: []HeaderKey{HeaderKey("other")},
		Stage:      CriticalHeaderAfterVerification,
		Verify: func(parameters CriticalHeaderParameters) error {
			var custom, other string
			if err := stdjson.Unmarshal(parameters[HeaderKey("custom")], &custom); err != nil {
				return err
			}
			if err := stdjson.Unmarshal(parameters[HeaderKey("other")], &other); err != nil {
				return err
			}
			if custom != "yes" || other != "present" {
				return errors.New("unexpected critical header parameters")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	rejecting, err := NewValidator(CriticalHeaderExtension{
		Name:  HeaderKey("custom"),
		Stage: CriticalHeaderBeforeVerification,
		Verify: func(CriticalHeaderParameters) error {
			return errors.New("rejected")
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	const iterations = 100
	var wait sync.WaitGroup
	var accepted, rejected int64
	wait.Add(iterations * 2)

	for range iterations {
		go func() {
			defer wait.Done()
			if payload, err := accepting.Verify(*obj, key); err == nil && string(payload) == "payload" {
				atomic.AddInt64(&accepted, 1)
			}
		}()

		go func() {
			defer wait.Done()
			if _, err := rejecting.Verify(*obj, key); err != nil {
				atomic.AddInt64(&rejected, 1)
			}
		}()
	}

	wait.Wait()

	if accepted != iterations || rejected != iterations {
		t.Fatalf("accepted = %d, rejected = %d, want %d for both", accepted, rejected, iterations)
	}

	if _, err := mustNewValidator(t).Verify(*obj, key); !errors.Is(err, ErrUnsupportedCriticalHeader) {
		t.Fatalf("validator without extension error = %v, want ErrUnsupportedCriticalHeader", err)
	}
	if _, err := obj.Verify(key); !errors.Is(err, ErrUnsupportedCriticalHeader) {
		t.Fatalf("default verify error = %v, want ErrUnsupportedCriticalHeader", err)
	}
}

func TestJWSValidatorUsesProtectedCriticalParameters(t *testing.T) {
	key := make([]byte, 32)
	signer, err := NewSigner(SigningKey{Algorithm: HS256, Key: key}, (&SignerOptions{}).
		WithHeader(HeaderKey("custom"), "protected").
		WithCritical("custom"))
	if err != nil {
		t.Fatal(err)
	}

	obj, err := signer.Sign([]byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	obj.Signatures[0].header = &rawHeader{}
	if err := obj.Signatures[0].header.set(HeaderKey("custom"), "unprotected"); err != nil {
		t.Fatal(err)
	}

	seen := make(chan string, 1)
	validator, err := NewValidator(CriticalHeaderExtension{
		Name:  HeaderKey("custom"),
		Stage: CriticalHeaderAfterVerification,
		Verify: func(parameters CriticalHeaderParameters) error {
			var custom string
			if err := stdjson.Unmarshal(parameters[HeaderKey("custom")], &custom); err != nil {
				return err
			}
			seen <- custom
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := validator.Verify(*obj, key); err != nil {
		t.Fatal(err)
	}

	if got := <-seen; got != "protected" {
		t.Fatalf("critical extension saw %q, want protected value", got)
	}
}

func TestJWSValidatorB64RemainsSupported(t *testing.T) {
	key := make([]byte, 32)
	signer, err := NewSigner(SigningKey{Algorithm: HS256, Key: key}, new(SignerOptions).WithBase64(false))
	if err != nil {
		t.Fatal(err)
	}

	obj, err := signer.Sign([]byte("$.02"))
	if err != nil {
		t.Fatal(err)
	}

	validator := mustNewValidator(t)

	if payload, err := validator.Verify(*obj, key); err != nil || string(payload) != "$.02" {
		t.Fatalf("validator b64 verify got payload %q, error %v", payload, err)
	}
}

func TestJWEValidatorCriticalHeaderIsolation(t *testing.T) {
	key := make([]byte, 16)
	encrypter, err := NewEncrypter(A128GCM, Recipient{Algorithm: DIRECT, Key: key}, (&EncrypterOptions{}).
		WithHeader(HeaderKey("custom"), "yes").
		WithHeader(HeaderKey("other"), "present").
		WithHeader(HeaderKey("crit"), []string{"custom"}))
	if err != nil {
		t.Fatal(err)
	}

	obj, err := encrypter.Encrypt([]byte("payload"))
	if err != nil {
		t.Fatal(err)
	}

	accepting, err := NewValidator(CriticalHeaderExtension{
		Name:       HeaderKey("custom"),
		Parameters: []HeaderKey{HeaderKey("other")},
		Stage:      CriticalHeaderAfterVerification,
		Verify: func(parameters CriticalHeaderParameters) error {
			var custom, other string
			if err := stdjson.Unmarshal(parameters[HeaderKey("custom")], &custom); err != nil {
				return err
			}
			if err := stdjson.Unmarshal(parameters[HeaderKey("other")], &other); err != nil {
				return err
			}
			if custom != "yes" || other != "present" {
				return errors.New("unexpected critical header parameters")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	rejecting, err := NewValidator(CriticalHeaderExtension{
		Name:  HeaderKey("custom"),
		Stage: CriticalHeaderBeforeVerification,
		Verify: func(CriticalHeaderParameters) error {
			return errors.New("rejected")
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	const iterations = 100
	var wait sync.WaitGroup
	var accepted, rejected int64
	wait.Add(iterations * 2)

	for range iterations {
		go func() {
			defer wait.Done()
			if plaintext, err := accepting.Decrypt(*obj, key); err == nil && string(plaintext) == "payload" {
				atomic.AddInt64(&accepted, 1)
			}
		}()

		go func() {
			defer wait.Done()
			if _, err := rejecting.Decrypt(*obj, key); err != nil {
				atomic.AddInt64(&rejected, 1)
			}
		}()
	}

	wait.Wait()

	if accepted != iterations || rejected != iterations {
		t.Fatalf("accepted = %d, rejected = %d, want %d for both", accepted, rejected, iterations)
	}

	if _, err := mustNewValidator(t).Decrypt(*obj, key); !errors.Is(err, ErrUnsupportedCriticalHeader) {
		t.Fatalf("validator without extension error = %v, want ErrUnsupportedCriticalHeader", err)
	}
}

func TestJWEValidatorUsesProtectedCriticalParameters(t *testing.T) {
	key := make([]byte, 16)
	encrypter, err := NewEncrypter(A128GCM, Recipient{Algorithm: DIRECT, Key: key}, (&EncrypterOptions{}).
		WithHeader(HeaderKey("custom"), "protected").
		WithHeader(HeaderKey("crit"), []string{"custom"}))
	if err != nil {
		t.Fatal(err)
	}

	obj, err := encrypter.Encrypt([]byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	obj.unprotected = &rawHeader{}
	if err := obj.unprotected.set(HeaderKey("custom"), "unprotected"); err != nil {
		t.Fatal(err)
	}

	seen := make(chan string, 1)
	validator, err := NewValidator(CriticalHeaderExtension{
		Name:  HeaderKey("custom"),
		Stage: CriticalHeaderAfterVerification,
		Verify: func(parameters CriticalHeaderParameters) error {
			var custom string
			if err := stdjson.Unmarshal(parameters[HeaderKey("custom")], &custom); err != nil {
				return err
			}
			seen <- custom
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := validator.Decrypt(*obj, key); err != nil {
		t.Fatal(err)
	}

	if got := <-seen; got != "protected" {
		t.Fatalf("critical extension saw %q, want protected value", got)
	}
}

func TestCriticalHeaderStagesAndRawParameterCopies(t *testing.T) {
	key := make([]byte, 32)
	opts := (&SignerOptions{}).
		WithHeader(HeaderKey("before"), "before-value").
		WithHeader(HeaderKey("after"), "after-value").
		WithCritical("before", "after")
	signer, err := NewSigner(SigningKey{Algorithm: HS256, Key: key}, opts)
	if err != nil {
		t.Fatal(err)
	}

	obj, err := signer.Sign([]byte("payload"))
	if err != nil {
		t.Fatal(err)
	}

	var stages []string
	validator, err := NewValidator(
		CriticalHeaderExtension{
			Name:  HeaderKey("before"),
			Stage: CriticalHeaderBeforeVerification,
			Verify: func(parameters CriticalHeaderParameters) error {
				stages = append(stages, "before")
				raw := parameters[HeaderKey("before")]
				for i := range raw {
					raw[i] = 'X'
				}
				return nil
			},
		},
		CriticalHeaderExtension{
			Name:  HeaderKey("after"),
			Stage: CriticalHeaderAfterVerification,
			Verify: func(parameters CriticalHeaderParameters) error {
				stages = append(stages, "after")
				var before string
				if err := stdjson.Unmarshal(parameters[HeaderKey("before")], &before); err != nil {
					return err
				}
				if before != "before-value" {
					return errors.New("raw parameters were shared with another extension")
				}
				return nil
			},
			Parameters: []HeaderKey{HeaderKey("before")},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := validator.Verify(*obj, key); err != nil {
		t.Fatal(err)
	}
	if len(stages) != 2 || stages[0] != "before" || stages[1] != "after" {
		t.Fatalf("stages = %v, want [before after]", stages)
	}

	var protectedValue string
	if err := stdjson.Unmarshal(*(*obj.Signatures[0].protected)[HeaderKey("before")], &protectedValue); err != nil {
		t.Fatal(err)
	}
	if protectedValue != "before-value" {
		t.Fatalf("protected header changed to %q after callback mutation", protectedValue)
	}

	if _, err := validator.Verify(*obj, key); err != nil {
		t.Fatalf("callback mutation changed later verification: %v", err)
	}
}

func TestJWEValidatorRejectsUnprotectedCriticalHeader(t *testing.T) {
	key := make([]byte, 16)
	encrypter, err := NewEncrypter(A128GCM, Recipient{Algorithm: DIRECT, Key: key}, (&EncrypterOptions{}).
		WithHeader(HeaderKey("custom"), "protected").
		WithCritical("custom"))
	if err != nil {
		t.Fatal(err)
	}

	obj, err := encrypter.Encrypt([]byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	obj.unprotected = &rawHeader{}
	if err := obj.unprotected.set(headerCritical, []string{"custom"}); err != nil {
		t.Fatal(err)
	}

	validator := mustNewValidator(t, CriticalHeaderExtension{
		Name:  HeaderKey("custom"),
		Stage: CriticalHeaderBeforeVerification,
		Verify: func(CriticalHeaderParameters) error {
			t.Error("extension must not be invoked for an unprotected crit header")
			return nil
		},
	})

	if _, err := validator.Decrypt(*obj, key); !errors.Is(err, ErrUnsupportedCriticalHeader) {
		t.Fatalf("unprotected JWE crit error = %v, want ErrUnsupportedCriticalHeader", err)
	}
}

func TestCriticalHeaderRejectsInvalidLocationsAndLists(t *testing.T) {
	key := make([]byte, 32)
	signer, err := NewSigner(SigningKey{Algorithm: HS256, Key: key}, (&SignerOptions{}).
		WithHeader(HeaderKey("b64"), false).
		WithCritical(headerB64))
	if err != nil {
		t.Fatal(err)
	}

	obj, err := signer.Sign([]byte("$.02"))
	if err != nil {
		t.Fatal(err)
	}

	protected := *obj.Signatures[0].protected
	obj.Signatures[0].protected = &protected
	obj.Signatures[0].header = &rawHeader{}
	if err := obj.Signatures[0].header.set(headerCritical, []string{headerB64}); err != nil {
		t.Fatal(err)
	}
	if _, err := mustNewValidator(t).Verify(*obj, key); !errors.Is(err, ErrUnsupportedCriticalHeader) {
		t.Fatalf("unprotected crit error = %v, want ErrUnsupportedCriticalHeader", err)
	}

	for _, crit := range []interface{}{
		[]string{},
		[]string{"custom"},
		[]string{"custom", "custom"},
		[]string{""},
		[]string{headerCritical},
		"custom",
		nil,
	} {
		obj.Signatures[0].header = &rawHeader{}
		obj.Signatures[0].protected = &rawHeader{}
		if err := obj.Signatures[0].protected.set(headerAlgorithm, HS256); err != nil {
			t.Fatal(err)
		}
		if err := obj.Signatures[0].protected.set(headerCritical, crit); err != nil {
			t.Fatal(err)
		}
		if err := obj.Signatures[0].protected.set(headerB64, false); err != nil {
			t.Fatal(err)
		}
		if _, err := mustNewValidator(t).Verify(*obj, key); !errors.Is(err, ErrUnsupportedCriticalHeader) {
			t.Fatalf("crit %#v error = %v, want ErrUnsupportedCriticalHeader", crit, err)
		}
	}
}

func mustNewValidator(t *testing.T, extensions ...CriticalHeaderExtension) *Validator {
	t.Helper()
	validator, err := NewValidator(extensions...)
	if err != nil {
		t.Fatal(err)
	}
	return validator
}
