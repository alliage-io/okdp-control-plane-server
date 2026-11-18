package service

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"testing"

	compiler "github.com/okdp/okdp-compiler"
	"github.com/okdp/okdp-gitops/rules"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// checkValues is the check of a connection's values on its write: the
// credentials the console writes (missingCredentials), the rest the
// compiler's (compiler.CheckConnection).
func checkValues(catalog ContractCatalog, contract string, values map[string]any) error {
	descriptor, _ := catalog.Get(contract)
	if err := missingCredentials(descriptor, values); err != nil {
		return err
	}
	return compiler.CheckConnection(rules.Standard(), catalog.Contracts(), "demo", compiler.Connection{Name: "c", Contract: contract, Values: withoutSecrets(descriptor, values), SecretRef: "c-credentials"}, nil)
}

func newTestCatalog(t *testing.T) ContractCatalog {
	t.Helper()
	catalog, err := testContractCatalog()
	require.NoError(t, err, "the embedded contracts must all parse")
	return catalog
}

func TestEmbeddedCatalogLoadsBuiltInTypes(t *testing.T) {
	catalog := newTestCatalog(t)

	names := map[string]bool{}
	for _, ct := range catalog.List() {
		names[ct.Name] = true
	}

	// One descriptor per ClusterContract the platform publishes.
	assert.True(t, names["database-server"])
	assert.True(t, names["s3"])
	assert.True(t, names["trino"])
	assert.True(t, names["hive"])
	assert.True(t, names["iceberg-catalog"])
}

func TestEveryTypeWithCredentialsMarksThemSecret(t *testing.T) {
	catalog := newTestCatalog(t)

	// A credential that is not marked secret would be written to the Connection
	// spec in clear, so assert the marking rather than trusting the JSON.
	for _, name := range []string{"database-server"} {
		ct, ok := catalog.Get(name)
		require.True(t, ok, name)
		assert.Contains(t, ct.SecretFields(), "password", name)
	}

	s3, ok := catalog.Get("s3")
	require.True(t, ok)
	assert.Contains(t, s3.SecretFields(), "secretKey")
	// Both halves of an S3 identity go to the Secret. Not because a key ID is a
	// secret on its own, it is not, but because the s3 Contract declares a Secret
	// "with keys: accessKey, secretKey" and consuming packages read both from it.
	assert.Contains(t, s3.SecretFields(), "accessKey", "the s3 contract expects the key ID in the Secret")
}

// A database outside the cluster is often reachable only in clear text
// public datasets, older corporate instances. Without "prefer" the only
// working choice is "disable", which never even attempts TLS, so a server
// that does support it would be talked to unencrypted.
func TestPostgreSQLOffersThePreferSSLMode(t *testing.T) {
	catalog := newTestCatalog(t)

	database, ok := catalog.Get("database-server")
	if !ok {
		t.Fatal("database-server type missing")
	}
	sslMode, ok := database.Field("sslMode")
	if !ok {
		t.Fatal("sslMode field missing")
	}

	var found bool
	for _, option := range sslMode.Options {
		if option == "prefer" {
			found = true
		}
	}
	if !found {
		t.Errorf("sslMode options = %v, want one of them to be \"prefer\"", sslMode.Options)
	}
	if sslMode.Default != "prefer" {
		t.Errorf("sslMode default = %v, want prefer", sslMode.Default)
	}
}

func TestValidateAcceptsAWellFormedPayload(t *testing.T) {
	catalog := newTestCatalog(t)

	err := checkValues(catalog, "database-server", map[string]any{
		"engine":   "postgresql",
		"driver":   "org.postgresql.Driver",
		"host":     "db.example.com",
		"port":     float64(5432),
		"dbName":   "analytics",
		"username": "reader",
		"password": "s3cret",
		"sslMode":  "require",
	})

	assert.NoError(t, err)
}

