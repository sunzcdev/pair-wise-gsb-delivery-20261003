package jwt

import (
	"errors"
	"strings"
	"testing"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/testutils/assert"
	"github.com/go-jose/go-jose/v4/testutils/require"
)

type chainTestClaims struct {
	Secret string `json:"secret"`
}

func TestChainVerifier(t *testing.T) {
	encryptionKey := []byte("0123456789abcdef0123456789abcdef")
	signatureKey := []byte("signature-secret-signature-secret")
	attackerSignatureKey := []byte("attacker-signature-key-32-bytes!")

	claims := chainTestClaims{Secret: "plaintext-chain-secret"}
	signedClaims := signChainClaims(t, claims, signatureKey)

	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.HS256, Key: signatureKey},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	require.NoError(t, err)

	encrypter, err := jose.NewEncrypter(
		jose.A256GCM,
		jose.Recipient{Algorithm: jose.DIRECT, Key: encryptionKey},
		(&jose.EncrypterOptions{}).WithType("JWT").WithContentType("JWT"),
	)
	require.NoError(t, err)

	correctNested := encryptChainToken(t, encrypter, signedClaims)
	reversedNested := signChainToken(t, signer, encryptChainClaims(t, encrypter, claims))
	replacedInner := encryptChainToken(t, encrypter, signChainClaims(t, chainTestClaims{
		Secret: "replaced-plaintext-chain-secret",
	}, attackerSignatureKey))
	extraLayer := encryptChainToken(t, encrypter, encryptChainToken(t, encrypter, signedClaims))
	extraInnerLayer := encryptChainToken(t, encrypter, signChainToken(t, signer, signedClaims))

	verifier := ChainVerifier{
		MaxDepth: 2,
		Layers: []Layer{
			{
				Kind:                        LayerJWE,
				Key:                         staticLayerKey(encryptionKey),
				Type:                        "JWT",
				ContentType:                 "JWT",
				KeyAlgorithms:               []jose.KeyAlgorithm{jose.DIRECT},
				ContentEncryptionAlgorithms: []jose.ContentEncryption{jose.A256GCM},
			},
			{
				Kind:                LayerJWS,
				Key:                 staticLayerKey(signatureKey),
				Type:                "JWT",
				SignatureAlgorithms: []jose.SignatureAlgorithm{jose.HS256},
			},
		},
	}

	tests := []struct {
		name      string
		token     string
		wantLayer int
		wantStage string
	}{
		{
			name:  "correct nested",
			token: correctNested,
		},
		{
			name:      "signature and encryption reversed",
			token:     reversedNested,
			wantLayer: 1,
			wantStage: LayerStageInvalidToken,
		},
		{
			name:      "inner token replaced",
			token:     replacedInner,
			wantLayer: 2,
			wantStage: LayerStageSignature,
		},
		{
			name:      "one extra inner layer",
			token:     extraInnerLayer,
			wantLayer: 2,
			wantStage: LayerStageInvalidClaims,
		},
		{
			name:      "one extra outer encryption layer",
			token:     extraLayer,
			wantLayer: 2,
			wantStage: LayerStageInvalidToken,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var got chainTestClaims
			err := verifier.Verify(test.token, &got)

			if test.wantLayer == 0 {
				require.NoError(t, err)
				assert.Equal(t, got, claims)
				return
			}

			if err == nil {
				t.Fatal("Verify() succeeded, want error")
			}
			var layerErr *LayerError
			if !errors.As(err, &layerErr) {
				t.Fatalf("Verify() error = %T, want *LayerError", err)
			}
			assert.Equal(t, layerErr.Layer, test.wantLayer)
			assert.Equal(t, layerErr.Stage, test.wantStage)
			if strings.Contains(err.Error(), claims.Secret) || strings.Contains(err.Error(), "replaced-plaintext-chain-secret") {
				t.Fatalf("Verify() error leaked plaintext: %q", err)
			}
		})
	}
}

