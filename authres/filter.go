package authres

import "strings"

// EntryTrust describes how a message entered the local system. It is provided
// by the caller, who knows the circumstances of reception (e.g. connection
// metadata), and is used to decide which pre-existing Authentication-Results
// header fields may be relied upon.
type EntryTrust struct {
	// FromTrustedRelay reports whether the message was received from a
	// relay that is trusted to faithfully pass along Authentication-Results
	// header fields (e.g. authenticated by IP address, TLS or SMTP AUTH).
	// If false, all pre-existing header fields are discarded.
	FromTrustedRelay bool
}

// Policy is a receiving-side policy deciding which pre-existing
// Authentication-Results header fields are trustworthy enough to be passed
// along to downstream consumers.
type Policy struct {
	// AuthServID is the authserv-id of the local authentication service, as
	// used when adding new Authentication-Results header fields.
	AuthServID string

	// TrustedUpstreams lists authserv-ids of upstream services whose
	// Authentication-Results header fields are trusted, provided the
	// message was received from a trusted relay.
	TrustedUpstreams []string
}

// HeaderDecision is the outcome of evaluating a single Authentication-Results
// header field against a Policy.
type HeaderDecision struct {
	// Header is the raw header field value that was evaluated.
	Header string
	// Identifier is the authserv-id parsed from the header field.
	Identifier string
	// Results are the authentication results parsed from the header field.
	// They are nil if the header field was malformed or discarded.
	Results []Result
	// Keep reports whether the header field may be passed downstream.
	Keep bool
	// Reason is a human-readable explanation of the decision.
	Reason string
}

// Filter evaluates a list of Authentication-Results header field values
// found in a received message, and decides for each of them whether it may
// be kept for downstream consumers.
//
// A header field is only kept if the message entered the system via a
// trusted relay and the header field's authserv-id is listed in
// Policy.TrustedUpstreams. Header fields carrying the local authserv-id are
// always discarded: a sender can trivially forge them, so a string match
// against the local identifier is not proof of origin. The local service is
// expected to add a fresh header field of its own.
//
// The result is purely a trust decision about pre-existing header fields.
// Keeping a header field does not mean the local system has performed or
// re-verified any DKIM, SPF or other authentication check itself.
func (p *Policy) Filter(headers []string, trust EntryTrust) []HeaderDecision {
	decisions := make([]HeaderDecision, 0, len(headers))
	for _, h := range headers {
		decisions = append(decisions, p.evaluate(h, trust))
	}
	return decisions
}

func (p *Policy) evaluate(header string, trust EntryTrust) HeaderDecision {
	d := HeaderDecision{Header: header}

	identifier, results, err := Parse(header)
	if err != nil {
		d.Reason = "malformed Authentication-Results header field"
		return d
	}
	d.Identifier = identifier

	if p.AuthServID != "" && strings.EqualFold(identifier, p.AuthServID) {
		d.Reason = "header field claims the local authserv-id, possibly forged"
		return d
	}

	if !trust.FromTrustedRelay {
		d.Reason = "message was not received from a trusted relay"
		return d
	}

	if !p.trustedUpstream(identifier) {
		d.Reason = "authserv-id is not a trusted upstream"
		return d
	}

	d.Results = results
	d.Keep = true
	d.Reason = "authserv-id is a trusted upstream"
	return d
}

func (p *Policy) trustedUpstream(identifier string) bool {
	for _, id := range p.TrustedUpstreams {
		if strings.EqualFold(identifier, id) {
			return true
		}
	}
	return false
}
