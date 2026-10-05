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
	"encoding/json"
	"errors"
)

// CriticalHeaderStage declares at which point of the verification or
// decryption process a [CriticalHeaderExtension] must be invoked.
type CriticalHeaderStage int

const (
	// CriticalHeaderStageBeforeVerification invokes the extension before the
	// cryptographic operation (signature verification or decryption) is
	// performed. Use this stage when the extension needs to influence how the
	// operation is carried out, or when rejecting an object early is desirable.
	CriticalHeaderStageBeforeVerification CriticalHeaderStage = iota + 1

	// CriticalHeaderStageAfterVerification invokes the extension only after the
	// cryptographic operation (signature verification or decryption) has
	// succeeded. This guarantees that the raw header parameters handed to the
	// callback have been authenticated.
	CriticalHeaderStageAfterVerification
)

// CriticalHeaderParams carries the raw, protected header parameters that a
// [CriticalHeaderExtension] declared an interest in via its Parameters field.
//
// Values are read exclusively from the integrity-protected header. Parameters
// sharing the same name in an unprotected or per-recipient header are never
// exposed here, so the values in this type can be trusted once the extension
// stage is [CriticalHeaderStageAfterVerification].
type CriticalHeaderParams struct {
	values map[HeaderKey]json.RawMessage
}

// Raw returns the raw JSON value of the requested protected header parameter.
// It returns ok == false if the parameter is not present.
func (p CriticalHeaderParams) Raw(name string) (json.RawMessage, bool) {
	v, ok := p.values[HeaderKey(name)]
	return v, ok
}

// Unmarshal decodes the raw JSON value of the requested protected header
// parameter into v. It returns ok == false if the parameter is not present.
func (p CriticalHeaderParams) Unmarshal(name string, v interface{}) (bool, error) {
	raw, ok := p.values[HeaderKey(name)]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(raw, v)
}

// CriticalHeaderExtension registers instance-level validation logic for a
// header parameter listed in the "crit" header of a JWS or JWE object.
//
// Unlike a global registry, extensions are supplied to a [Verifier] and apply
// only to operations performed through that verifier.
type CriticalHeaderExtension struct {
	// Name is the critical header parameter handled by this extension. It must
	// match an entry in the "crit" header for the extension to be invoked.
	Name string

	// Parameters lists the additional raw header parameters, besides Name,
	// that the Validate callback needs to inspect. Every declared parameter
	// must be present in the integrity-protected header; if any is missing,
	// the object is rejected without calling Validate.
	Parameters []string

	// Stage declares when Validate is called, see [CriticalHeaderStage].
	Stage CriticalHeaderStage

	// Validate inspects the raw protected header parameters. It is called
	// with a [CriticalHeaderParams] value containing Name and every entry of
	// Parameters. Returning a non-nil error rejects the object.
	Validate func(params CriticalHeaderParams) error
}

// Verifier verifies JWS signatures and decrypts JWE objects using an
// instance-specific set of [CriticalHeaderExtension] values.
//
// A zero-value Verifier behaves like the package-level Verify/Decrypt methods
// and understands only the built-in extensions (currently "b64"). Extensions
// registered on one Verifier never affect any other Verifier; there is no
// global mutable registry.
type Verifier struct {
	crit *criticalExtensions
}

// NewVerifier returns a Verifier configured with the given critical header
// extensions. The built-in "b64" extension (RFC 7797) is always registered and
// keeps working under its original rules.
//
// It returns an error if an extension is malformed (for example, if it has an
// empty name, an unknown stage, a nil Validate callback, or duplicates another
// extension or a built-in one).
func NewVerifier(extensions ...CriticalHeaderExtension) (*Verifier, error) {
	crit, err := newCriticalExtensions(extensions)
	if err != nil {
		return nil, err
	}
	return &Verifier{crit: crit}, nil
}

// criticalExtensions is the immutable per-Verifier registry. The only built-in
// extension is "b64" (RFC 7797), which needs no callback: its presence and
// meaning are enforced while the signing input is constructed.
type criticalExtensions struct {
	extensions map[string]CriticalHeaderExtension
	// supported maps every understood header parameter name (both extension
	// names and their declared Parameters) to the extension that owns it.
	supported map[string]CriticalHeaderExtension
}

func newCriticalExtensions(exts []CriticalHeaderExtension) (*criticalExtensions, error) {
	c := &criticalExtensions{extensions: map[string]CriticalHeaderExtension{}}
	// The built-in "b64" extension is understood and processed by go-jose
	// itself, so it needs no Validate callback.
	c.extensions[headerB64] = CriticalHeaderExtension{
		Name:  headerB64,
		Stage: CriticalHeaderStageAfterVerification,
	}

	for _, ext := range exts {
		if ext.Name == "" || ext.Name == headerCritical {
			return nil, errors.New("go-jose/go-jose: invalid critical header extension name")
		}
		if ext.Stage != CriticalHeaderStageBeforeVerification &&
			ext.Stage != CriticalHeaderStageAfterVerification {
			return nil, errors.New("go-jose/go-jose: invalid critical header extension stage")
		}
		if ext.Validate == nil {
			return nil, errors.New("go-jose/go-jose: critical header extension is missing a Validate callback")
		}
		if _, exists := c.extensions[ext.Name]; exists {
			return nil, errors.New("go-jose/go-jose: duplicate critical header extension: " + ext.Name)
		}
		c.extensions[ext.Name] = ext
	}

	c.supported = make(map[string]CriticalHeaderExtension, len(c.extensions))
	for _, ext := range c.extensions {
		for _, name := range ext.requiredParameters() {
			if owner, exists := c.supported[name]; exists && owner.Name != ext.Name {
				return nil, errors.New("go-jose/go-jose: critical header parameter claimed by multiple extensions: " + name)
			}
			c.supported[name] = ext
		}
	}

	return c, nil
}

