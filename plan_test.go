package ws

import (
	"bufio"
	"bytes"
	"errors"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/gobwas/httphead"
)

func validPlanRequest() *http.Request {
	req := mustMakeRequest("GET", "ws://example.org", http.Header{
		headerUpgradeCanonical:    []string{"websocket"},
		headerConnectionCanonical: []string{"Upgrade"},
		headerSecVersionCanonical: []string{"13"},
	})
	req.Header[headerSecKeyCanonical] = []string{string(mustMakeNonce())}
	return req
}

func rereadRequest(t *testing.T, req *http.Request) *http.Request {
	t.Helper()
	parsed, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(dumpRequest(req))))
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestPreviewSuccess(t *testing.T) {
	var protocolCalls int32
	var negotiateCalls int32

	u := HTTPUpgrader{
		Protocol: func(string) bool {
			atomic.AddInt32(&protocolCalls, 1)
			return true
		},
		Negotiate: func(opt httphead.Option) (httphead.Option, error) {
			atomic.AddInt32(&negotiateCalls, 1)
			return opt.Clone(), nil
		},
	}

	req := validPlanRequest()
	req.Header[headerSecProtocolCanonical] = []string{"chat"}
	req.Header[headerSecExtensionsCanonical] = []string{"permessage-deflate"}

	plan, err := u.Preview(req)
	if err != nil {
		t.Fatalf("Preview() error: %v", err)
	}
	if plan.Protocol != "chat" {
		t.Errorf("protocol = %q, want %q", plan.Protocol, "chat")
	}
	if len(plan.Extensions) != 1 || string(plan.Extensions[0].Name) != "permessage-deflate" {
		t.Fatalf("extensions = %v, want permessage-deflate", plan.Extensions)
	}

	// Preview must not hijack nor write anything.
	rec := newRecorder()
	if rec.hijacked {
		t.Fatal("Preview hijacked the response writer")
	}

	// The actual upgrade must not invoke the negotiation callbacks again.
	req = rereadRequest(t, req)
	conn, _, hs, err := plan.Upgrade(req, rec)
	if err != nil {
		t.Fatalf("plan.Upgrade() error: %v", err)
	}
	defer conn.Close()

	if got := atomic.LoadInt32(&protocolCalls); got != 1 {
		t.Errorf("Protocol callback called %d times, want 1", got)
	}
	if got := atomic.LoadInt32(&negotiateCalls); got != 1 {
		t.Errorf("Negotiate callback called %d times, want 1", got)
	}
	if hs.Protocol != "chat" {
		t.Errorf("upgraded protocol = %q, want %q", hs.Protocol, "chat")
	}

	act := sortHeaders(rec.Bytes())
	if !bytes.Contains(act, []byte("101 Switching Protocols")) {
		t.Fatalf("response is not 101:\n%s", act)
	}
	if !bytes.Contains(act, []byte(headerSecProtocol+": chat")) {
		t.Fatalf("response missing negotiated protocol:\n%s", act)
	}
	if !bytes.Contains(act, []byte(headerSecExtensions+": permessage-deflate")) {
		t.Fatalf("response missing negotiated extension:\n%s", act)
	}
}

func TestPreviewRejects(t *testing.T) {
	u := HTTPUpgrader{}

	cases := []struct {
		name string
		// mutate mutates the request before it is serialized and parsed back,
		// so it can also remove headers.
		mutate func(*http.Request)
		want   error
		status int
	}{
		{"bad_method", func(r *http.Request) { r.Method = http.MethodPost }, ErrHandshakeBadMethod, 405},
		{"bad_protocol", func(r *http.Request) { setProto(1, 0, r) }, ErrHandshakeBadProtocol, 505},
		{"bad_host", func(r *http.Request) { withoutHeader(headerHost, r) }, ErrHandshakeBadHost, 400},
		{"bad_upgrade", func(r *http.Request) { r.Header[headerUpgradeCanonical] = []string{"h2c"} }, ErrHandshakeBadUpgrade, 400},
		{"bad_connection", func(r *http.Request) { r.Header[headerConnectionCanonical] = []string{"keep-alive"} }, ErrHandshakeBadConnection, 400},
		{"bad_seckey", func(r *http.Request) { r.Header[headerSecKeyCanonical] = []string{"short"} }, ErrHandshakeBadSecKey, 400},
		{"bad_secversion", func(r *http.Request) { r.Header[headerSecVersionCanonical] = []string{"12"} }, ErrHandshakeUpgradeRequired, 426},
		{"missing_secversion", func(r *http.Request) { delete(r.Header, headerSecVersionCanonical) }, ErrHandshakeBadSecVersion, 400},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := validPlanRequest()
			tc.mutate(req)

			plan, err := u.Preview(req)
			if plan != nil {
				t.Fatalf("Preview() returned plan %v, want nil", plan)
			}
			var rej *HandshakeRejection
			if !errors.As(err, &rej) {
				t.Fatalf("Preview() error = %T %v, want *HandshakeRejection", err, err)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("errors.Is(err, %v) = false; got %v", tc.want, err)
			}
			if rej.Status != tc.status {
				t.Errorf("rejection status = %d, want %d", rej.Status, tc.status)
			}

			// Direct upgrade on the very same request must reject with the
			// same status.
			parsed := rereadRequest(t, req)
			rec := newRecorder()
			_, _, _, upErr := u.Upgrade(parsed, rec)
			if upErr == nil {
				t.Fatal("direct Upgrade() error = nil, want rejection")
			}
			if !bytes.Contains(rec.Bytes(), []byte(" "+strconv.Itoa(tc.status)+" ")) {
				t.Fatalf("direct upgrade response status is not %d:\n%s", tc.status, rec.Bytes())
			}
		})
	}
}

