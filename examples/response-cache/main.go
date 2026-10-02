package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/elazarl/goproxy"
)

func main() {
	listenAddr := flag.String("addr", ":8080", "proxy listen address")
	flag.Parse()

	proxy := goproxy.NewProxyHttpServer()
	proxy.Verbose = true

	// The shared GET cache is opt-in. Assigning a CacheConfig enables
	// Cache-Control/Age freshness, ETag/Last-Modified revalidation and Vary,
	// backed by a bounded in-memory LRU store.
	proxy.Cache = &goproxy.CacheConfig{
		MaxResources:        256,
		MaxBodyBytes:        8 << 20, // 8 MiB per response
		RevalidationTimeout: 30 * time.Second,
		// Store is replaceable (implement goproxy.CacheStore).
		// Clock defaults to the wall clock; use goproxy.NewManualClock in tests.
	}

	log.Printf("caching proxy listening on %s", *listenAddr)
	log.Fatal(http.ListenAndServe(*listenAddr, proxy))
}
