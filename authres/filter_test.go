package authres

import (
	"strings"
	"testing"
)

var filterPolicy = &IngressPolicy{
	AuthServID: "mx.example.com",
	TrustedUpstreams: []TrustedUpstream{
		{AuthServID: "mail.upstream.example"},
		{AuthServID: "dkim-only.example", Methods: []string{"dkim"}},
	},
}

var filterTests = []struct {
	name           string
	peerAuthServID string
	value          string
	wantKeep       bool
	wantResults    int
	wantReason     string
}{
	{
		name:           "local impersonation",
		peerAuthServID: "mail.upstream.example",
		value:          "mx.example.com; dkim=pass header.d=example.org",
		wantKeep:       false,
		wantReason:     "impersonates this gateway",
	},
	{
		name:           "trusted upstream",
		peerAuthServID: "mail.upstream.example",
		value: "mail.upstream.example; dkim=pass header.d=example.org;" +
			" spf=pass smtp.mailfrom=example.org",
		wantKeep:    true,
		wantResults: 2,
		wantReason:  `received from trusted upstream "mail.upstream.example"`,
	},
	{
		name:           "unknown extension method from trusted upstream",
		peerAuthServID: "mail.upstream.example",
		value:          "mail.upstream.example; x-emoji=pass policy.emoji=smile",
		wantKeep:       true,
		wantResults:    1,
		wantReason:     "received from trusted upstream",
	},
	{
		name:           "trusted name, wrong peer",
		peerAuthServID: "evil.example",
		value:          "mail.upstream.example; dkim=pass header.d=example.org",
		wantKeep:       false,
		wantReason:     "the message was not received from that upstream",
	},
	{
		name:           "unknown authserv-id",
		peerAuthServID: "mail.upstream.example",
		value:          "stranger.example; spf=pass smtp.mailfrom=example.org",
		wantKeep:       false,
		wantReason:     "not a trusted upstream",
	},
	{
		name:           "malformed header",
		peerAuthServID: "mail.upstream.example",
		value:          "mail.upstream.example; dkim",
		wantKeep:       false,
		wantReason:     "malformed Authentication-Results header field",
	},
	{
		name:           "missing authserv-id",
		peerAuthServID: "mail.upstream.example",
		value:          "; dkim=pass header.d=example.org",
		wantKeep:       false,
		wantReason:     "missing authserv-id",
	},
	{
		name:           "method allowlist strips other methods",
		peerAuthServID: "dkim-only.example",
		value: "dkim-only.example; dkim=pass header.d=example.org;" +
			" spf=pass smtp.mailfrom=example.org",
		wantKeep:    true,
		wantResults: 1,
		wantReason:  "stripped results for methods not allowed by policy: spf",
	},
	{
		name:           "method allowlist rejects all methods",
		peerAuthServID: "dkim-only.example",
		value:          "dkim-only.example; spf=pass smtp.mailfrom=example.org",
		wantKeep:       false,
		wantReason:     "none of its methods (spf) are allowed by policy",
	},
}

func TestIngressPolicyFilter(t *testing.T) {
	for _, test := range filterTests {
		t.Run(test.name, func(t *testing.T) {
			ctx := &IngressContext{PeerAuthServID: test.peerAuthServID}
			decisions := filterPolicy.Filter(ctx, []string{test.value})
			if len(decisions) != 1 {
				t.Fatalf("Expected 1 decision, got %v", len(decisions))
			}

			d := decisions[0]
			if d.Keep != test.wantKeep {
				t.Errorf("Expected keep=%v, got %v (reason: %v)", test.wantKeep, d.Keep, d.Reason)
			}
			if len(d.Results) != test.wantResults {
				t.Errorf("Expected %v results, got %v", test.wantResults, len(d.Results))
			}
			if !strings.Contains(d.Reason, test.wantReason) {
				t.Errorf("Expected reason to contain %q, got %q", test.wantReason, d.Reason)
			}
			if !d.Keep && d.Results != nil {
				t.Errorf("Expected nil results for a removed header field, got %v", d.Results)
			}
		})
	}
}

func TestIngressPolicyFilterUnknownMethodResult(t *testing.T) {
	ctx := &IngressContext{PeerAuthServID: "mail.upstream.example"}
	decisions := filterPolicy.Filter(ctx, []string{
		"mail.upstream.example; x-emoji=pass policy.emoji=smile",
	})

	d := decisions[0]
	if !d.Keep {
		t.Fatalf("Expected header field to be kept, got reason: %v", d.Reason)
	}
	r, ok := d.Results[0].(*GenericResult)
	if !ok {
		t.Fatalf("Expected unknown method to yield a GenericResult, got %T", d.Results[0])
	}
	if r.Method != "x-emoji" || r.Value != ResultPass {
		t.Errorf("Expected x-emoji=pass, got %v=%v", r.Method, r.Value)
	}
}

func TestIngressPolicyFilterOrder(t *testing.T) {
	ctx := &IngressContext{PeerAuthServID: "mail.upstream.example"}
	values := []string{
		"mx.example.com; dkim=pass header.d=example.org",
		"mail.upstream.example; dkim=pass header.d=example.org",
		"stranger.example; spf=pass smtp.mailfrom=example.org",
	}

	decisions := filterPolicy.Filter(ctx, values)
	if len(decisions) != len(values) {
		t.Fatalf("Expected %v decisions, got %v", len(values), len(decisions))
	}
	for i, v := range values {
		if decisions[i].Value != v {
			t.Errorf("Expected decision %v to keep order of value %q", i, v)
		}
	}

	wantKeep := []bool{false, true, false}
	for i, want := range wantKeep {
		if decisions[i].Keep != want {
			t.Errorf("Expected decision %v keep=%v, got %v (%v)",
				i, want, decisions[i].Keep, decisions[i].Reason)
		}
	}
}