func TestPlanUpgradeStaleOnModifiedField(t *testing.T) {
	u := HTTPUpgrader{}

	req := validPlanRequest()
	plan, err := u.Preview(req)
	if err != nil {
		t.Fatalf("Preview() error: %v", err)
	}

	// Mutate a field that participates in validation after planning.
	req.Header[headerUpgradeCanonical] = []string{"h2c"}

	rec := newRecorder()
	_, _, _, err = plan.Upgrade(req, rec)
	if !errors.Is(err, ErrHandshakePlanStale) {
		t.Fatalf("plan.Upgrade() error = %v, want ErrHandshakePlanStale", err)
	}
	if rec.hijacked {
		t.Fatal("stale plan must not hijack the connection")
	}

	// Restoring the field must not help: the plan was rejected as stale but
	// remains usable only if the request again matches; ensure it was not
	// consumed by the failed attempt.
	req.Header[headerUpgradeCanonical] = []string{"websocket"}
	rec = newRecorder()
	conn, _, _, err := plan.Upgrade(rereadRequest(t, req), rec)
	if err != nil {
		t.Fatalf("plan.Upgrade() after restore error: %v", err)
	}
	conn.Close()
}

func TestPlanUpgradeSingleUse(t *testing.T) {
	u := HTTPUpgrader{}

	req := validPlanRequest()
	plan, err := u.Preview(req)
	if err != nil {
		t.Fatalf("Preview() error: %v", err)
	}

	firstReq := rereadRequest(t, req)
	conn, _, _, err := plan.Upgrade(firstReq, newRecorder())
	if err != nil {
		t.Fatalf("first plan.Upgrade() error: %v", err)
	}
	conn.Close()

	// Second consumption must be refused.
	secondReq := rereadRequest(t, req)
	_, _, _, err = plan.Upgrade(secondReq, newRecorder())
	if !errors.Is(err, ErrHandshakePlanConsumed) {
		t.Fatalf("second plan.Upgrade() error = %v, want ErrHandshakePlanConsumed", err)
	}
}

func TestPlanUpgradeIgnoresUnboundHeader(t *testing.T) {
	u := HTTPUpgrader{}

	req := validPlanRequest()
	plan, err := u.Preview(req)
	if err != nil {
		t.Fatalf("Preview() error: %v", err)
	}

	// Modifying a header that does not participate in handshake validation
	// must not invalidate the plan.
	req.Header.Set("X-Application-Trace", "abc")

	rec := newRecorder()
	conn, _, _, err := plan.Upgrade(rereadRequest(t, req), rec)
	if err != nil {
		t.Fatalf("plan.Upgrade() error: %v", err)
	}
	conn.Close()
	if !bytes.Contains(rec.Bytes(), []byte("101 Switching Protocols")) {
		t.Fatalf("response is not 101:\n%s", rec.Bytes())
	}
}

func TestPreviewCustomResponseHeader(t *testing.T) {
	u := HTTPUpgrader{
		Header: http.Header{
			"X-Test": []string{"1"},
		},
	}
	req := validPlanRequest()

	plan, err := u.Preview(req)
	if err != nil {
		t.Fatalf("Preview() error: %v", err)
	}
	if plan.Header == nil {
		t.Fatal("plan.Header is nil, want custom header")
	}

	req = rereadRequest(t, req)
	rec := newRecorder()
	conn, _, _, err := plan.Upgrade(req, rec)
	if err != nil {
		t.Fatalf("plan.Upgrade() error: %v", err)
	}
	conn.Close()

	if !bytes.Contains(rec.Bytes(), []byte("X-Test: 1")) {
		t.Fatalf("response missing custom header:\n%s", rec.Bytes())
	}
}

func TestPreviewNegotiateRejectionStatusMatchesDirectUpgrade(t *testing.T) {
	u := HTTPUpgrader{
		Negotiate: func(httphead.Option) (httphead.Option, error) {
			return httphead.Option{}, RejectConnectionError(
				RejectionStatus(http.StatusForbidden),
				RejectionReason("nope"),
			)
		},
	}

	req := validPlanRequest()
	req.Header[headerSecExtensionsCanonical] = []string{"permessage-deflate"}

	_, err := u.Preview(req)
	var rej *HandshakeRejection
	if !errors.As(err, &rej) || rej.Status != http.StatusForbidden {
		t.Fatalf("Preview() rejection = %v, want 403", err)
	}
	if err.Error() != "nope" {
		t.Fatalf("Preview() reason = %q, want %q", err.Error(), "nope")
	}

	req = rereadRequest(t, req)
	rec := newRecorder()
	_, _, _, err = u.Upgrade(req, rec)
	if err == nil {
		t.Fatal("direct Upgrade() error = nil, want rejection")
	}
	if !bytes.Contains(rec.Bytes(), []byte(" 403 ")) {
		t.Fatalf("direct upgrade response status is not 403:\n%s", rec.Bytes())
	}
}
