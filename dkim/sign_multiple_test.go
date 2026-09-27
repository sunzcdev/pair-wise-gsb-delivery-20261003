package dkim

import (
	"context"
	"crypto"
	"errors"
	"io"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

// During key rotation a message is signed with both the old and the new
// selector. The two signatures deliberately use different header field sets
// and body canonicalizations.
func TestSignMultiple(t *testing.T) {
	optionsList := []*SignOptions{
		{
			Domain:     "example.org",
			Selector:   "brisbane",
			Signer:     testPrivateKey,
			HeaderKeys: []string{"From", "To", "Subject"},
		},
		{
			Domain:                 "football.example.com",
			Selector:               "test",
			Signer:                 testPrivateKey,
			HeaderCanonicalization: CanonicalizationRelaxed,
			BodyCanonicalization:   CanonicalizationRelaxed,
			HeaderKeys:             []string{"From", "Date", "Message-ID"},
		},
	}

	results, err := SignMultiple(strings.NewReader(mailString), optionsList)
	if err != nil {
		t.Fatal("Expected no error while signing mail, got:", err)
	}
	if len(results) != len(optionsList) {
		t.Fatalf("Expected %v results, got %v", len(optionsList), len(results))
	}

	var signed strings.Builder
	for i, res := range results {
		if res.Err != nil {
			t.Fatalf("Expected no error for signature %v, got: %v", i, res.Err)
		}
		if !strings.HasPrefix(res.Signature, "DKIM-Signature:") {
			t.Errorf("Expected signature %v to be a DKIM-Signature header field, got %q", i, res.Signature)
		}
		signed.WriteString(res.Signature)
	}
	signed.WriteString(mailString)

	verifications, err := Verify(strings.NewReader(signed.String()))
	if err != nil {
		t.Fatalf("Expected no error while verifying signatures, got: %v", err)
	}
	if len(verifications) != len(optionsList) {
		t.Fatalf("Expected exactly %v verifications, got %v", len(optionsList), len(verifications))
	}
	for i, v := range verifications {
		if v.Err != nil {
			t.Errorf("Expected no error when verifying signature %v, got: %v", i, v.Err)
		}
		if want := optionsList[i].Domain; v.Domain != want {
			t.Errorf("Expected domain to be %q but got %q", want, v.Domain)
		}
	}
}

// The message body must be read exactly once, no matter how many signatures
// are generated.
func TestSignMultiple_readsBodyOnce(t *testing.T) {
	optionsList := []*SignOptions{
		{Domain: "example.org", Selector: "brisbane", Signer: testPrivateKey},
		{Domain: "football.example.com", Selector: "test", Signer: testPrivateKey},
	}

	cr := &countingReader{r: strings.NewReader(mailString)}
	if _, err := SignMultiple(cr, optionsList); err != nil {
		t.Fatal("Expected no error while signing mail, got:", err)
	}
	if cr.n != int64(len(mailString)) {
		t.Errorf("Expected exactly %v bytes to be read, got %v", len(mailString), cr.n)
	}
}

type countingReader struct {
	r io.Reader
	n int64
}

func (cr *countingReader) Read(b []byte) (int, error) {
	n, err := cr.r.Read(b)
	cr.n += int64(n)
	return n, err
}

// Options entries sharing the same body canonicalization must share the
// canonicalization and hashing work.
func TestSignMultiple_sharedCanonicalization(t *testing.T) {
	var simpleBodyCalls, relaxedBodyCalls int
	defer swapCanonicalizer(CanonicalizationSimple, &countingCanonicalizer{
		canonicalizer: canonicalizers[CanonicalizationSimple],
		bodyCalls:     &simpleBodyCalls,
	})()
	defer swapCanonicalizer(CanonicalizationRelaxed, &countingCanonicalizer{
		canonicalizer: canonicalizers[CanonicalizationRelaxed],
		bodyCalls:     &relaxedBodyCalls,
	})()

	optionsList := []*SignOptions{
		{Domain: "example.org", Selector: "brisbane", Signer: testPrivateKey},
		{Domain: "football.example.com", Selector: "test", Signer: testPrivateKey},
		{
			Domain:               "example.org",
			Selector:             "brisbane",
			Signer:               testPrivateKey,
			BodyCanonicalization: CanonicalizationRelaxed,
		},
	}

	if _, err := SignMultiple(strings.NewReader(mailString), optionsList); err != nil {
		t.Fatal("Expected no error while signing mail, got:", err)
	}
	if simpleBodyCalls != 1 {
		t.Errorf("Expected simple body canonicalization to run once, got %v", simpleBodyCalls)
	}
	if relaxedBodyCalls != 1 {
		t.Errorf("Expected relaxed body canonicalization to run once, got %v", relaxedBodyCalls)
	}
}

type countingCanonicalizer struct {
	canonicalizer
	bodyCalls *int
}

func (c *countingCanonicalizer) CanonicalizeBody(w io.Writer) io.WriteCloser {
	*c.bodyCalls++
	return c.canonicalizer.CanonicalizeBody(w)
}

func swapCanonicalizer(can Canonicalization, c canonicalizer) func() {
	orig := canonicalizers[can]
	canonicalizers[can] = c
	return func() { canonicalizers[can] = orig }
}

// A failing crypto.Signer must not prevent the other signatures from being
// generated, and the failure must be identifiable in the results.
func TestSignMultiple_signerFails(t *testing.T) {
	optionsList := []*SignOptions{
		{Domain: "example.org", Selector: "brisbane", Signer: failingSigner{testPrivateKey.Public()}},
		{Domain: "football.example.com", Selector: "test", Signer: testPrivateKey},
	}

	results, err := SignMultiple(strings.NewReader(mailString), optionsList)
	if err != nil {
		t.Fatal("Expected no error while signing mail, got:", err)
	}
	if len(results) != 2 {
		t.Fatalf("Expected 2 results, got %v", len(results))
	}

	if res := results[0]; res.Err == nil {
		t.Error("Expected an error for the failing signer, got nil")
	} else if res.Signature != "" {
		t.Error("Expected no signature for the failing signer")
	}

	res := results[1]
	if res.Err != nil {
		t.Fatal("Expected no error for the working signer, got:", res.Err)
	}

	verifications, err := Verify(strings.NewReader(res.Signature + mailString))
	if err != nil {
		t.Fatalf("Expected no error while verifying signature, got: %v", err)
	}
	if len(verifications) != 1 {
		t.Fatalf("Expected exactly one verification, got %v", len(verifications))
	}
	if v := verifications[0]; v.Err != nil {
		t.Errorf("Expected no error when verifying signature, got: %v", v.Err)
	} else if v.Domain != optionsList[1].Domain {
		t.Errorf("Expected domain to be %q but got %q", optionsList[1].Domain, v.Domain)
	}
}

type failingSigner struct {
	pub crypto.PublicKey
}

func (s failingSigner) Public() crypto.PublicKey {
	return s.pub
}

func (s failingSigner) Sign(rand io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	return nil, errors.New("signer exploded")
}

// When the source reader fails or the caller cancels, SignMultiple must
// return immediately. Since SignMultiple is fully synchronous, no internal
// pipeline is left behind.
func TestSignMultiple_readerFails(t *testing.T) {
	optionsList := []*SignOptions{
		{Domain: "example.org", Selector: "brisbane", Signer: testPrivateKey},
		{Domain: "football.example.com", Selector: "test", Signer: testPrivateKey},
	}

	for _, test := range []struct {
		name string
		err  error
	}{
		{"source fails", errors.New("source exploded")},
		{"caller cancels", context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Fail in the middle of the body
			r := &failingReader{
				r:   strings.NewReader(mailString),
				n:   len(mailHeaderString) + 2 + 16,
				err: test.err,
			}
			_, err := SignMultiple(r, optionsList)
			if !errors.Is(err, test.err) {
				t.Fatalf("Expected error to be %v, got: %v", test.err, err)
			}
		})
	}
}

