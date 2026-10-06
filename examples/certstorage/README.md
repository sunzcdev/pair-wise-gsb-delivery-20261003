# CertStorage

CertStorage example is important to improve the performance of an
HTTPS proxy server, which you can build using goproxy.
Without a `proxy.CertStore`, every HTTPS request will generate new TLS
certificates and this, repeated for hundreds of request, will destroy your CPU.

A lot of people opened issues in the projects complaining about this, because
they didn't use a certificates cache.
The cache implementation is up to you, maybe you can cache only the
most used hostnames, if you want to.

## Rotating the CA

If you replace the MITM CA at runtime with `goproxy.CAConfig` and
`ReplaceCA`, new TLS handshakes are signed by the new CA immediately while
already established connections keep using their old session.

The cache distinguishes certificates by the signing CA generation, so a
certificate signed by the old CA can never be served for the new CA and old
generations are reclaimed on rotation. Implement `goproxy.GenerationCertStorage`
(`FetchForSigner`) to make a custom cache generation-aware. Existing
`goproxy.CertStorage` implementations (just `Fetch`) keep working: goproxy
verifies each cached certificate against the active CA and re-signs stale
entries transparently, without anyone having to clear the cache.
