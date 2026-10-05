package digest

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
)

// mixed field pieces: a Basic scheme, two distinct strong digest
// challenges, an MD5 default, a qop containing a comma, a duplicated
// algorithm parameter, an unknown algorithm, and a truncated quoted
// string followed by a valid challenge.
func mixedChunks() []string {
	return []string{
		`Basic realm="mix"`,
		`Digest realm="mix", nonce="n-sha256", algorithm=SHA-256, qop="auth,auth-int"`,
		`Digest realm="mix", nonce="n-sha512", algorithm=SHA-512, qop="auth,auth-int"`,
		`Digest realm="mix", nonce="n-default", qop="auth"`,
		`Digest realm="mix", nonce="n-dup", algorithm=MD5, algorithm=MD5, qop="auth"`,
		`Digest realm="mix", nonce="n-unknown", algorithm=KRB5, qop="auth"`,
		`Digest realm="mix", nonce="n-broken, qop=auth, Digest realm="mix", nonce="n-after", algorithm=SHA-256, qop="auth"`,
	}
}

func key(c *Challenge) string {
	return strings.Join([]string{
		c.Realm,
		c.Nonce,
		c.Algorithm,
		strings.Join(c.QOP, "|"),
	}, ";")
}

func TestParseChallenges_MixedField(t *testing.T) {
	field := strings.Join(mixedChunks(), ", ")
	h := http.Header{}
	h.Set("WWW-Authenticate", field)

	parsed := ParseChallenges(h)
	// Basic + 6 digest texts, but the truncated-quote text splits into
	// a broken entry plus the recovered valid digest after it: 8 total.
	assert.Equal(t, len(parsed), 8)

	assert.Equal(t, parsed[0].Scheme, "Basic")
	assert.Assert(t, parsed[0].Challenge == nil)

	assert.Equal(t, parsed[1].Challenge.Algorithm, "SHA-256")
	assert.DeepEqual(t, parsed[1].Challenge.QOP, []string{"auth", "auth-int"})
	assert.Equal(t, parsed[2].Challenge.Algorithm, "SHA-512")

	// missing algorithm: not rewritten here (defaults apply at selection)
	assert.Equal(t, parsed[3].Challenge.Algorithm, "")

	// duplicated algorithm parameter: last wins
	assert.Equal(t, parsed[4].Challenge.Algorithm, "MD5")

	// unknown algorithm is preserved but unsupported
	assert.Equal(t, parsed[5].Challenge.Algorithm, "KRB5")
	assert.Assert(t, !CanDigest(parsed[5].Challenge))

	// truncated quote: broken entry must not swallow the following one
	assert.Assert(t, parsed[6].Err != nil)
	assert.Assert(t, parsed[6].Challenge == nil)
	assert.Equal(t, parsed[7].Challenge.Nonce, "n-after")
	assert.Equal(t, parsed[7].Challenge.Algorithm, "SHA-256")
}

func TestSplitAuthField_TruncatedQuoteRecovery(t *testing.T) {
	field := `Digest realm="mix", nonce="n-broken, qop=auth, Digest realm="mix", nonce="n-after", algorithm=SHA-256, qop="auth"`
	raw := splitAuthField(field)
	assert.Equal(t, len(raw), 2)
	assert.Equal(t, raw[0].scheme, "Digest")
	assert.Assert(t, raw[0].err != nil)
	assert.Equal(t, raw[1].scheme, "Digest")
	assert.Equal(t, raw[1].body, `realm="mix", nonce="n-after", algorithm=SHA-256, qop="auth"`)

	h := http.Header{}
	h.Set("WWW-Authenticate", field)
	parsed := ParseChallenges(h)
	assert.Equal(t, len(parsed), 2)
	assert.Assert(t, parsed[0].Err != nil)
	assert.Equal(t, parsed[1].Challenge.Nonce, "n-after")
	assert.Equal(t, parsed[1].Challenge.Algorithm, "SHA-256")
}

func TestSplitAuthField_QuotedCommaAndEscapes(t *testing.T) {
	field := `Digest realm="a, b", nonce="a\"b,c", qop="auth,auth-int", opaque="x"`
	raw := splitAuthField(field)
	assert.Equal(t, len(raw), 1)

	c, err := ParseChallenge(field)
	assert.NilError(t, err)
	assert.Equal(t, c.Realm, "a, b")
	assert.Equal(t, c.Nonce, `a"b,c`)
	assert.DeepEqual(t, c.QOP, []string{"auth", "auth-int"})
}