// defaultCriticalExtensions is used by the package-level Verify/Decrypt
// methods. It is immutable and contains only the built-in extensions.
var defaultCriticalExtensions = &criticalExtensions{
	extensions: map[string]CriticalHeaderExtension{
		headerB64: {
			Name:  headerB64,
			Stage: CriticalHeaderStageAfterVerification,
		},
	},
	supported: map[string]CriticalHeaderExtension{
		headerB64: {Name: headerB64},
	},
}

func (v *Verifier) criticals() *criticalExtensions {
	if v == nil || v.crit == nil {
		return defaultCriticalExtensions
	}
	return v.crit
}

// Decrypt decrypts and validates a JWE object using the critical header
// extensions registered on the Verifier. It behaves like
// [JSONWebEncryption.Decrypt] apart from the extensions in effect.
func (v *Verifier) Decrypt(obj *JSONWebEncryption, decryptionKey interface{}) ([]byte, error) {
	return obj.decrypt(decryptionKey, v.criticals())
}

// DecryptMulti decrypts and validates a JWE object with support for multiple
// recipients using the critical header extensions registered on the Verifier.
// It behaves like [JSONWebEncryption.DecryptMulti] apart from the extensions
// in effect.
func (v *Verifier) DecryptMulti(obj *JSONWebEncryption, decryptionKey interface{}) (int, Header, []byte, error) {
	return obj.decryptMulti(decryptionKey, v.criticals())
}

// resolve validates the "crit" header of a single integrity-protected header
// and returns the callbacks grouped by their stage.
//
// It enforces RFC 7515 section 4.1.11 / RFC 7516 section 4.1.13:
//   - "crit" must be a non-empty JSON array of non-empty, unique strings;
//   - every name in "crit" must be present in the same protected header;
//   - every name must be understood, either by go-jose or by a registered
//     extension; unknown names are never skipped.
func (c *criticalExtensions) resolve(protected *rawHeader) (before, after []CriticalHeaderExtension, err error) {
	if protected == nil {
		return nil, nil, nil
	}

	crit, present := (*protected)[headerCritical]
	if !present {
		return nil, nil, nil
	}

	var names []string
	if err := json.Unmarshal(*crit, &names); err != nil {
		return nil, nil, ErrUnsupportedCriticalHeader
	}

	if len(names) == 0 {
		return nil, nil, ErrUnsupportedCriticalHeader
	}

	seen := map[string]struct{}{}
	for _, name := range names {
		if name == "" {
			return nil, nil, ErrUnsupportedCriticalHeader
		}
		if _, dup := seen[name]; dup {
			return nil, nil, ErrUnsupportedCriticalHeader
		}
		seen[name] = struct{}{}

		ext, ok := c.supported[name]
		if !ok {
			return nil, nil, ErrUnsupportedCriticalHeader
		}

		raw, present := (*protected)[HeaderKey(name)]
		if !present || raw == nil {
			return nil, nil, ErrUnsupportedCriticalHeader
		}

		// The extension's Validate callback is triggered by its own Name, not
		// by auxiliary parameters it merely consumes.
		if ext.Name != name || ext.Validate == nil {
			continue
		}

		for _, p := range ext.requiredParameters() {
			raw, present := (*protected)[HeaderKey(p)]
			if !present || raw == nil {
				return nil, nil, ErrUnsupportedCriticalHeader
			}
		}

		if ext.Stage == CriticalHeaderStageBeforeVerification {
			before = append(before, ext)
		} else {
			after = append(after, ext)
		}
	}

	return before, after, nil
}

func (ext CriticalHeaderExtension) requiredParameters() []string {
	params := make([]string, 0, len(ext.Parameters)+1)
	params = append(params, ext.Name)
	for _, p := range ext.Parameters {
		if p != "" && p != ext.Name {
			params = append(params, p)
		}
	}
	return params
}

func runCriticalExtensions(exts []CriticalHeaderExtension, protected *rawHeader) error {
	if len(exts) == 0 {
		return nil
	}

	values := make(map[HeaderKey]json.RawMessage, len(exts))
	for _, ext := range exts {
		for _, name := range ext.requiredParameters() {
			if _, ok := values[HeaderKey(name)]; ok {
				continue
			}
			values[HeaderKey(name)] = json.RawMessage(*(*protected)[HeaderKey(name)])
		}
	}

	params := CriticalHeaderParams{values: values}
	for _, ext := range exts {
		if ext.Validate == nil {
			continue
		}
		if err := ext.Validate(params); err != nil {
			return err
		}
	}
	return nil
}
