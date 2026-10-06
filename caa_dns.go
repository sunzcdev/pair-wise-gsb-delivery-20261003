// Copyright 2026 Matthew Holt
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
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// maxCNAMEHops bounds CNAME chasing when locating a CAA record set.
const maxCNAMEHops = 8

// DNSCAAQuerier is the default CAAQuerier. It queries CAA records over DNS
// using github.com/miekg/dns, following CNAME aliases and classifying the
// response per RFC 8659 §4.7. It talks to a recursive resolver; it does not
// perform its own iterative resolution or DNSSEC validation.
//
// Tests should inject a custom CAAQuerier instead of using this type so that
// no real DNS is contacted.
type DNSCAAQuerier struct {
	// Config describes the recursive resolvers to use. If nil, the system
	// configuration is used (/etc/resolv.conf on Unix-like systems). If
	// that cannot be read, well-known public resolvers are used as a
	// best-effort fallback.
	Config *dns.ClientConfig

	// Timeout bounds a single DNS exchange. The zero value uses 5 seconds.
	Timeout time.Duration

	// Attempts is the number of retries against each configured resolver
	// before moving to the next one. The zero value uses 2.
	Attempts int

	once   sync.Once
	config *dns.ClientConfig
}

// NewDefaultDNSCAAQuerier returns a DNSCAAQuerier that resolves its
// configuration lazily from the operating system.
func NewDefaultDNSCAAQuerier() *DNSCAAQuerier {
	return &DNSCAAQuerier{}
}

// ensureConfig resolves the effective resolver configuration once.
func (q *DNSCAAQuerier) ensureConfig() *dns.ClientConfig {
	q.once.Do(func() {
		switch {
		case q.Config != nil:
			q.config = q.Config
		default:
			if cfg := systemDNSConfig(); cfg != nil && len(cfg.Servers) > 0 {
				q.config = cfg
			} else {
				q.config = &dns.ClientConfig{
					Servers: []string{"1.1.1.1", "8.8.8.8", "9.9.9.9"},
					Port:    "53",
					Timeout: 5,
				}
			}
		}
	})
	return q.config
}

// LookupCAA implements CAAQuerier.
func (q *DNSCAAQuerier) LookupCAA(ctx context.Context, name string) ([]*CAARecord, CAALookupStatus, error) {
	target := dns.Fqdn(name)

	for hop := 0; hop < maxCNAMEHops; hop++ {
		resp, err := q.query(ctx, target)
		if err != nil {
			return nil, CAALookupIndeterminate, err
		}

		switch resp.Rcode {
		case dns.RcodeSuccess:
			var caas []*dns.CAA
			cname := ""
			for _, rr := range resp.Answer {
				switch v := rr.(type) {
				case *dns.CAA:
					caas = append(caas, v)
				case *dns.CNAME:
					if strings.EqualFold(dns.Fqdn(v.Hdr.Name), target) {
						cname = v.Target
					}
				}
			}

			if len(caas) > 0 {
				records := make([]*CAARecord, 0, len(caas))
				for _, rr := range caas {
					rec, err := parseCAARecord(rr.Flag, rr.Tag, rr.Value)
					if err != nil {
						return nil, CAALookupIndeterminate,
							fmt.Errorf("parsing CAA record at %s: %w", name, err)
					}
					records = append(records, &rec)
				}
				return records, CAALookupFound, nil
			}

			if cname != "" {
				// Follow the alias per RFC 8659 §4.4 and look for CAA at
				// the canonical name.
				target = cname
				continue
			}

			// NODATA: authoritative negative answers carry an SOA in the
			// authority section (RFC 2308). Anything else is ambiguous.
			if authorityHasSOA(resp) {
				return nil, CAALookupAuthoritativeEmpty, nil
			}
			return nil, CAALookupIndeterminate, nil

		case dns.RcodeNameError:
			// Authoritative NXDOMAIN: per RFC 8659 §4.7 the tree walk
			// retries at the parent zone.
			return nil, CAALookupNameError, nil

		default:
			label := dns.RcodeToString[resp.Rcode]
			if label == "" {
				label = fmt.Sprintf("rcode %d", resp.Rcode)
			}
			return nil, CAALookupIndeterminate, errors.New("DNS lookup returned " + label)
		}
	}

	return nil, CAALookupIndeterminate, errors.New("too many CNAME redirects resolving " + name)
}

// query performs a single CAA question, trying each configured resolver and
// retrying over TCP when the UDP response is truncated.
func (q *DNSCAAQuerier) query(ctx context.Context, fqdn string) (*dns.Msg, error) {
	cfg := q.ensureConfig()

	question := new(dns.Msg)
	question.SetQuestion(fqdn, dns.TypeCAA)
	question.SetEdns0(4096, false)

	timeout := q.Timeout
	if timeout == 0 {
		if cfg.Timeout > 0 {
			timeout = time.Duration(cfg.Timeout) * time.Second
		} else {
			timeout = 5 * time.Second
		}
	}

	attempts := q.Attempts
	if attempts <= 0 {
		if cfg.Attempts > 0 {
			attempts = cfg.Attempts
		} else {
			attempts = 2
		}
	}

	port := cfg.Port
	if port == "" {
		port = "53"
	}

	var lastErr error
	for _, server := range cfg.Servers {
		addr := net.JoinHostPort(server, port)
		for attempt := 0; attempt < attempts; attempt++ {
			resp, err := exchange(ctx, "udp", addr, question, timeout)
			if err == nil && resp != nil {
				if resp.Truncated {
					tcpResp, tcpErr := exchange(ctx, "tcp", addr, question, timeout)
					if tcpErr == nil {
						return tcpResp, nil
					}
					lastErr = tcpErr
					break // this server failed the TCP fallback; try next
				}
				return resp, nil
			}
			lastErr = err
		}
	}

	if lastErr == nil {
		lastErr = errors.New("no DNS resolvers configured")
	}
	return nil, lastErr
}

// exchange sends one DNS message over the given transport.
func exchange(ctx context.Context, network, addr string, msg *dns.Msg, timeout time.Duration) (*dns.Msg, error) {
	client := &dns.Client{
		Net:     network,
		Timeout: timeout,
	}
	resp, _, err := client.ExchangeContext(ctx, msg, addr)
	if err != nil {
		return nil, err
	}
	if resp == nil || !resp.Response {
		return nil, errors.New("no DNS response from " + addr)
	}
	return resp, nil
}

// authorityHasSOA reports whether the message's authority section contains
// an SOA record, marking an authoritative negative (NODATA) answer.
func authorityHasSOA(msg *dns.Msg) bool {
	for _, rr := range msg.Ns {
		if _, ok := rr.(*dns.SOA); ok {
			return true
		}
	}
	return false
}

var _ CAAQuerier = (*DNSCAAQuerier)(nil)
