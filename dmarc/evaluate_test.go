package dmarc

import (
	"errors"
	"net"
	"strings"
	"testing"
)

// fakePSL is a static Public Suffix List used to resolve organizational
// domains in tests.
type fakePSL map[string]bool

func (psl fakePSL) orgDomain(domain string) (string, error) {
	labels := strings.Split(domain, ".")
	for i := 0; i < len(labels); i++ {
		suffix := strings.Join(labels[i:], ".")
		if psl[suffix] {
			if i == 0 {
				return "", errors.New("domain is a public suffix")
			}
			return labels[i-1] + "." + suffix, nil
		}
	}
	return "", errors.New("no public suffix found")
}

type tempNetError struct{}

func (tempNetError) Error() string   { return "temporary DNS failure" }
func (tempNetError) Timeout() bool   { return false }
func (tempNetError) Temporary() bool { return true }

// fakeDNS serves static TXT records and can inject temporary failures.
type fakeDNS struct {
	records  map[string][]string
	tempFail map[string]bool
}

func (dns fakeDNS) lookupTXT(domain string) ([]string, error) {
	if dns.tempFail[domain] {
		return nil, tempNetError{}
	}
	if txts, ok := dns.records[domain]; ok {
		return txts, nil
	}
	return nil, &net.DNSError{IsNotFound: true}
}

var testPSL = fakePSL{
	"com":   true,
	"uk":    true,
	"co.uk": true,
	"net":   true,
}

func testOptions(dns fakeDNS) *EvaluateOptions {
	return &EvaluateOptions{
		LookupTXT: dns.lookupTXT,
		OrgDomain: testPSL.orgDomain,
	}
}

func evaluate(t *testing.T, msg *Message, options *EvaluateOptions) *Result {
	t.Helper()
	res, err := EvaluateWithOptions(msg, options)
	if err != nil {
		t.Fatalf("EvaluateWithOptions() = %v", err)
	}
	return res
}

func TestEvaluateDKIMMultipleSignatures(t *testing.T) {
	dns := fakeDNS{records: map[string][]string{
		"_dmarc.example.com": {"v=DMARC1; p=reject"},
	}}
	msg := &Message{
		From: []string{"alice@sub.example.com"},
		DKIM: []DKIMResult{
			{Domain: "example.com", Valid: false},    // aligned but broken
			{Domain: "unrelated.net", Valid: true},   // valid but not aligned
			{Domain: "sub.example.com", Valid: true}, // valid and aligned
		},
	}
	res := evaluate(t, msg, testOptions(dns))
	if res.Alignment != AlignmentDKIM {
		t.Errorf("Alignment = %v, want dkim", res.Alignment)
	}
	if res.Disposition != DispositionNone {
		t.Errorf("Disposition = %v, want none", res.Disposition)
	}

	// Without the aligned signature, the policy applies.
	msg.DKIM = msg.DKIM[:2]
	res = evaluate(t, msg, testOptions(dns))
	if res.Alignment != AlignmentNone {
		t.Errorf("Alignment = %v, want none", res.Alignment)
	}
	if res.Disposition != DispositionReject {
		t.Errorf("Disposition = %v, want reject", res.Disposition)
	}
}

func TestEvaluateAlignmentModes(t *testing.T) {
	dns := fakeDNS{records: map[string][]string{
		"_dmarc.example.com": {"v=DMARC1; p=reject; adkim=s; aspf=s"},
	}}
	options := testOptions(dns)

	// Strict: subdomains do not align.
	msg := &Message{
		From: []string{"alice@example.com"},
		SPF:  &SPFResult{Domain: "mail.example.com", Pass: true},
		DKIM: []DKIMResult{{Domain: "sub.example.com", Valid: true}},
	}
	res := evaluate(t, msg, options)
	if res.Alignment != AlignmentNone || res.Disposition != DispositionReject {
		t.Errorf("strict: got (%v, %v), want (none, reject)", res.Alignment, res.Disposition)
	}

	// Strict: exact domains align.
	msg.SPF.Domain = "example.com"
	msg.DKIM[0].Domain = "example.com"
	res = evaluate(t, msg, options)
	if res.Alignment != AlignmentDKIM || res.Disposition != DispositionNone {
		t.Errorf("strict: got (%v, %v), want (dkim, none)", res.Alignment, res.Disposition)
	}

	// Relaxed: same organizational domain aligns.
	dns.records["_dmarc.example.com"] = []string{"v=DMARC1; p=reject; adkim=r; aspf=r"}
	msg.SPF.Domain = "mail.example.com"
	msg.DKIM[0].Domain = "other.example.com"
	res = evaluate(t, msg, options)
	if res.Alignment != AlignmentDKIM || res.Disposition != DispositionNone {
		t.Errorf("relaxed: got (%v, %v), want (dkim, none)", res.Alignment, res.Disposition)
	}

	// Relaxed: a different organizational domain never aligns.
	msg.SPF.Domain = "mail.example.net"
	msg.DKIM[0].Domain = "example.net"
	res = evaluate(t, msg, options)
	if res.Alignment != AlignmentNone || res.Disposition != DispositionReject {
		t.Errorf("relaxed: got (%v, %v), want (none, reject)", res.Alignment, res.Disposition)
	}
}

