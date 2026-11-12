package service

import (
	"context"
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
	}, netguard.Default())

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
	}, netguard.Default())

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
	}, netguard.Default())
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
	}, netguard.Default())
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
	}, loopbackAllowed())
	require.Len(t, checks, 1)
	assert.False(t, checks[0].Success)
	assert.NotContains(t, checks[0].Message, "s3cr3t")
	assert.Equal(t, models.TestReasonUnknown, checks[0].Reason)
}
