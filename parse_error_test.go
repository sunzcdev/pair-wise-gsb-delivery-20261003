package paseto_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"aidanwoods.dev/go-paseto"
	"github.com/stretchr/testify/require"
)

const secretClaimValue = "super-secret-claim-value"

// entryPoint covers one of the six Parser parse functions, along with the
// key material needed to build tokens for it.
type entryPoint struct {
	name         string
	header       string
	parse        func(token string) error
	sealRaw      func(content []byte) string
	validToken   string
	expiredToken string
	keyHex       string
}

func makeToken(exp time.Time) paseto.Token {
	token := paseto.NewToken()
	token.SetExpiration(exp)
	token.SetString("claim", secretClaimValue)

	return token
}

func testEntryPoints(t *testing.T) []entryPoint {
	t.Helper()

	implicit := []byte("implicit")
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)

	v2Key := paseto.NewV2SymmetricKey()
	v2Sk := paseto.NewV2AsymmetricSecretKey()
	v3Key := paseto.NewV3SymmetricKey()
	v3Sk := paseto.NewV3AsymmetricSecretKey()
	v4Key := paseto.NewV4SymmetricKey()
	v4Sk := paseto.NewV4AsymmetricSecretKey()

	return []entryPoint{
		{
			name:   "v2.local",
			header: "v2.local.",
			parse: func(token string) error {
				_, err := paseto.NewParser().ParseV2Local(v2Key, token)
				return err
			},
			sealRaw: func(content []byte) string {
				return paseto.V2LocalEncrypt(paseto.NewPacket(content, nil), v2Key, make([]byte, 24)).Encoded()
			},
			validToken:   makeToken(future).V2Encrypt(v2Key),
			expiredToken: makeToken(past).V2Encrypt(v2Key),
			keyHex:       v2Key.ExportHex(),
		},
		{
			name:   "v2.public",
			header: "v2.public.",
			parse: func(token string) error {
				_, err := paseto.NewParser().ParseV2Public(v2Sk.Public(), token)
				return err
			},
			sealRaw: func(content []byte) string {
				return paseto.V2PublicSign(paseto.NewPacket(content, nil), v2Sk).Encoded()
			},
			validToken:   makeToken(future).V2Sign(v2Sk),
			expiredToken: makeToken(past).V2Sign(v2Sk),
			keyHex:       v2Sk.ExportHex(),
		},
		{
			name:   "v3.local",
			header: "v3.local.",
			parse: func(token string) error {
				_, err := paseto.NewParser().ParseV3Local(v3Key, token, implicit)
				return err
			},
			sealRaw: func(content []byte) string {
				return paseto.V3LocalEncrypt(paseto.NewPacket(content, nil), v3Key, implicit, make([]byte, 32)).Encoded()
			},
			validToken:   makeToken(future).V3Encrypt(v3Key, implicit),
			expiredToken: makeToken(past).V3Encrypt(v3Key, implicit),
			keyHex:       v3Key.ExportHex(),
		},
		{
			name:   "v3.public",
			header: "v3.public.",
			parse: func(token string) error {
				_, err := paseto.NewParser().ParseV3Public(v3Sk.Public(), token, implicit)
				return err
			},
			sealRaw: func(content []byte) string {
				return paseto.V3PublicSign(paseto.NewPacket(content, nil), v3Sk, implicit).Encoded()
			},
			validToken:   makeToken(future).V3Sign(v3Sk, implicit),
			expiredToken: makeToken(past).V3Sign(v3Sk, implicit),
			keyHex:       v3Sk.ExportHex(),
		},
		{
			name:   "v4.local",
			header: "v4.local.",
			parse: func(token string) error {
				_, err := paseto.NewParser().ParseV4Local(v4Key, token, implicit)
				return err
			},
			sealRaw: func(content []byte) string {
				return paseto.V4LocalEncrypt(paseto.NewPacket(content, nil), v4Key, implicit, make([]byte, 32)).Encoded()
			},
			validToken:   makeToken(future).V4Encrypt(v4Key, implicit),
			expiredToken: makeToken(past).V4Encrypt(v4Key, implicit),
			keyHex:       v4Key.ExportHex(),
		},
		{
			name:   "v4.public",
			header: "v4.public.",
			parse: func(token string) error {
				_, err := paseto.NewParser().ParseV4Public(v4Sk.Public(), token, implicit)
				return err
			},
			sealRaw: func(content []byte) string {
				return paseto.V4PublicSign(paseto.NewPacket(content, nil), v4Sk, implicit).Encoded()
			},
			validToken:   makeToken(future).V4Sign(v4Sk, implicit),
			expiredToken: makeToken(past).V4Sign(v4Sk, implicit),
			keyHex:       v4Sk.ExportHex(),
		},
	}
}

func corruptPayloadChar(token string) string {
	parts := strings.Split(token, ".")
	payload := []byte(parts[2])
	if payload[0] == 'A' {
		payload[0] = 'B'
	} else {
		payload[0] = 'A'
	}
	parts[2] = string(payload)

	return strings.Join(parts, ".")
}

func otherHeader(header string) string {
	if header == "v2.local." {
		return "v4.local."
	}

	return "v2.local."
}