func TestChainVerifierConfiguration(t *testing.T) {
	encryptionKey := []byte("0123456789abcdef0123456789abcdef")
	signatureKey := []byte("signature-secret-signature-secret")

	tests := []struct {
		name     string
		verifier ChainVerifier
	}{
		{
			name: "no layers",
			verifier: ChainVerifier{
				MaxDepth: 2,
			},
		},
		{
			name: "depth below one",
			verifier: ChainVerifier{
				MaxDepth: 0,
				Layers:   []Layer{{Kind: LayerJWS}},
			},
		},
		{
			name: "chain longer than maximum depth",
			verifier: ChainVerifier{
				MaxDepth: 1,
				Layers: []Layer{
					{Kind: LayerJWE},
					{Kind: LayerJWS},
				},
			},
		},
		{
			name: "missing key source",
			verifier: ChainVerifier{
				MaxDepth: 1,
				Layers: []Layer{{
					Kind:                LayerJWS,
					SignatureAlgorithms: []jose.SignatureAlgorithm{jose.HS256},
				}},
			},
		},
		{
			name: "final layer not signed",
			verifier: ChainVerifier{
				MaxDepth: 1,
				Layers: []Layer{{
					Kind:                        LayerJWE,
					Key:                         staticLayerKey(encryptionKey),
					KeyAlgorithms:               []jose.KeyAlgorithm{jose.DIRECT},
					ContentEncryptionAlgorithms: []jose.ContentEncryption{jose.A256GCM},
				}},
			},
		},
		{
			name: "outer layer not encrypted",
			verifier: ChainVerifier{
				MaxDepth: 2,
				Layers: []Layer{
					{
						Kind:                LayerJWS,
						Key:                 staticLayerKey(signatureKey),
						SignatureAlgorithms: []jose.SignatureAlgorithm{jose.HS256},
					},
					{
						Kind:                LayerJWS,
						Key:                 staticLayerKey(signatureKey),
						SignatureAlgorithms: []jose.SignatureAlgorithm{jose.HS256},
					},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.verifier.Verify("unused", nil)
			if !errors.Is(err, ErrInvalidVerificationChain) {
				t.Fatalf("Verify() error = %v, want ErrInvalidVerificationChain", err)
			}
		})
	}
}

func TestChainVerifierLayerPolicy(t *testing.T) {
	encryptionKey := []byte("0123456789abcdef0123456789abcdef")
	signatureKey := []byte("signature-secret-signature-secret")
	claims := chainTestClaims{Secret: "plaintext-chain-secret"}
	signedClaims := signChainClaims(t, claims, signatureKey)

	completeEncrypter, err := jose.NewEncrypter(
		jose.A256GCM,
		jose.Recipient{Algorithm: jose.DIRECT, Key: encryptionKey},
		(&jose.EncrypterOptions{}).WithType("JWT").WithContentType("JWT"),
	)
	require.NoError(t, err)

	typOnlyEncrypter, err := jose.NewEncrypter(
		jose.A256GCM,
		jose.Recipient{Algorithm: jose.DIRECT, Key: encryptionKey},
		(&jose.EncrypterOptions{}).WithType("JWT"),
	)
	require.NoError(t, err)

	correctNested := encryptChainToken(t, completeEncrypter, signedClaims)
	missingContentType := encryptChainToken(t, typOnlyEncrypter, signedClaims)

	verifier := ChainVerifier{
		MaxDepth: 2,
		Layers: []Layer{
			{
				Kind:                        LayerJWE,
				Key:                         staticLayerKey(encryptionKey),
				Type:                        "JWT",
				ContentType:                 "JWT",
				KeyAlgorithms:               []jose.KeyAlgorithm{jose.DIRECT},
				ContentEncryptionAlgorithms: []jose.ContentEncryption{jose.A256GCM},
			},
			{
				Kind:                LayerJWS,
				Key:                 staticLayerKey(signatureKey),
				Type:                "JWT",
				SignatureAlgorithms: []jose.SignatureAlgorithm{jose.HS256},
			},
		},
	}

	var got chainTestClaims
	require.NoError(t, verifier.Verify(correctNested, &got))
	assert.Equal(t, got, claims)

	err = verifier.Verify(missingContentType, &got)
	requireLayerError(t, err, 1, LayerStageInvalidHeader)

	wrongAlgorithm := verifier
	wrongAlgorithm.Layers[1].SignatureAlgorithms = []jose.SignatureAlgorithm{jose.HS384}
	err = wrongAlgorithm.Verify(correctNested, &got)
	requireLayerError(t, err, 2, LayerStageInvalidToken)

	wrongContentEncryption := verifier
	wrongContentEncryption.Layers[0].ContentEncryptionAlgorithms = []jose.ContentEncryption{jose.A128GCM}
	err = wrongContentEncryption.Verify(correctNested, &got)
	requireLayerError(t, err, 1, LayerStageInvalidToken)
}

func requireLayerError(t *testing.T, err error, layer int, stage string) {
	t.Helper()

	if err == nil {
		t.Fatal("Verify() succeeded, want error")
	}
	var layerErr *LayerError
	if !errors.As(err, &layerErr) {
		t.Fatalf("Verify() error = %T, want *LayerError", err)
	}
	assert.Equal(t, layerErr.Layer, layer)
	assert.Equal(t, layerErr.Stage, stage)
}

func staticLayerKey(key interface{}) LayerKey {
	return func(jose.Header) (interface{}, error) {
		return key, nil
	}
}

func signChainClaims(t *testing.T, claims chainTestClaims, key []byte) string {
	t.Helper()

	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.HS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	require.NoError(t, err)

	token, err := Signed(signer).Claims(claims).Serialize()
	require.NoError(t, err)
	return token
}

func signChainToken(t *testing.T, signer jose.Signer, token string) string {
	t.Helper()

	signed, err := signer.Sign([]byte(token))
	require.NoError(t, err)

	raw, err := signed.CompactSerialize()
	require.NoError(t, err)
	return raw
}

func encryptChainClaims(t *testing.T, encrypter jose.Encrypter, claims chainTestClaims) string {
	t.Helper()

	token, err := Encrypted(encrypter).Claims(claims).Serialize()
	require.NoError(t, err)
	return token
}

func encryptChainToken(t *testing.T, encrypter jose.Encrypter, token string) string {
	t.Helper()

	encrypted, err := encrypter.Encrypt([]byte(token))
	require.NoError(t, err)

	raw, err := encrypted.CompactSerialize()
	require.NoError(t, err)
	return raw
}
