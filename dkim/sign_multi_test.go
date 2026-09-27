package dkim

import (
	"crypto"
	"errors"
	"io"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testMultiSignOptions() []*SignOptions {
	return []*SignOptions{
		{
			Domain:   "example.org",
			Selector: "brisbane",
			Signer:   testPrivateKey,
		},
		{
			Domain:               "example.org",
			Selector:             "test2026",
			Signer:               testPrivateKey,
			BodyCanonicalization: CanonicalizationRelaxed,
			HeaderKeys:           []string{"From", "To", "Subject"},
		},
	}
}

func TestSignMultiple(t *testing.T) {
	options := testMultiSignOptions()

	results, err := SignMultiple(strings.NewReader(mailString), options)
	if err != nil {
		t.Fatal("Expected no error while signing mail, got:", err)
	}
	if len(results) != len(options) {
		t.Fatalf("Expected %v results, got %v", len(options), len(results))
	}

	var signed strings.Builder
	for i, res := range results {
		if res.Err != nil {
			t.Fatalf("Expected no error for signature %v, got: %v", i, res.Err)
		}
		if !strings.HasPrefix(res.Signature, "DKIM-Signature:") {
			t.Errorf("Expected signature %v to be a DKIM-Signature header field, got: %q", i, res.Signature)
		}
		signed.WriteString(res.Signature)
	}
	signed.WriteString(mailString)

	verifications, err := Verify(strings.NewReader(signed.String()))
	if err != nil {
		t.Fatalf("Expected no error while verifying signatures, got: %v", err)
	}
	if len(verifications) != len(options) {
		t.Fatalf("Expected %v verifications, got %v", len(options), len(verifications))
	}
	for i, v := range verifications {
		if v.Err != nil {
			t.Errorf("Expected no error when verifying signature %v, got: %v", i, v.Err)
		}
		if v.Domain != options[i].Domain {
			t.Errorf("Expected domain to be %q but got %q", options[i].Domain, v.Domain)
		}
	}

	// The second signature uses a restricted header field list
	wantHeaderKeys := []string{"From", "To", "Subject"}
	if !reflect.DeepEqual(verifications[1].HeaderKeys, wantHeaderKeys) {
		t.Errorf("Expected header keys to be %v but got %v", wantHeaderKeys, verifications[1].HeaderKeys)
	}
}

// failingSigner is a crypto.Signer which always fails to sign.
type failingSigner struct {
	crypto.Signer
}

func (s failingSigner) Sign(rand io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	return nil, errors.New("test error")
}

func TestSignMultiple_signerFails(t *testing.T) {
	options := []*SignOptions{
		{
			Domain:   "example.org",
			Selector: "broken",
			Signer:   failingSigner{testPrivateKey},
		},
		{
			Domain:   "example.org",
			Selector: "brisbane",
			Signer:   testPrivateKey,
		},
	}

	results, err := SignMultiple(strings.NewReader(mailString), options)
	if err != nil {
		t.Fatal("Expected no error while signing mail, got:", err)
	}
	if len(results) != len(options) {
		t.Fatalf("Expected %v results, got %v", len(options), len(results))
	}

	if results[0].Err == nil {
		t.Error("Expected an error for the failing signature")
	}
	if results[0].Signature != "" {
		t.Error("Expected an empty signature for the failing signer")
	}

	// The other signature must still be generated and verifiable
	if results[1].Err != nil {
		t.Fatalf("Expected no error for the second signature, got: %v", results[1].Err)
	}
	signed := results[1].Signature + mailString
	verifications, err := Verify(strings.NewReader(signed))
	if err != nil {
		t.Fatalf("Expected no error while verifying signature, got: %v", err)
	}
	if len(verifications) != 1 {
		t.Fatalf("Expected exactly one verification, got %v", len(verifications))
	}
	if err := verifications[0].Err; err != nil {
		t.Errorf("Expected no error when verifying signature, got: %v", err)
	}
}

func TestSignMultiple_invalidOptions(t *testing.T) {
	if _, err := SignMultiple(strings.NewReader(mailString), nil); err == nil {
		t.Error("Expected an error when signing without options")
	}

	options := testMultiSignOptions()
	options[1] = &SignOptions{}
	if _, err := SignMultiple(strings.NewReader(mailString), options); err == nil {
		t.Error("Expected an error when signing with invalid options")
	}
}

func TestMultiSigner_cancel(t *testing.T) {
	s, err := NewMultiSigner(testMultiSignOptions())
	if err != nil {
		t.Fatal("Expected no error while creating multi-signer, got:", err)
	}

	// Write a partial message, then abort before completing it
	if _, err := s.Write([]byte("From: Joe SixPack <joe@football.example.com>\r\n")); err != nil {
		t.Fatal("Expected no error while writing partial message, got:", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- s.Close()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Expected Close to return after cancelling")
	}
}

// brokenReader is an io.Reader which always fails.
type brokenReader struct {
	err error
}

func (r brokenReader) Read([]byte) (int, error) {
	return 0, r.err
}

func TestSignMultiple_readerFails(t *testing.T) {
	testErr := errors.New("test read error")
	r := io.MultiReader(strings.NewReader(mailString[:len(mailString)/2]), brokenReader{testErr})

	before := runtime.NumGoroutine()

	if _, err := SignMultiple(r, testMultiSignOptions()); !errors.Is(err, testErr) {
		t.Fatalf("Expected read error, got: %v", err)
	}

	// The internal pipeline must have terminated
	for i := 0; i < 100; i++ {
		if runtime.NumGoroutine() <= before {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("Expected no goroutine leak after a read error")
}

// countingCanonicalizer counts how many times a body canonicalizer is
// instantiated.
type countingCanonicalizer struct {
	canonicalizer
	bodies int
}

func (c *countingCanonicalizer) CanonicalizeBody(w io.Writer) io.WriteCloser {
	c.bodies++
	return c.canonicalizer.CanonicalizeBody(w)
}

func TestSignMultiple_sharedCanonicalization(t *testing.T) {
	simple := &countingCanonicalizer{canonicalizer: canonicalizers[CanonicalizationSimple]}
	relaxed := &countingCanonicalizer{canonicalizer: canonicalizers[CanonicalizationRelaxed]}
	canonicalizers[CanonicalizationSimple] = simple
	canonicalizers[CanonicalizationRelaxed] = relaxed
	defer func() {
		canonicalizers[CanonicalizationSimple] = simple.canonicalizer
		canonicalizers[CanonicalizationRelaxed] = relaxed.canonicalizer
	}()

	// Two signatures use simple, one uses relaxed
	options := append(testMultiSignOptions(), &SignOptions{
		Domain:   "example.org",
		Selector: "test2027",
		Signer:   testPrivateKey,
	})

	if _, err := SignMultiple(strings.NewReader(mailString), options); err != nil {
		t.Fatal("Expected no error while signing mail, got:", err)
	}

	if simple.bodies != 1 {
		t.Errorf("Expected simple body canonicalizer to be instantiated once, got %v", simple.bodies)
	}
	if relaxed.bodies != 1 {
		t.Errorf("Expected relaxed body canonicalizer to be instantiated once, got %v", relaxed.bodies)
	}
}

// fillReader is an infinite io.Reader which fills buffers with a fixed byte.
type fillReader struct {
	b byte
}

func (r fillReader) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = r.b
	}
	return len(b), nil
}

func TestSignMultiple_memory(t *testing.T) {
	options := testMultiSignOptions()

	// Warm up
	if _, err := SignMultiple(strings.NewReader(mailString), options); err != nil {
		t.Fatal("Expected no error while signing mail, got:", err)
	}

	const bodySize = 16 << 20
	r := io.MultiReader(
		strings.NewReader(mailHeaderString+"\r\n"),
		io.LimitReader(fillReader{b: 'x'}, bodySize),
	)

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	if _, err := SignMultiple(r, options); err != nil {
		t.Fatal("Expected no error while signing mail, got:", err)
	}

	runtime.GC()
	runtime.ReadMemStats(&after)

	if grown := int64(after.HeapAlloc) - int64(before.HeapAlloc); grown > bodySize/8 {
		t.Errorf("Expected memory usage not to grow with the body size, grew by %v bytes for a %v bytes body", grown, bodySize)
	}
}