func TestEvaluateSubdomainPolicy(t *testing.T) {
	dns := fakeDNS{records: map[string][]string{
		"_dmarc.example.com":      {"v=DMARC1; p=reject; sp=none"},
		"_dmarc.news.example.com": {"v=DMARC1; p=quarantine"},
	}}
	options := testOptions(dns)

	// No record on the subdomain: the organizational record's sp applies.
	msg := &Message{From: []string{"alice@mail.example.com"}}
	res := evaluate(t, msg, options)
	if res.Disposition != DispositionNone {
		t.Errorf("sp=none: Disposition = %v, want none", res.Disposition)
	}

	// Without sp, subdomains inherit p.
	dns.records["_dmarc.example.com"] = []string{"v=DMARC1; p=reject"}
	res = evaluate(t, msg, options)
	if res.Disposition != DispositionReject {
		t.Errorf("no sp: Disposition = %v, want reject", res.Disposition)
	}

	// A subdomain's own record takes precedence over the parent's sp.
	msg.From = []string{"alice@news.example.com"}
	res = evaluate(t, msg, options)
	if res.Disposition != DispositionQuarantine {
		t.Errorf("own record: Disposition = %v, want quarantine", res.Disposition)
	}
}

func TestEvaluatePublicSuffixBoundary(t *testing.T) {
	dns := fakeDNS{records: map[string][]string{
		"_dmarc.example.co.uk": {"v=DMARC1; p=quarantine"},
	}}
	options := testOptions(dns)

	// The organizational domain of sub.example.co.uk is example.co.uk, not
	// co.uk.
	msg := &Message{From: []string{"alice@sub.example.co.uk"}}
	res := evaluate(t, msg, options)
	if res.Disposition != DispositionQuarantine {
		t.Errorf("Disposition = %v, want quarantine", res.Disposition)
	}

	// Relaxed alignment holds within example.co.uk...
	msg.DKIM = []DKIMResult{{Domain: "example.co.uk", Valid: true}}
	res = evaluate(t, msg, options)
	if res.Alignment != AlignmentDKIM {
		t.Errorf("Alignment = %v, want dkim", res.Alignment)
	}

	// ...but not across the public suffix boundary into another
	// organizational domain under co.uk.
	msg.DKIM = []DKIMResult{{Domain: "other.co.uk", Valid: true}}
	res = evaluate(t, msg, options)
	if res.Alignment != AlignmentNone || res.Disposition != DispositionQuarantine {
		t.Errorf("got (%v, %v), want (none, quarantine)", res.Alignment, res.Disposition)
	}
}

func TestEvaluateDistinctFailures(t *testing.T) {
	dns := fakeDNS{
		records: map[string][]string{
			"_dmarc.example.com": {"v=DMARC1; p=reject"},
		},
		tempFail: map[string]bool{"_dmarc.example.net": true},
	}
	options := testOptions(dns)

	// Multiple From addresses: reported as its own error, not a reject.
	msg := &Message{From: []string{"alice@example.com", "bob@example.com"}}
	res, err := EvaluateWithOptions(msg, options)
	if !errors.Is(err, ErrMultipleFrom) {
		t.Errorf("multiple From: err = %v, want ErrMultipleFrom", err)
	}
	if res != nil {
		t.Errorf("multiple From: res = %+v, want nil", res)
	}

	// From domain is a public suffix: the organizational domain cannot be
	// determined.
	msg = &Message{From: []string{"alice@co.uk"}}
	res, err = EvaluateWithOptions(msg, options)
	if !errors.Is(err, ErrNoOrgDomain) {
		t.Errorf("public suffix From: err = %v, want ErrNoOrgDomain", err)
	}
	if res != nil {
		t.Errorf("public suffix From: res = %+v, want nil", res)
	}

	// Temporary DNS failure: surfaced as a temporary error, not a reject.
	msg = &Message{From: []string{"alice@example.net"}}
	res, err = EvaluateWithOptions(msg, options)
	if !IsTempFail(err) {
		t.Errorf("temp fail: err = %v, want temporary failure", err)
	}
	if res != nil {
		t.Errorf("temp fail: res = %+v, want nil", res)
	}
}

func TestEvaluatePercentSampling(t *testing.T) {
	dns := fakeDNS{records: map[string][]string{
		"_dmarc.example.com": {"v=DMARC1; p=reject; pct=50"},
	}}
	options := testOptions(dns)
	msg := &Message{From: []string{"alice@example.com"}, MessageID: "<abc@example.com>"}

	// Injectable source: a percentile below pct applies the policy, a
	// percentile above it downgrades the policy one notch.
	options.Percentile = func(messageID string) int { return 25 }
	res := evaluate(t, msg, options)
	if res.Disposition != DispositionReject {
		t.Errorf("percentile 25: Disposition = %v, want reject", res.Disposition)
	}

	options.Percentile = func(messageID string) int { return 75 }
	res = evaluate(t, msg, options)
	if res.Disposition != DispositionQuarantine {
		t.Errorf("percentile 75: Disposition = %v, want quarantine", res.Disposition)
	}

	// Default sampler: the same message always receives the same decision.
	options.Percentile = nil
	first := evaluate(t, msg, options)
	for i := 0; i < 10; i++ {
		if res := evaluate(t, msg, options); res.Disposition != first.Disposition {
			t.Fatalf("run %d: Disposition = %v, want %v", i, res.Disposition, first.Disposition)
		}
	}
}

func TestEvaluateNoRecord(t *testing.T) {
	msg := &Message{
		From: []string{"alice@example.com"},
		SPF:  &SPFResult{Domain: "example.com", Pass: true},
	}
	res := evaluate(t, msg, testOptions(fakeDNS{}))
	if res.Record != nil {
		t.Errorf("Record = %+v, want nil", res.Record)
	}
	if res.Alignment != AlignmentSPF {
		t.Errorf("Alignment = %v, want spf", res.Alignment)
	}
	if res.Disposition != DispositionNone {
		t.Errorf("Disposition = %v, want none", res.Disposition)
	}
}
