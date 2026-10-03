package arc

import (
	"github.com/emersion/go-msgauth/dkim"
)

// KeyResolver retrieves the public key used by an ARC set. It is called with
// the "d=" (domain) and "s=" (selector) tags of an AMS or AS.
//
// Implementations may return an error: temporary failures are detected with
// [dkim.IsTempFail] (or [IsTempFail]), permanent failures with
// [dkim.IsPermFail] (or [IsPermFail]). Any other error is treated as a
// permanent failure.
type KeyResolver interface {
	LookupARCKey(domain, selector string) (*dkim.PublicKey, error)
}

// The KeyResolverFunc type is an adapter to allow the use of ordinary
// functions as KeyResolvers.
type KeyResolverFunc func(domain, selector string) (*dkim.PublicKey, error)

func (f KeyResolverFunc) LookupARCKey(domain, selector string) (*dkim.PublicKey, error) {
	return f(domain, selector)
}

type dnsResolver struct {
	lookupTXT func(domain string) ([]string, error)
}

func (r dnsResolver) LookupARCKey(domain, selector string) (*dkim.PublicKey, error) {
	return dkim.LookupKey(domain, selector, r.lookupTXT)
}

// defaultResolver returns the DNS-based resolver, using the DKIM key record
// published at <selector>._domainkey.<domain> as a TXT record.
func defaultResolver(lookupTXT func(domain string) ([]string, error)) KeyResolver {
	return dnsResolver{lookupTXT: lookupTXT}
}

func fetchKey(resolver KeyResolver, domain, selector string) (*dkim.PublicKey, error) {
	if domain == "" || selector == "" {
		return nil, permFailError("signature missing domain or selector")
	}
	key, err := resolver.LookupARCKey(domain, selector)
	if err != nil {
		return nil, asPermFail(err)
	}
	if key == nil {
		return nil, permFailError("no public key found")
	}
	return key, nil
}
