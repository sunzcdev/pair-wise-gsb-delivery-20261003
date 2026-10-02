package dkim

import (
	"bytes"
	"errors"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"
)

func newMailStringReader(s string) io.Reader {
	return strings.NewReader(strings.Replace(s, "\n", "\r\n", -1))
}

const unsignedMailString = `From: Joe SixPack <joe@football.example.com>
To: Suzie Q <suzie@shopping.example.net>
Subject: Is dinner ready?
Date: Fri, 11 Jul 2003 21:00:37 -0700 (PDT)
Message-ID: <20030712040037.46341.5F8J@football.example.com>

Hi.

We lost the game. Are you hungry yet?

Joe.
`

func TestVerify_unsigned(t *testing.T) {
	r := newMailStringReader(unsignedMailString)

	verifications, err := Verify(r)
	if err != nil {
		t.Fatalf("Expected no error while verifying signature, got: %v", err)
	} else if len(verifications) != 0 {
		t.Fatalf("Expected exactly zero verification, got %v", len(verifications))
	}
}

const verifiedMailString = `DKIM-Signature: v=1; a=rsa-sha256; s=brisbane; d=example.com;
      c=simple/simple; q=dns/txt; i=joe@football.example.com;
      h=Received : From : To : Subject : Date : Message-ID;
      bh=2jUSOH9NhtVGCQWNr9BrIAPreKQjO6Sn7XIkfJVOzv8=;
      b=AuUoFEfDxTDkHlLXSZEpZj79LICEps6eda7W3deTVFOk4yAUoqOB
      4nujc7YopdG5dWLSdNg6xNAZpOPr+kHxt1IrE+NahM6L/LbvaHut
      KVdkLLkpVaVVQPzeRDI009SO2Il5Lu7rDNH6mZckBdrIx0orEtZV
      4bmp/YzhwvcubU4=;
Received: from client1.football.example.com  [192.0.2.1]
      by submitserver.example.com with SUBMISSION;
      Fri, 11 Jul 2003 21:01:54 -0700 (PDT)
From: Joe SixPack <joe@football.example.com>
To: Suzie Q <suzie@shopping.example.net>
Subject: Is dinner ready?
Date: Fri, 11 Jul 2003 21:00:37 -0700 (PDT)
Message-ID: <20030712040037.46341.5F8J@football.example.com>

Hi.

We lost the game. Are you hungry yet?

Joe.
`

var testVerification = &Verification{
	Domain:     "example.com",
	Identifier: "joe@football.example.com",
	HeaderKeys: []string{"Received", "From", "To", "Subject", "Date", "Message-ID"},
	HeaderCoverage: []HeaderCoverage{
		{Key: "Received", Index: 1},
		{Key: "From", Index: 2},
		{Key: "To", Index: 3},
		{Key: "Subject", Index: 4},
		{Key: "Date", Index: 5},
		{Key: "Message-ID", Index: 6},
	},
}

func TestVerify(t *testing.T) {
	r := newMailStringReader(verifiedMailString)

	verifications, err := Verify(r)
	if err != nil {
		t.Fatalf("Expected no error while verifying signature, got: %v", err)
	} else if len(verifications) != 1 {
		t.Fatalf("Expected exactly one verification, got %v", len(verifications))
	}

	v := verifications[0]
	if !reflect.DeepEqual(testVerification, v) {
		t.Errorf("Expected verification to be \n%+v\n but got \n%+v", testVerification, v)
	}
}

func TestVerifyWithOption(t *testing.T) {
	r := newMailStringReader(verifiedMailString)
	option := VerifyOptions{}
	verifications, err := VerifyWithOptions(r, &option)
	if err != nil {
		t.Fatalf("Expected no error while verifying signature, got: %v", err)
	} else if len(verifications) != 1 {
		t.Fatalf("Expected exactly one verification, got %v", len(verifications))
	}

	v := verifications[0]
	if !reflect.DeepEqual(testVerification, v) {
		t.Errorf("Expected verification to be \n%+v\n but got \n%+v", testVerification, v)
	}

	r = newMailStringReader(verifiedMailString)
	option = VerifyOptions{LookupTXT: net.LookupTXT}
	verifications, err = VerifyWithOptions(r, &option)
	if err != nil {
		t.Fatalf("Expected no error while verifying signature, got: %v", err)
	} else if len(verifications) != 1 {
		t.Fatalf("Expected exactly one verification, got %v", len(verifications))
	}

	v = verifications[0]
	if !reflect.DeepEqual(testVerification, v) {
		t.Errorf("Expected verification to be \n%+v\n but got \n%+v", testVerification, v)
	}
}

