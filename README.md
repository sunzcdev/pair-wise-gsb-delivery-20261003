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

When the compressed data is available as an `io.ReaderAt` together
with its size (for instance an `*os.File` or a `*bytes.Reader`),
`NewRandomReader` provides random access without decompressing the
whole file. It scans the stream footers and indexes backwards, so it
supports multiple concatenated streams and the legal inter-stream
padding. Index CRC-32 checksums, footer and header checksums, block
headers and all size relations are validated when the reader is
created.

```go
rr, err := xz.NewRandomReader(f, size)
if err != nil {
    log.Fatalf("NewRandomReader error %s", err)
}
p := make([]byte, 100)
// reads and verifies only the blocks covering [5000, 5100)
n, err := rr.ReadAt(p, 5000)
```

`ReadAt` maps the logical uncompressed offset to the correct block and
returns blocks in order when the requested interval spans more than
one block. A read block is fully decompressed and its block check
(CRC-32, CRC-64 or SHA-256) is verified before the requested bytes are
returned. Blocks that the request doesn't touch are not decompressed
and their content checks are not verified.

`RandomReaderConfig` limits the memory used for indexes
(`MaxIndexMemory`), LZMA2 dictionaries (`MaxDictCap`) and a single
decompressed block (`MaxBlockMemory`); files that announce overflowing
or contradictory lengths are rejected.

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
