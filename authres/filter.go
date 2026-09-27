package authres

import (
	"fmt"
	"strings"
)

// TrustedUpstream describes an upstream authentication service whose
// Authentication-Results header fields this gateway is willing to relay
// downstream.
type TrustedUpstream struct {
	// AuthServID is the authserv-id the upstream stamps on its
	// Authentication-Results header fields.
	AuthServID string

	// Methods lists the authentication methods whose results may be
	// relayed (e.g. "dkim", "spf"). If empty, results for all methods
	// are relayed, including extension methods unknown to this package.
	Methods []string
}

func (u *TrustedUpstream) allowsMethod(method string) bool {
	if len(u.Methods) == 0 {
		return true
	}
	for _, m := range u.Methods {
		if strings.EqualFold(m, method) {
			return true
		}
	}
	return false
}

// IngressContext carries trust facts about how a message entered the local
// trust domain. These facts are established by the receiver at reception
// time (e.g. from the connection or the Received header chain), never from
// data supplied by the message itself.
type IngressContext struct {
	// PeerAuthServID is the authserv-id of the immediate upstream that
	// delivered this message, as verified by the receiver.
	PeerAuthServID string
}

// IngressPolicy is a receiving-side policy that decides which incoming
// Authentication-Results header fields may be passed downstream.
//
// The policy only makes a trust decision about existing header fields: it
// relays results asserted by trusted upstreams and discards the rest. It
// does not perform, and does not claim to have performed, any DKIM, SPF or
// other authentication verification itself.
type IngressPolicy struct {
	// AuthServID is this gateway's own authserv-id. Any incoming header
	// field carrying it is treated as an impersonation attempt, since a
	// sender can trivially forge a header field with the local name.
	AuthServID string

	// TrustedUpstreams lists the upstreams whose results may be relayed.
	TrustedUpstreams []TrustedUpstream
}

// FilteredHeader is the trust decision for a single incoming
// Authentication-Results header field.
type FilteredHeader struct {
	// Value is the raw header field value.
	Value string

	// Identifier is the authserv-id carried by the header field.
	Identifier string

	// Results are the authentication results to relay downstream. When
	// the trusted upstream is restricted to a subset of methods, results
	// for other methods are stripped. Nil when Keep is false.
	Results []Result

	// Keep reports whether the header field may be passed downstream.
	Keep bool

	// Reason is a human-readable explanation of the decision.
	Reason string
}

// Filter evaluates each Authentication-Results header field value against
// the policy and returns one decision per value, in the same order.
//
// A header field is only kept if its authserv-id belongs to a configured
// trusted upstream and the message was actually received from that upstream
// (ctx.PeerAuthServID). A string match against the whitelist alone is never
// sufficient: header fields naming this gateway's own authserv-id, naming a
// trusted upstream the message did not come from, or naming an unknown
// service are removed.
func (p *IngressPolicy) Filter(ctx *IngressContext, values []string) []FilteredHeader {
	decisions := make([]FilteredHeader, 0, len(values))
	for _, v := range values {
		decisions = append(decisions, p.filterOne(ctx, v))
	}
	return decisions
}

func (p *IngressPolicy) filterOne(ctx *IngressContext, v string) FilteredHeader {
	d := FilteredHeader{Value: v}

	identifier, results, err := Parse(v)
	if err != nil {
		d.Reason = fmt.Sprintf("malformed Authentication-Results header field: %v", err)
		return d
	}
	d.Identifier = identifier

	if identifier == "" {
		d.Reason = "missing authserv-id"
		return d
	}

	if strings.EqualFold(identifier, p.AuthServID) {
		d.Reason = "authserv-id impersonates this gateway; a sender can forge " +
			"a header field with the local name, so it cannot be trusted"
		return d
	}

	upstream := p.trustedUpstream(identifier)
	if upstream == nil {
		d.Reason = "authserv-id is not a trusted upstream"
		return d
	}

	if ctx == nil || !strings.EqualFold(ctx.PeerAuthServID, upstream.AuthServID) {
		d.Reason = fmt.Sprintf("authserv-id %q is trusted, but the message was not "+
			"received from that upstream; the header field may be forged", identifier)
		return d
	}

	kept := results[:0]
	var stripped []string
	for _, r := range results {
		if upstream.allowsMethod(resultMethod(r)) {
			kept = append(kept, r)
		} else {
			stripped = append(stripped, resultMethod(r))
		}
	}

	if len(kept) == 0 && len(results) > 0 {
		d.Reason = fmt.Sprintf("trusted upstream, but none of its methods (%s) "+
			"are allowed by policy", strings.Join(stripped, ", "))
		return d
	}

	d.Keep = true
	d.Results = kept
	d.Reason = fmt.Sprintf("received from trusted upstream %q", upstream.AuthServID)
	if len(stripped) > 0 {
		d.Reason += fmt.Sprintf("; stripped results for methods not allowed by policy: %s",
			strings.Join(stripped, ", "))
	}
	return d
}

func (p *IngressPolicy) trustedUpstream(identifier string) *TrustedUpstream {
	for i := range p.TrustedUpstreams {
		if strings.EqualFold(p.TrustedUpstreams[i].AuthServID, identifier) {
			return &p.TrustedUpstreams[i]
		}
	}
	return nil
}
