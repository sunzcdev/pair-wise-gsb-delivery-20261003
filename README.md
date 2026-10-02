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

## Hash Domains

Registers only make sense relative to the hash function that produced them.
`Insert` pins that hash to MetroHash64 with seed 1337; callers that feed
pre-hashed values with `InsertHash` and a custom algorithm or seed used to
share a type that could not tell two hash domains apart, and merging such
sketches silently mixed unrelated register values.

`DomainSketch` wraps a `Sketch` with a `HashConfig`: an algorithm name, a
seed, and a caller chosen domain tag. `NewDomainSketch` records the domain,
`Merge` rejects sketches whose domain differs, and `MarshalBinary` writes the
algorithm, seed and tag alongside the registers. The hash function itself is
a pointer and is never serialized; the reading side reconstructs the sketch
with `UnmarshalDomainBinary` and supplies the matching config:

```go
cfg := hyperloglog.MetroHashConfig(1337, "login-events")
sk, _ := hyperloglog.NewDomainSketch(14, true, cfg)

data, _ := sk.MarshalBinary()

// On another process, with the same algorithm, seed and domain tag.
restored, err := hyperloglog.UnmarshalDomainBinary(data, hyperloglog.MetroHashConfig(1337, "login-events"))
```

Callers with their own hash use `CustomHashConfig`. A non default config must
carry a non empty domain tag. Plain binary written by older versions records
no domain: load it with `LegacyDomainSketch`, which marks the data as an
unspecified domain. Such sketches merge with each other but never with a
declared domain, since an unknown hash cannot be assumed equal to a known
one.

Users can choose the precision that best fits their use case, balancing memory usage against estimation accuracy.

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
