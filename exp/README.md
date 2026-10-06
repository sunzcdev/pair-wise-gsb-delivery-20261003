# client_golang experimental module

Contains experimental utilities and APIs for Prometheus.

Packages within this module are listed below.

## Bounded

The `bounded` package provides experimental counter and histogram vectors
with a bounded number of admitted label combinations. The first
`Config.Capacity` distinct label combinations get their own series; all
further observations are merged into a shared overflow series in which the
configured collapsible labels carry a reserved overflow value
(`__overflow__` by default). Totals are preserved, so counters and histogram
counts and sums stay correct even when label cardinality explodes (e.g. when
request parameters leak into label values).

```go
    import (
        "github.com/prometheus/client_golang/exp/bounded"
        "github.com/prometheus/client_golang/prometheus"
    )
    ...

    requests := bounded.NewCounterVec(
        prometheus.CounterOpts{Name: "http_requests_total", Help: "HTTP requests."},
        []string{"code", "request"},
        bounded.Config{Capacity: 1000, CollapsibleLabels: []string{"request"}},
    )
    prometheus.MustRegister(requests)
```

Deleting an admitted combination frees its capacity slot. The overflow value
is reserved in collapsible label positions, so the overflow series can never
collide with a legitimate user series.

## Remote

This module contains production quality code with explicitly unstable API. Any code in this module can change its API or be removed; use with care.

The intention is that, with maturity some of the packages would graduate to stable version of client_golang module.

Packages within this module are listed below.

## Remote

NOTE: The `api/remote` package is used by `prometheus/prometheus`. Any changes to this package should ensure Prometheus is not affected (e.g. does not break Prometheus on upgrade or if it breaks, the Prometheus gets updated soon.
Implements bindings from Prometheus remote APIs (remote write v1 and v2 for now).

Contains flexible method for building API clients, that can send remote write protocol messages.

```go
    import (
        "github.com/prometheus/client_golang/exp/api/remote"
    )
    ...

	remoteAPI, err := remote.NewAPI(
		"https://your-remote-endpoint",
		remote.WithAPIHTTPClient(httpClient),
		remote.WithAPILogger(logger.With("component", "remote_write_api")),
	)
    ...

    stats, err := remoteAPI.Write(ctx, remote.WriteV2MessageType, protoWriteReq)
```

Also contains handler methods for applications that would like to handle and store remote write requests.

```go
    import (
        "net/http"
        "log"

        "github.com/prometheus/client_golang/exp/api/remote"
    )
    ...
    
    type db {}

    func NewStorage() *db {}

    func (d *db) Store(ctx context.Context, msgType remote.WriteMessageType, req *http.Request) (*remote.WriteResponse, error) {}
    ...

	mux := http.NewServeMux()

	remoteWriteHandler := remote.NewHandler(storage, remote.WithHandlerLogger(logger.With("component", "remote_write_handler")))
	mux.Handle("/api/v1/write", remoteWriteHandler)

	server := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
```

For more details, see [go doc](https://pkg.go.dev/github.com/prometheus/client_golang/exp/api/remote).
