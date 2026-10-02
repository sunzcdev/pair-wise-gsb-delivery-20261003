package nosurf

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Eager mode must not emit a duplicate Vary: Cookie header, even when the
// token is regenerated (invalid/missing cookie) after Vary was already added.
func TestEagerModeAddsVaryCookieOnce(t *testing.T) {
	hand := New(http.HandlerFunc(succHand))

	req, err := http.NewRequest("POST", "https://dum.my/", formBodyR(nil))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "dum.my"
	req.Header.Set("Referer", "https://dum.my/")

	rec := httptest.NewRecorder()
	hand.ServeHTTP(rec, req)

	var count int
	for _, v := range rec.Header().Values("Vary") {
		if v == "Cookie" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly one Vary: Cookie header value, got %d", count)
	}
}
