Gzip Handler
============

This is a tiny Go package which wraps HTTP handlers to transparently gzip the
response body, for clients which support it. Although it's usually simpler to
leave that to a reverse proxy (like nginx or Varnish), this package is useful
when that's undesirable.

## Install
```bash
go get -u github.com/NYTimes/gziphandler
```

## Usage

Call `GzipHandler` with any handler (an object which implements the
`http.Handler` interface), and it'll return a new handler which gzips the
response. For example:

```go
package main

import (
	"io"
	"net/http"
	"github.com/NYTimes/gziphandler"
)

func main() {
	withoutGz := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "Hello, World")
	})

	withGz := gziphandler.GzipHandler(withoutGz)

	http.Handle("/", withGz)
	http.ListenAndServe("0.0.0.0:8000", nil)
}
```


## Documentation

The docs can be found at [godoc.org][docs], as usual.

## Streaming flushes

By default, responses smaller than `MinSize` are buffered and the first
`Flush()` is a no-op until enough data has been written. `WithFlushStrategy`
makes the first `Flush()` finalize the encoding immediately, so small
streaming responses are not held back:

```go
// Always start gzip on the first Flush, even below MinSize.
h, _ := gziphandler.GzipHandlerWithOpts(
    gziphandler.WithFlushStrategy(gziphandler.FlushGzipImmediately),
)

// Or commit to passthrough (identity) on the first Flush.
h2, _ := gziphandler.GzipHandlerWithOpts(
    gziphandler.WithFlushStrategy(gziphandler.FlushAsIdentity),
)
```

Once the first `Flush()` has chosen gzip or identity, the encoding never
changes for the rest of the response. A custom
`gziphandler.FlushStrategy` receives the number of buffered bytes and may
also return `gziphandler.FlushDefer` to keep buffering.

`GzipResponseWriter` exposes `FlushError() error`, which
`http.ResponseController.Flush` (Go 1.20+) picks up automatically, so
failures while flushing can be observed. The plain `Flush()` method keeps
its previous, error-swallowing behavior.


## License

[Apache 2.0][license].




[docs]:     https://godoc.org/github.com/NYTimes/gziphandler
[license]:  https://github.com/NYTimes/gziphandler/blob/master/LICENSE