const verifiedRawRSAMailString = `DKIM-Signature: a=rsa-sha256; bh=2jUSOH9NhtVGCQWNr9BrIAPreKQjO6Sn7XIkfJVOzv8=;
 c=simple/simple; d=example.com;
 h=Received:From:To:Subject:Date:Message-ID; i=joe@football.example.com;
 s=newengland; t=1615825284; v=1;
 b=Xh4Ujb2wv5x54gXtulCiy4C0e+plRm6pZ4owF+kICpYzs/8WkTVIDBrzhJP0DAYCpnL62T0G
 k+0OH8pi/yqETVjKtKk+peMnNvKkut0GeWZMTze0bfq3/JUK3Ln3jTzzpXxrgVnvBxeY9EZIL4g
 s4wwFRRKz/1bksZGSjD8uuSU=
Received: from client1.football.example.com  [192.0.2.1]
      by submitserver.example.com with SUBMISSION;
      Fri, 11 Jul 2003 21:01:54 -0700 (PDT)
From: Joe SixPack <joe@football.example.com>
To: Suzie Q <suzie@shopping.example.net>
Subject: Is dinner ready?
Date: Fri, 11 Jul 2003 21:00:37 -0700 (PDT)
Message-ID: <20030712040037.46341.5F8J@football.example.com>

Hi.

We lost the game. Are you hungry yet?

Joe.
`

var testRawRSAVerification = &Verification{
	Domain:     "example.com",
	Identifier: "joe@football.example.com",
	HeaderKeys: []string{"Received", "From", "To", "Subject", "Date", "Message-ID"},
	HeaderCoverage: []HeaderCoverage{
		{Key: "Received", Index: 1},
		{Key: "From", Index: 2},
		{Key: "To", Index: 3},
		{Key: "Subject", Index: 4},
		{Key: "Date", Index: 5},
		{Key: "Message-ID", Index: 6},
	},
	Time: time.Unix(1615825284, 0),
}

func TestVerify_rawRSA(t *testing.T) {
	r := newMailStringReader(verifiedRawRSAMailString)

	verifications, err := Verify(r)
	if err != nil {
		t.Fatalf("Expected no error while verifying signature, got: %v", err)
	} else if len(verifications) != 1 {
		t.Fatalf("Expected exactly one verification, got %v", len(verifications))
	}

	v := verifications[0]
	if !reflect.DeepEqual(testRawRSAVerification, v) {
		t.Errorf("Expected verification to be \n%+v\n but got \n%+v", testRawRSAVerification, v)
	}
}

const verifiedEd25519MailString = `DKIM-Signature: v=1; a=ed25519-sha256; c=relaxed/relaxed;
 d=football.example.com; i=@football.example.com;
 q=dns/txt; s=brisbane; t=1528637909; h=from : to :
 subject : date : message-id : from : subject : date;
 bh=2jUSOH9NhtVGCQWNr9BrIAPreKQjO6Sn7XIkfJVOzv8=;
 b=/gCrinpcQOoIfuHNQIbq4pgh9kyIK3AQUdt9OdqQehSwhEIug4D11Bus
 Fa3bT3FY5OsU7ZbnKELq+eXdp1Q1Dw==
DKIM-Signature: v=1; a=rsa-sha256; c=relaxed/relaxed;
 d=football.example.com; i=@football.example.com;
 q=dns/txt; s=test; t=1528637909; h=from : to : subject :
 date : message-id : from : subject : date;
 bh=2jUSOH9NhtVGCQWNr9BrIAPreKQjO6Sn7XIkfJVOzv8=;
 b=F45dVWDfMbQDGHJFlXUNB2HKfbCeLRyhDXgFpEL8GwpsRe0IeIixNTe3
 DhCVlUrSjV4BwcVcOF6+FF3Zo9Rpo1tFOeS9mPYQTnGdaSGsgeefOsk2Jz
 dA+L10TeYt9BgDfQNZtKdN1WO//KgIqXP7OdEFE4LjFYNcUxZQ4FADY+8=
From: Joe SixPack <joe@football.example.com>
To: Suzie Q <suzie@shopping.example.net>
Subject: Is dinner ready?
Date: Fri, 11 Jul 2003 21:00:37 -0700 (PDT)
Message-ID: <20030712040037.46341.5F8J@football.example.com>

Hi.

We lost the game.  Are you hungry yet?

Joe.`

