// Command lab spins up two fake upstream proxies (each with its own
// credentials), a target service that records received headers, and the
// failover proxy in front of them. It is a self-contained playground for
// observing the failover boundaries and Proxy-Authorization scoping.
//
// Usage:
//
//	go run .
//
// The process prints its ports and stays up until interrupted. Exercise it
// with any HTTP client using the failover proxy as a forward proxy.
package main

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/elazarl/goproxy"
)

type fakeProxy struct {
	name     string
	user     string
	pass     string
	refuse   bool
	auth407  bool
	ln       net.Listener
	mu       sync.Mutex
	hits     int
	authSeen []string
}

func (f *fakeProxy) auth() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(f.user+":"+f.pass))
}

func (f *fakeProxy) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(c)
	}
}

func (f *fakeProxy) handle(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	auth := req.Header.Get("Proxy-Authorization")
	f.mu.Lock()
	f.hits++
	f.authSeen = append(f.authSeen, auth)
	f.mu.Unlock()

	deny := func() {
		io.WriteString(c, "HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"lab\"\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
	}

	if f.auth407 || auth != f.auth() {
		if req.Method == http.MethodConnect {
			deny()
			return
		}
		if strings.EqualFold(req.Header.Get("Expect"), "100-continue") {
			deny()
			return
		}
	}

	if req.Method == http.MethodConnect {
		target, err := net.Dial("tcp", req.Host)
		if err != nil {
			fmt.Fprintf(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
			return
		}
		defer target.Close()
		io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
		done := make(chan struct{}, 2)
		go func() { io.Copy(target, c); done <- struct{}{} }()
		go func() { io.Copy(c, target); done <- struct{}{} }()
		<-done
		return
	}

	body, _ := io.ReadAll(req.Body)
	target, err := net.Dial("tcp", req.Host)
	if err != nil {
		fmt.Fprintf(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	defer target.Close()
	req.RequestURI = ""
	req.Header.Del("Proxy-Authorization")
	req.Header.Del("Expect")
	req.Body = io.NopCloser(strings.NewReader(string(body)))
	req.ContentLength = int64(len(body))
	req.Header.Set("Connection", "close")
	if len(body) == 0 {
		req.Body = http.NoBody
	}
	req.Write(target)
	resp, _ := http.ReadResponse(bufio.NewReader(target), req)
	resp.Header.Set("Connection", "close")
	resp.Write(c)
}

func startFake(name, user, pass string, refuse, auth407 bool) *fakeProxy {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	f := &fakeProxy{name: name, user: user, pass: pass, refuse: refuse, auth407: auth407, ln: ln}
	if refuse {
		// Reserve the port, then release it so dials are refused.
		_ = ln.Close()
		return f
	}
	go f.serve()
	return f
}

type recordedHeaders struct {
	mu      sync.Mutex
	entries []http.Header
}

func (r *recordedHeaders) add(h http.Header) {
	r.mu.Lock()
	r.entries = append(r.entries, h.Clone())
	r.mu.Unlock()
}

func main() {
	mode := flag.String("mode", "ok", "primary behavior: ok|refuse|auth407")
	flag.Parse()

	records := &recordedHeaders{}
	targetHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		records.add(r.Header)
		log.Printf("TARGET %s %s proxy-auth=%q preferred=%q", r.Method, r.URL.Path,
			r.Header.Get("Proxy-Authorization"), r.Header.Get(goproxy.UpstreamPreferenceHeader))
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "echo:%s", b)
	})
	httpTarget := httptest.NewServer(targetHandler)
	defer httpTarget.Close()
	tlsTarget := httptest.NewTLSServer(targetHandler)
	defer tlsTarget.Close()

	primary := startFake("primary", "puser", "ppass", *mode == "refuse", *mode == "auth407")
	backup := startFake("backup", "buser", "bpass", false, false)

	failover := goproxy.NewUpstreamFailover([]goproxy.UpstreamLine{
		{Name: "primary", Addr: primary.ln.Addr().String(), Username: "puser", Password: "ppass"},
		{Name: "backup", Addr: backup.ln.Addr().String(), Username: "buser", Password: "bpass"},
	},
		goproxy.WithAuthProbeTimeout(500*time.Millisecond),
		goproxy.WithDialTimeout(2*time.Second))

	front := goproxy.NewProxyHttpServer()
	front.Verbose = true
	failover.Attach(front)
	frontLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	go (&http.Server{Handler: front}).Serve(frontLn)

	client := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(mustURL("http://" + frontLn.Addr().String())),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}

	run := func(label, targetURL string, header http.Header) {
		req, _ := http.NewRequest(http.MethodPost, targetURL, strings.NewReader("hello-body"))
		if header != nil {
			req.Header = header
		}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("[%s] error: %v", label, err)
			return
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		log.Printf("[%s] status=%d body=%q primary.hits=%d backup.hits=%d primary.auth=%v backup.auth=%v",
			label, resp.StatusCode, string(data), primary.hits, backup.hits, safeAuths(primary.authSeen), safeAuths(backup.authSeen))
	}

	run("http", strings.Replace(httpTarget.URL, "127.0.0.1", "localhost", 1)+"/r", nil)
	run("https", strings.Replace(tlsTarget.URL, "127.0.0.1", "localhost", 1)+"/r", nil)
	pref := http.Header{}
	pref.Set(goproxy.UpstreamPreferenceHeader, "backup")
	run("http-pref-backup", strings.Replace(httpTarget.URL, "127.0.0.1", "localhost", 1)+"/r", pref)

	log.Printf("failover proxy at %s (mode=%s); http target %s ; tls target %s",
		frontLn.Addr().String(), *mode, httpTarget.URL, tlsTarget.URL)
	log.Print("lab finished its scripted requests; set a breakpoint/keep process to inspect. exiting")
}

func safeAuths(in []string) []string {
	out := make([]string, len(in))
	for i, a := range in {
		if a == "" {
			out[i] = "<none>"
			continue
		}
		if dec, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(a, "Basic ")); err == nil {
			out[i] = "Basic[" + string(dec) + "]"
		} else {
			out[i] = "Basic[?]"
		}
	}
	return out
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		log.Fatal(err)
	}
	return u
}
