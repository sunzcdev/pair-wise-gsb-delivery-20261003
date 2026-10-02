package dkim

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

const duplicateHeaderMailString = "Received: from hop1.example.com by relay.example.com\r\n" +
	"Received: from client.example.com by hop1.example.com\r\n" +
	"From: Joe SixPack <joe@football.example.com>\r\n" +
	"To: Suzie Q <suzie@shopping.example.net>\r\n" +
	"Subject: Is dinner ready?\r\n" +
	"Date: Fri, 11 Jul 2003 21:00:37 -0700 (PDT)\r\n" +
	"Message-ID: <20030712040037.46341.5F8J@football.example.com>\r\n" +
	"\r\n" +
	mailBodyString

// signDuplicateMail signs the duplicate-header message. When oversign is true,
// an extra "Received" reference is added to h= to reserve an oversigning slot.
func signDuplicateMail(t *testing.T, oversign bool) string {
	t.Helper()

	keys := []string{"From", "To", "Subject", "Date", "Message-ID"}
	receivedRefs := 2
	if oversign {
		receivedRefs = 3
	}
	headerKeys := make([]string, 0, receivedRefs+len(keys))
	for i := 0; i < receivedRefs; i++ {
		headerKeys = append(headerKeys, "Received")
	}
	headerKeys = append(headerKeys, keys...)

	options := &SignOptions{
		Domain:     "example.org",
		Selector:   "brisbane",
		Signer:     testPrivateKey,
		HeaderKeys: headerKeys,
	}

	var b bytes.Buffer
	if err := Sign(&b, strings.NewReader(duplicateHeaderMailString), options); err != nil {
		t.Fatalf("Expected no error while signing mail, got: %v", err)
	}
	return b.String()
}

// insertHeader inserts a new "Received" header immediately before the header
// whose raw text starts with marker (matched at a line boundary).
func insertHeader(msg, marker, inserted string) string {
	idx := strings.Index(msg, "\r\n"+marker)
	if idx < 0 {
		panic("marker not found: " + marker)
	}
	idx += 2
	return msg[:idx] + inserted + msg[idx:]
}

func parseTestHeader(t *testing.T, msg string) header {
	t.Helper()
	h, err := readHeader(bufio.NewReader(strings.NewReader(msg)))
	if err != nil {
		t.Fatalf("failed to read header: %v", err)
	}
	return h
}

func indexesWithPrefix(h header, key, prefix string) []int {
	var indexes []int
	for i, kv := range h {
		k, v := parseHeaderField(kv)
		if strings.EqualFold(k, key) && strings.HasPrefix(v, prefix) {
			indexes = append(indexes, i)
		}
	}
	return indexes
}

func receivedCoverage(v *Verification) *HeaderFieldCoverage {
	return v.HeaderCoverage.Fields["received"]
}

var coveragePolicy = &Policy{Headers: map[string]HeaderRequirement{
	"from":     {},
	"subject":  {},
	"received": {},
}}

var oversignPolicy = &Policy{Headers: map[string]HeaderRequirement{
	"from":     {},
	"subject":  {},
	"received": {RequireOversign: true},
}}

// TestCoverage_duplicateHeaders signs a message with two same-named headers and
// checks that the coverage report points at the actual signed instances.
func TestCoverage_duplicateHeaders(t *testing.T) {
	msg := signDuplicateMail(t, false)

	verifications, err := VerifyWithOptions(strings.NewReader(msg), &VerifyOptions{Policy: coveragePolicy})
	if err != nil {
		t.Fatalf("Verify returned error: %v", err)
	}
	v := verifications[0]
	if v.Err != nil {
		t.Fatalf("Expected signature to verify, got: %v", v.Err)
	}

	h := parseTestHeader(t, msg)
	hop1 := indexesWithPrefix(h, "Received", "from hop1")
	hop2 := indexesWithPrefix(h, "Received", "from client")
	if len(hop1) != 1 || len(hop2) != 1 {
		t.Fatalf("Expected two Received instances, got %v and %v", hop1, hop2)
	}

	c := receivedCoverage(v)
	want := &HeaderFieldCoverage{
		Key:        "received",
		Instances:  concatInts(hop1, hop2),
		Signed:     concatInts(hop1, hop2),
		Unsigned:   nil,
		Oversigned: 0,
	}
	if !equalInts(c.Instances, want.Instances) || !equalInts(c.Signed, want.Signed) ||
		len(c.Unsigned) != 0 || c.Oversigned != 0 || !c.FullySigned() {
		t.Errorf("Unexpected Received coverage: %+v", c)
	}

	if len(v.HeaderCoverage.Picks) != len(v.HeaderKeys) {
		t.Errorf("Expected %d picks, got %d", len(v.HeaderKeys), len(v.HeaderCoverage.Picks))
	}
	if v.PolicyResult == nil || v.PolicyResult.Err != nil {
		t.Errorf("Expected policy to pass, got %+v", v.PolicyResult)
	}
}

