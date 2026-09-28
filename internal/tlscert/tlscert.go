// Package tlscert gives the helper a stable, self-signed TLS certificate.
//
// Nobody can issue a publicly trusted certificate for a box on a home LAN,
// so the app pins this certificate's fingerprint the first time it pairs
// and accepts nothing else afterwards. What matters is that the certificate
// never changes while the data volume survives — not who signed it — so it
// is generated once, stored next to the events DB, and valid for decades.
package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

const (
	certFile = "tls-cert.pem"
	keyFile  = "tls-key.pem"

	// Long enough that the pin outlives the hardware. The app evaluates the
	// certificate by fingerprint, not by date or issuer.
	validity = 25 * 365 * 24 * time.Hour
)

// LoadOrCreate returns the certificate stored in dir, generating and saving
// one on first run. A pair that exists but won't load is replaced rather
// than left to disable TLS for good; the app then asks for a fresh pairing,
// since the fingerprint it pinned no longer matches.
func LoadOrCreate(dir string) (tls.Certificate, error) {
	certPath := filepath.Join(dir, certFile)
	keyPath := filepath.Join(dir, keyFile)

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err == nil {
		return cert, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		slog.Warn("tls: stored certificate unusable; generating a new one", "err", err)
	}

	certPEM, keyPEM, err := generate()
	if err != nil {
		return tls.Certificate{}, err
	}
	// Key first: a crash between the two writes then leaves a key with no
	// certificate, which the next start simply regenerates.
	if err := writeFileAtomic(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("save key: %w", err)
	}
	if err := writeFileAtomic(certPath, certPEM, 0o644); err != nil {
		return tls.Certificate{}, fmt.Errorf("save certificate: %w", err)
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

// Fingerprint is the lowercase hex SHA-256 of the certificate's DER bytes —
// the value the app pins, and what `openssl x509 -fingerprint -sha256`
// prints (minus the colons).
func Fingerprint(cert tls.Certificate) string {
	if len(cert.Certificate) == 0 {
		return ""
	}
	sum := sha256.Sum256(cert.Certificate[0])
	return hex.EncodeToString(sum[:])
}

func generate() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate serial: %w", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "zephyr-helper"},
		DNSNames:              []string{"zephyr-helper"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("encode key: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
