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
	"errors"
	"net"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// DNSCAAQueryer is the default CAAQueryer. It sends plain DNS (UDP,
// with TCP fallback for truncated responses) CAA queries to one or
// more recursive resolvers.
//
// The zero value uses the system's default resolver where available.
// DNSSEC validation, if any, is performed by the resolver; a bogus
// response typically surfaces as an error and is reported as
// "undetermined" rather than allowed.
type DNSCAAQueryer struct {
	// Servers lists recursive resolvers as host or host:port
	// addresses. The first server that answers wins. If empty, the
	// system resolver configuration is used.
	Servers []string

	// Timeout bounds each query attempt. Default: 5s.
	Timeout time.Duration
}

// LookupCAA implements CAAQueryer.
func (q *DNSCAAQueryer) LookupCAA(ctx context.Context, fqdn string) ([]CAARecord, string, error) {
	servers, err := q.servers()
	if err != nil {
		return nil, "", err
	}

	timeout := q.Timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}

	qname := dns.Fqdn(strings.TrimSpace(fqdn))
	var lastErr error

	for _, server := range servers {
		records, cname, err := q.queryServer(ctx, qname, server, timeout)
		if err == nil {
			return records, cname, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no DNS servers configured")
	}
	return nil, "", lastErr
}

func (q *DNSCAAQueryer) queryServer(ctx context.Context, qname, server string, timeout time.Duration) ([]CAARecord, string, error) {
	query := new(dns.Msg)
	query.SetQuestion(qname, dns.TypeCAA)

	dialer := &net.Dialer{Timeout: timeout}
	client := &dns.Client{Net: "udp", Dialer: dialer}

	resp, _, err := client.ExchangeContext(ctx, query, server)
	if err != nil {
		return nil, "", err
	}
	if resp.Truncated {
		tcpClient := &dns.Client{Net: "tcp", Dialer: dialer}
		resp, _, err = tcpClient.ExchangeContext(ctx, query, server)
		if err != nil {
			return nil, "", err
		}
	}

	switch resp.Rcode {
	case dns.RcodeSuccess, dns.RcodeNameError:
		// NXDOMAIN means no records here; the climb continues.
	default:
		return nil, "", errors.New(dns.RcodeToString[resp.Rcode])
	}

	cname := qname
	var records []CAARecord
	for _, rr := range resp.Answer {
		switch typed := rr.(type) {
		case *dns.CAA:
			records = append(records, parseCAARecord(typed.Tag, typed.Value, typed.Flag&1 == 1))
		case *dns.CNAME:
			cname = strings.TrimSuffix(typed.Target, ".")
		}
	}
	if cname == qname {
		cname = strings.TrimSuffix(qname, ".")
	}
	return records, cname, nil
}

func (q *DNSCAAQueryer) servers() ([]string, error) {
	var servers []string
	for _, server := range q.Servers {
		servers = append(servers, withDNSPort(server))
	}
	if len(servers) > 0 {
		return servers, nil
	}

	systemServers, err := systemResolvers()
	if err != nil {
		return nil, err
	}
	for _, server := range systemServers {
		servers = append(servers, withDNSPort(server))
	}
	return servers, nil
}

func withDNSPort(server string) string {
	if _, _, err := net.SplitHostPort(server); err == nil {
		return server
	}
	return net.JoinHostPort(server, "53")
}
