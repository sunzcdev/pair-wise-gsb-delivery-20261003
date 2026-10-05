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
	"fmt"
)

// CriticalHeaderStage identifies when a critical header extension is invoked.
type CriticalHeaderStage int

const (
	// CriticalHeaderBeforeVerification invokes an extension before the JWS
	// signature or JWE content is verified. Extensions that affect verification
	// input, such as JWS "b64", must use this stage.
	CriticalHeaderBeforeVerification CriticalHeaderStage = iota + 1

	// CriticalHeaderAfterVerification invokes an extension only after the JWS
	// signature or JWE content has been verified.
	CriticalHeaderAfterVerification
)

// CriticalHeaderParameters contains the raw protected header parameters
// requested by a critical header extension.
type CriticalHeaderParameters map[HeaderKey]stdjson.RawMessage

// CriticalHeaderExtension registers application-specific validation for a
// critical ("crit") header parameter.
type CriticalHeaderExtension struct {
	// Name is the header parameter listed in "crit".
	Name HeaderKey

	// Parameters lists raw protected header parameters made available to
	// Verify. Name is included automatically.
	Parameters []HeaderKey

	// Stage determines whether Verify runs before or after cryptographic
	// verification.
	Stage CriticalHeaderStage

	// Verify validates the requested raw protected header parameters.
	Verify func(CriticalHeaderParameters) error
}

// Validator verifies JWS and JWE objects using instance-specific critical
// header extensions. A zero Validator must not be used; create one with
// NewValidator.
type Validator struct {
	jwsCriticalHeaders *criticalHeaderRegistry
	jweCriticalHeaders *criticalHeaderRegistry
}

type criticalHeaderRegistry struct {
	extensions map[HeaderKey]CriticalHeaderExtension
	jws        bool
}

// NewValidator creates a validator with its own set of critical header
// extensions. The built-in JWS "b64" extension remains enabled and cannot be
// overridden.
func NewValidator(extensions ...CriticalHeaderExtension) (*Validator, error) {
	jwsExtensions, err := newCriticalHeaderRegistry(extensions, true)
	if err != nil {
		return nil, err
	}

	jweExtensions, err := newCriticalHeaderRegistry(extensions, false)
	if err != nil {
		return nil, err
	}

	return &Validator{
		jwsCriticalHeaders: jwsExtensions,
		jweCriticalHeaders: jweExtensions,
	}, nil
}

func newCriticalHeaderRegistry(extensions []CriticalHeaderExtension, jws bool) (*criticalHeaderRegistry, error) {
	registry := &criticalHeaderRegistry{
		extensions: make(map[HeaderKey]CriticalHeaderExtension, len(extensions)),
		jws:        jws,
	}

	for _, extension := range extensions {
		if extension.Name == "" {
			return nil, errors.New("go-jose/go-jose: critical header extension name is empty")
		}
		if extension.Name == headerCritical || extension.Name == headerB64 {
			return nil, fmt.Errorf("go-jose/go-jose: critical header extension %q is reserved", extension.Name)
		}
		if extension.Stage != CriticalHeaderBeforeVerification && extension.Stage != CriticalHeaderAfterVerification {
			return nil, fmt.Errorf("go-jose/go-jose: critical header extension %q has an invalid stage", extension.Name)
		}
		if extension.Verify == nil {
			return nil, fmt.Errorf("go-jose/go-jose: critical header extension %q has no verifier", extension.Name)
		}
		if _, ok := registry.extensions[extension.Name]; ok {
			return nil, fmt.Errorf("go-jose/go-jose: duplicate critical header extension %q", extension.Name)
		}

		seen := map[HeaderKey]struct{}{extension.Name: {}}
		parameters := []HeaderKey{extension.Name}
		for _, name := range extension.Parameters {
			if name == "" {
				return nil, fmt.Errorf("go-jose/go-jose: critical header extension %q requests an empty parameter", extension.Name)
			}
			if _, ok := seen[name]; !ok {
				seen[name] = struct{}{}
				parameters = append(parameters, name)
			}
		}
		extension.Parameters = parameters
		registry.extensions[extension.Name] = extension
	}

	return registry, nil
}

func defaultCriticalHeaderRegistry(forJWS bool) *criticalHeaderRegistry {
	return &criticalHeaderRegistry{extensions: map[HeaderKey]CriticalHeaderExtension{}, jws: forJWS}
}

