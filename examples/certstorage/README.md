# CertStorage

CertStorage example is important to improve the performance of an
HTTPS proxy server, which you can build using goproxy.
Without a `proxy.CertStore`, every HTTPS request will generate new TLS
certificates and this, repeated for hundreds of request, will destroy your CPU.

A lot of people opened issues in the projects complaining about this, because
they didn't use a certificates cache.
The cache implementation is up to you, maybe you can cache only the
most used hostnames, if you want to.

## CA generations (rotation)

When the signing CA is rotated at runtime (via `goproxy.NewRotatingCA` and
`ReplaceCertificate`), this cache keeps certificates in a separate bucket per
CA generation. A leaf signed by the old CA can therefore never be served after
the switch, and `DropGeneration` releases an old bucket once every connection
that used it has finished, so repeatedly rotating the CA does not leak a
permanent extra map each time.
