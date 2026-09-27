package dmarc

import (
	"errors"
	"net"
	"strings"
	"testing"
)

type tempDNSError struct{}

func (tempDNSError) Error() string   { return "server misbehaving" }
func (tempDNSError) Timeout() bool   { return false }
func (tempDNSError) Temporary() bool { return true }

type fakeDNS struct {
	records map[string][]string
	errs    map[string]error
	queries []string
}

func (dns *fakeDNS) lookupTXT(domain string) ([]string, error) {
	dns.queries = append(dns.queries, domain)
	if err, ok := dns.errs[domain]; ok {
		return nil, err
	}
	if txts, ok := dns.records[domain]; ok {
		return txts, nil
	}
	return nil, &net.DNSError{IsNotFound: true}
}

func TestVerifyReportDestinations(t *testing.T) {
	dns := &fakeDNS{
		records: map[string][]string{
			"example.com._report._dmarc.example.net": {"v=DMARC1"},
			"example.com._report._dmarc.wrong.example": {"v=spf1 -all"},
			"example.com._report._dmarc.multi.example": {"not-dmarc", "v=DMARC1"},
		},
		errs: map[string]error{
			"example.com._report._dmarc.flaky.example": tempDNSError{},
			"example.com._report._dmarc.broken.example": errors.New("connection refused"),
		},
	}
	options := &LookupOptions{LookupTXT: dns.lookupTXT}

	uris := []string{
		"mailto:dmarc@example.com",              // same domain: authorized without DNS
		"mailto:dmarc@example.net",              // authorized externally
		"mailto:dmarc@missing.example",          // no authorization record
		"mailto:dmarc@wrong.example",            // TXT record is not a DMARC record
		"mailto:dmarc@multi.example",            // one of several TXT records matches
		"mailto:dmarc@flaky.example",            // temporary DNS failure
		"mailto:dmarc@broken.example",           // permanent DNS failure
		"mailto:dmarc%40escaped.example",        // percent-escaped @
		"mailto:dmarc@sized.example!10m",        // size limit suffix
		"mailto:dmarc@example.net",              // duplicate destination
		"mailto:dmarc@EXAMPLE.NET!50k",          // duplicate with different case/size limit
		"https://reports.example.com/dmarc",     // unsupported scheme
		"mailto:not-an-address",                 // missing @
	}

	// Seed authorization records for the escaped/sized destinations.
	dns.records["example.com._report._dmarc.escaped.example"] = []string{"v=DMARC1"}
	dns.records["example.com._report._dmarc.sized.example"] = []string{"v=DMARC1"}

	results := VerifyReportDestinations("example.com", uris, options)

	if len(results) != len(uris) {
		t.Fatalf("expected %d results, got %d", len(uris), len(results))
	}

	type want struct {
		authorized bool
		temporary  bool
		address    string
	}
	wants := []want{
		{authorized: true, address: "dmarc@example.com"},
		{authorized: true, address: "dmarc@example.net"},
		{authorized: false, address: "dmarc@missing.example"},
		{authorized: false, address: "dmarc@wrong.example"},
		{authorized: true, address: "dmarc@multi.example"},
		{authorized: false, temporary: true, address: "dmarc@flaky.example"},
		{authorized: false, address: "dmarc@broken.example"},
		{authorized: true, address: "dmarc@escaped.example"},
		{authorized: true, address: "dmarc@sized.example"},
		{authorized: true, address: "dmarc@example.net"},
		{authorized: true, address: "dmarc@EXAMPLE.NET"},
		{authorized: false},
		{authorized: false},
	}

	for i, res := range results {
		w := wants[i]
		if res.Authorized != w.authorized || res.Temporary != w.temporary {
			t.Errorf("result %d (%s): authorized=%v temporary=%v, want authorized=%v temporary=%v (reason: %s)",
				i, res.URI, res.Authorized, res.Temporary, w.authorized, w.temporary, res.Reason)
		}
		if res.Address != w.address {
			t.Errorf("result %d (%s): address=%q, want %q", i, res.URI, res.Address, w.address)
		}
		if res.Reason == "" {
			t.Errorf("result %d (%s): empty reason", i, res.URI)
		}
	}

	// Duplicates must share the conclusion of the first occurrence.
	if results[9] != results[1] || results[10].Authorized != results[1].Authorized {
		t.Errorf("duplicate destinations got inconsistent results: %+v / %+v / %+v",
			results[1], results[9], results[10])
	}

	// Each unique external destination must be queried exactly once, and the
	// same-domain destination must not be queried at all.
	seen := make(map[string]int)
	for _, q := range dns.queries {
		seen[q]++
		if !strings.Contains(q, "._report._dmarc.") {
			t.Errorf("unexpected DNS query: %s", q)
		}
	}
	for q, n := range seen {
		if n != 1 {
			t.Errorf("destination %s queried %d times, want 1", q, n)
		}
	}
	if _, ok := seen["example.com._report._dmarc.example.com"]; ok {
		t.Errorf("same-domain destination triggered a DNS query")
	}
	if n := len(seen); n != 8 {
		t.Errorf("expected 8 unique DNS queries, got %d: %v", n, dns.queries)
	}
}

func TestVerifyReportDestinationsReasons(t *testing.T) {
	dns := &fakeDNS{
		records: map[string][]string{},
		errs: map[string]error{
			"example.com._report._dmarc.flaky.example": tempDNSError{},
		},
	}
	options := &LookupOptions{LookupTXT: dns.lookupTXT}

	results := VerifyReportDestinations("example.com", []string{
		"mailto:dmarc@missing.example",
		"mailto:dmarc@flaky.example",
	}, options)

	if !strings.Contains(results[0].Reason, "no authorization record") {
		t.Errorf("missing authorization reason = %q", results[0].Reason)
	}
	if !results[1].Temporary || !strings.Contains(results[1].Reason, "temporary") {
		t.Errorf("temporary failure reason = %q, temporary=%v", results[1].Reason, results[1].Temporary)
	}
}
