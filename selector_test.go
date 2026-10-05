package digest

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
)

func TestSplitChallenges(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{
			input: `Basic realm="simple", Digest realm="a", nonce="n1"`,
			want:  []string{`Basic realm="simple"`, ` Digest realm="a", nonce="n1"`},
		},
		{
			// commas inside a quoted qop list are not separators
			input: `Digest realm="a", qop="auth,auth-int", nonce="n1"`,
			want:  []string{`Digest realm="a", qop="auth,auth-int", nonce="n1"`},
		},
		{
			// escaped quotes don't terminate the quoted-string
			input: `Digest realm="a\"b\\", nonce="n1"`,
			want:  []string{`Digest realm="a\"b\\", nonce="n1"`},
		},
		{
			input: `Digest realm="a", nonce="n1", Digest realm="b", nonce="n2"`,
			want:  []string{`Digest realm="a", nonce="n1"`, ` Digest realm="b", nonce="n2"`},
		},
		{
			input: `Digest realm="unterminated`,
			want:  []string{`Digest realm="unterminated`},
		},
	}
	for _, tt := range tests {
		got := splitChallenges(tt.input)
		assert.DeepEqual(t, tt.want, got)
	}
}

func mixedHeader() http.Header {
	h := http.Header{}
	h.Add("WWW-Authenticate", `Basic realm="simple", `+
		`Digest realm="a\"b", nonce="n1", algorithm=MD5, qop="auth,auth-int", `+
		`Digest realm="c", nonce="n2", algorithm=SHA-256`)
	return h
}

func TestSelector_MixedField(t *testing.T) {
	// default: first usable challenge, matching the old FindChallenge
	chal, err := FindChallenge(mixedHeader())
	assert.NilError(t, err)
	assert.Equal(t, `a"b`, chal.Realm)
	assert.Equal(t, "n1", chal.Nonce)
	assert.DeepEqual(t, []string{"auth", "auth-int"}, chal.QOP)

	// prefer SHA-256: the second digest challenge wins
	chal, err = (Selector{Prefer: []string{"SHA-256"}}).Select(mixedHeader())
	assert.NilError(t, err)
	assert.Equal(t, "c", chal.Realm)
	assert.Equal(t, "n2", chal.Nonce)
}

func TestSelector_UnknownAlgorithm(t *testing.T) {
	h := http.Header{}
	h.Add("WWW-Authenticate", `Digest realm="x", nonce="super-secret-nonce", algorithm=SHA3-512`)
	_, err := FindChallenge(h)
	var sel *SelectionError
	assert.Assert(t, errors.As(err, &sel))
	assert.Assert(t, strings.Contains(err.Error(), `unsupported algorithm "SHA3-512"`))
	assert.Assert(t, !strings.Contains(err.Error(), "super-secret-nonce"))
}

func TestSelector_BrokenChallengeIsolation(t *testing.T) {
	// a syntactically broken challenge must not swallow the parameters
	// of the challenges that follow it
	h := http.Header{}
	h.Add("WWW-Authenticate", `Digest stale=oops!, Digest realm="ok", nonce="n"`)
	chal, err := FindChallenge(h)
	assert.NilError(t, err)
	assert.Equal(t, "ok", chal.Realm)
	assert.Equal(t, "n", chal.Nonce)

	// a truncated quoted-string in one field must not affect the next field
	h = http.Header{}
	h.Add("WWW-Authenticate", `Digest realm="truncated`)
	h.Add("WWW-Authenticate", `Digest realm="ok", nonce="n"`)
	chal, err = FindChallenge(h)
	assert.NilError(t, err)
	assert.Equal(t, "ok", chal.Realm)

	// a lone truncated challenge is reported without its partial nonce
	h = http.Header{}
	h.Add("WWW-Authenticate", `Digest nonce="partial-secret`)
	_, err = FindChallenge(h)
	var sel *SelectionError
	assert.Assert(t, errors.As(err, &sel))
	assert.Assert(t, !strings.Contains(err.Error(), "partial-secret"))
}

func TestSelector_DuplicateAlgorithm(t *testing.T) {
	h := http.Header{}
	h.Add("WWW-Authenticate", `Digest realm="first", nonce="n1", algorithm=SHA-256`)
	h.Add("WWW-Authenticate", `Digest realm="second", nonce="n2", algorithm=SHA-256`)
	// ties are broken by receive order
	chal, err := (Selector{Prefer: []string{"SHA-256"}}).Select(h)
	assert.NilError(t, err)
	assert.Equal(t, "first", chal.Realm)
}

func TestSelector_DefaultAlgorithm(t *testing.T) {
	h := http.Header{}
	// a missing algorithm parameter means MD5 (the protocol default)
	h.Add("WWW-Authenticate", `Digest realm="defaulted", nonce="n1"`)
	h.Add("WWW-Authenticate", `Digest realm="explicit", nonce="n2", algorithm=MD5`)
	chal, err := (Selector{Prefer: []string{"SHA-256", "MD5"}}).Select(h)
	assert.NilError(t, err)
	assert.Equal(t, "defaulted", chal.Realm)
}

func TestSelector_NoDigest(t *testing.T) {
	h := http.Header{}
	h.Add("WWW-Authenticate", `Basic realm="simple"`)
	_, err := (Selector{Prefer: []string{"SHA-256"}}).Select(h)
	assert.Assert(t, errors.Is(err, ErrNoChallenge))
}

func permutations[T any](items []T) [][]T {
	if len(items) <= 1 {
		return [][]T{items}
	}
	var out [][]T
	for i, item := range items {
		rest := make([]T, 0, len(items)-1)
		rest = append(rest, items[:i]...)
		rest = append(rest, items[i+1:]...)
		for _, perm := range permutations(rest) {
			out = append(out, append([]T{item}, perm...))
		}
	}
	return out
}

func TestSelector_PreferPermutation(t *testing.T) {
	challenges := []string{
		`Digest realm="defaulted", nonce="n0"`, // missing algorithm => MD5
		`Digest realm="a", nonce="n1", algorithm=SHA-256`,
		`Digest realm="b", nonce="n2", algorithm=SHA-256`, // duplicate algorithm
		`Digest realm="c", nonce="n3", algorithm=SHA-512`,
		`Digest realm="d", nonce="n4", algorithm=SHA3-512`, // unknown algorithm
	}
	prefer := []string{"SHA-512", "SHA-256"}
	for _, perm := range permutations(challenges) {
		// all challenges crammed into a single field, after a Basic one
		h := http.Header{}
		h.Add("WWW-Authenticate", `Basic realm="simple", `+strings.Join(perm, ", "))
		chal, err := (Selector{Prefer: prefer}).Select(h)
		assert.NilError(t, err)
		assert.Equal(t, "c", chal.Realm)
		assert.Equal(t, "n3", chal.Nonce)

		// one challenge per field
		h = http.Header{}
		h.Add("WWW-Authenticate", `Basic realm="simple"`)
		for _, c := range perm {
			h.Add("WWW-Authenticate", c)
		}
		chal, err = (Selector{Prefer: prefer}).Select(h)
		assert.NilError(t, err)
		assert.Equal(t, "c", chal.Realm)
		assert.Equal(t, "n3", chal.Nonce)
	}
}