var testEd25519Verification = &Verification{
	Domain:     "football.example.com",
	Identifier: "@football.example.com",
	HeaderKeys: []string{"from", "to", "subject", "date", "message-id", "from", "subject", "date"},
	HeaderCoverage: []HeaderCoverage{
		{Key: "from", Index: 2},
		{Key: "to", Index: 3},
		{Key: "subject", Index: 4},
		{Key: "date", Index: 5},
		{Key: "message-id", Index: 6},
		{Key: "from", Index: -1},
		{Key: "subject", Index: -1},
		{Key: "date", Index: -1},
	},
	Time: time.Unix(1528637909, 0),
}

func TestVerify_ed25519(t *testing.T) {
	r := newMailStringReader(verifiedEd25519MailString)

	verifications, err := Verify(r)
	if err != nil {
		t.Fatalf("Expected no error while verifying signature, got: %v", err)
	} else if len(verifications) != 2 {
		t.Fatalf("Expected exactly two verifications, got %v", len(verifications))
	}

	v := verifications[0]
	if !reflect.DeepEqual(testEd25519Verification, v) {
		t.Errorf("Expected verification to be \n%+v\n but got \n%+v", testEd25519Verification, v)
	}
}

// errorReader reads from r and then returns an arbitrary error.
type errorReader struct {
	r   io.Reader
	err error
}

func (r *errorReader) Read(b []byte) (int, error) {
	n, err := r.r.Read(b)
	if err == io.EOF {
		return n, r.err
	}
	return n, err
}

func TestVerify_invalid(t *testing.T) {
	r := newMailStringReader("asdf")
	_, err := Verify(r)
	if err == nil {
		t.Fatalf("Expected error while verifying signature, got nil")
	}

	expectedErr := errors.New("expected test error")

	r = &errorReader{
		r:   newMailStringReader(verifiedEd25519MailString),
		err: expectedErr,
	}
	_, err = Verify(r)
	if err != expectedErr {
		t.Fatalf("Expected error while verifying signature, got: %v", err)
	}
}

const tooManySignaturesMailString = `DKIM-Signature: v=1; a=rsa-sha256; s=brisbane; d=example.com;
      c=simple/simple; q=dns/txt; i=joe@football.example.com;
      h=Received : From : To : Subject : Date : Message-ID;
      bh=2jUSOH9NhtVGCQWNr9BrIAPreKQjO6Sn7XIkfJVOzv8=;
      b=AuUoFEfDxTDkHlLXSZEpZj79LICEps6eda7W3deTVFOk4yAUoqOB
      4nujc7YopdG5dWLSdNg6xNAZpOPr+kHxt1IrE+NahM6L/LbvaHut
      KVdkLLkpVaVVQPzeRDI009SO2Il5Lu7rDNH6mZckBdrIx0orEtZV
      4bmp/YzhwvcubU4=;
DKIM-Signature: v=1; a=rsa-sha256; s=brisbane; d=example.com;
      c=simple/simple; q=dns/txt; i=joe@football.example.com;
      h=Received : From : To : Subject : Date : Message-ID;
      bh=2jUSOH9NhtVGCQWNr9BrIAPreKQjO6Sn7XIkfJVOzv8=;
      b=AuUoFEfDxTDkHlLXSZEpZj79LICEps6eda7W3deTVFOk4yAUoqOB
      4nujc7YopdG5dWLSdNg6xNAZpOPr+kHxt1IrE+NahM6L/LbvaHut
      KVdkLLkpVaVVQPzeRDI009SO2Il5Lu7rDNH6mZckBdrIx0orEtZV
      4bmp/YzhwvcubU4=;
DKIM-Signature: v=1; a=rsa-sha256; s=brisbane; d=example.com;
      c=simple/simple; q=dns/txt; i=joe@football.example.com;
      h=Received : From : To : Subject : Date : Message-ID;
      bh=2jUSOH9NhtVGCQWNr9BrIAPreKQjO6Sn7XIkfJVOzv8=;
      b=AuUoFEfDxTDkHlLXSZEpZj79LICEps6eda7W3deTVFOk4yAUoqOB
      4nujc7YopdG5dWLSdNg6xNAZpOPr+kHxt1IrE+NahM6L/LbvaHut
      KVdkLLkpVaVVQPzeRDI009SO2Il5Lu7rDNH6mZckBdrIx0orEtZV
      4bmp/YzhwvcubU4=;
Received: from client1.football.example.com  [192.0.2.1]
      by submitserver.example.com with SUBMISSION;
      Fri, 11 Jul 2003 21:01:54 -0700 (PDT)
From: Joe SixPack <joe@football.example.com>
To: Suzie Q <suzie@shopping.example.net>
Subject: Is dinner ready?
Date: Fri, 11 Jul 2003 21:00:37 -0700 (PDT)
Message-ID: <20030712040037.46341.5F8J@football.example.com>

Hi.

We lost the game. Are you hungry yet?

Joe.
`

