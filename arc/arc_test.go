package arc

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/emersion/go-msgauth/authres"
	"github.com/emersion/go-msgauth/dkim"
)

type arcKeys struct {
	source ed25519.PrivateKey
	relay1 ed25519.PrivateKey
	relay2 ed25519.PrivateKey
}

func newARCKeys(t *testing.T) arcKeys {
	t.Helper()
	_, source, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, relay1, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, relay2, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return arcKeys{source: source, relay1: relay1, relay2: relay2}
}

func ed25519TXT(key ed25519.PrivateKey) string {
	return "v=DKIM1; k=ed25519; p=" + base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
}

func lookupForKeys(keys arcKeys) func(string) ([]string, error) {
	records := map[string]string{
		"origin._domainkey.football.example.com": ed25519TXT(keys.source),
		"arc1._domainkey.relay1.example":         ed25519TXT(keys.relay1),
		"arc2._domainkey.relay2.example":         ed25519TXT(keys.relay2),
	}
	return func(domain string) ([]string, error) {
		if record, ok := records[domain]; ok {
			return []string{record}, nil
		}
		return nil, errors.New("no test DNS record")
	}
}

func trustedRelay(domain string) bool {
	return domain == "relay1.example" || domain == "relay2.example"
}

func sealMessage(t *testing.T, message []byte, options *SealOptions) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := Seal(&b, bytes.NewReader(message), options); err != nil {
		t.Fatalf("ARC seal i=%d failed: %v", options.Instance, err)
	}
	return b.Bytes()
}

func verifyMessage(t *testing.T, message []byte, lookup func(string) ([]string, error), trusted func(string) bool) *Verification {
	t.Helper()
	result, err := Verify(bytes.NewReader(message), &VerifyOptions{
		LookupTXT:       lookup,
		IsDomainTrusted: trusted,
	})
	if err != nil && result == nil {
		t.Fatalf("ARC verification failed before returning a result: %v", err)
	}
	return result
}

func buildTwoHopChain(t *testing.T) ([]byte, arcKeys) {
	t.Helper()
	keys := newARCKeys(t)
	lookup := lookupForKeys(keys)

	original := []byte("From: Joe <joe@football.example.com>\r\n" +
		"To: List <list@example.org>\r\n" +
		"Subject: News\r\n" +
		"\r\n" +
		"Original body\r\n")

	var signed bytes.Buffer
	err := dkim.Sign(&signed, bytes.NewReader(original), &dkim.SignOptions{
		Domain:                 "football.example.com",
		Selector:               "origin",
		Signer:                 keys.source,
		HeaderCanonicalization: dkim.CanonicalizationRelaxed,
		BodyCanonicalization:   dkim.CanonicalizationRelaxed,
	})
	if err != nil {
		t.Fatal(err)
	}

	dkimResults, err := dkim.VerifyWithOptions(bytes.NewReader(signed.Bytes()), &dkim.VerifyOptions{
		LookupTXT: lookup,
	})
	if err != nil || len(dkimResults) != 1 || dkimResults[0].Err != nil {
		t.Fatalf("first hop DKIM authentication failed: results=%v err=%v", dkimResults, err)
	}

	first := sealMessage(t, signed.Bytes(), &SealOptions{
		Domain:     "relay1.example",
		Selector:   "arc1",
		Signer:     keys.relay1,
		Instance:   1,
		Chain:      ChainNone,
		AuthServID: "relay1.example",
		Results: []authres.Result{
			&authres.DKIMResult{
				Value:  authres.ResultPass,
				Domain: "football.example.com",
			},
		},
	})
	beforeMutation := verifyMessage(t, first, lookup, trustedRelay)
	if beforeMutation.Status != ChainStatusValid {
		t.Fatalf("first hop chain status = %q, err = %v", beforeMutation.Status, beforeMutation.Err)
	}

	modified := bytes.Replace(first, []byte("Original body\r\n"), []byte("Mailing list body\r\n"), 1)
	second := sealMessage(t, modified, &SealOptions{
		Domain:     "relay2.example",
		Selector:   "arc2",
		Signer:     keys.relay2,
		Instance:   2,
		Chain:      ChainPass,
		AuthServID: "relay2.example",
		Results: []authres.Result{
			&authres.ARCResult{Value: authres.ResultPass, OldestPass: 1},
			&authres.DKIMResult{
				Value:  authres.ResultFail,
				Domain: "football.example.com",
			},
		},
	})
	return second, keys
}

func TestTwoHopSealAndVerify(t *testing.T) {
	message, keys := buildTwoHopChain(t)
	lookup := lookupForKeys(keys)

	result := verifyMessage(t, message, lookup, trustedRelay)
	if result.Status != ChainStatusValid {
		t.Fatalf("chain status = %q; %v", result.Status, result.Err)
	}
	if result.LatestChain != ChainPass {
		t.Fatalf("latest cv = %q, want pass", result.LatestChain)
	}
	if len(result.Instances) != 2 {
		t.Fatalf("got %d instances, want 2", len(result.Instances))
	}

	first := result.Instances[0]
	second := result.Instances[1]
	if first.Chain != ChainNone || second.Chain != ChainPass {
		t.Fatalf("cv values = %q/%q", first.Chain, second.Chain)
	}
	if first.AMSErr != nil || first.ASErr != nil || first.AARErr != nil {
		t.Fatalf("old seal headers do not verify: %+v", first)
	}
	if first.AMSBodyErr == nil || !strings.Contains(first.AMSBodyErr.Error(), "body hash") {
		t.Fatalf("old AMS body mismatch = %v, want body hash failure", first.AMSBodyErr)
	}
	if second.AMSErr != nil || second.AMSBodyErr != nil || second.ASErr != nil || second.AARErr != nil {
		t.Fatalf("latest seal failed: %+v", second)
	}
	if !first.Trusted || !second.Trusted {
		t.Fatalf("expected both trusted relay domains")
	}

	untrusted := verifyMessage(t, message, lookup, func(string) bool { return false })
	if untrusted.Status != ChainStatusValid {
		t.Fatalf("cryptographic status changed under trust policy: %v", untrusted.Err)
	}
	if untrusted.Latest.Trusted || untrusted.Instances[0].Trusted {
		t.Fatalf("a cryptographically valid chain must not become trusted without caller policy")
	}
}

