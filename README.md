# go-webdav

[![Go Reference](https://pkg.go.dev/badge/github.com/emersion/go-webdav.svg)](https://pkg.go.dev/github.com/emersion/go-webdav)

A Go library for [WebDAV], [CalDAV] and [CardDAV].

The server backends optionally support WebDAV Collection Synchronization
([RFC 6578]): a `sync-collection` REPORT with an empty token performs the
initial full sync, and subsequent REPORTs with the returned token provide
incremental added, modified and deleted entries plus a new token. Backends
opt in by implementing `webdav.SyncCollectionFileSystem`,
`caldav.SyncCalendarBackend` or `carddav.SyncAddressBookBackend`; backends
that don't keep their original capability declaration.

## License

MIT

[WebDAV]: https://tools.ietf.org/html/rfc4918
[CalDAV]: https://tools.ietf.org/html/rfc4791
[CardDAV]: https://tools.ietf.org/html/rfc6352
[RFC 6578]: https://www.rfc-editor.org/rfc/rfc6578
