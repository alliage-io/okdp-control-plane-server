package trust

import (
	"crypto/tls"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A server whose certificate a CA of the test signs is reached with the
// CA bundle of OKDP_CA_FILE, refused with the system's alone; a file
// that cannot be read, or holds no certificate, is an error.
func TestTheCABundleIsTrustedBesideTheSystems(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	file := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	get := func(r *Roots) error {
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: r.Pool, MinVersion: tls.VersionTLS12}}}
		resp, err := client.Get(srv.URL)
		if err == nil {
			_ = resp.Body.Close()
		}
		return err
	}

	with, err := Load(file)
	if err != nil || len(with.PEM) == 0 {
		t.Fatalf("Load: %v", err)
	}
	if err := get(with); err != nil {
		t.Errorf("with the CA bundle: %v", err)
	}
	system, err := Load("")
	if err != nil || system.PEM != nil {
		t.Fatalf("Load without a file: %v", err)
	}
	if err := get(system); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("without the CA bundle: %v", err)
	}

	if _, err := Load(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a missing file was accepted")
	}
	junk := filepath.Join(t.TempDir(), "junk")
	if err := os.WriteFile(junk, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(junk); err == nil || !strings.Contains(err.Error(), "holds no PEM certificate") {
		t.Errorf("a file of no certificate: %v", err)
	}
}
