package tlscert

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
)

// proofLabel separates this HMAC from any other use of the pairing token.
const proofLabel = "zephyr-helper-tls-v1"

// Nonce bounds, in hex characters. The app sends 32 random bytes.
const (
	minNonceLen = 32
	maxNonceLen = 128
)

// Proof shows that whoever serves the certificate with this fingerprint
// also holds the pairing token.
func Proof(token, fingerprint, nonce string) string {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte(proofLabel + "\n" + fingerprint + "\n" + nonce))
	return hex.EncodeToString(mac.Sum(nil))
}

// ProofHandler answers GET /tls?nonce=… with the certificate's fingerprint
// and a Proof for it.
//
// The app asks over HTTPS and checks the proof against the fingerprint it
// saw in that handshake, so it can pin the certificate without sending the
// token over a connection it hasn't verified. A proxy in the middle has its
// own certificate and no token, so it can't produce a proof for it, and
// relaying the helper's proof doesn't help because that proof names the
// helper's certificate. The same check lets the app re-pin on its own when
// the certificate legitimately changes, such as after a lost data volume.
//
// It needs no bearer token, which is the point, and hands out nothing that
// isn't already public: the fingerprint is in every handshake, and the HMAC
// doesn't reveal its key.
type ProofHandler struct {
	Token       string
	Fingerprint string
}

func (h ProofHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	nonce := r.URL.Query().Get("nonce")
	if !validNonce(nonce) {
		http.Error(w, "nonce must be 32 to 128 hex characters", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"fingerprint_sha256": h.Fingerprint,
		"proof":              Proof(h.Token, h.Fingerprint, nonce),
	})
}

func validNonce(nonce string) bool {
	if len(nonce) < minNonceLen || len(nonce) > maxNonceLen {
		return false
	}
	_, err := hex.DecodeString(nonce)
	return err == nil
}
