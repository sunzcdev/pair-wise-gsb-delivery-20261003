package dmarc

import (
	"errors"
	"strings"
	"testing"
)

// fakeProvider is a PolicyProvider backed by static maps and a fixed list of
// public suffixes.
type fakeProvider struct {
	records   map[string]*Record
	lookupErr map[string]error
	suffixes  []string
}

func (p *fakeProvider) LookupRecord(domain string) (*Record, error) {
	if err, ok := p.lookupErr[domain]; ok {
		return nil, err
	}
	if rec, ok := p.records[domain]; ok {
		return rec, nil
	}
	return nil, ErrNoPolicy
}

func (p *fakeProvider) OrganizationalDomain(domain string) (string, error) {
	domain = strings.ToLower(domain)
	var suffix string
	for _, s := range p.suffixes {
		if (domain == s || strings.HasSuffix(domain, "."+s)) && len(s) > len(suffix) {
			suffix = s
		}
	}
	if suffix == "" {
		return "", errors.New("no public suffix found")
	}
	if domain == suffix {
		return "", errors.New("domain is a public suffix")
	}
	rest := domain[:len(domain)-len(suffix)-1]
	labels := strings.Split(rest, ".")
	return labels[len(labels)-1] + "." + suffix, nil
}

func intPtr(i int) *int { return &i }

func testProvider() *fakeProvider {
	return &fakeProvider{
		records: map[string]*Record{
			"example.com": {
				Policy:        PolicyReject,
				DKIMAlignment: AlignmentRelaxed,
				SPFAlignment:  AlignmentRelaxed,
			},
			"strict.example.net": {
				Policy:        PolicyReject,
				DKIMAlignment: AlignmentStrict,
				SPFAlignment:  AlignmentStrict,
			},
			"example.org": {
				Policy:          PolicyReject,
				SubdomainPolicy: PolicyQuarantine,
				DKIMAlignment:   AlignmentRelaxed,
				SPFAlignment:    AlignmentRelaxed,
			},
			"sampled.example.com": {
				Policy:        PolicyReject,
				Percent:       intPtr(50),
				DKIMAlignment: AlignmentRelaxed,
				SPFAlignment:  AlignmentRelaxed,
			},
			"example.co.uk": {
				Policy:        PolicyReject,
				DKIMAlignment: AlignmentRelaxed,
				SPFAlignment:  AlignmentRelaxed,
			},
		},
		lookupErr: map[string]error{},
		suffixes:  []string{"com", "org", "net", "uk", "co.uk", "example.net"},
	}
}

func evalOne(t *testing.T, p PolicyProvider, from string, spf []SPFResult, dkim []DKIMResult) (*Evaluation, error) {
	t.Helper()
	return Evaluate([]string{from}, spf, dkim, p, nil)
}

func TestEvaluateMultipleDKIMSignatures(t *testing.T) {
	p := testProvider()

	// A failing aligned signature plus a passing aligned signature: the
	// passing one is enough.
	eval, err := evalOne(t, p, "alice@example.com", nil, []DKIMResult{
		{Pass: false, Domain: "example.com"},
		{Pass: true, Domain: "example.com"},
	})
	if err != nil {
		t.Fatalf("Evaluate() = %v", err)
	}
	if !eval.DKIMAligned || !eval.Pass() {
		t.Errorf("expected DKIM alignment, got %+v", eval)
	}
	if eval.Disposition != PolicyNone {
		t.Errorf("Disposition = %v, want none", eval.Disposition)
	}

	// A passing unaligned signature plus a failing aligned signature: no
	// alignment, policy applies.
	eval, err = evalOne(t, p, "alice@example.com", nil, []DKIMResult{
		{Pass: true, Domain: "thirdparty.net"},
		{Pass: false, Domain: "example.com"},
	})
	if err != nil {
		t.Fatalf("Evaluate() = %v", err)
	}
	if eval.Pass() {
		t.Errorf("expected DMARC failure, got %+v", eval)
	}
	if eval.Disposition != PolicyReject {
		t.Errorf("Disposition = %v, want reject", eval.Disposition)
	}
}

