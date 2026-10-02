package jwt

import (
	"errors"
	"fmt"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/json"
)

// LayerKind identifies the JOSE serialization required at one layer.
type LayerKind int

const (
	// LayerJWE requires the layer to be a compact JWE.
	LayerJWE LayerKind = iota + 1
	// LayerJWS requires the layer to be a compact JWS.
	LayerJWS
)

// Stages reported by LayerError.
const (
	LayerStageMaximumDepth  = "maximum chain depth exceeded"
	LayerStageInvalidToken  = "invalid token"
	LayerStageInvalidHeader = "invalid token header"
	LayerStageKeySource     = "key source failed"
	LayerStageDecryption    = "decryption failed"
	LayerStageSignature     = "signature verification failed"
	LayerStageInvalidClaims = "invalid claims"
)

// LayerKey selects the cryptographic key for a layer. The header has passed
// the layer's algorithm and typ/cty policy when Key is called, but the
// cryptographic operation has not yet succeeded. Use header values only to
// select the key.
type LayerKey func(jose.Header) (interface{}, error)

// Layer describes one required layer in verification order. The outermost
// layer must be first. Algorithm fields that do not apply to LayerKind are
// ignored.
type Layer struct {
	// Kind is the required serialization at this layer.
	Kind LayerKind

	// Key returns the key used to decrypt or verify the layer.
	Key LayerKey

	// Type optionally requires an exact JWT "typ" header value.
	Type string
	// ContentType optionally requires an exact JWT "cty" header value.
	ContentType string

	// SignatureAlgorithms is the allow-list for a JWS "alg" header.
	SignatureAlgorithms []jose.SignatureAlgorithm
	// KeyAlgorithms is the allow-list for a JWE "alg" header.
	KeyAlgorithms []jose.KeyAlgorithm
	// ContentEncryptionAlgorithms is the allow-list for a JWE "enc" header.
	ContentEncryptionAlgorithms []jose.ContentEncryption
}

// ChainVerifier verifies a token by following exactly the declared layers.
// It never probes alternative serializations or accepts an intermediate
// plaintext as verified claims.
type ChainVerifier struct {
	// Layers are ordered from the outermost token to the innermost token.
	Layers []Layer
	// MaxDepth limits the number of declared and processed layers. It must be
	// at least 1.
	MaxDepth int
}

// LayerError identifies the layer and stage at which verification failed.
// Error messages do not include token plaintext.
type LayerError struct {
	// Layer is the one-based layer at which failure occurred.
	Layer int
	// Stage identifies the failed verification operation at Layer.
	Stage string

	err error
}

func (e *LayerError) Error() string {
	return fmt.Sprintf("go-jose/go-jose/jwt: chain verification failed at layer %d: %s", e.Layer, e.Stage)
}

// Unwrap returns the underlying failure without exposing it in Error().
func (e *LayerError) Unwrap() error {
	return e.err
}

// Verify processes every declared layer and unmarshals the final payload into
// claims. Claims are unmarshaled only after the final JWS signature has been
// verified.
func (v ChainVerifier) Verify(raw string, claims interface{}) error {
	if err := v.validate(); err != nil {
		return err
	}

	payload := []byte(raw)
	for i, layer := range v.Layers {
		if i >= v.MaxDepth {
			return newLayerError(i+1, LayerStageMaximumDepth, errMaximumChainDepth)
		}

		var err error
		switch layer.Kind {
		case LayerJWE:
			payload, err = v.verifyEncryptionLayer(i+1, layer, string(payload))
		case LayerJWS:
			payload, err = v.verifySignatureLayer(i+1, layer, payload)
		default:
			err = invalidLayer(i, "unknown layer kind")
		}
		if err != nil {
			return err
		}
	}

	if err := json.Unmarshal(payload, claims); err != nil {
		return newLayerError(len(v.Layers), LayerStageInvalidClaims, err)
	}
	return nil
}