// corruptedSamples returns the shared set of broken tokens every parse entry
// point must classify the same way, so callers can rely on the kind (e.g. to
// pick a log level) rather than matching error strings.
func corruptedSamples(ep entryPoint) []struct {
	name  string
	token string
	kind  paseto.ErrorKind
} {
	return []struct {
		name  string
		token string
		kind  paseto.ErrorKind
	}{
		{"too few message parts", "not-a-token", paseto.ErrorKindSyntax},
		{"bad base64 payload", ep.header + "!!!", paseto.ErrorKindSyntax},
		{"payload too short", ep.header + "YWI", paseto.ErrorKindSyntax},
		{"wrong protocol header", otherHeader(ep.header) + strings.TrimPrefix(ep.validToken, ep.header), paseto.ErrorKindProtocol},
		{"corrupted authentication", corruptPayloadChar(ep.validToken), paseto.ErrorKindAuthentication},
		{"non-json claims", ep.sealRaw([]byte("this is not json")), paseto.ErrorKindClaims},
		{"expired token", ep.expiredToken, paseto.ErrorKindRule},
	}
}

func TestParseErrorClassification(t *testing.T) {
	for _, ep := range testEntryPoints(t) {
		t.Run(ep.name, func(t *testing.T) {
			for _, sample := range corruptedSamples(ep) {
				t.Run(sample.name, func(t *testing.T) {
					err := ep.parse(sample.token)
					require.Error(t, err)

					kind, ok := paseto.Classify(err)
					require.True(t, ok, "error should carry a classification")
					require.Equal(t, sample.kind, kind)

					var parseErr *paseto.ParseError
					require.ErrorAs(t, err, &parseErr)
					require.Equal(t, sample.kind, parseErr.Kind())

					// Public error text must be a safe summary only.
					require.NotContains(t, err.Error(), sample.token)
					require.NotContains(t, err.Error(), secretClaimValue)
					require.NotContains(t, err.Error(), ep.keyHex)

					// The original error chain stays intact for Is/As.
					switch sample.kind {
					case paseto.ErrorKindSyntax, paseto.ErrorKindProtocol, paseto.ErrorKindAuthentication:
						require.ErrorIs(t, err, paseto.TokenError{})
						require.NotErrorIs(t, err, paseto.RuleError{})
					case paseto.ErrorKindRule:
						require.ErrorIs(t, err, paseto.RuleError{})
						require.NotErrorIs(t, err, paseto.TokenError{})
					case paseto.ErrorKindClaims:
						require.NotErrorIs(t, err, paseto.TokenError{})
						require.NotErrorIs(t, err, paseto.RuleError{})
					}
				})
			}
		})
	}
}

func TestParseErrorRuleMetadata(t *testing.T) {
	key := paseto.NewV4SymmetricKey()

	expired := paseto.NewToken()
	expired.SetExpiration(time.Now().Add(-time.Hour))
	token := expired.V4Encrypt(key, nil)

	// Default parser has NotExpired preloaded at index 0.
	_, err := paseto.NewParser().ParseV4Local(key, token, nil)
	require.Error(t, err)

	var parseErr *paseto.ParseError
	require.ErrorAs(t, err, &parseErr)
	require.Equal(t, paseto.ErrorKindRule, parseErr.Kind())
	require.Equal(t, 0, parseErr.RuleIndex())
	require.Contains(t, parseErr.RuleName(), "NotExpired")

	// Custom rules: the failing rule's index, name, and original error are
	// all preserved.
	sentinel := errors.New("custom rule failure")
	customRule := func(paseto.Token) error { return sentinel }

	valid := paseto.NewToken()
	valid.SetExpiration(time.Now().Add(time.Hour))
	validToken := valid.V4Encrypt(key, nil)

	parser := paseto.MakeParser([]paseto.Rule{paseto.NotExpired(), customRule})
	_, err = parser.ParseV4Local(key, validToken, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, sentinel)
	require.ErrorIs(t, err, paseto.RuleError{})

	require.ErrorAs(t, err, &parseErr)
	require.Equal(t, paseto.ErrorKindRule, parseErr.Kind())
	require.Equal(t, 1, parseErr.RuleIndex())
	require.NotEmpty(t, parseErr.RuleName())
	require.NotContains(t, parseErr.RuleName(), "NotExpired")
}

func TestParseErrorNonRuleMetadataEmpty(t *testing.T) {
	key := paseto.NewV4SymmetricKey()

	_, err := paseto.NewParser().ParseV4Local(key, "not-a-token", nil)
	require.Error(t, err)

	var parseErr *paseto.ParseError
	require.ErrorAs(t, err, &parseErr)
	require.Equal(t, -1, parseErr.RuleIndex())
	require.Empty(t, parseErr.RuleName())
}

func TestClassifyUnclassifiedError(t *testing.T) {
	_, ok := paseto.Classify(errors.New("some other error"))
	require.False(t, ok)

	_, ok = paseto.Classify(nil)
	require.False(t, ok)
}

func TestErrorKindString(t *testing.T) {
	require.Equal(t, "syntax", paseto.ErrorKindSyntax.String())
	require.Equal(t, "protocol", paseto.ErrorKindProtocol.String())
	require.Equal(t, "authentication", paseto.ErrorKindAuthentication.String())
	require.Equal(t, "claims", paseto.ErrorKindClaims.String())
	require.Equal(t, "rule", paseto.ErrorKindRule.String())
	require.Equal(t, "unknown", paseto.ErrorKind(0).String())
}

func TestUnsafeParseFooterClassified(t *testing.T) {
	_, err := paseto.NewParser().UnsafeParseFooter(paseto.V4Local, "not-a-token")
	require.Error(t, err)

	kind, ok := paseto.Classify(err)
	require.True(t, ok)
	require.Equal(t, paseto.ErrorKindSyntax, kind)
}
