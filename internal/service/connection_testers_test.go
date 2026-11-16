package service

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"io"
	"net"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/netguard"
)

// A probe reading a field the descriptor does not write is invisible: the value
// comes out empty, the server refuses the connection, and the user is told their
// credentials are wrong. That is exactly what happened when the descriptor
// renamed database/user to dbName/username and these two functions kept the old
// names. The probe therefore gets tested against the descriptor itself, not
// against a hand-written fixture that could drift the same way.
func TestProbesReadTheFieldsTheDescriptorDeclares(t *testing.T) {
	catalog := newTestCatalog(t)
	database, ok := catalog.Get("database-server")
	require.True(t, ok)

	// One distinctive value per declared field, so a probe reading the wrong
	// name gets the empty string and the assertions below catch it.
	values := connectionValues{}
	for i := range database.Fields {
		values[database.Fields[i].Name] = "probe-" + database.Fields[i].Name
	}
	values["port"] = float64(5432)
	values["engine"] = "postgresql"
	values["sslMode"] = "disable"

	pg, err := postgresConfig(values)
	require.NoError(t, err)
	assert.Equal(t, "probe-host", pg.Host)
	assert.Equal(t, "probe-dbName", pg.Database, "the probe must read the database name the form writes")
	assert.Equal(t, "probe-username", pg.User, "the probe must read the user the form writes")
	assert.Equal(t, "probe-password", pg.Password)

	values["engine"] = "mysql"
	values["tls"] = "preferred"
	my, err := mysqlConfig(values)
	require.NoError(t, err)
	assert.Equal(t, "probe-dbName", my.DBName, "the probe must read the database name the form writes")
	assert.Equal(t, "probe-username", my.User, "the probe must read the user the form writes")
	assert.Equal(t, "probe-password", my.Passwd)
}

// An S3 store publishes a public URL and an in-cluster one. Testing only the
// public one, as this did, reported success for a store the deployed workload
// could not reach: the ingress answers from the outside, the service address
// does not resolve, and the green test said nothing about it.
func TestS3ProbesEveryAddressTheContractPublishes(t *testing.T) {
	checks := testS3(context.Background(), connectionValues{
		// Reserved by RFC 6761 to never resolve, so both probes fail on the
		// network and the test says nothing about a live store.
		"apiUrl":      "https://s3.invalid",
		"internalUrl": "https://s3.internal.invalid",
		"bucket":      "warehouse",
		"accessKey":   "key",
		"secretKey":   "secret",
	}, probeNetwork{guard: netguard.Default()})

	require.Len(t, checks, 2)
	assert.Equal(t, "In-cluster URL", checks[0].Label)
	assert.True(t, checks[0].Decisive, "the address a workload will take decides the verdict")
	assert.Equal(t, "Public URL", checks[1].Label)
	assert.False(t, checks[1].Decisive, "reported for diagnosis, it is not what the workload uses")
}

// A store declared by hand often has no in-cluster address at all, and then the
// public one is what the workload will use.
func TestS3MakesThePublicURLDecisiveWhenItIsTheOnlyOne(t *testing.T) {
	checks := testS3(context.Background(), connectionValues{
		"apiUrl":    "https://s3.invalid",
		"bucket":    "warehouse",
		"accessKey": "key",
		"secretKey": "secret",
	}, probeNetwork{guard: netguard.Default()})

	require.Len(t, checks, 1)
	assert.Equal(t, "Public URL", checks[0].Label)
	assert.True(t, checks[0].Decisive)
}

// A test dials through the editor's guard: the pod's own loopback (or the
// cloud metadata endpoint) is not a target, whatever the caller typed.
func TestATestNeverReachesTheLoopback(t *testing.T) {
	f := startFakePG(t, nil, askCleartext)
	checks := testDatabaseServer(context.Background(), connectionValues{
		"engine": "postgresql", "host": "127.0.0.1", "port": float64(f.port),
		"username": "u", "password": "p", "sslMode": "disable",
	}, probeNetwork{guard: netguard.Default()})
	require.Len(t, checks, 1)
	assert.False(t, checks[0].Success)
	assert.Equal(t, models.TestReasonUnreachable, checks[0].Reason)
	select {
	case <-f.startup:
		t.Fatal("the server was reached")
	default:
	}

	checks = testS3(context.Background(), connectionValues{
		"apiUrl": "http://169.254.169.254", "bucket": "warehouse", "accessKey": "k", "secretKey": "s",
	}, probeNetwork{guard: netguard.Default()})
	require.Len(t, checks, 1)
	assert.Equal(t, models.TestReasonUnreachable, checks[0].Reason)
}

// What a server answers is reduced to a category: its text never reaches
// the caller, a test is no way to read a service.
func TestATestReportsNoRemoteText(t *testing.T) {
	f := startFakePG(t, nil, func(be *pgproto3.Backend) {
		be.Send(&pgproto3.ErrorResponse{Severity: "FATAL", Code: "XX000", Message: "internal banner s3cr3t"})
		_ = be.Flush()
	})
	checks := testDatabaseServer(context.Background(), connectionValues{
		"engine": "postgresql", "host": "127.0.0.1", "port": float64(f.port),
		"username": "u", "password": "p", "sslMode": "disable",
	}, probeNetwork{guard: loopbackAllowed()})
	require.Len(t, checks, 1)
	assert.False(t, checks[0].Success)
	assert.NotContains(t, checks[0].Message, "s3cr3t")
	assert.Equal(t, models.TestReasonUnknown, checks[0].Reason)
}

