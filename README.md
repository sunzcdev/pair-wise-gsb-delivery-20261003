# HyperLogLog - an algorithm for approximating the number of distinct elements

[![CI](https://github.com/axiomhq/hyperloglog/actions/workflows/ci.yml/badge.svg)](https://github.com/axiomhq/hyperloglog/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/axiomhq/hyperloglog.svg)](https://pkg.go.dev/github.com/axiomhq/hyperloglog)
[![Go Report Card](https://goreportcard.com/badge/github.com/axiomhq/hyperloglog)](https://goreportcard.com/report/github.com/axiomhq/hyperloglog)

An improved version of [HyperLogLog](https://en.wikipedia.org/wiki/HyperLogLog) for the count-distinct problem, approximating the number of distinct elements in a multiset. This implementation offers enhanced performance, flexibility, and simplicity while maintaining accuracy.

## Note on Implementation History

The initial version of this work (tagged as v0.1.0) was based on ["Better with fewer bits: Improving the performance of cardinality estimation of large data streams - Qingjun Xiao, You Zhou, Shigang Chen"](https://www.cise.ufl.edu/~sgchen/Publications/XZC17.pdf). However, the current implementation has evolved significantly from this original basis, notably moving away from the tailcut method.

## Current Implementation

The current implementation is based on the LogLog-Beta algorithm, as described in:

["LogLog-Beta and More: A New Algorithm for Cardinality Estimation Based on LogLog Counting"](https://arxiv.org/pdf/1612.02284) by Jason Qin, Denys Kim, and Yumei Tung (2016).

Key features of the current implementation:
* **Metro hash** used instead of xxhash
* **Sparse representation** for lower cardinalities (like HyperLogLog++)
* **LogLog-Beta** for dynamic bias correction across all cardinalities
* **8-bit registers** for convenience and simplified implementation
* **Order-independent insertions and merging** for consistent results regardless of data input order
* **Removal of tailcut method** for a more straightforward approach
* **Flexible precision** allowing for 2^4 to 2^18 registers

This implementation is now more straightforward, efficient, and flexible, while remaining backwards compatible with previous versions. It provides a balance between precision, memory usage, speed, and ease of use.

## Precision and Memory Usage

This implementation allows for creating HyperLogLog sketches with arbitrary precision between 2^4 and 2^18 registers. The memory usage scales with the number of registers:

* Minimum (2^4 registers): 16 bytes
* Default (2^14 registers): 16 KB
* Maximum (2^18 registers): 256 KB

Users can choose the precision that best fits their use case, balancing memory usage against estimation accuracy.

## Streaming Frames

To store or transport more than one sketch in a single file or stream, the
package provides a self-delimiting outer frame around the existing binary
encoding. A frame has a fixed header followed by the unchanged inner encoding,
so historical inner encoding versions keep parsing unchanged:

* a 4 byte magic (`HLLF`) that marks the start of a frame
* a 1 byte outer frame version
* a 4 byte big endian payload length
* a 4 byte big endian CRC-32 (IEEE) checksum of the payload
* the payload produced by `MarshalBinary`

Use `(*Sketch).WriteTo` to write a frame and `(*Sketch).ReadFrom` to read one
frame and replace the receiver. Reading a concatenated stream is a loop that
stops on `io.EOF`; the declared payload length is capped
(`DefaultMaxFramePayloadSize`, overridable through `ReadFrame`) so a hostile
length cannot force a large allocation. Truncation, an unknown frame version,
and a checksum mismatch are reported as distinct errors, and the receiver is
only replaced after both the checksum and the inner encoding have validated.

The checksum detects accidental corruption only. It is a CRC, not a message
authentication code, so it does not protect against an adversary who can modify
the frame.

## Note
A big thank you to Prof. Shigang Chen and his team at the University of Florida who are actively conducting research around "Big Network Data".

## Contributing

Kindly check our [contributing guide](https://github.com/axiomhq/hyperloglog/blob/main/Contributing.md) on how to propose bugfixes and improvements, and submit pull requests to the project.

Run the same checks used by CI before opening a pull request:

```sh
make ci
```

Individual targets are available for formatting (`make fmt`), linting
(`make lint`), tests (`make test`), race-enabled tests (`make test-race`), and
coverage (`make coverage`).

Version tags matching `v*.*.*` are validated by the release workflow before it
publishes a GitHub Release with generated release notes. Tags with a pre-release
suffix, such as `v1.0.0-rc.1`, publish as pre-releases.

## License

&copy; Axiom, Inc., 2026

Distributed under the MIT License (`The MIT License`).

See [LICENSE](LICENSE) for more information.