type failingReader struct {
	r   io.Reader
	n   int // number of bytes to serve before failing
	err error
}

func (fr *failingReader) Read(b []byte) (int, error) {
	if fr.n <= 0 {
		return 0, fr.err
	}
	if len(b) > fr.n {
		b = b[:fr.n]
	}
	n, err := fr.r.Read(b)
	fr.n -= n
	return n, err
}

// Memory usage must not grow with the message size.
func TestSignMultiple_constantMemory(t *testing.T) {
	optionsList := []*SignOptions{
		{Domain: "example.org", Selector: "brisbane", Signer: testPrivateKey},
		{
			Domain:               "football.example.com",
			Selector:             "test",
			Signer:               testPrivateKey,
			BodyCanonicalization: CanonicalizationRelaxed,
		},
	}

	const bodySize = 32 << 20 // 32 MiB attachment
	mr := &memCheckReader{}
	r := io.MultiReader(
		strings.NewReader(mailHeaderString+"\r\n"),
		io.LimitReader(mr, bodySize),
	)

	// Keep the GC aggressive so that temporary streaming garbage doesn't
	// hide the resident memory growth we're checking for
	oldGC := debug.SetGCPercent(10)
	defer debug.SetGCPercent(oldGC)

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	mr.base = before.HeapAlloc

	results, err := SignMultiple(r, optionsList)
	if err != nil {
		t.Fatal("Expected no error while signing mail, got:", err)
	}
	for i, res := range results {
		if res.Err != nil {
			t.Fatalf("Expected no error for signature %v, got: %v", i, res.Err)
		}
	}

	if peak := mr.peak - mr.base; peak > bodySize/8 {
		t.Errorf("Expected memory usage not to grow with the message size, peak heap grew by %v bytes for a %v bytes body", peak, bodySize)
	}
}

// memCheckReader serves zeroes and tracks the peak heap size observed while
// the message is being streamed.
type memCheckReader struct {
	base uint64
	peak uint64
}

func (mr *memCheckReader) Read(b []byte) (int, error) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	if m.HeapAlloc > mr.peak {
		mr.peak = m.HeapAlloc
	}
	return len(b), nil
}

func TestSignMultiple_invalidOptions(t *testing.T) {
	if _, err := SignMultiple(strings.NewReader(mailString), nil); err == nil {
		t.Error("Expected an error when signing without options")
	}

	optionsList := []*SignOptions{
		{Domain: "example.org", Selector: "brisbane", Signer: testPrivateKey},
		{Domain: "example.org", Signer: testPrivateKey}, // no selector
	}
	if _, err := SignMultiple(strings.NewReader(mailString), optionsList); err == nil {
		t.Error("Expected an error when signing with invalid options")
	}
}
