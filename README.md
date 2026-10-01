sftp
----

The `sftp` package provides support for file system operations on remote ssh
servers using the SFTP subsystem. It also implements an SFTP server for serving
files from the filesystem.

![CI Status](https://github.com/pkg/sftp/workflows/CI/badge.svg?branch=master&event=push) [![Go Reference](https://pkg.go.dev/badge/github.com/pkg/sftp.svg)](https://pkg.go.dev/github.com/pkg/sftp)

usage and examples
------------------

See [https://pkg.go.dev/github.com/pkg/sftp](https://pkg.go.dev/github.com/pkg/sftp) for
examples and usage.

The basic operation of the package mirrors the facilities of the
[os](http://golang.org/pkg/os) package.

The Walker interface for directory traversal is heavily inspired by Keith
Rarick's [fs](https://pkg.go.dev/github.com/kr/fs) package.

roadmap
-------

* There is way too much duplication in the Client methods. If there was an
  unmarshal(interface{}) method this would reduce a heap of the duplication.

sparse file transfer
--------------------

Large backup images usually contain holes: regions that logically read as
zero bytes but occupy no disk blocks. Copying them as a plain byte stream
wastes bandwidth and forces the destination to allocate blocks for every
hole.

The package implements a negotiable `sparse-file@ietf.org` extension. Each
side independently declares whether it can *discover* allocated data regions
(`query-allocated@ietf.org`) and whether it can recreate holes
(`allocate-regions@ietf.org`). The ability set is exchanged on
SSH_FXP_INIT/SSH_FXP_VERSION, so a peer without the extension simply
completes an ordinary transfer.

Use `Client.SparseCapabilities` to inspect a server. A high-level
`CopySparseFile` copies between two files (local or remote), skipping only
real filesystem holes. Explicitly allocated data that happens to be all zeros
is still transmitted, and the logical length — including a trailing hole — is
preserved. `SparseCopyStats.BytesTransferred` reports the payload that
actually crossed the wire.

Local filesystem support is provided by `OSSparseFile` and reported by
`LocalSparseCapabilities`: full discover+allocate on Linux (SEEK_DATA/
SEEK_HOLE + fallocate) and Windows NTFS (FSCTL_QUERY_ALLOCATED_RANGES /
FSCTL_SET_ZERO_DATA), allocate-only on macOS, and an automatic plain-copy
fallback elsewhere.

An end-to-end demo lives in
[examples/sparse-transfer](examples/sparse-transfer/main.go).

contributing
------------

We welcome pull requests, bug fixes and issue reports.

Before proposing a large change, first please discuss your change by raising an
issue.

For API/code bugs, please include a small, self contained code example to
reproduce the issue. For pull requests, remember test coverage.

We try to handle issues and pull requests with a 0 open philosophy. That means
we will try to address the submission as soon as possible and will work toward
a resolution. If progress can no longer be made (eg. unreproducible bug) or
stops (eg. unresponsive submitter), we will close the bug.

Thanks.