func TestEvaluateAlignmentModes(t *testing.T) {
	p := testProvider()

	// Relaxed mode: a parent-domain DKIM signature aligns with a subdomain
	// From.
	eval, err := evalOne(t, p, "alice@mail.example.com", nil, []DKIMResult{
		{Pass: true, Domain: "example.com"},
	})
	if err != nil {
		t.Fatalf("Evaluate() = %v", err)
	}
	if !eval.DKIMAligned {
		t.Errorf("expected relaxed DKIM alignment, got %+v", eval)
	}

	// Strict mode: the same signature does not align.
	eval, err = evalOne(t, p, "alice@mail.strict.example.net", nil, []DKIMResult{
		{Pass: true, Domain: "strict.example.net"},
	})
	if err != nil {
		t.Fatalf("Evaluate() = %v", err)
	}
	if eval.DKIMAligned {
		t.Errorf("expected no strict DKIM alignment, got %+v", eval)
	}
	if eval.Disposition != PolicyReject {
		t.Errorf("Disposition = %v, want reject", eval.Disposition)
	}

	// Strict mode: an exact-domain signature aligns.
	eval, err = evalOne(t, p, "alice@mail.strict.example.net", nil, []DKIMResult{
		{Pass: true, Domain: "mail.strict.example.net"},
	})
	if err != nil {
		t.Fatalf("Evaluate() = %v", err)
	}
	if !eval.DKIMAligned {
		t.Errorf("expected strict DKIM alignment, got %+v", eval)
	}

	// SPF follows its own mode (aspf): relaxed SPF alignment still passes
	// when DKIM strict alignment fails.
	eval, err = evalOne(t, p, "alice@mail.strict.example.net",
		[]SPFResult{{Pass: true, Domain: "strict.example.net"}}, nil)
	if err != nil {
		t.Fatalf("Evaluate() = %v", err)
	}
	if eval.SPFAligned {
		t.Errorf("expected no strict SPF alignment, got %+v", eval)
	}
}

func TestEvaluateSubdomainPolicy(t *testing.T) {
	p := testProvider()

	// No record on the subdomain: fall back to the organizational domain
	// record, and apply sp instead of p.
	eval, err := evalOne(t, p, "alice@mail.example.org", nil, nil)
	if err != nil {
		t.Fatalf("Evaluate() = %v", err)
	}
	if eval.Record == nil || eval.Record.Policy != PolicyReject {
		t.Fatalf("expected organizational record, got %+v", eval.Record)
	}
	if eval.Disposition != PolicyQuarantine {
		t.Errorf("Disposition = %v, want quarantine (sp)", eval.Disposition)
	}

	// The organizational domain itself uses p, not sp.
	eval, err = evalOne(t, p, "alice@example.org", nil, nil)
	if err != nil {
		t.Fatalf("Evaluate() = %v", err)
	}
	if eval.Disposition != PolicyReject {
		t.Errorf("Disposition = %v, want reject (p)", eval.Disposition)
	}
}

func TestEvaluatePublicSuffixBoundary(t *testing.T) {
	p := testProvider()

	// Relaxed alignment across a multi-label public suffix: mail.example.co.uk
	// and example.co.uk share the organizational domain example.co.uk.
	eval, err := evalOne(t, p, "alice@mail.example.co.uk", nil, []DKIMResult{
		{Pass: true, Domain: "example.co.uk"},
	})
	if err != nil {
		t.Fatalf("Evaluate() = %v", err)
	}
	if eval.OrganizationalDomain != "example.co.uk" {
		t.Errorf("OrganizationalDomain = %v, want example.co.uk", eval.OrganizationalDomain)
	}
	if !eval.DKIMAligned {
		t.Errorf("expected relaxed alignment across co.uk boundary, got %+v", eval)
	}

	// Sibling domains under the same public suffix do not align: their
	// organizational domains differ.
	p.records["other.co.uk"] = &Record{
		Policy:        PolicyReject,
		DKIMAlignment: AlignmentRelaxed,
		SPFAlignment:  AlignmentRelaxed,
	}
	eval, err = evalOne(t, p, "alice@other.co.uk", nil, []DKIMResult{
		{Pass: true, Domain: "example.co.uk"},
	})
	if err != nil {
		t.Fatalf("Evaluate() = %v", err)
	}
	if eval.DKIMAligned {
		t.Errorf("expected no alignment between sibling organizational domains, got %+v", eval)
	}
	if eval.Disposition != PolicyReject {
		t.Errorf("Disposition = %v, want reject", eval.Disposition)
	}
}

