# HTTP Digest Access Authentication

[![go.dev reference](https://img.shields.io/badge/go.dev-reference-007d9c?logo=go&logoColor=white&style=flat-square)](https://pkg.go.dev/github.com/icholy/digest)

> This package provides a http.RoundTripper implementation which re-uses digest challenges

``` go
package main

import (
	"net/http"

	"github.com/icholy/digest"
)

func main() {
	client := &http.Client{
		Transport: &digest.Transport{
			Username: "foo",
			Password: "bar",
		},
	}
	res, err := client.Get("http://localhost:8080/some_outdated_service")
	if err != nil {
		panic(err)
	}
	defer res.Body.Close()
}
```

## Using Cookies

If you're using an `http.CookieJar` the `digest.Transport` needs a reference to it.

``` go
package main

import (
	"net/http"
	"net/http/cookiejar"

	"github.com/icholy/digest"
)

func main() {
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Transport: &digest.Transport{
			Jar:      jar,
			Username: "foo",
			Password: "bar",
		},
	}
	res, err := client.Get("http://localhost:8080/digest_with_cookies")
	if err != nil {
		panic(err)
	}
	defer res.Body.Close()
}
```

## Custom Authenticate Header

``` go
package main

import (
	"net/http"

	"github.com/icholy/digest"
)

func main() {
	client := &http.Client{
		Transport: &digest.Transport{
			Username: "foo",
			Password: "bar",
			FindChallenge: func(h http.Header) (*digest.Challenge, error) {
				value := h.Get("Custom-Authenticate-Header")
				if value == "" {
					return nil, digest.ErrNoChallenge
				}
				return digest.ParseChallenge(value)
			},
		},
	}
	res, err := client.Get("http://localhost:8080/non_compliant")
	if err != nil {
		panic(err)
	}
	defer res.Body.Close()
}
```

## Override Digest Options

``` go
package main

import (
	"fmt"
	"net/http"

	"github.com/icholy/digest"
)

func main() {
	client := &http.Client{
		Transport: &digest.Transport{
			Digest: func(req *http.Request, chal *digest.Challenge, opt digest.Options) (*digest.Credentials, error) {
				switch req.URL.Hostname() {
				case "badauth.org":
					opt.Username = "foo"
					opt.Password = "bar"
				case "poorsecurity.com":
					opt.Username = "zoo"
					opt.Password = "boo"
				default:
					return nil, fmt.Errorf("unsuported host: %q", req.URL)
				}
				return digest.Digest(chal, opt)
			},
		},
	}
	res, err := client.Get("http://poorsecurity.com/legacy.php")
	if err != nil {
		panic(err)
	}
	defer res.Body.Close()
}
```


## Large Request Bodies (Probe)

By default the first (unauthenticated) request already carries the body, because
that request is what surfaces the `401` challenge. Most servers drain that body
before replying, so the only cost is sending the body twice. Some servers,
however, answer the `401` immediately and close the connection *without* draining
the body — with a large body the write then fails with a broken pipe before the
challenge is ever read.

Set `Probe` to discover the challenge with a bodyless request first, so the
body is only ever sent on the authenticated request:

``` go
package main

import (
	"net/http"
	"os"

	"github.com/icholy/digest"
)

func main() {
	client := &http.Client{
		Transport: &digest.Transport{
			Username: "foo",
			Password: "bar",
			Probe:    true,
		},
	}
	f, _ := os.Open("large.bin")
	defer f.Close()
	res, err := client.Post("http://localhost:8080/upload", "application/octet-stream", f)
	if err != nil {
		panic(err)
	}
	defer res.Body.Close()
}
```

In the normal flow `Probe` costs no extra round-trip: the bodyless request
simply replaces the unauthenticated request that would otherwise have carried
the body. If the server answers the bodyless request with something other than
a `401`, that response is discarded and the original request is sent as-is.
Once a challenge is cached, requests go out authenticated on the first try as
usual.

## Low Level API

## Verifying Server Responses

Some servers prove their identity in the `Authentication-Info` response
header using `rspauth`. By default the transport ignores that proof. Set
`VerifyResponse` to opt in:

``` go
client := &http.Client{
    Transport: &digest.Transport{
        Username:       "foo",
        Password:       "bar",
        VerifyResponse: digest.VerifyStrict, // require a correct rspauth
    },
}
```

Two modes are available:

- `digest.VerifyCompat` accepts responses without an `rspauth` proof, but
  rejects responses whose proof is wrong (`digest.ErrRspAuthMismatch`).
- `digest.VerifyStrict` requires the proof on every authenticated response;
  a missing proof fails with `digest.ErrRspAuthMissing`.

The proof is always bound to this request's `nonce`, `nc`, `cnonce`, `URI`,
qop and digest algorithm. With `qop=auth` it is checked before the response
is returned. With `qop=auth-int` the response body is wrapped and the entity
digest is computed while streaming: the verdict arrives at `EOF`, so closing
the body early reports `digest.ErrRspAuthMissing`; underlying read errors are
passed through unchanged. The body is streamed through a single read and never
buffered whole. `nextnonce` is ignored.

``` go
func main() {
  // get the challenge from a 401 response
  header := res.Header.Get("WWW-Authenticate")
  chal, _ := digest.ParseChallenge(header)

  // use it to create credentials for the next request
  cred, _ := digest.Digest(chal, digest.Options{
    Username: "foo",
    Password: "bar",
    Method:   req.Method,
    URI:      req.URL.RequestURI(),
    GetBody:  req.GetBody,
    Count:    1,
  })
  req.Header.Set("Authorization", cred.String())

  // if you use the same challenge again, you must increment the Count
  cred2, _ := digest.Digest(chal, digest.Options{
    Username: "foo",
    Password: "bar",
    Method:   req2.Method,
    URI:      req2.URL.RequestURI(),
    GetBody:  req2.GetBody,
    Count:    2,
  })
  req2.Header.Set("Authorization", cred.String())
}
```