func TestVerify_tooManySignatures(t *testing.T) {
	r := strings.NewReader(tooManySignaturesMailString)
	options := VerifyOptions{MaxVerifications: 2}
	verifs, err := VerifyWithOptions(r, &options)
	if err != ErrTooManySignatures {
		t.Fatalf("Expected ErrTooManySignatures, got %v", err)
	}
	if len(verifs) != options.MaxVerifications {
		t.Fatalf("Expected %v verifications, got %v", options.MaxVerifications, len(verifs))
	}
}

const duplicateHeaderMailString = "From: Joe SixPack <joe@football.example.com>\r\n" +
	"To: Suzie Q <suzie@shopping.example.net>\r\n" +
	"Subject: Is dinner ready?\r\n" +
	"X-Test: one\r\n" +
	"X-Test: two\r\n" +
	"Message-ID: <20030712040037.46341.5F8J@football.example.com>\r\n" +
	"\r\n" +
	mailBodyString

func signDuplicateHeaderMail(t *testing.T, headerKeys []string) string {
	t.Helper()

	options := &SignOptions{
		Domain:     "example.org",
		Selector:   "brisbane",
		Signer:     testPrivateKey,
		HeaderKeys: headerKeys,
	}

	var b bytes.Buffer
	if err := Sign(&b, strings.NewReader(duplicateHeaderMailString), options); err != nil {
		t.Fatal("Expected no error while signing mail, got:", err)
	}
	return b.String()
}

func verifyOne(t *testing.T, mail string, options *VerifyOptions) *Verification {
	t.Helper()

	verifications, err := VerifyWithOptions(strings.NewReader(mail), options)
	if err != nil {
		t.Fatalf("Expected no error while verifying signature, got: %v", err)
	}
	if len(verifications) != 1 {
		t.Fatalf("Expected exactly one verification, got %v", len(verifications))
	}
	return verifications[0]
}

