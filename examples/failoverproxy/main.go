// Command failoverproxy runs a forward HTTP proxy that chains two upstream
// proxy lines. Each request uses a preferred line and fails over to the backup
// only during the connection / proxy-authentication handshake, before any
// request body byte has been committed. Each line carries its own credential,
// and Proxy-Authorization never propagates past that line's hop.
//
// Per-request preferred line: set the X-Upstream-Preferred header to a
// comma-separated list of line names, e.g. "backup,primary".
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/elazarl/goproxy"
)

type lineConfig struct {
	Name     string `json:"name"`
	Addr     string `json:"addr"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type config struct {
	Listen             string       `json:"listen"`
	Lines              []lineConfig `json:"lines"`
	MaxBufferedBody    int64        `json:"max_buffered_body_bytes"`
	DialTimeout        string       `json:"dial_timeout"`
	AuthProbeTimeout   string       `json:"auth_probe_timeout"`
}

func main() {
	configPath := flag.String("config", "config.json", "path to the failover proxy configuration file")
	verbose := flag.Bool("v", false, "log per-request activity")
	flag.Parse()

	raw, err := os.ReadFile(*configPath)
	if err != nil {
		log.Fatalf("read config: %v", err)
	}
	var cfg config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		log.Fatalf("parse config: %v", err)
	}
	if len(cfg.Lines) == 0 {
		log.Fatal("config must define at least one upstream line")
	}

	lines := make([]goproxy.UpstreamLine, 0, len(cfg.Lines))
	for _, l := range cfg.Lines {
		if l.Name == "" || l.Addr == "" {
			log.Fatal("each line requires a name and an addr")
		}
		lines = append(lines, goproxy.UpstreamLine{
			Name:     l.Name,
			Addr:     l.Addr,
			Username: l.Username,
			Password: l.Password,
		})
	}

	options := []goproxy.FailoverOption{}
	if cfg.MaxBufferedBody > 0 {
		options = append(options, goproxy.WithMaxBufferedBody(cfg.MaxBufferedBody))
	}
	if d, err := time.ParseDuration(cfg.DialTimeout); err == nil && d > 0 {
		options = append(options, goproxy.WithDialTimeout(d))
	}
	if d, err := time.ParseDuration(cfg.AuthProbeTimeout); err == nil && d > 0 {
		options = append(options, goproxy.WithAuthProbeTimeout(d))
	}

	failover := goproxy.NewUpstreamFailover(lines, options...)

	proxy := goproxy.NewProxyHttpServer()
	proxy.Verbose = *verbose
	failover.Attach(proxy)

	listen := cfg.Listen
	if listen == "" {
		listen = ":8080"
	}
	log.Printf("failover proxy listening on %s; lines:", listen)
	for _, l := range cfg.Lines {
		log.Printf("  - %s -> %s (auth=%t)", l.Name, l.Addr, l.Username != "" || l.Password != "")
	}
	log.Fatal(http.ListenAndServe(listen, proxy))
}
