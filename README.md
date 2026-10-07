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

## Random access

When the underlying data supports `io.ReaderAt` (for example an
`*os.File` or `*bytes.Reader`) and its total size is known, the package
can read arbitrary uncompressed intervals without decompressing the
file from the beginning. The reader locates the streams backwards using
their Footer and Index, supports multiple concatenated streams and the
legal padding between them, and maps a logical uncompressed offset to
the containing block:

```go
sr, err := xz.NewReaderAt(file, size)
if err != nil {
    log.Fatalf("xz.NewReaderAt error %s", err)
}
buf := make([]byte, 1024)
if _, err := sr.ReadAt(buf, 4096); err != nil {
    log.Fatalf("sr.ReadAt error %s", err)
}
```

The index CRC, footer/header flags and the size relations between the
index and the block headers are validated when the reader is created.
The content checksum of a block is verified only once that block is
actually decoded; blocks that are never read are not content-verified.
`ReaderConfig.IndexMemCap` and `ReaderConfig.BlockDictCap` bound the
memory used for indexes and block dictionaries.

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
