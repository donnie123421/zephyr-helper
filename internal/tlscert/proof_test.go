package tlscert

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

const (
	vectorToken       = "example-pairing-token"
	vectorFingerprint = "3f7a0c5e9b1d2f4a6c8e0b2d4f6a8c0e2b4d6f8a0c2e4b6d8f0a2c4e6b8d0f2a"
	vectorNonce       = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
)

// The app computes the same HMAC with CryptoKit; this vector (from
// `openssl dgst -sha256 -hmac`) pins the exact message layout both sides use.
func TestProofMatchesVector(t *testing.T) {
	const want = "05578868b57958bd25a0d5099b89f560f7106127ac4aa9058f7958fb27b5d840"
	if got := Proof(vectorToken, vectorFingerprint, vectorNonce); got != want {
		t.Fatalf("proof = %s, want %s", got, want)
	}
}

func TestProofIsBoundToTheCertificate(t *testing.T) {
	relayed := Proof(vectorToken, vectorFingerprint, vectorNonce)
	otherCert := "aa" + vectorFingerprint[2:]
	if Proof(vectorToken, otherCert, vectorNonce) == relayed {
		t.Fatal("a proof for one certificate must not verify another")
	}
	if Proof("another-token", vectorFingerprint, vectorNonce) == relayed {
		t.Fatal("a proof must depend on the pairing token")
	}
}

func TestProofHandler(t *testing.T) {
	h := ProofHandler{Token: vectorToken, Fingerprint: vectorFingerprint}

	for _, nonce := range []string{"", "abc", "zz" + vectorNonce[2:], vectorNonce[:31], vectorNonce + vectorNonce + vectorNonce + vectorNonce + "00"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tls?nonce="+nonce, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("nonce %q: status %d, want 400", nonce, rec.Code)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tls?nonce="+vectorNonce, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	var body struct {
		Fingerprint string `json:"fingerprint_sha256"`
		Proof       string `json:"proof"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Fingerprint != vectorFingerprint || body.Proof != Proof(vectorToken, vectorFingerprint, vectorNonce) {
		t.Fatalf("body = %+v", body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}