func hasCriticalHeader(headers *rawHeader) bool {
	if headers == nil {
		return false
	}

	_, ok := (*headers)[headerCritical]
	return ok
}

func checkCriticalHeaderList(protected *rawHeader) error {
	if protected == nil {
		return nil
	}

	_, err := getCriticalHeaderList(*protected)
	return err
}

func (registry *criticalHeaderRegistry) runCriticalHeaderExtensions(
	protected *rawHeader,
	stage CriticalHeaderStage,
) error {
	if protected == nil {
		return nil
	}

	critical, err := getCriticalHeaderList(*protected)
	if err != nil {
		return err
	}

	for _, name := range critical {
		if name == headerB64 {
			if registry.jws {
				continue
			}
			return ErrUnsupportedCriticalHeader
		}

		extension, ok := registry.extensions[name]
		if !ok {
			return ErrUnsupportedCriticalHeader
		}
		if extension.Stage != stage {
			continue
		}

		parameters := make(CriticalHeaderParameters, len(extension.Parameters))
		for _, parameter := range extension.Parameters {
			value, ok := (*protected)[parameter]
			if !ok || value == nil {
				return ErrUnsupportedCriticalHeader
			}
			raw := make(stdjson.RawMessage, len(*value))
			copy(raw, *value)
			parameters[parameter] = raw
		}

		if err := extension.Verify(parameters); err != nil {
			return err
		}
	}

	return nil
}

func getCriticalHeaderList(protected rawHeader) ([]HeaderKey, error) {
	raw, ok := protected[headerCritical]
	if !ok || raw == nil {
		return nil, nil
	}

	var names []string
	if err := stdjson.Unmarshal(*raw, &names); err != nil || names == nil {
		return nil, ErrUnsupportedCriticalHeader
	}
	if len(names) == 0 {
		return nil, ErrUnsupportedCriticalHeader
	}

	critical := make([]HeaderKey, 0, len(names))
	seen := make(map[HeaderKey]struct{}, len(names))
	for _, name := range names {
		header := HeaderKey(name)
		if header == "" || header == headerCritical {
			return nil, ErrUnsupportedCriticalHeader
		}
		if _, ok := seen[header]; ok {
			return nil, ErrUnsupportedCriticalHeader
		}
		seen[header] = struct{}{}

		value, ok := protected[header]
		if !ok || value == nil {
			return nil, ErrUnsupportedCriticalHeader
		}

		critical = append(critical, header)
	}

	return critical, nil
}

// Verify verifies a single-signature JWS object and returns its payload.
func (validator *Validator) Verify(jws JSONWebSignature, verificationKey interface{}) ([]byte, error) {
	if err := validator.DetachedVerify(jws, jws.payload, verificationKey); err != nil {
		return nil, err
	}
	return jws.payload, nil
}

// DetachedVerify verifies a detached single-signature JWS object.
func (validator *Validator) DetachedVerify(jws JSONWebSignature, payload []byte, verificationKey interface{}) error {
	return jws.detachedVerify(payload, verificationKey, validator.jwsCriticalHeaders)
}

// VerifyMulti verifies one signature on a JWS object.
func (validator *Validator) VerifyMulti(jws JSONWebSignature, verificationKey interface{}) (int, Signature, []byte, error) {
	index, signature, err := jws.DetachedVerifyMulti(jws.payload, verificationKey)
	if err != nil {
		return -1, Signature{}, nil, err
	}
	return index, signature, jws.payload, nil
}

// DetachedVerifyMulti verifies one signature on a detached JWS object.
func (validator *Validator) DetachedVerifyMulti(jws JSONWebSignature, payload []byte, verificationKey interface{}) (int, Signature, error) {
	return jws.detachedVerifyMulti(payload, verificationKey, validator.jwsCriticalHeaders)
}

// Decrypt decrypts a single-recipient JWE object.
func (validator *Validator) Decrypt(jwe JSONWebEncryption, decryptionKey interface{}) ([]byte, error) {
	return jwe.decrypt(decryptionKey, validator.jweCriticalHeaders)
}

// DecryptMulti decrypts a multi-recipient JWE object.
func (validator *Validator) DecryptMulti(jwe JSONWebEncryption, decryptionKey interface{}) (int, Header, []byte, error) {
	return jwe.decryptMulti(decryptionKey, validator.jweCriticalHeaders)
}