func TestSplitAuthField_BasicToken68(t *testing.T) {
	field := `Basic QWxhZGRpbjpvcGVuIHNlc2FtZQ==, Digest realm="r", nonce="n", qop="auth"`
	raw := splitAuthField(field)
	assert.Equal(t, len(raw), 2)
	assert.Equal(t, raw[0].scheme, "Basic")
	assert.Equal(t, raw[0].body, `QWxhZGRpbjpvcGVuIHNlc2FtZQ==`)
	assert.Equal(t, raw[1].scheme, "Digest")

	h := http.Header{}
	h.Set("WWW-Authenticate", field)
	parsed := ParseChallenges(h)
	assert.Equal(t, len(parsed), 2)
	assert.Assert(t, parsed[0].Challenge == nil)
	assert.Equal(t, parsed[1].Challenge.Nonce, "n")
}

func TestSplitAuthField_TwoDigestsDifferentQOP(t *testing.T) {
	field := `Digest realm="r", nonce="a", qop="auth,auth-int", Digest realm="r", nonce="b", qop="auth"`
	raw := splitAuthField(field)
	assert.Equal(t, len(raw), 2)
	c, err := ParseChallenge(`Digest ` + raw[1].body)
	assert.NilError(t, err)
	assert.Equal(t, c.Nonce, "b")
}

func TestChallengeSelector_PreferredPermutations(t *testing.T) {
	chunks := mixedChunks()
	sel := ChallengeSelector{
		Algorithms: []string{"SHA-512-256", "SHA-512", "SHA-256", "MD5"},
	}
	var wantKey string
	for order := range permutations(len(chunks)) {
		perm := make([]string, len(chunks))
		for i, idx := range order {
			perm[i] = chunks[idx]
		}
		h := http.Header{}
		h.Set("WWW-Authenticate", strings.Join(perm, ", "))

		selection := sel.Select(h)
		assert.Assert(t, selection.Err == nil, "order %v: %v", order, selection.Err)
		assert.Equal(t, selection.Challenge.Algorithm, "SHA-512")
		assert.Equal(t, selection.Challenge.Nonce, "n-sha512")

		got := key(selection.Challenge)
		if wantKey == "" {
			wantKey = got
		} else {
			assert.Equal(t, got, wantKey)
		}

		// every other entry is rejected, and no text may carry a nonce
		assert.Equal(t, len(selection.Rejections), 7)
		allText := selection.ErrString()
		for _, r := range selection.Rejections {
			allText += " " + r.Detail
		}
		for _, n := range []string{"n-sha256", "n-sha512", "n-default", "n-dup", "n-unknown", "n-broken", "n-after"} {
			assert.Assert(t, !strings.Contains(allText, n))
		}
	}
}

func TestChallengeSelector_PreferredAcrossFields(t *testing.T) {
	chunks := mixedChunks()
	sel := ChallengeSelector{
		Algorithms: []string{"SHA-512", "SHA-256", "MD5"},
	}
	for order := range permutations(len(chunks)) {
		h := http.Header{}
		for _, idx := range order {
			h.Add("WWW-Authenticate", chunks[idx])
		}
		chal, err := sel.Find(h)
		assert.NilError(t, err)
		assert.Equal(t, chal.Nonce, "n-sha512")
		assert.Equal(t, chal.Algorithm, "SHA-512")
	}
}

func TestChallengeSelector_LegacyFirstUsable(t *testing.T) {
	chunks := mixedChunks()
	for order := range permutations(len(chunks)) {
		h := http.Header{}
		for _, idx := range order {
			h.Add("WWW-Authenticate", chunks[idx])
		}
		// compute the expected first usable digest with the shared parser
		var firstUsable string
		for _, pc := range ParseChallenges(h) {
			if pc.Challenge != nil && CanDigest(pc.Challenge) {
				firstUsable = pc.Challenge.Nonce
				break
			}
		}
		chal, err := FindChallenge(h)
		assert.NilError(t, err)
		assert.Equal(t, chal.Nonce, firstUsable)
	}
}

func TestChallengeSelector_MissingAlgorithmDefaultsToMD5(t *testing.T) {
	sel := ChallengeSelector{Algorithms: []string{"MD5"}}
	h := http.Header{}
	h.Set("WWW-Authenticate", `Digest realm="r", nonce="n", qop="auth"`)
	sel2 := sel.Select(h)
	assert.NilError(t, sel2.Err)
	assert.Equal(t, sel2.Challenge.Nonce, "n")

	// missing algorithm is rejected when preferences exclude MD5
	strong := ChallengeSelector{Algorithms: []string{"SHA-256"}}
	sel3 := strong.Select(h)
	assert.Assert(t, sel3.Err != nil)
	assert.Assert(t, strings.Contains(sel3.Err.Error(), "no usable challenge"))

	// unknown algorithm: not eligible, and a usable sibling still wins
	h2 := http.Header{}
	h2.Set("WWW-Authenticate",
		`Digest realm="r", nonce="n1", algorithm=KRB5, qop="auth", `+
			`Digest realm="r", nonce="n2", algorithm=SHA-256, qop="auth"`)
	sel4 := ChallengeSelector{Algorithms: []string{"SHA-256", "MD5"}}.Select(h2)
	assert.NilError(t, sel4.Err)
	assert.Equal(t, sel4.Challenge.Nonce, "n2")
}