// startFakeMySQL is a MySQL server that offers TLS in its greeting, reads
// the client's SSL request and runs the TLS handshake with tlsConfig; the
// handshake's outcome goes to the returned channel, then the connection
// is closed.
func startFakeMySQL(t *testing.T, tlsConfig *tls.Config) (int, <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	handshakes := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		const (
			clientLongPassword        = 0x00000001
			clientProtocol41          = 0x00000200
			clientSSL                 = 0x00000800
			clientSecureConn          = 0x00008000
			clientPluginAuth          = 0x00080000
			capabilities       uint32 = clientLongPassword | clientProtocol41 | clientSSL | clientSecureConn | clientPluginAuth
		)
		greeting := []byte{10}
		greeting = append(greeting, "8.0.0-fake\x00"...)
		greeting = binary.LittleEndian.AppendUint32(greeting, 1)
		greeting = append(greeting, "abcdefgh"...)
		greeting = append(greeting, 0)
		greeting = binary.LittleEndian.AppendUint16(greeting, uint16(capabilities&0xffff))
		greeting = append(greeting, 45)
		greeting = binary.LittleEndian.AppendUint16(greeting, 2)
		greeting = binary.LittleEndian.AppendUint16(greeting, uint16(capabilities>>16))
		greeting = append(greeting, 21)
		greeting = append(greeting, make([]byte, 10)...)
		greeting = append(greeting, "ijklmnopqrst\x00"...)
		greeting = append(greeting, "mysql_native_password\x00"...)
		header := []byte{byte(len(greeting)), byte(len(greeting) >> 8), byte(len(greeting) >> 16), 0}
		if _, err := conn.Write(append(header, greeting...)); err != nil {
			handshakes <- err
			return
		}

		// The SSL request: a 4-byte header and 32 bytes of capabilities,
		// packet size and character set.
		if _, err := io.ReadFull(conn, make([]byte, 4+32)); err != nil {
			handshakes <- err
			return
		}
		handshakes <- tls.Server(conn, tlsConfig).Handshake()
	}()
	return ln.Addr().(*net.TCPAddr).Port, handshakes
}

// MySQL's tls: true verifies the server's certificate with the probe's
// trust pool, the platform's CA bundle among it (OKDP_CA_FILE): a
// certificate it issued is accepted, one of no trusted CA refused.
func TestMySQLTLSIsVerifiedWithThePlatformsCA(t *testing.T) {
	serverTLS, pool := selfSigned(t)
	probe := func(roots *x509.CertPool) (models.ConnectionTestCheck, error) {
		port, handshakes := startFakeMySQL(t, serverTLS)
		checks := testDatabaseServer(context.Background(), connectionValues{
			"engine": "mysql", "host": "127.0.0.1", "port": float64(port),
			"username": "u", "password": "p", "tls": "true",
		}, probeNetwork{guard: loopbackAllowed(), roots: roots})
		require.Len(t, checks, 1)
		return checks[0], <-handshakes
	}

	result, handshake := probe(pool)
	require.NoError(t, handshake, "the certificate of the platform's CA is accepted")
	assert.False(t, result.Success, "the fake server answers no authentication")
	assert.NotContains(t, result.Message, "certificate")

	result, handshake = probe(x509.NewCertPool())
	require.Error(t, handshake, "the client refused the certificate")
	assert.False(t, result.Success)
	assert.Equal(t, models.TestReasonInvalidConfig, result.Reason)
	assert.Contains(t, result.Message, "certificate could not be verified")
}

// tls: true sets a verifying configuration, its server name the host's;
// the other modes keep the driver's own names.
func TestMySQLConfigTLSModes(t *testing.T) {
	base := connectionValues{"host": "db.example", "port": float64(3306)}
	with := func(mode string) connectionValues {
		v := connectionValues{"tls": mode}
		for k, x := range base {
			v[k] = x
		}
		return v
	}

	config, err := mysqlConfig(with("true"))
	require.NoError(t, err)
	require.NotNil(t, config.TLS)
	assert.False(t, config.TLS.InsecureSkipVerify)
	assert.False(t, config.AllowCleartextPasswords)

	for _, mode := range []string{"preferred", "skip-verify"} {
		config, err = mysqlConfig(with(mode))
		require.NoError(t, err)
		assert.Nil(t, config.TLS)
		assert.Equal(t, mode, config.TLSConfig)
	}

	config, err = mysqlConfig(with(""))
	require.NoError(t, err)
	assert.Nil(t, config.TLS)
	assert.Equal(t, "false", config.TLSConfig)

	_, err = mysqlConfig(with("custom"))
	require.Error(t, err)
}
