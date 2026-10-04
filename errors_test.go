package paseto_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"aidanwoods.dev/go-paseto"
	"github.com/stretchr/testify/require"
)

// rejectToken is a custom Rule which always fails, used to check rule name
// and index reporting.
func rejectToken(paseto.Token) error {
	return errors.New("token rejected by custom rule")
}

type parseProtocol struct {
	name        string
	header      string
	wrongHeader string
	encode      func(content []byte) string
	parse       func(parser paseto.Parser, token string) error
	parseBadKey func(token string) error
}

func testParseProtocols() []parseProtocol {
	v2Key, v2KeyWrong := paseto.NewV2SymmetricKey(), paseto.NewV2SymmetricKey()
	v2Secret, v2SecretWrong := paseto.NewV2AsymmetricSecretKey(), paseto.NewV2AsymmetricSecretKey()
	v3Key, v3KeyWrong := paseto.NewV3SymmetricKey(), paseto.NewV3SymmetricKey()
	v3Secret, v3SecretWrong := paseto.NewV3AsymmetricSecretKey(), paseto.NewV3AsymmetricSecretKey()
	v4Key, v4KeyWrong := paseto.NewV4SymmetricKey(), paseto.NewV4SymmetricKey()
	v4Secret, v4SecretWrong := paseto.NewV4AsymmetricSecretKey(), paseto.NewV4AsymmetricSecretKey()

	return []parseProtocol{
		{
			name:        "v2.local",
			header:      "v2.local.",
			wrongHeader: "v2.public.",
			encode: func(content []byte) string {
				return paseto.V2LocalEncrypt(paseto.NewPacket(content, nil), v2Key, nil).Encoded()
			},
			parse: func(parser paseto.Parser, token string) error {
				_, err := parser.ParseV2Local(v2Key, token)
				return err
			},
			parseBadKey: func(token string) error {
				_, err := paseto.NewParserWithoutExpiryCheck().ParseV2Local(v2KeyWrong, token)
				return err
			},
		},
		{
			name:        "v2.public",
			header:      "v2.public.",
			wrongHeader: "v2.local.",
			encode: func(content []byte) string {
				return paseto.V2PublicSign(paseto.NewPacket(content, nil), v2Secret).Encoded()
			},
			parse: func(parser paseto.Parser, token string) error {
				_, err := parser.ParseV2Public(v2Secret.Public(), token)
				return err
			},
			parseBadKey: func(token string) error {
				_, err := paseto.NewParserWithoutExpiryCheck().ParseV2Public(v2SecretWrong.Public(), token)
				return err
			},
		},
		{
			name:        "v3.local",
			header:      "v3.local.",
			wrongHeader: "v3.public.",
			encode: func(content []byte) string {
				return paseto.V3LocalEncrypt(paseto.NewPacket(content, nil), v3Key, nil, nil).Encoded()
			},
			parse: func(parser paseto.Parser, token string) error {
				_, err := parser.ParseV3Local(v3Key, token, nil)
				return err
			},
			parseBadKey: func(token string) error {
				_, err := paseto.NewParserWithoutExpiryCheck().ParseV3Local(v3KeyWrong, token, nil)
				return err
			},
		},
		{
			name:        "v3.public",
			header:      "v3.public.",
			wrongHeader: "v3.local.",
			encode: func(content []byte) string {
				return paseto.V3PublicSign(paseto.NewPacket(content, nil), v3Secret, nil).Encoded()
			},
			parse: func(parser paseto.Parser, token string) error {
				_, err := parser.ParseV3Public(v3Secret.Public(), token, nil)
				return err
			},
			parseBadKey: func(token string) error {
				_, err := paseto.NewParserWithoutExpiryCheck().ParseV3Public(v3SecretWrong.Public(), token, nil)
				return err
			},
		},
		{
			name:        "v4.local",
			header:      "v4.local.",
			wrongHeader: "v4.public.",
			encode: func(content []byte) string {
				return paseto.V4LocalEncrypt(paseto.NewPacket(content, nil), v4Key, nil, nil).Encoded()
			},
			parse: func(parser paseto.Parser, token string) error {
				_, err := parser.ParseV4Local(v4Key, token, nil)
				return err
			},
			parseBadKey: func(token string) error {
				_, err := paseto.NewParserWithoutExpiryCheck().ParseV4Local(v4KeyWrong, token, nil)
				return err
			},
		},
		{
			name:        "v4.public",
			header:      "v4.public.",
			wrongHeader: "v4.local.",
			encode: func(content []byte) string {
				return paseto.V4PublicSign(paseto.NewPacket(content, nil), v4Secret, nil).Encoded()
			},
			parse: func(parser paseto.Parser, token string) error {
				_, err := parser.ParseV4Public(v4Secret.Public(), token, nil)
				return err
			},
			parseBadKey: func(token string) error {
				_, err := paseto.NewParserWithoutExpiryCheck().ParseV4Public(v4SecretWrong.Public(), token, nil)
				return err
			},
		},
	}
}