// TestCoverage_insertedTopNoOversign: without oversigning, a header prepended
// above the signed ones is not covered, yet the signature still verifies. The
// cryptographic conclusion (pass) and the policy conclusion (fail) are both
// retained.
func TestCoverage_insertedTopNoOversign(t *testing.T) {
	msg := signDuplicateMail(t, false)
	msg = insertHeader(msg, "Received: from hop1", "Received: from attacker.example.net\r\n")

	verifications, err := VerifyWithOptions(strings.NewReader(msg), &VerifyOptions{Policy: coveragePolicy})
	if err != nil {
		t.Fatalf("Verify returned error: %v", err)
	}
	v := verifications[0]

	if v.Err != nil {
		t.Fatalf("Expected cryptographic verification to pass, got: %v", v.Err)
	}
	if v.PolicyResult == nil || v.PolicyResult.Err == nil {
		t.Fatalf("Expected policy failure, got %+v", v.PolicyResult)
	}

	h := parseTestHeader(t, msg)
	attacker := indexesWithPrefix(h, "Received", "from attacker")
	hop1 := indexesWithPrefix(h, "Received", "from hop1")
	hop2 := indexesWithPrefix(h, "Received", "from client")

	c := receivedCoverage(v)
	if len(attacker) != 1 || c.Unsigned[0] != attacker[0] {
		t.Errorf("Expected the prepended instance %v to be unsigned, got signed=%v unsigned=%v",
			attacker, c.Signed, c.Unsigned)
	}
	if !equalInts(c.Signed, concatInts(hop1, hop2)) {
		t.Errorf("Expected only the original instances to be signed, got %v (want %v)",
			c.Signed, concatInts(hop1, hop2))
	}

	problem := *v.PolicyResult.Headers["received"].Problem
	if problem != HeaderPartiallySigned {
		t.Errorf("Expected HeaderPartiallySigned, got %v", problem)
	}
}

// TestCoverage_insertedBottomNoOversign: a header appended below the signed
// ones becomes the bottom-most instance that "h=" now resolves to, displacing a
// signed instance. Cryptographic verification fails and the policy fails too.
func TestCoverage_insertedBottomNoOversign(t *testing.T) {
	msg := signDuplicateMail(t, false)
	msg = insertHeader(msg, "From:", "Received: from attacker.example.net\r\n")

	verifications, err := VerifyWithOptions(strings.NewReader(msg), &VerifyOptions{Policy: coveragePolicy})
	if err != nil {
		t.Fatalf("Verify returned error: %v", err)
	}
	v := verifications[0]

	if !isFail(v.Err) {
		t.Fatalf("Expected a cryptographic failure, got: %v", v.Err)
	}
	if v.PolicyResult == nil || v.PolicyResult.Err == nil {
		t.Fatalf("Expected policy failure to be retained alongside crypto failure")
	}

	h := parseTestHeader(t, msg)
	attacker := indexesWithPrefix(h, "Received", "from attacker")
	hop1 := indexesWithPrefix(h, "Received", "from hop1")
	hop2 := indexesWithPrefix(h, "Received", "from client")

	c := receivedCoverage(v)
	// The two "h=" references now bind to the injected (bottom-most) and the
	// original bottom instance; the top-most original instance is displaced.
	if !equalInts(c.Signed, concatInts(hop2, attacker)) || !equalInts(c.Unsigned, hop1) {
		t.Errorf("Unexpected coverage after bottom insertion: signed=%v unsigned=%v",
			c.Signed, c.Unsigned)
	}
}

