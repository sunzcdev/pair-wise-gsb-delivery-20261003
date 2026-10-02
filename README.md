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

### Streaming the first flush

By default, responses smaller than `MinSize` are buffered and only handed to
the client once they grow large enough (or, at the end, sent uncompressed), so
a `Flush()` before that point is a no-op. For streaming endpoints you can
choose what the first `Flush()` should do via `StreamingFlush`:

- `gziphandler.FlushAsGzip` — commit to gzip immediately and send the bytes
  buffered so far (possibly none) as a gzip stream. This is useful for
  long-lived responses that may never reach `MinSize`.
- `gziphandler.FlushAsPlain` — commit to an uncompressed passthrough response
  and send the buffered bytes immediately.

The encoding chosen by that first flush is fixed for the rest of the response.
The flush returns an error through `http.ResponseController.Flush` (or the
`FlushError` method) if the underlying flush fails:

```go
wrapper, err := gziphandler.GzipHandlerWithOpts(
	gziphandler.MinSize(1400),
	gziphandler.StreamingFlush(gziphandler.FlushAsGzip),
)
if err != nil {
	log.Fatal(err)
}
handler := wrapper(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, "partial response")
	// This emits the bytes as gzip right away, even though they are small.
	if err := http.NewResponseController(w).Flush(); err != nil {
		log.Print(err)
	}
}))
```


## Documentation

The docs can be found at [godoc.org][docs], as usual.


## License

[Apache 2.0][license].




[docs]:     https://godoc.org/github.com/NYTimes/gziphandler
[license]:  https://github.com/NYTimes/gziphandler/blob/master/LICENSE
