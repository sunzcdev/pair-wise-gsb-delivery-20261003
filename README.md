# Package xz

This Go language package supports the reading and writing of xz
compressed streams. It includes also a gxz command for compressing and
decompressing data. The package is completely written in Go and doesn't
have any dependency on any C code.

The package is currently under development. There might be bugs and APIs
are not considered stable. At this time the package cannot compete with
the xz tool regarding compression speed and size. The algorithms there
have been developed over a long time and are highly optimized. However
there are a number of improvements planned and I'm very optimistic about
parallel compression and decompression. Stay tuned!

## Using the API

The following example program shows how to use the API.

```go
package main

import (
    "bytes"
    "io"
    "log"
    "os"

    "github.com/ulikunitz/xz"
)

func main() {
    const text = "The quick brown fox jumps over the lazy dog.\n"
    var buf bytes.Buffer
    // compress text
    w, err := xz.NewWriter(&buf)
    if err != nil {
        log.Fatalf("xz.NewWriter error %s", err)
    }
    if _, err := io.WriteString(w, text); err != nil {
        log.Fatalf("WriteString error %s", err)
    }
    if err := w.Close(); err != nil {
        log.Fatalf("w.Close error %s", err)
    }
    // decompress buffer and write output to stdout
    r, err := xz.NewReader(&buf)
    if err != nil {
        log.Fatalf("NewReader error %s", err)
    }
    if _, err = io.Copy(os.Stdout, r); err != nil {
        log.Fatalf("io.Copy error %s", err)
    }
}
```

## Documentation

You can find the full documentation at [pkg.go.dev](https://pkg.go.dev/github.com/ulikunitz/xz).

## Filter chains

By default only the LZMA2 filter is used. A `WriterConfig` can enable
the Delta filter and the x86 branch/call/jump (BCJ) filter, which are
applied in the order Delta, x86 BCJ, LZMA2 as required by the xz
format. `DeltaDistance` selects the Delta distance (1–256); `BCJ`
enables x86 conversion and `BCJStartOffset` sets its start offset.

```go
cfg := xz.WriterConfig{
    DeltaDistance: 1,
    BCJ:           true,
    BCJStartOffset: 0,
}
w, err := cfg.NewWriter(&buf)
```

Streams containing unsupported filters are rejected with an explicit
error rather than decoded as unfiltered data.

## Using the gxz compression tool

The package includes a gxz command line utility for compression and
decompression.

Use following command for installation:

    $ go get github.com/ulikunitz/xz/cmd/gxz

To test it call the following command.

    $ gxz bigfile

After some time a much smaller file bigfile.xz will replace bigfile.
To decompress it use the following command.

    $ gxz -d bigfile.xz

## Security & Vulnerabilities

The security policy is documented in [SECURITY.md](SECURITY.md). 

The software is not affected by the supply chain attack on the original xz
implementation, [CVE-2024-3094](https://nvd.nist.gov/vuln/detail/CVE-2024-3094).
This implementation doesn't share any files with the original xz implementation
and no patches or pull requests are accepted without a review.

All security advisories for this project are published under
[github.com/ulikunitz/xz/security/advisories](https://github.com/ulikunitz/xz/security/advisories?state=published).
