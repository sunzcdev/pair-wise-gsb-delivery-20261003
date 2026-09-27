package authres

import (
	"strings"
	"testing"
)

func TestFilter(t *testing.T) {
	policy := &Policy{
		AuthServID:       "mx.example.org",
		TrustedUpstreams: []string{"mail.example.net"},
	}
	headers := []string{
		// Forged by the sender, claims our own authserv-id.
		"mx.example.org; dkim=pass header.d=example.com; spf=pass smtp.mailfrom=example.com",
		// Legitimate header field added by a trusted upstream.
		"mail.example.net; dkim=pass header.d=example.com; spf=pass smtp.mailfrom=example.com",
		// Trusted upstream using an unknown extension method.
		"mail.example.net; x-reputation=pass score=high",
		// Malformed header field.
		"mail.example.net; dkim",
		// Unknown authserv-id.
		"random.example.com; spf=pass smtp.mailfrom=example.com",
	}
	decisions := policy.Filter(headers, EntryTrust{FromTrustedRelay: true})
	if len(decisions) != len(headers) {
		t.Fatalf("Expected %v decisions, got %v", len(headers), len(decisions))
	}
	expected := []struct {
		keep   bool
		reason string
	}{
		{false, "claims the local authserv-id"},
		{true, "trusted upstream"},
		{true, "trusted upstream"},
		{false, "malformed"},
		{false, "not a trusted upstream"},
	}
	for i, want := range expected {
		d := decisions[i]
		if d.Keep != want.keep {
			t.Errorf("Header %v: expected keep=%v, got %v (reason: %q)", i, want.keep, d.Keep, d.Reason)
		}
		if !strings.Contains(d.Reason, want.reason) {
			t.Errorf("Header %v: expected reason to contain %q, got %q", i, want.reason, d.Reason)
		}
		if d.Keep && d.Results == nil {
			t.Errorf("Header %v: kept header field has no parsed results", i)
		}
		if !d.Keep && d.Results != nil {
			t.Errorf("Header %v: discarded header field must not expose results", i)
		}
	}
	// The forged header field must not be trusted even though its authserv-id
	// string-matches the local one.
	if decisions[0].Keep {
		t.Error("Forged header field with local authserv-id was kept")
	}
	// Unknown extension methods from a trusted upstream are kept as-is: the
	// decision is about trust, not about understanding the method.
	ext, ok := decisions[2].Results[0].(*GenericResult)
	if !ok {
		t.Fatalf("Expected unknown extension method to be a GenericResult, got %T", decisions[2].Results[0])
	}
	if ext.Method != "x-reputation" || ext.Value != ResultPass {
		t.Errorf("Expected x-reputation=pass, got %v=%v", ext.Method, ext.Value)
	}
}

func TestFilterUntrustedEntry(t *testing.T) {
	policy := &Policy{
		AuthServID:       "mx.example.org",
		TrustedUpstreams: []string{"mail.example.net"},
	}
	headers := []string{
		"mail.example.net; dkim=pass header.d=example.com",
	}
	decisions := policy.Filter(headers, EntryTrust{FromTrustedRelay: false})
	if len(decisions) != 1 {
		t.Fatalf("Expected 1 decision, got %v", len(decisions))
	}
	if decisions[0].Keep {
		t.Error("Header field from untrusted entry point was kept")
	}
	if !strings.Contains(decisions[0].Reason, "not received from a trusted relay") {
		t.Errorf("Unexpected reason: %q", decisions[0].Reason)
	}
}
