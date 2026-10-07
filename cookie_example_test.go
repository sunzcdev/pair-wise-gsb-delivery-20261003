package dns_test

import (
	"fmt"
	"net"

	"github.com/miekg/dns"
)

func ExampleServerCookieHandler() {
	// The key ring is shared across all clients; no per-client session is
	// kept. Rotate secrets by calling Rotate: old cookies continue to
	// validate until the old secret is retired with Replace.
	secret, err := dns.GenerateServerCookieSecret()
	if err != nil {
		panic(err)
	}
	ring, err := dns.NewCookieKeyRing(secret)
	if err != nil {
		panic(err)
	}
	// A nil clock means wall-clock time; tests can inject a fixed clock.
	cookies := dns.NewServerCookieHandler(ring, nil)

	// Inside the request handler, the caller owns response policy. On UDP a
	// missing, expired or bad server cookie gets RcodeBadCookie with only the
	// question section and a freshly minted server cookie; on TCP a bad
	// cookie is ignored. The size of the BADCOOKIE response is the caller's
	// decision, keeping it small avoids amplifying spoofed-source traffic.

	answer := func(req *dns.Msg, remote *net.UDPAddr, udp bool) *dns.Msg {
		result, err := cookies.CheckRequest(req, remote)
		if err != nil {
			panic(err)
		}

		resp := new(dns.Msg)
		resp.SetReply(req)
		switch result.Status {
		case dns.ServerCookieMissing, dns.ServerCookieExpired, dns.ServerCookieBad:
			if udp {
				resp.Rcode = dns.RcodeBadCookie
				resp.Answer = nil
				resp.Ns = nil
				resp.Extra = nil
			}
		}

		opt := resp.IsEdns0()
		if opt == nil {
			resp.SetEdns0(512, false)
			opt = resp.IsEdns0()
		}
		if option := result.Option(); option != nil {
			opt.Option = append(opt.Option, option)
		}
		return resp
	}

	_ = answer
	fmt.Println(cookies.KeyRing.SecretCount())
	// Output: 1
}