func TestValidateRejectsBadPayloads(t *testing.T) {
	catalog := newTestCatalog(t)

	base := func() map[string]any {
		return map[string]any{
			"engine":   "postgresql",
			"driver":   "org.postgresql.Driver",
			"host":     "db.example.com",
			"port":     float64(5432),
			"dbName":   "analytics",
			"username": "reader",
			"password": "s3cret",
			"sslMode":  "require",
		}
	}

	tests := []struct {
		name    string
		mutate  func(map[string]any)
		message string
	}{
		{
			name:    "missing required field",
			mutate:  func(v map[string]any) { delete(v, "host") },
			message: `field "host" is required`,
		},
		{
			name:    "empty required field",
			mutate:  func(v map[string]any) { v["host"] = "" },
			message: `field "host" is required`,
		},
		{
			name:    "port out of range",
			mutate:  func(v map[string]any) { v["port"] = float64(70000) },
			message: "maximum: got 70,000, want 65,535",
		},
		{
			name:    "port not a number",
			mutate:  func(v map[string]any) { v["port"] = "5432" },
			message: "got string, want integer",
		},
		{
			name:    "value outside the enum",
			mutate:  func(v map[string]any) { v["sslMode"] = "maybe" },
			message: "value must be one of",
		},
		{
			name:    "unknown field",
			mutate:  func(v map[string]any) { v["hostname"] = "db.example.com" },
			message: `field "hostname" is not part of the database-server contract`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := base()
			tt.mutate(values)

			err := checkValues(catalog, "database-server", values)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.message)
		})
	}
}