// TestParseErrorClassification feeds the same set of corrupted samples
// through all six parse entry points and checks that callers can stably
// classify each failure (e.g. to pick a log level) via Classify, without
// matching error message strings.
func TestParseErrorClassification(t *testing.T) {
	validClaims := []byte(`{"data":"secret-claim-value","exp":"2099-01-01T00:00:00Z"}`)

	for _, protocol := range testParseProtocols() {
		t.Run(protocol.name, func(t *testing.T) {
			valid := protocol.encode(validClaims)

			samples := []struct {
				name  string
				token string
				kind  paseto.ErrorKind
			}{
				{"not a token", "this-is-not-a-paseto-token", paseto.ErrorKindSyntax},
				{"missing payload", protocol.header, paseto.ErrorKindSyntax},
				{"bad payload encoding", protocol.header + "!!!not-base64!!!", paseto.ErrorKindSyntax},
				{"truncated payload", protocol.header + "YWI", paseto.ErrorKindSyntax},
				{"protocol mismatch", protocol.wrongHeader + strings.TrimPrefix(valid, protocol.header), paseto.ErrorKindProtocolMismatch},
				{"non-json claims", protocol.encode([]byte("this is not json")), paseto.ErrorKindClaimsDecode},
			}

			for _, sample := range samples {
				t.Run(sample.name, func(t *testing.T) {
					err := protocol.parse(paseto.NewParserWithoutExpiryCheck(), sample.token)
					require.Error(t, err)

					kind, ok := paseto.Classify(err)
					require.True(t, ok, "parse errors should always be classified")
					require.Equal(t, sample.kind, kind)

					var parseErr *paseto.ParseError
					require.ErrorAs(t, err, &parseErr)
					require.Equal(t, sample.kind, parseErr.Kind())
					require.Equal(t, -1, parseErr.RuleIndex())
					require.Empty(t, parseErr.RuleName())

					// original error chain is preserved
					require.ErrorIs(t, err, paseto.TokenError{})
					require.NotErrorIs(t, err, &paseto.RuleError{})

					// public error text is a safe summary only
					require.NotContains(t, err.Error(), sample.token)
					require.NotContains(t, err.Error(), "secret-claim-value")
				})
			}

			t.Run("wrong key", func(t *testing.T) {
				err := protocol.parseBadKey(valid)
				require.Error(t, err)

				kind, ok := paseto.Classify(err)
				require.True(t, ok)
				require.Equal(t, paseto.ErrorKindAuthentication, kind)

				require.ErrorIs(t, err, paseto.TokenError{})
				require.NotContains(t, err.Error(), valid)
				require.NotContains(t, err.Error(), "secret-claim-value")
			})
		})
	}
}

// TestParseErrorClaimsDecodeChain checks the original decoding error remains
// available via errors.As when claims cannot be decoded.
func TestParseErrorClaimsDecodeChain(t *testing.T) {
	key := paseto.NewV4SymmetricKey()
	token := paseto.V4LocalEncrypt(paseto.NewPacket([]byte("this is not json"), nil), key, nil, nil).Encoded()

	_, err := paseto.NewParserWithoutExpiryCheck().ParseV4Local(key, token, nil)
	require.Error(t, err)

	kind, ok := paseto.Classify(err)
	require.True(t, ok)
	require.Equal(t, paseto.ErrorKindClaimsDecode, kind)

	var syntaxErr *json.SyntaxError
	require.ErrorAs(t, err, &syntaxErr)
}

// TestParseErrorRuleInfo checks that rule failures are classified, and that
// the failing rule's name and index can be queried without inspecting the
// error text.
func TestParseErrorRuleInfo(t *testing.T) {
	validClaims := []byte(`{"aud":"super-secret-audience","exp":"2099-01-01T00:00:00Z"}`)

	for _, protocol := range testParseProtocols() {
		t.Run(protocol.name, func(t *testing.T) {
			valid := protocol.encode(validClaims)

			parser := paseto.MakeParser([]paseto.Rule{
				paseto.ForAudience("expected-audience"),
				rejectToken,
			})

			err := protocol.parse(parser, valid)
			require.Error(t, err)

			kind, ok := paseto.Classify(err)
			require.True(t, ok)
			require.Equal(t, paseto.ErrorKindRule, kind)

			var parseErr *paseto.ParseError
			require.ErrorAs(t, err, &parseErr)
			require.Equal(t, 0, parseErr.RuleIndex())
			require.Contains(t, parseErr.RuleName(), "ForAudience")

			require.ErrorIs(t, err, &paseto.RuleError{})
			require.NotErrorIs(t, err, &paseto.TokenError{})

			// public error text must not leak claim values or expectations
			require.NotContains(t, err.Error(), "super-secret-audience")
			require.NotContains(t, err.Error(), "expected-audience")
			require.NotContains(t, err.Error(), valid)

			// the custom rule fails at index 1 once the first rule passes
			parser = paseto.MakeParser([]paseto.Rule{
				paseto.ForAudience("super-secret-audience"),
				rejectToken,
			})

			err = protocol.parse(parser, valid)
			require.Error(t, err)

			require.ErrorAs(t, err, &parseErr)
			require.Equal(t, paseto.ErrorKindRule, parseErr.Kind())
			require.Equal(t, 1, parseErr.RuleIndex())
			require.Contains(t, parseErr.RuleName(), "rejectToken")
			require.NotContains(t, err.Error(), "token rejected by custom rule")
		})
	}
}

// TestParseErrorValidTokensAccepted is a sanity check that classification
// does not interfere with successful parses.
func TestParseErrorValidTokensAccepted(t *testing.T) {
	validClaims := []byte(`{"data":"secret-claim-value","exp":"2099-01-01T00:00:00Z"}`)

	for _, protocol := range testParseProtocols() {
		t.Run(protocol.name, func(t *testing.T) {
			require.NoError(t, protocol.parse(paseto.NewParserWithoutExpiryCheck(), protocol.encode(validClaims)))
		})
	}
}
