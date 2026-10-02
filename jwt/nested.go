package jwt

import (
	"encoding/base64"
	"fmt"
	"strings"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/json"
)

// LayerKind identifies the JOSE serialization expected at one nesting layer.
type LayerKind int

const (
	// LayerJWS declares a signed (JWS compact serialization) layer.
	LayerJWS LayerKind = iota
	// LayerJWE declares an encrypted (JWE compact serialization) layer.
	LayerJWE
)

// LayerPolicy declares the validation requirements for a single nesting
// layer of a token. Every layer must explicitly state its kind, the
// algorithms that are acceptable, and the key used to verify or decrypt it.
// Type and ContentType optionally pin the "typ" and "cty" header parameters
// (compared case-insensitively); leave them empty to not require them.
type LayerPolicy struct {
	// Kind declares whether this layer must be a JWS or a JWE.
	Kind LayerKind

	// SignatureAlgorithms lists the acceptable "alg" values for a JWS
	// layer. Required for LayerJWS, ignored otherwise.
	SignatureAlgorithms []jose.SignatureAlgorithm

	// KeyAlgorithms lists the acceptable "alg" values for a JWE layer.
	// Required for LayerJWE, ignored otherwise.
	KeyAlgorithms []jose.KeyAlgorithm

	// ContentEncryption lists the acceptable "enc" values for a JWE
	// layer. Required for LayerJWE, ignored otherwise.
	ContentEncryption []jose.ContentEncryption

	// Key is the key used to verify (JWS) or decrypt (JWE) this layer.
	// Its type depends on the algorithm, exactly as for
	// JSONWebToken.Claims.
	Key interface{}

	// Type, if non-empty, is the required value of the "typ" header.
	Type string

	// ContentType, if non-empty, is the required value of the "cty"
	// header.
	ContentType string
}

// NestedPolicy declares the exact sequence of layers a nested token must
// have, from the outermost layer to the innermost one. The token is only
// accepted if it matches the declared sequence exactly: layers are never
// reordered, skipped, or probed for alternative interpretations, and a
// token that is nested deeper than declared is rejected.
type NestedPolicy struct {
	// Layers lists the expected layers, outermost first. For the common
	// "signed, then encrypted" JWT this is a JWE layer followed by a JWS
	// layer.
	Layers []LayerPolicy

	// MaxDepth is the maximum number of nesting layers accepted. If
	// zero, it defaults to the number of declared layers, so the token
	// must match the declared chain exactly.
	MaxDepth int
}

// LayerError describes a validation failure at a specific nesting layer.
// Layer numbers start at 0 for the outermost layer. The wrapped error never
// contains plaintext recovered from the token.
type LayerError struct {
	Layer int
	Err   error
}

func (e *LayerError) Error() string {
	return fmt.Sprintf("go-jose/go-jose/jwt: layer %d: %s", e.Layer, e.Err)
}

func (e *LayerError) Unwrap() error {
	return e.Err
}

// NestedToken is a nested JWT whose complete declared layer chain has been
// verified and decrypted. Claims are only available after every layer
// passed validation.
type NestedToken struct {
	// Headers contains the JOSE header of each layer, outermost first.
	Headers []jose.Header

	payload []byte
}

// Claims deserializes the claims of the innermost layer into dest. Because
// a NestedToken is only produced by ParseNested after the whole chain
// validated, these claims are authenticated by every declared layer.
func (t *NestedToken) Claims(dest ...interface{}) error {
	for _, d := range dest {
		if err := json.Unmarshal(t.payload, d); err != nil {
			return err
		}
	}
	return nil
}