// TestVerify_headerCoverage signs a message containing duplicate header
// fields, then inserts a header field with the same name at various
// positions and compares the verification and coverage results.
func TestVerify_headerCoverage(t *testing.T) {
	headerKeys := []string{"From", "To", "Subject", "X-Test", "X-Test"}
	signed := signDuplicateHeaderMail(t, headerKeys)

	wantCoverage := []HeaderCoverage{
		{Key: "From", Index: 1},
		{Key: "To", Index: 2},
		{Key: "Subject", Index: 3},
		{Key: "X-Test", Index: 5},
		{Key: "X-Test", Index: 4},
	}

	// The untouched message verifies, and each h= entry covers an existing
	// header field instance.
	v := verifyOne(t, signed, nil)
	if v.Err != nil {
		t.Fatalf("Expected no error when verifying signature, got: %v", v.Err)
	}
	if !reflect.DeepEqual(wantCoverage, v.HeaderCoverage) {
		t.Errorf("Expected header coverage to be \n%+v\n but got \n%+v", wantCoverage, v.HeaderCoverage)
	}

	// Prepending a header field with a signed name doesn't break the
	// signature: the signature keeps covering the two bottom-most
	// instances, and the injected instance is left uncovered. The
	// cryptographic verification passes, but the application policy
	// rejects the message: both conclusions are retained.
	injectedTop := strings.Replace(signed, "\r\nFrom:", "\r\nX-Test: injected\r\nFrom:", 1)
	options := &VerifyOptions{HeaderPolicy: RequireSignedHeaders("From", "Subject", "X-Test")}
	v = verifyOne(t, injectedTop, options)
	if v.Err != nil {
		t.Errorf("Expected no error when verifying signature with prepended header, got: %v", v.Err)
	}
	if v.PolicyErr == nil {
		t.Errorf("Expected policy error for signature with prepended header, got nil")
	}
	wantCoverage = []HeaderCoverage{
		{Key: "From", Index: 2},
		{Key: "To", Index: 3},
		{Key: "Subject", Index: 4},
		{Key: "X-Test", Index: 6},
		{Key: "X-Test", Index: 5},
	}
	if !reflect.DeepEqual(wantCoverage, v.HeaderCoverage) {
		t.Errorf("Expected header coverage to be \n%+v\n but got \n%+v", wantCoverage, v.HeaderCoverage)
	}

	// Appending a header field with a signed name breaks the signature,
	// because the newly added instance is covered instead of the original
	// top-most one.
	injectedBottom := strings.Replace(signed, "\r\n\r\n", "\r\nX-Test: injected\r\n\r\n", 1)
	v = verifyOne(t, injectedBottom, options)
	if v.Err == nil {
		t.Errorf("Expected error when verifying signature with appended header, got nil")
	}
	wantCoverage = []HeaderCoverage{
		{Key: "From", Index: 1},
		{Key: "To", Index: 2},
		{Key: "Subject", Index: 3},
		{Key: "X-Test", Index: 7},
		{Key: "X-Test", Index: 5},
	}
	if !reflect.DeepEqual(wantCoverage, v.HeaderCoverage) {
		t.Errorf("Expected header coverage to be \n%+v\n but got \n%+v", wantCoverage, v.HeaderCoverage)
	}
}

// TestVerify_headerCoverageOversigned checks that oversigned header fields
// are reported as covering absent header field instances, and that adding
// an instance afterwards breaks the signature.
func TestVerify_headerCoverageOversigned(t *testing.T) {
	headerKeys := []string{"From", "To", "Subject", "X-Test", "X-Test", "X-Test"}
	signed := signDuplicateHeaderMail(t, headerKeys)

	// The third X-Test entry covers no existing instance: it's an
	// oversigning slot preventing a X-Test header field from being added.
	v := verifyOne(t, signed, nil)
	if v.Err != nil {
		t.Fatalf("Expected no error when verifying signature, got: %v", v.Err)
	}
	wantCoverage := []HeaderCoverage{
		{Key: "From", Index: 1},
		{Key: "To", Index: 2},
		{Key: "Subject", Index: 3},
		{Key: "X-Test", Index: 5},
		{Key: "X-Test", Index: 4},
		{Key: "X-Test", Index: -1},
	}
	if !reflect.DeepEqual(wantCoverage, v.HeaderCoverage) {
		t.Errorf("Expected header coverage to be \n%+v\n but got \n%+v", wantCoverage, v.HeaderCoverage)
	}
	if v.HeaderCoverage[5].Present() {
		t.Errorf("Expected oversigning slot to cover no existing header field")
	}

	// Adding a X-Test header field anywhere fills the oversigning slot and
	// breaks the signature.
	injectedTop := strings.Replace(signed, "\r\nFrom:", "\r\nX-Test: injected\r\nFrom:", 1)
	v = verifyOne(t, injectedTop, nil)
	if v.Err == nil {
		t.Errorf("Expected error when verifying oversigned signature with prepended header, got nil")
	}
	wantCoverage = []HeaderCoverage{
		{Key: "From", Index: 2},
		{Key: "To", Index: 3},
		{Key: "Subject", Index: 4},
		{Key: "X-Test", Index: 6},
		{Key: "X-Test", Index: 5},
		{Key: "X-Test", Index: 1},
	}
	if !reflect.DeepEqual(wantCoverage, v.HeaderCoverage) {
		t.Errorf("Expected header coverage to be \n%+v\n but got \n%+v", wantCoverage, v.HeaderCoverage)
	}
}