func TestValidateRejectsUnknownType(t *testing.T) {
	catalog := newTestCatalog(t)

	err := checkValues(catalog, "oracle", map[string]any{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "the oracle contract cannot be read")
}

func TestValidateAllowsOmittingOptionalFields(t *testing.T) {
	catalog := newTestCatalog(t)

	// region and pathStyle carry defaults and must not be demanded.
	err := checkValues(catalog, "s3", map[string]any{
		"apiUrl":    "https://s3.example.com",
		"bucket":    "warehouse",
		"accessKey": "AKIA",
		"secretKey": "secret",
	})

	assert.NoError(t, err)
}

// The driver class and the engine cannot be allowed to disagree: a connection
// declaring MySQL with the PostgreSQL driver opens for nobody, and the user has
// no way of telling which of the two fields was the wrong one.
func TestNormalizeDerivesTheDriverFromTheEngine(t *testing.T) {
	catalog := newTestCatalog(t)

	values := catalog.Normalize("database-server", map[string]any{
		"engine": "mysql",
		"driver": "org.postgresql.Driver",
		"host":   "db.example.com",
	})

	assert.Equal(t, "com.mysql.cj.jdbc.Driver", values["driver"])
}

// PostgreSQL and MySQL name their TLS modes differently, and the form offers
// whichever belongs to the chosen engine. A value for the other one is a
// leftover from switching engines, not a setting: it must not be stored, and
// must not be demanded either.
func TestNormalizeDropsTheFieldsTheEngineRulesOut(t *testing.T) {
	catalog := newTestCatalog(t)

	values := catalog.Normalize("database-server", map[string]any{
		"engine":  "mysql",
		"host":    "db.example.com",
		"sslMode": "prefer",
		"tls":     "preferred",
	})

	_, hasSSLMode := values["sslMode"]
	assert.False(t, hasSSLMode, "the PostgreSQL wording has no place on a MySQL connection")
	assert.Equal(t, "preferred", values["tls"])
}

func TestValidateIgnoresAFieldTheEngineRulesOut(t *testing.T) {
	catalog := newTestCatalog(t)

	err := checkValues(catalog, "database-server", map[string]any{
		"engine":   "mysql",
		"driver":   "com.mysql.cj.jdbc.Driver",
		"host":     "db.example.com",
		"port":     float64(3306),
		"dbName":   "analytics",
		"username": "reader",
		"password": "s3cret",
		"tls":      "preferred",
	})

	assert.NoError(t, err)
}

// The address of a connection depends on its contract: database-server carries
// a host and a port, trino a URI, hive a thrift URI, s3 a URL. Reading host and
// port only, as the code did, showed "not available yet" on a Ready trino, hive
// or s3 connection whose address sat in its own values.
func TestEndpointComesFromTheContract(t *testing.T) {
	catalog := newTestCatalog(t)

	// A URI contract: the in-cluster address wins over the public one.
	assert.Equal(t,
		"thrift://demo-hms.demo.svc.cluster.local:9083",
		endpointFrom(map[string]any{"thriftUri": "thrift://demo-hms.demo.svc.cluster.local:9083"}, catalog, "hive"))
	assert.Equal(t,
		"jdbc:trino://trino.demo.svc.cluster.local:8080",
		endpointFrom(map[string]any{
			"url":         "https://trino.example.com",
			"uri":         "jdbc:trino://trino.example.com:443",
			"internalUri": "jdbc:trino://trino.demo.svc.cluster.local:8080",
		}, catalog, "trino"))

	// The host+port convention still works with no declaration.
	assert.Equal(t, "db.example.com:5432",
		endpointFrom(map[string]any{"host": "db.example.com", "port": float64(5432)}, catalog, "database-server"))

	// Nothing to show is an empty string, never a guess.
	assert.Equal(t, "", endpointFrom(map[string]any{}, catalog, "hive"))
	assert.Equal(t, "", endpointFrom(map[string]any{"realm": "main"}, catalog, "unknown-contract"))
}

// Every contract the platform publishes must be declarable, or the wizard
// leaves the user stuck with "No compatible connection available" and no way to
// create one. These are the five ClusterContracts of platform-packages: the
// OIDC provider is deliberately not among them, it is platform configuration
// read from the Context, not something a project connects to.
func TestEveryPlatformContractIsInTheCatalog(t *testing.T) {
	catalog := newTestCatalog(t)

	for _, name := range []string{"database-server", "s3", "trino", "hive", "iceberg-catalog"} {
		ct, ok := catalog.Get(name)
		require.True(t, ok, "%s is a ClusterContract of the platform and must be declarable", name)
		assert.True(t, ct.External, "%s must offer a creation form", name)
	}
}

// The consumers and the connectivity test read the values, not the descriptor,
// so an omitted optional field must carry its declared default.
func TestNormalizeWritesTheDeclaredDefaults(t *testing.T) {
	catalog, err := testContractCatalog()
	if err != nil {
		t.Fatalf("loading the catalog: %v", err)
	}

	values := catalog.Normalize("database-server", map[string]any{
		"engine": "postgresql",
		"host":   "db.example.com",
		"port":   float64(5432),
		"dbName": "analytics",
	})

	if values["sslMode"] != "prefer" {
		t.Fatalf("sslMode: got %v, want the declared default \"prefer\"", values["sslMode"])
	}
}

func TestTheCompilerChecksTheConnectionAtItsDeclaration(t *testing.T) {
	catalog := newTestCatalog(t)
	assert.NoError(t, checkValues(catalog, "hive", map[string]any{"thriftUri": "thrift://hms:9083"}))
	assert.Error(t, checkValues(catalog, "hive", map[string]any{}), "a hive connection without thriftUri")
	assert.Error(t, checkValues(catalog, "database-server", map[string]any{
		"engine": "postgresql", "driver": "org.postgresql.Driver", "host": "db", "port": 5432.5, "dbName": "x",
	}), "a fractional port passed the integer type")
}

// testContractCatalog loads testdata/contracts, the contracts of the OKDP
// catalogs.
func testContractCatalog() (ContractCatalog, error) {
	return NewContractCatalog(os.DirFS("testdata/contracts"))
}

func TestTheContractsOfTheCatalogsAreReadAgainAndKeptWhenUnreadable(t *testing.T) {
	calls := 0
	var fail error
	catalog := NewCatalogContracts(func(context.Context) (fs.FS, error) {
		calls++
		if fail != nil {
			return nil, fail
		}
		return os.DirFS("testdata/contracts"), nil
	})
	if _, ok := catalog.Get("s3"); !ok {
		t.Fatal("s3 is a contract of the catalogs")
	}
	catalog.List()
	if calls != 1 {
		t.Fatalf("read %d times, want once within the refresh interval", calls)
	}

	broken := NewCatalogContracts(func(context.Context) (fs.FS, error) { return nil, errors.New("catalog unreachable") })
	if list := broken.List(); len(list) != 0 {
		t.Fatalf("no contract read yet: %v", list)
	}
}

func TestTheClaimSchemaIsWhatAClaimMayRequest(t *testing.T) {
	catalog := newTestCatalog(t)

	db, ok := catalog.Get("database-server")
	require.True(t, ok)
	require.NotNil(t, db.ClaimSchema, "database-server takes requests (x-okdp-claim)")
	assert.Contains(t, db.ClaimSchema["properties"], "extensions")

	// hive has no provisioner: a claim of it requests nothing.
	hive, _ := catalog.Get("hive")
	assert.Nil(t, hive.ClaimSchema)
}

func TestAClaimIDIsNoOutputName(t *testing.T) {
	assert.True(t, IsClaimID("demo.hive.db"))
	assert.True(t, IsClaimID("demo.analytics"))
	assert.False(t, IsClaimID("demo-hive"))
	assert.False(t, IsClaimID("demo-pg-hive"))
	assert.False(t, IsClaimID("demo..hive"), "the compiler's form: DNS-1123 labels joined by dots")
}
