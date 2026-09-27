package dmarc

import (
	"net"
	"strings"
	"testing"
)

type tempDNSError struct{ msg string }

func (err tempDNSError) Error() string   { return err.msg }
func (err tempDNSError) Timeout() bool   { return false }
func (err tempDNSError) Temporary() bool { return true }

var errNotFound = &net.DNSError{IsNotFound: true, Err: "no such host", Name: "test"}

// fakeDNS is an injectable DNS resolver mapping names to TXT records.
type fakeDNS struct {
	records map[string][]string
	errs    map[string]error
	queries []string
}

func (dns *fakeDNS) lookupTXT(name string) ([]string, error) {
	dns.queries = append(dns.queries, name)
	if err, ok := dns.errs[name]; ok {
		return nil, err
	}
	if txts, ok := dns.records[name]; ok {
		return txts, nil
	}
	return nil, errNotFound
}

func verifyWithDNS(domain string, uris []string, dns *fakeDNS) []ReportDestination {
	return VerifyReportURIs(domain, uris, &LookupOptions{LookupTXT: dns.lookupTXT})
}

func TestVerifyReportURIsAuthorized(t *testing.T) {
	dns := &fakeDNS{records: map[string][]string{
		"dmarc-reports._report._dmarc.example.net": {"v=DMARC1"},
	}}
	results := verifyWithDNS("example.com",
		[]string{"mailto:dmarc-reports@example.net"}, dns)

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if r.Status != ReportAuthorized {
		t.Errorf("Status = %v, want ReportAuthorized (%q)", r.Status, r.Reason)
	}
	if r.Address != "dmarc-reports@example.net" {
		t.Errorf("Address = %q, want %q", r.Address, "dmarc-reports@example.net")
	}
	if len(dns.queries) != 1 || dns.queries[0] != "dmarc-reports._report._dmarc.example.net" {
		t.Errorf("queries = %v, want [dmarc-reports._report._dmarc.example.net]", dns.queries)
	}
}

func TestVerifyReportURIsInternal(t *testing.T) {
	dns := &fakeDNS{}
	results := verifyWithDNS("example.com", []string{
		"mailto:dmarc@example.com",
		"mailto:dmarc@reports.example.com",
	}, dns)

	for i, r := range results {
		if r.Status != ReportAuthorized {
			t.Errorf("results[%d].Status = %v, want ReportAuthorized (%q)", i, r.Status, r.Reason)
		}
	}
	if len(dns.queries) != 0 {
		t.Errorf("expected no DNS queries for internal destinations, got %v", dns.queries)
	}
}

func TestVerifyReportURIsMissingAuthorization(t *testing.T) {
	dns := &fakeDNS{}
	results := verifyWithDNS("example.com",
		[]string{"mailto:dmarc@example.net"}, dns)

	if results[0].Status != ReportUnauthorized {
		t.Errorf("Status = %v, want ReportUnauthorized", results[0].Status)
	}
	if !strings.Contains(results[0].Reason, "no report authorization record") {
		t.Errorf("Reason = %q, want it to mention the missing record", results[0].Reason)
	}
}

func TestVerifyReportURIsWrongRecord(t *testing.T) {
	dns := &fakeDNS{records: map[string][]string{
		"dmarc._report._dmarc.example.net": {"v=spf1 include:example.net ~all"},
	}}
	results := verifyWithDNS("example.com",
		[]string{"mailto:dmarc@example.net"}, dns)

	if results[0].Status != ReportUnauthorized {
		t.Errorf("Status = %v, want ReportUnauthorized", results[0].Status)
	}
	if !strings.Contains(results[0].Reason, "no valid report authorization record") {
		t.Errorf("Reason = %q, want it to mention the invalid record", results[0].Reason)
	}
}

func TestVerifyReportURIsTempFail(t *testing.T) {
	dns := &fakeDNS{errs: map[string]error{
		"dmarc._report._dmarc.example.net": tempDNSError{"server failure"},
	}}
	results := verifyWithDNS("example.com",
		[]string{"mailto:dmarc@example.net"}, dns)

	if results[0].Status != ReportTempFail {
		t.Errorf("Status = %v, want ReportTempFail", results[0].Status)
	}
	if results[0].Reason == "" {
		t.Error("expected a reason for the temporary failure")
	}
}