func (v ChainVerifier) validate() error {
	if v.MaxDepth < 1 {
		return fmt.Errorf("%w: maximum depth must be at least one", ErrInvalidVerificationChain)
	}
	if len(v.Layers) == 0 {
		return fmt.Errorf("%w: at least two layers are required", ErrInvalidVerificationChain)
	}
	if len(v.Layers) > v.MaxDepth {
		return fmt.Errorf("%w: chain depth exceeds maximum depth", ErrInvalidVerificationChain)
	}
	if len(v.Layers) < 2 || v.Layers[0].Kind != LayerJWE {
		return fmt.Errorf("%w: outer layer must be encrypted", ErrInvalidVerificationChain)
	}
	if v.Layers[len(v.Layers)-1].Kind != LayerJWS {
		return fmt.Errorf("%w: final layer must be signed", ErrInvalidVerificationChain)
	}

	for i, layer := range v.Layers {
		if layer.Key == nil {
			return invalidLayer(i, "key source is required")
		}

		switch layer.Kind {
		case LayerJWE:
			if len(layer.KeyAlgorithms) == 0 {
				return invalidLayer(i, "key algorithm allow-list is required")
			}
			if len(layer.ContentEncryptionAlgorithms) == 0 {
				return invalidLayer(i, "content encryption algorithm allow-list is required")
			}
		case LayerJWS:
			if len(layer.SignatureAlgorithms) == 0 {
				return invalidLayer(i, "signature algorithm allow-list is required")
			}
		default:
			return invalidLayer(i, "unknown layer kind")
		}
	}
	return nil
}

func (v ChainVerifier) verifyEncryptionLayer(layerNumber int, layer Layer, raw string) ([]byte, error) {
	enc, err := jose.ParseEncryptedCompact(raw, layer.KeyAlgorithms, layer.ContentEncryptionAlgorithms)
	if err != nil {
		return nil, newLayerError(layerNumber, LayerStageInvalidToken, err)
	}
	if err := validateLayerHeader(enc.Header, layer); err != nil {
		return nil, newLayerError(layerNumber, LayerStageInvalidHeader, err)
	}

	key, err := layer.Key(enc.Header)
	if err != nil {
		return nil, newLayerError(layerNumber, LayerStageKeySource, err)
	}

	payload, err := enc.Decrypt(key)
	if err != nil {
		return nil, newLayerError(layerNumber, LayerStageDecryption, err)
	}
	return payload, nil
}

func (v ChainVerifier) verifySignatureLayer(layerNumber int, layer Layer, raw []byte) ([]byte, error) {
	sig, err := jose.ParseSignedCompact(string(raw), layer.SignatureAlgorithms)
	if err != nil {
		return nil, newLayerError(layerNumber, LayerStageInvalidToken, err)
	}

	header := jose.Header{}
	if len(sig.Signatures) == 1 {
		header = sig.Signatures[0].Header
	}
	if err := validateLayerHeader(header, layer); err != nil {
		return nil, newLayerError(layerNumber, LayerStageInvalidHeader, err)
	}

	key, err := layer.Key(header)
	if err != nil {
		return nil, newLayerError(layerNumber, LayerStageKeySource, err)
	}

	payload, err := sig.Verify(key)
	if err != nil {
		return nil, newLayerError(layerNumber, LayerStageSignature, err)
	}
	return payload, nil
}

func validateLayerHeader(header jose.Header, layer Layer) error {
	if layer.Type != "" {
		if value, _ := header.ExtraHeaders[jose.HeaderType].(string); value != layer.Type {
			return errInvalidLayerHeader
		}
	}
	if layer.ContentType != "" {
		if value, _ := header.ExtraHeaders[jose.HeaderContentType].(string); value != layer.ContentType {
			return errInvalidLayerHeader
		}
	}
	return nil
}

func invalidLayer(i int, message string) error {
	return fmt.Errorf("%w: layer %d: %s", ErrInvalidVerificationChain, i+1, message)
}

func newLayerError(layer int, stage string, err error) *LayerError {
	return &LayerError{Layer: layer, Stage: stage, err: err}
}

var (
	errMaximumChainDepth  = errors.New("go-jose/go-jose/jwt: maximum chain depth exceeded")
	errInvalidLayerHeader = errors.New("go-jose/go-jose/jwt: invalid layer header")
)