func TestEvaluateFromIdentityErrors(t *testing.T) {
	p := testProvider()

	if _, err := Evaluate(nil, nil, nil, p, nil); err != ErrNoFrom {
		t.Errorf("Evaluate() with no From = %v, want ErrNoFrom", err)
	}

	_, err := Evaluate([]string{"alice@example.com", "bob@example.com"}, nil, nil, p, nil)
	if err != ErrMultipleFrom {
		t.Errorf("Evaluate() with multiple From = %v, want ErrMultipleFrom", err)
	}

	// A From domain which is itself a public suffix has no organizational
	// domain.
	_, err = evalOne(t, p, "alice@co.uk", nil, nil)
	if !errors.Is(err, ErrNoOrganizationalDomain) {
		t.Errorf("Evaluate() = %v, want ErrNoOrganizationalDomain", err)
	}
}

func TestEvaluateTempFail(t *testing.T) {
	p := testProvider()
	p.lookupErr["example.com"] = tempFailError("TXT record unavailable")

	eval, err := evalOne(t, p, "alice@example.com", nil, nil)
	if err == nil {
		t.Fatalf("Evaluate() = %+v, want temporary failure", eval)
	}
	if !IsTempFail(err) {
		t.Errorf("IsTempFail(%v) = false, want true", err)
	}
	if eval != nil {
		t.Errorf("expected no evaluation on temporary failure, got %+v", eval)
	}
}

func TestEvaluateNoPolicy(t *testing.T) {
	p := testProvider()

	eval, err := evalOne(t, p, "alice@unknown.net", nil, nil)
	if err != nil {
		t.Fatalf("Evaluate() = %v", err)
	}
	if eval.Record != nil {
		t.Errorf("Record = %+v, want nil", eval.Record)
	}
	if eval.Disposition != PolicyNone {
		t.Errorf("Disposition = %v, want none", eval.Disposition)
	}
}

func TestEvaluatePercent(t *testing.T) {
	p := testProvider()

	// Injectable sampler: the caller controls the selection.
	selected := false
	sampler := Sampler(func(from string, pct int) bool { return selected })

	selected = false
	eval, err := Evaluate([]string{"alice@sampled.example.com"}, nil, nil, p, sampler)
	if err != nil {
		t.Fatalf("Evaluate() = %v", err)
	}
	if !eval.SampledOut || eval.Disposition != PolicyNone {
		t.Errorf("expected sampled-out message with disposition none, got %+v", eval)
	}

	selected = true
	eval, err = Evaluate([]string{"alice@sampled.example.com"}, nil, nil, p, sampler)
	if err != nil {
		t.Fatalf("Evaluate() = %v", err)
	}
	if eval.SampledOut || eval.Disposition != PolicyReject {
		t.Errorf("expected selected message with disposition reject, got %+v", eval)
	}

	// pct = 0: the policy never applies.
	p.records["zero.example.com"] = &Record{Policy: PolicyReject, Percent: intPtr(0)}
	eval, err = evalOne(t, p, "alice@zero.example.com", nil, nil)
	if err != nil {
		t.Fatalf("Evaluate() = %v", err)
	}
	if eval.Disposition != PolicyNone {
		t.Errorf("Disposition = %v, want none for pct=0", eval.Disposition)
	}

	// pct = 100: the policy always applies.
	p.records["full.example.com"] = &Record{Policy: PolicyReject, Percent: intPtr(100)}
	eval, err = evalOne(t, p, "alice@full.example.com", nil, nil)
	if err != nil {
		t.Fatalf("Evaluate() = %v", err)
	}
	if eval.Disposition != PolicyReject {
		t.Errorf("Disposition = %v, want reject for pct=100", eval.Disposition)
	}
}

func TestHashSamplerDeterministic(t *testing.T) {
	p := testProvider()

	// The default sampler must make the same choice for the same message.
	first, err := evalOne(t, p, "alice@sampled.example.com", nil, nil)
	if err != nil {
		t.Fatalf("Evaluate() = %v", err)
	}
	for i := 0; i < 10; i++ {
		eval, err := evalOne(t, p, "alice@sampled.example.com", nil, nil)
		if err != nil {
			t.Fatalf("Evaluate() = %v", err)
		}
		if eval.SampledOut != first.SampledOut || eval.Disposition != first.Disposition {
			t.Fatalf("run %d: got %+v, want %+v", i, eval, first)
		}
	}

	// Sanity check the hash sampler against the pct bounds.
	if HashSampler("alice@sampled.example.com", 0) {
		t.Error("HashSampler(pct=0) = true, want false")
	}
	if !HashSampler("alice@sampled.example.com", 100) {
		t.Error("HashSampler(pct=100) = false, want true")
	}
}
