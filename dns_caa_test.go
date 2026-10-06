// Copyright 2020 Matthew Holt
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package acmez

import (
	"context"
	"net"
	"testing"

	"github.com/mholt/acmez/v3/acme"
	"github.com/miekg/dns"
)

// startLocalDNSServer starts a UDP DNS server on a loopback address
// that answers from the given handler, so the queryer test never
// reaches the real DNS.
func startLocalDNSServer(t *testing.T, handle func(w dns.ResponseWriter, m *dns.Msg)) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("binding UDP socket: %v", err)
	}
	mux := dns.NewServeMux()
	mux.HandleFunc(".", handle)
	server := &dns.Server{PacketConn: conn, Handler: mux}
	go func() { _ = server.ActivateAndServe() }()
	t.Cleanup(func() { _ = server.Shutdown() })
	return conn.LocalAddr().String()
}

func caaRR(name string, flag uint8, tag, value string) *dns.CAA {
	return &dns.CAA{
		Hdr:   dns.RR_Header{Name: dns.Fqdn(name), Rrtype: dns.TypeCAA, Class: dns.ClassINET, Ttl: 300},
		Flag:  flag,
		Tag:   tag,
		Value: value,
	}
}

func TestDNSCAAQueryerSuccess(t *testing.T) {
	server := startLocalDNSServer(t, func(w dns.ResponseWriter, m *dns.Msg) {
		resp := new(dns.Msg)
		resp.SetReply(m)
		if m.Question[0].Name == dns.Fqdn("alias.example.com") {
			resp.Answer = append(resp.Answer,
				&dns.CNAME{
					Hdr:    dns.RR_Header{Name: dns.Fqdn("alias.example.com"), Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 300},
					Target: dns.Fqdn("target.example.net"),
				},
				caaRR("target.example.net", 0, "issue", "letsencrypt.org; validationmethods=dns-01"),
			)
		}
		_ = w.WriteMsg(resp)
	})

	q := &DNSCAAQueryer{Servers: []string{server}}
	records, cname, err := q.LookupCAA(context.Background(), "alias.example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cname != "target.example.net" {
		t.Fatalf("cname = %q, want target.example.net", cname)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	rec := records[0]
	if rec.Tag != "issue" {
		t.Fatalf("tag = %q, want issue", rec.Tag)
	}
	if len(rec.ValidationMethods) != 1 || rec.ValidationMethods[0] != "dns-01" {
		t.Fatalf("validation methods = %v, want [dns-01]", rec.ValidationMethods)
	}
}

func TestDNSCAAQueryerNXDOMAIN(t *testing.T) {
	server := startLocalDNSServer(t, func(w dns.ResponseWriter, m *dns.Msg) {
		resp := new(dns.Msg)
		resp.SetRcode(m, dns.RcodeNameError)
		_ = w.WriteMsg(resp)
	})

	q := &DNSCAAQueryer{Servers: []string{server}}
	records, _, err := q.LookupCAA(context.Background(), "missing.example.com")
	if err != nil {
		t.Fatalf("NXDOMAIN should not be an error: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("expected no records, got %d", len(records))
	}
}

func TestDNSCAAQueryerServfail(t *testing.T) {
	server := startLocalDNSServer(t, func(w dns.ResponseWriter, m *dns.Msg) {
		resp := new(dns.Msg)
		resp.SetRcode(m, dns.RcodeServerFailure)
		_ = w.WriteMsg(resp)
	})

	q := &DNSCAAQueryer{Servers: []string{server}}
	if _, _, err := q.LookupCAA(context.Background(), "fail.example.com"); err == nil {
		t.Fatal("SERVFAIL must be returned as an error")
	}
}

func TestDNSCAAQueryerIntegration(t *testing.T) {
	server := startLocalDNSServer(t, func(w dns.ResponseWriter, m *dns.Msg) {
		resp := new(dns.Msg)
		resp.SetReply(m)
		switch m.Question[0].Name {
		case dns.Fqdn("www.example.com"):
			// no records; climb continues
		case dns.Fqdn("example.com"):
			resp.Answer = append(resp.Answer, caaRR("example.com", 0, "issue", "other-ca.example"))
		default:
			resp.Rcode = dns.RcodeNameError
		}
		_ = w.WriteMsg(resp)
	})

	checker := &CAAChecker{
		Queryer:       &DNSCAAQueryer{Servers: []string{server}},
		CAAIdentities: []string{"letsencrypt.org"},
	}
	results := checker.CheckResults(context.Background(), []CAACheckRequest{{
		Identifier: acme.Identifier{Type: "dns", Value: "www.example.com"},
	}})
	if results[0].Status != CAAStatusDenied {
		t.Fatalf("status = %q (%s), want denied", results[0].Status, results[0].Reason)
	}
}
