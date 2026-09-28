package tlsmux

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/tlscert"
)

// serve starts an http.Server on a tlsmux listener and returns its address,
// the certificate it presents, and a pool that trusts that certificate.
func serve(t *testing.T, sniffTimeout time.Duration) (string, *x509.CertPool) {
	t.Helper()
	cert, err := tlscert.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("certificate: %v", err)
	}
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln := New(inner, &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}}, sniffTimeout)

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.TLS != nil {
				_, _ = io.WriteString(w, "https")
				return
			}
			_, _ = io.WriteString(w, "http")
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return ln.Addr().String(), pool
}

func get(t *testing.T, client *http.Client, url string) string {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return string(body)
}

func TestServesHTTPAndHTTPSOnOnePort(t *testing.T) {
	addr, pool := serve(t, DefaultSniffTimeout)

	// Apps from before HTTPS: plain HTTP must keep working unchanged.
	if got := get(t, http.DefaultClient, "http://"+addr+"/"); got != "http" {
		t.Errorf("plain request answered %q, want http", got)
	}

	httpsClient := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "zephyr-helper"},
	}}
	if got := get(t, httpsClient, "https://"+addr+"/"); got != "https" {
		t.Errorf("TLS request answered %q, want https", got)
	}

	// Interleaved again, to be sure neither protocol wedges the other.
	if got := get(t, http.DefaultClient, "http://"+addr+"/"); got != "http" {
		t.Errorf("plain request after TLS answered %q, want http", got)
	}
}

func TestSilentConnectionIsDropped(t *testing.T) {
	addr, _ := serve(t, 200*time.Millisecond)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Send nothing. The listener should give up on classifying and close.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("expected the silent connection to be closed")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("connection was still open after the sniff timeout")
	}
}
