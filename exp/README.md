# client_golang experimental module

Contains experimental utilities and APIs for Prometheus.

Packages within this module are listed below.

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

## Exemplar

The `exemplar` package provides a configurable, bounded exemplar reservoir.
Instead of keeping only the most recent trace per bucket, it retains a
bounded set of candidate exemplars observed within a sliding time window and
selects one of them for exposition according to an explicit sampling policy
(newest, uniform random, or weight-proportional random). The clock and the
random source are replaceable, so tests can reproduce selection results.

```go
    import (
        "github.com/prometheus/client_golang/exp/exemplar"
    )
    ...

    reservoir := exemplar.NewReservoir(exemplar.Config{
        MaxCandidates: 16,
        Window:        5 * time.Minute,
        Policy:        exemplar.PolicyUniformRandom,
    })

    reservoir.Observe(exemplar.Exemplar{
        Value:  latencySeconds,
        Labels: map[string]string{"trace_id": traceID},
    })

    if e, ok := reservoir.Exemplar(); ok {
        // Expose e as the exemplar for the current window.
    }
```

For more details, see [go doc](https://pkg.go.dev/github.com/prometheus/client_golang/exp/exemplar).
