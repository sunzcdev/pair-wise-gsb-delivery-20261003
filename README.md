# nosurf

[![Build Status](https://travis-ci.org/justinas/nosurf.svg?branch=master)](https://travis-ci.org/justinas/nosurf)
[![GoDoc](http://godoc.org/github.com/justinas/nosurf?status.png)](http://godoc.org/github.com/justinas/nosurf)

`nosurf` is an HTTP package for Go
that helps you prevent Cross-Site Request Forgery attacks.
It acts like a middleware and therefore 
is compatible with basically any Go HTTP application.

### Why?
Even though CSRF is a prominent vulnerability,
Go's web-related package infrastructure mostly consists of
micro-frameworks that neither do implement CSRF checks,
nor should they.

`nosurf` solves this problem by providing a `CSRFHandler`
that wraps your `http.Handler` and checks for CSRF attacks
on every non-safe (non-GET/HEAD/OPTIONS/TRACE) method.

`nosurf` requires Go 1.1 or later.

### Features

* Supports any `http.Handler` (frameworks, your own handlers, etc.)
and acts like one itself.
* Allows exempting specific endpoints from CSRF checks by
an exact URL, a glob, or a regular expression.
* Allows specifying your own failure handler. 
Want to present the hacker with an ASCII middle finger
instead of the plain old `HTTP 400`? No problem.
* Uses masked tokens to mitigate the BREACH attack.
* Has no dependencies outside the Go standard library.

### Example
```go
package main

import (
	"fmt"
	"github.com/justinas/nosurf"
	"html/template"
	"net/http"
)

var templateString string = `
<!doctype html>
<html>
<body>
{{ if .name }}
<p>Your name: {{ .name }}</p>
{{ end }}
<form action="/" method="POST">
<input type="text" name="name">

<!-- Try removing this or changing its value
     and see what happens -->
<input type="hidden" name="csrf_token" value="{{ .token }}">
<input type="submit" value="Send">
</form>
</body>
</html>
`
var templ = template.Must(template.New("t1").Parse(templateString))

func myFunc(w http.ResponseWriter, r *http.Request) {
	context := make(map[string]string)
	context["token"] = nosurf.Token(r)
	if r.Method == "POST" {
		context["name"] = r.FormValue("name")
	}
	
	templ.Execute(w, context)
}

func main() {
	myHandler := http.HandlerFunc(myFunc)
	fmt.Println("Listening on http://127.0.0.1:8000/")
	http.ListenAndServe(":8000", nosurf.New(myHandler))
}
```

### Manual token verification
In some cases the CSRF token may be send through a non standard way,
e.g. a body or request is a JSON encoded message with one of the fields
being a token.

In such case the handler(path) should be excluded from an automatic
verification by using one of the exemption methods:

```go
	func (h *CSRFHandler) ExemptFunc(fn func(r *http.Request) bool)
	func (h *CSRFHandler) ExemptGlob(pattern string)
	func (h *CSRFHandler) ExemptGlobs(patterns ...string)
	func (h *CSRFHandler) ExemptPath(path string)
	func (h *CSRFHandler) ExemptPaths(paths ...string)
	func (h *CSRFHandler) ExemptRegexp(re interface{})
	func (h *CSRFHandler) ExemptRegexps(res ...interface{})
```

Later on, the token **must** be verified by manually getting the token from the cookie
and providing the token sent in body through: `VerifyToken(tkn, tkn2 string) bool`.

Example:
```go
func HandleJson(w http.ResponseWriter, r *http.Request) {
	d := struct{
		X,Y int
		Tkn string
	}{}
	json.Unmarshal(ioutil.ReadAll(r.Body), &d)
	if !nosurf.VerifyToken(nosurf.Token(r), d.Tkn) {
		http.Errorf(w, "CSRF token incorrect", http.StatusBadRequest)
		return
	}
	// do smth cool
}
```

### On-demand cookies

By default nosurf sets the CSRF cookie (and a `Vary: Cookie` header) on every
request, including plain `GET` pages that never render a form. If you prefer to
send the cookie only when a token is actually used, enable on-demand issuance:

```go
h := nosurf.New(app)
h.SetCookieOnDemand(true)
```

In this mode a token is generated (or the existing cookie's token reused) and
its cookie is scheduled only when a downstream handler obtains the token via
`nosurf.Token(r)` or `nosurf.EnsureToken(w, r)`. The `Vary: Cookie` header is
added at the same time. Unsafe methods (`POST`, `PUT`, ...) are still fully
verified.

`EnsureToken` is the explicit, error-aware variant of `Token`. If the response
has already been committed (the handler called `WriteHeader`, wrote the body or
flushed) before the token is requested, no `Set-Cookie` header can reach the
browser. Rather than returning a token that would never round-trip,
`EnsureToken` returns `nosurf.ErrResponseAlreadyCommitted` (and `Token` returns
an empty string):

```go
token, err := nosurf.EnsureToken(w, r)
if errors.Is(err, nosurf.ErrResponseAlreadyCommitted) {
    // The response headers are already gone; render the form on the next
    // request instead of relying on a cookie the browser will not receive.
}
```

Repeated calls to `Token`/`EnsureToken` within a single request always return
the same token and schedule the cookie at most once. A custom failure handler
can therefore decide explicitly whether to request a token while producing its
error response.

### Contributing

0. Find an issue that bugs you / open a new one.
1. Discuss.
2. Branch off, commit, test.
3. Make a pull request / attach the commits to the issue.
