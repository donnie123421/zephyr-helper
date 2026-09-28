package tlscert

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadOrCreateIsStable(t *testing.T) {
	dir := t.TempDir()

	first, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	second, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	// The app pins this value; a restart must not change it.
	if Fingerprint(first) == "" || Fingerprint(first) != Fingerprint(second) {
		t.Fatalf("fingerprint changed across reload: %q vs %q", Fingerprint(first), Fingerprint(second))
	}

	info, err := os.Stat(filepath.Join(dir, keyFile))
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode = %o, want 600", perm)
	}
}

func TestCertificateShape(t *testing.T) {
	cert, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if leaf.NotAfter.Before(time.Now().AddDate(20, 0, 0)) {
		t.Errorf("certificate expires %v; it should outlive the hardware", leaf.NotAfter)
	}
	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("ext key usage = %v, want server auth", leaf.ExtKeyUsage)
	}
	if got := Fingerprint(cert); len(got) != 64 {
		t.Errorf("fingerprint %q is not 64 hex characters", got)
	}
}

func TestUnreadablePairIsReplaced(t *testing.T) {
	dir := t.TempDir()
	original, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, certFile), []byte("not a certificate"), 0o644); err != nil {
		t.Fatalf("corrupt: %v", err)
	}

	replaced, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	if Fingerprint(replaced) == Fingerprint(original) {
		t.Error("a corrupt certificate should be replaced, not reused")
	}
}
