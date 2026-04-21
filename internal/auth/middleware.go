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
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimPrefix(h, prefix)
}
