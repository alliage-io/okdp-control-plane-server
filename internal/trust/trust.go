// Package trust is what the server's outbound TLS connections trust: the
// system's certificates and the platform's CA bundle (OKDP_CA_FILE).
package trust

import (
	"crypto/x509"
	"fmt"
	"os"
)

// Roots are the certificates a TLS client of the server verifies a
// server with.
type Roots struct {
	// Pool is the system's certificates and those of PEM.
	Pool *x509.CertPool
	// PEM is the platform's CA bundle as read, nil without one: for the
	// clients that take PEM (go-git's CABundle).
	PEM []byte
}

// Load reads the CA bundle of file, PEM certificates, beside the system's;
// the system's alone when file is "". A file that cannot be read, or holds
// no certificate, is an error: the server does not start trusting less
// than it was told to.
func Load(file string) (*Roots, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if file == "" {
		return &Roots{Pool: pool}, nil
	}
	pem, err := os.ReadFile(file) // #nosec G304 -- the operator's setting
	if err != nil {
		return nil, err
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s holds no PEM certificate", file)
	}
	return &Roots{Pool: pool, PEM: pem}, nil
}