// ParseNested parses and validates a nested JWT according to policy.
//
// Each layer is processed strictly in the declared order: the compact
// serialization is parsed with the layer's declared kind and algorithms,
// authenticated with the layer's key, and checked against the layer's
// typ/cty requirements before the next (inner) layer is examined. A
// successful outer decryption is never treated as proof that inner claims
// are trustworthy: the returned token is only produced once the innermost
// layer has been authenticated as well.
//
// On failure a *LayerError is returned identifying the layer (0 =
// outermost) at which validation stopped. Error messages never include
// decrypted plaintext.
func ParseNested(s string, policy NestedPolicy) (*NestedToken, error) {
	if len(policy.Layers) == 0 {
		return nil, fmt.Errorf("%w: at least one layer must be declared", ErrInvalidNestedPolicy)
	}
	maxDepth := policy.MaxDepth
	if maxDepth <= 0 {
		maxDepth = len(policy.Layers)
	}
	if len(policy.Layers) > maxDepth {
		return nil, fmt.Errorf("%w: %d declared layers exceed maximum depth %d",
			ErrInvalidNestedPolicy, len(policy.Layers), maxDepth)
	}
	for i, layer := range policy.Layers {
		switch layer.Kind {
		case LayerJWS:
			if len(layer.SignatureAlgorithms) == 0 {
				return nil, fmt.Errorf("%w: JWS layer %d requires signature algorithms", ErrInvalidNestedPolicy, i)
			}
		case LayerJWE:
			if len(layer.KeyAlgorithms) == 0 || len(layer.ContentEncryption) == 0 {
				return nil, fmt.Errorf("%w: JWE layer %d requires key and content encryption algorithms", ErrInvalidNestedPolicy, i)
			}
		default:
			return nil, fmt.Errorf("%w: layer %d has unknown kind", ErrInvalidNestedPolicy, i)
		}
	}

	headers := make([]jose.Header, 0, len(policy.Layers))
	current := s

	for i, layer := range policy.Layers {
		if i >= maxDepth {
			return nil, &LayerError{Layer: i, Err: ErrNestedTooDeep}
		}

		var (
			header  jose.Header
			payload []byte
			err     error
		)
		switch layer.Kind {
		case LayerJWS:
			header, payload, err = verifySignedLayer(current, layer)
		case LayerJWE:
			header, payload, err = decryptEncryptedLayer(current, layer)
		}
		if err != nil {
			return nil, &LayerError{Layer: i, Err: err}
		}
		if err := checkLayerHeaders(header, layer); err != nil {
			return nil, &LayerError{Layer: i, Err: err}
		}
		headers = append(headers, header)

		if i == len(policy.Layers)-1 {
			// The declared chain is exhausted: the payload must be the
			// claims, not yet another JOSE token.
			if isCompactJOSE(string(payload)) {
				return nil, &LayerError{Layer: i + 1, Err: ErrNestedTooDeep}
			}
			return &NestedToken{Headers: headers, payload: payload}, nil
		}
		current = string(payload)
	}

	// Unreachable: the loop always returns on the last declared layer.
	return nil, &LayerError{Layer: len(policy.Layers), Err: ErrNestedTooDeep}
}

func verifySignedLayer(s string, layer LayerPolicy) (jose.Header, []byte, error) {
	if len(layer.SignatureAlgorithms) == 0 {
		return jose.Header{}, nil, fmt.Errorf("%w: JWS layer requires signature algorithms", ErrInvalidNestedPolicy)
	}
	sig, err := jose.ParseSignedCompact(s, layer.SignatureAlgorithms)
	if err != nil {
		return jose.Header{}, nil, err
	}
	payload, err := sig.Verify(layer.Key)
	if err != nil {
		return jose.Header{}, nil, err
	}
	return sig.Signatures[0].Header, payload, nil
}

func decryptEncryptedLayer(s string, layer LayerPolicy) (jose.Header, []byte, error) {
	if len(layer.KeyAlgorithms) == 0 || len(layer.ContentEncryption) == 0 {
		return jose.Header{}, nil, fmt.Errorf("%w: JWE layer requires key and content encryption algorithms", ErrInvalidNestedPolicy)
	}
	enc, err := parseEncryptedCompact(s, layer.KeyAlgorithms, layer.ContentEncryption)
	if err != nil {
		return jose.Header{}, nil, err
	}
	payload, err := enc.Decrypt(layer.Key)
	if err != nil {
		return jose.Header{}, nil, err
	}
	return enc.Header, payload, nil
}

func checkLayerHeaders(h jose.Header, layer LayerPolicy) error {
	if layer.Type != "" {
		typ, _ := h.ExtraHeaders[jose.HeaderType].(string)
		if !strings.EqualFold(typ, layer.Type) {
			return ErrInvalidType
		}
	}
	if layer.ContentType != "" {
		cty, _ := h.ExtraHeaders[jose.HeaderContentType].(string)
		if !strings.EqualFold(cty, layer.ContentType) {
			return ErrInvalidContentType
		}
	}
	return nil
}

// isCompactJOSE reports whether s looks like a JWS or JWE compact
// serialization. It is used only to detect nesting deeper than declared;
// it never discloses or alters the input.
func isCompactJOSE(s string) bool {
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) != 3 && len(parts) != 5 {
		return false
	}
	for _, p := range parts {
		// Empty segments are legitimate (e.g. the encrypted key of a JWE
		// using "dir"); non-empty segments must be base64url.
		if p == "" {
			continue
		}
		if _, err := base64.RawURLEncoding.DecodeString(p); err != nil {
			return false
		}
	}
	return true
}