// TestCoverage_oversigningSlots: an extra "h=" reference beyond the existing
// instances is reported as an oversigning slot (Index -1), not as a signed
// instance.
func TestCoverage_oversigningSlots(t *testing.T) {
	msg := signDuplicateMail(t, true)

	verifications, err := VerifyWithOptions(strings.NewReader(msg), &VerifyOptions{Policy: oversignPolicy})
	if err != nil {
		t.Fatalf("Verify returned error: %v", err)
	}
	v := verifications[0]
	if v.Err != nil {
		t.Fatalf("Expected signature to verify, got: %v", v.Err)
	}

	h := parseTestHeader(t, msg)
	hop1 := indexesWithPrefix(h, "Received", "from hop1")
	hop2 := indexesWithPrefix(h, "Received", "from client")

	c := receivedCoverage(v)
	if !equalInts(c.Signed, concatInts(hop1, hop2)) || c.Oversigned != 1 {
		t.Errorf("Expected 2 signed instances and 1 oversigning slot, got %+v", c)
	}

	var emptySlots []int
	for _, p := range v.HeaderCoverage.Picks {
		if p.Key == "received" && p.Index == -1 {
			emptySlots = append(emptySlots, p.Index)
		}
	}
	if len(emptySlots) != 1 {
		t.Errorf("Expected exactly one empty (oversigning) reference, got %d", len(emptySlots))
	}

	if v.PolicyResult.Err != nil {
		t.Errorf("Expected oversign policy to pass, got %v", v.PolicyResult.Err)
	}

	// Without the RequireOversign requirement the same signature passes, and a
	// policy requiring oversign fails on the plain (non-oversigned) signature.
	plain := firstVerification(t, signDuplicateMail(t, false), oversignPolicy)
	if plain.PolicyResult.Err == nil {
		t.Errorf("Expected RequireOversign policy to fail on the plain signature")
	} else if problem := *plain.PolicyResult.Headers["received"].Problem; problem != HeaderNotOversigned {
		t.Errorf("Expected HeaderNotOversigned, got %v", problem)
	}
}

// TestCoverage_oversignInsertionDetected: a header inserted into a range
// protected by an oversigning slot consumes the slot and breaks the signature,
// regardless of whether it is prepended or appended.
func TestCoverage_oversignInsertionDetected(t *testing.T) {
	for _, tc := range []struct {
		name   string
		marker string
	}{
		{"prepended", "Received: from hop1"},
		{"appended", "From:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := signDuplicateMail(t, true)
			msg = insertHeader(msg, tc.marker, "Received: from attacker.example.net\r\n")

			verifications, err := VerifyWithOptions(strings.NewReader(msg), &VerifyOptions{Policy: oversignPolicy})
			if err != nil {
				t.Fatalf("Verify returned error: %v", err)
			}
			v := verifications[0]
			if !isFail(v.Err) {
				t.Fatalf("Expected cryptographic failure for %s header, got: %v", tc.name, v.Err)
			}

			// The previously empty slot now resolves to the inserted instance,
			// so there is no oversigning slot left in the report.
			c := receivedCoverage(v)
			if c.Oversigned != 0 {
				t.Errorf("Expected the oversigning slot to be consumed, got %d slots", c.Oversigned)
			}
			if len(c.Unsigned) != 0 {
				t.Errorf("Expected all instances to be referenced, got unsigned=%v", c.Unsigned)
			}
		})
	}
}

// TestCoverage_missingRequiredHeader: a required header absent from the
// message is reported independently of the cryptographic result.
func TestCoverage_missingRequiredHeader(t *testing.T) {
	msg := signDuplicateMail(t, false)
	policy := &Policy{Headers: map[string]HeaderRequirement{
		"reply-to": {},
	}}

	v := firstVerification(t, msg, policy)
	if v.Err != nil {
		t.Fatalf("Expected signature to verify, got: %v", v.Err)
	}
	problem := *v.PolicyResult.Headers["reply-to"].Problem
	if problem != HeaderMissing {
		t.Errorf("Expected HeaderMissing, got %v", problem)
	}
}

func firstVerification(t *testing.T, msg string, policy *Policy) *Verification {
	t.Helper()
	verifications, err := VerifyWithOptions(strings.NewReader(msg), &VerifyOptions{Policy: policy})
	if err != nil {
		t.Fatalf("Verify returned error: %v", err)
	}
	return verifications[0]
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func concatInts(a, b []int) []int {
	out := make([]int, 0, len(a)+len(b))
	out = append(out, a...)
	out = append(out, b...)
	return out
}