func TestChallengeSelector_DiagnosticsOmitNonce(t *testing.T) {
	sel := ChallengeSelector{Algorithms: []string{"SHA-512"}}
	h := http.Header{}
	h.Set("WWW-Authenticate", strings.Join([]string{
		`Basic realm="mix"`,
		`Digest realm="mix", nonce="SECRETNONCE", algorithm=MD5, qop="auth"`,
		`Digest realm="mix", nonce="SECRETNONCE2", algorithm=KRB5`,
		`Digest realm="mix", nonce="BROKEN`,
	}, ", "))
	sel2 := sel.Select(h)
	assert.Assert(t, sel2.Err != nil)
	msg := sel2.Err.Error()
	assert.Assert(t, !strings.Contains(msg, "SECRETNONCE"))
	assert.Assert(t, !strings.Contains(msg, "SECRETNONCE2"))
	assert.Assert(t, !strings.Contains(msg, "BROKEN"))
	// the broken raw text must not leak either
	for _, r := range sel2.Rejections {
		assert.Assert(t, !strings.Contains(r.Detail, "SECRETNONCE"))
	}
}

func TestChallengeSelector_StableTieBreak(t *testing.T) {
	a := `Digest realm="r", nonce="zzz", algorithm=SHA-256, qop="auth"`
	b := `Digest realm="r", nonce="aaa", algorithm=SHA-256, qop="auth"`
	sel := ChallengeSelector{Algorithms: []string{"SHA-256"}}

	h1 := http.Header{}
	h1.Set("WWW-Authenticate", a+", "+b)
	c1, err := sel.Find(h1)
	assert.NilError(t, err)

	h2 := http.Header{}
	h2.Set("WWW-Authenticate", b+", "+a)
	c2, err := sel.Find(h2)
	assert.NilError(t, err)

	assert.Equal(t, c1.Nonce, c2.Nonce)
	assert.Equal(t, c1.Nonce, "aaa")
}

func TestTransport_ChallengeSelector(t *testing.T) {
	username := "foo"
	password := "bar"
	chosen := &Challenge{
		Realm:     "mix",
		Nonce:     "n-sha512",
		Algorithm: "SHA-512",
		QOP:       []string{"auth"},
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var authorized bool
		if auth := r.Header.Get("Authorization"); auth != "" {
			cred, err := ParseCredentials(auth)
			if err == nil && cred.Algorithm == "SHA-512" {
				want, err := Digest(chosen, Options{
					Method:   r.Method,
					URI:      r.URL.RequestURI(),
					Cnonce:   cred.Cnonce,
					Count:    cred.Nc,
					Username: username,
					Password: password,
				})
				authorized = err == nil && cred.Response == want.Response
			}
		}
		if !authorized {
			w.Header().Set("WWW-Authenticate", strings.Join(mixedChunks(), ", "))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.WriteString(w, "ok")
	}))
	defer ts.Close()

	client := http.Client{
		Transport: &Transport{
			Username: username,
			Password: password,
			FindChallenge: ChallengeSelector{
				Algorithms: []string{"SHA-512-256", "SHA-512", "SHA-256", "MD5"},
			}.Find,
		},
	}
	res, err := client.Get(ts.URL)
	assert.NilError(t, err)
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	assert.NilError(t, err)
	assert.Equal(t, string(body), "ok")
}

// Selection.ErrString is a small test helper.
func (s Selection) ErrString() string {
	if s.Err == nil {
		return ""
	}
	return s.Err.Error()
}

// permutations yields every permutation of [0..n) on its channel.
func permutations(n int) <-chan []int {
	ch := make(chan []int)
	go func() {
		defer close(ch)
		idx := make([]int, n)
		for i := range idx {
			idx[i] = i
		}
		var rec func(int)
		rec = func(k int) {
			if k == 1 {
				out := make([]int, n)
				copy(out, idx)
				ch <- out
				return
			}
			for i := 0; i < k; i++ {
				rec(k - 1)
				if k%2 == 0 {
					idx[i], idx[k-1] = idx[k-1], idx[i]
				} else {
					idx[0], idx[k-1] = idx[k-1], idx[0]
				}
			}
		}
		rec(n)
	}()
	return ch
}
