package middleware

import (
	"net/http"
	"path"

	"github.com/go-chi/chi/v5"
)

// CleanPath middleware will clean out double slash mistakes from a user's request path.
// For example, if a user requests /users//1 or //users////1 will both be treated as: /users/1
func CleanPath(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rctx := chi.RouteContext(r.Context())

		routePath := rctx.RoutePath
		if routePath == "" {
			if rctx.PathStrategy() == chi.PathStrategyUnescaped {
				// Clean the escaped form of the path so that escaped
				// slashes (%2F) are not treated as segment boundaries.
				routePath = r.URL.EscapedPath()
			} else if r.URL.RawPath != "" {
				routePath = r.URL.RawPath
			} else {
				routePath = r.URL.Path
			}
			rctx.RoutePath = path.Clean(routePath)
		}

		next.ServeHTTP(w, r)
	})
}
