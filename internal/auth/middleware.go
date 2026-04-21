package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// Require wraps next with a constant-time bearer-token check against the
// configured pairing token. Empty/missing/mismatched gets a 401.
func Require(expected string, next http.Handler) http.Handler {
	exp := []byte(expected)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := bearerToken(r)
		if got == "" || subtle.ConstantTimeCompare([]byte(got), exp) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	// Prefer X-Zephyr-Auth — URLSessionWebSocketTask on iOS strips the standard
	// Authorization header from WebSocket upgrade handshakes, so clients that
	// care about WS endpoints send both and we accept either.
	if h := r.Header.Get("X-Zephyr-Auth"); strings.HasPrefix(h, prefix) {
		return strings.TrimPrefix(h, prefix)
	}
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimPrefix(h, prefix)
}