func TestVerifyReportURIsDuplicates(t *testing.T) {
	dns := &fakeDNS{records: map[string][]string{
		"dmarc._report._dmarc.example.net": {"v=DMARC1"},
	}}
	uris := []string{
		"mailto:dmarc@example.net",
		"mailto:dmarc@example.net!10m",
		"mailto:dmarc@example.net",
	}
	results := verifyWithDNS("example.com", uris, dns)

	if len(results) != len(uris) {
		t.Fatalf("expected %d results, got %d", len(uris), len(results))
	}
	for i, r := range results {
		if r.Status != ReportAuthorized {
			t.Errorf("results[%d].Status = %v, want ReportAuthorized", i, r.Status)
		}
		if r.URI != uris[i] {
			t.Errorf("results[%d].URI = %q, want %q", i, r.URI, uris[i])
		}
	}
	if results[1].MaxSize != 10e6 {
		t.Errorf("results[1].MaxSize = %d, want %d", results[1].MaxSize, int64(10e6))
	}
	if len(dns.queries) != 1 {
		t.Errorf("expected 1 DNS query for duplicate destinations, got %v", dns.queries)
	}
}

func TestVerifyReportURIsEscapedLocalPart(t *testing.T) {
	dns := &fakeDNS{records: map[string][]string{
		"dmarc+reports._report._dmarc.example.net": {"v=DMARC1"},
	}}
	results := verifyWithDNS("example.com",
		[]string{"mailto:dmarc%2Breports@example.net"}, dns)

	if results[0].Status != ReportAuthorized {
		t.Errorf("Status = %v, want ReportAuthorized (%q)", results[0].Status, results[0].Reason)
	}
	if results[0].Address != "dmarc+reports@example.net" {
		t.Errorf("Address = %q, want %q", results[0].Address, "dmarc+reports@example.net")
	}
}

func TestVerifyReportURIsInvalid(t *testing.T) {
	dns := &fakeDNS{}
	results := verifyWithDNS("example.com", []string{
		"https://reports.example.net/dmarc",
		"mailto:dmarc",
		"mailto:dmarc%zz@example.net",
	}, dns)

	for i, r := range results {
		if r.Status != ReportUnauthorized {
			t.Errorf("results[%d].Status = %v, want ReportUnauthorized", i, r.Status)
		}
		if r.Reason == "" {
			t.Errorf("results[%d]: expected a reason", i)
		}
	}
	if len(dns.queries) != 0 {
		t.Errorf("expected no DNS queries for invalid URIs, got %v", dns.queries)
	}
}

func TestVerifyReportURIsSizeLimits(t *testing.T) {
	tests := []struct {
		uri     string
		addr    string
		maxSize int64
	}{
		{"mailto:dmarc@example.com", "dmarc@example.com", 0},
		{"mailto:dmarc@example.com!512", "dmarc@example.com", 512},
		{"mailto:dmarc@example.com!10k", "dmarc@example.com", 10e3},
		{"mailto:dmarc@example.com!50m", "dmarc@example.com", 50e6},
		{"mailto:dmarc@example.com!2g", "dmarc@example.com", 2e9},
		{"mailto:dmarc@example.com!1t", "dmarc@example.com", 1e12},
	}
	for _, test := range tests {
		results := verifyWithDNS("example.com", []string{test.uri}, &fakeDNS{})
		r := results[0]
		if r.Status != ReportAuthorized {
			t.Errorf("%s: Status = %v, want ReportAuthorized", test.uri, r.Status)
		}
		if r.Address != test.addr {
			t.Errorf("%s: Address = %q, want %q", test.uri, r.Address, test.addr)
		}
		if r.MaxSize != test.maxSize {
			t.Errorf("%s: MaxSize = %d, want %d", test.uri, r.MaxSize, test.maxSize)
		}
	}
}

func TestRecordVerifyReportDestinations(t *testing.T) {
	rec, err := Parse("v=DMARC1; p=reject; " +
		"rua=mailto:agg@example.net,mailto:agg@example.com; " +
		"ruf=mailto:fail@example.net")
	if err != nil {
		t.Fatal(err)
	}

	dns := &fakeDNS{records: map[string][]string{
		"agg._report._dmarc.example.net":  {"v=DMARC1"},
		"fail._report._dmarc.example.net": {"v=DMARC1"},
	}}
	results := rec.VerifyReportDestinations("example.com",
		&LookupOptions{LookupTXT: dns.lookupTXT})

	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	for i, r := range results {
		if r.Status != ReportAuthorized {
			t.Errorf("results[%d].Status = %v, want ReportAuthorized (%q)", i, r.Status, r.Reason)
		}
	}
}

func TestParseReportSizeInvalid(t *testing.T) {
	for _, s := range []string{"", "m", "10x", "-5", "99999999999999999999t"} {
		if _, ok := parseReportSize(s); ok {
			t.Errorf("parseReportSize(%q) succeeded, want failure", s)
		}
	}
}

var _ net.Error = tempDNSError{}