func TestTamperedOldSealFailsAtOuterSeal(t *testing.T) {
	message, keys := buildTwoHopChain(t)
	message = tamperARCSeal(t, message, 1)

	result := verifyMessage(t, message, lookupForKeys(keys), trustedRelay)
	if result.Status != ChainStatusInvalid {
		t.Fatalf("status = %q, want invalid", result.Status)
	}
	if result.Instances[1].ASErr == nil {
		t.Fatalf("expected chain failure at outer instance 2 AS, got nil")
	}
	if !strings.Contains(result.Instances[1].ASErr.Error(), "invalid Ed25519 signature") {
		t.Fatalf("instance 2 AS error = %v", result.Instances[1].ASErr)
	}
	if result.Instances[0].ASErr == nil || !strings.Contains(result.Instances[0].ASErr.Error(), "invalid Ed25519 signature") {
		t.Fatalf("tampered instance 1 AS error = %v", result.Instances[0].ASErr)
	}
}

func TestDeletedInstanceSetFailsChain(t *testing.T) {
	message, keys := buildTwoHopChain(t)
	message = removeARCInstance(t, message, 1)

	result := verifyMessage(t, message, lookupForKeys(keys), trustedRelay)
	if result.Status != ChainStatusInvalid {
		t.Fatalf("status = %q, want invalid", result.Status)
	}
	if result.Instances[0].AMSErr == nil || result.Instances[0].ASErr == nil || result.Instances[0].AARErr == nil {
		t.Fatalf("deleted set should be reported at instance 1: %+v", result.Instances[0])
	}
	if result.Instances[1].AMSErr == nil || result.Instances[1].ASErr == nil {
		t.Fatalf("instance 2 must fail because its referenced instance 1 headers were removed: %+v", result.Instances[1])
	}
}

func TestDuplicateInstanceNumberFailsChain(t *testing.T) {
	message, keys := buildTwoHopChain(t)
	message = duplicateARCHeader(t, message, amsHeaderName, 2)

	result := verifyMessage(t, message, lookupForKeys(keys), trustedRelay)
	if result.Status != ChainStatusInvalid {
		t.Fatalf("status = %q, want invalid", result.Status)
	}
	if result.Err == nil || !strings.Contains(result.Err.Error(), "duplicate instance 2") {
		t.Fatalf("error = %v, want duplicate instance 2", result.Err)
	}
}

func removeARCInstance(t *testing.T, message []byte, instance int) []byte {
	t.Helper()
	fields, bodyReader, err := dkim.ReadMessageHeader(bytes.NewReader(message))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(bodyReader)
	if err != nil {
		t.Fatal(err)
	}

	var b bytes.Buffer
	for _, field := range fields {
		name, value := dkim.ParseHeaderField(field)
		if isARCHeader(name) {
			n, err := parseInstance(value)
			if err == nil && n == instance {
				continue
			}
		}
		b.WriteString(field)
	}
	b.WriteString("\r\n")
	b.Write(body)
	return b.Bytes()
}

func duplicateARCHeader(t *testing.T, message []byte, wantedName string, wantedInstance int) []byte {
	t.Helper()
	fields, bodyReader, err := dkim.ReadMessageHeader(bytes.NewReader(message))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(bodyReader)
	if err != nil {
		t.Fatal(err)
	}

	var duplicate string
	filtered := make(dkim.Header, 0, len(fields)+1)
	for _, field := range fields {
		name, value := dkim.ParseHeaderField(field)
		if duplicate == "" && strings.EqualFold(name, wantedName) {
			if n, err := parseInstance(value); err == nil && n == wantedInstance {
				duplicate = field
			}
		}
		filtered = append(filtered, field)
	}
	if duplicate == "" {
		t.Fatalf("header %s i=%d not found", wantedName, wantedInstance)
	}

	var b bytes.Buffer
	b.WriteString(duplicate)
	for _, field := range filtered {
		b.WriteString(field)
	}
	b.WriteString("\r\n")
	b.Write(body)
	return b.Bytes()
}

func tamperARCSeal(t *testing.T, message []byte, instance int) []byte {
	t.Helper()
	fields, bodyReader, err := dkim.ReadMessageHeader(bytes.NewReader(message))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(bodyReader)
	if err != nil {
		t.Fatal(err)
	}

	var b bytes.Buffer
	for _, field := range fields {
		name, value := dkim.ParseHeaderField(field)
		if strings.EqualFold(name, "arc-seal") {
			if n, err := parseInstance(value); err == nil && n == instance {
				loc := regexp.MustCompile(`b=[A-Za-z0-9+/]`).FindStringIndex(field)
				if loc == nil {
					t.Fatalf("b tag not found in ARC-Seal i=%d", instance)
				}
				pos := loc[1] - 1
				replacement := byte('A')
				if field[pos] == 'A' {
					replacement = 'B'
				}
				field = field[:pos] + string(replacement) + field[pos+1:]
			}
		}
		b.WriteString(field)
	}
	b.WriteString("\r\n")
	b.Write(body)
	return b.Bytes()
}
