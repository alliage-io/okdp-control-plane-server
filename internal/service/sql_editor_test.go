package service

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	compiler "github.com/okdp/okdp-compiler"
	"github.com/okdp/okdp-gitops/gitops"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/okdp/okdp-control-plane-server/internal/apperr"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/netguard"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
)

// --- Where the credentials come from ---

// sqlTargets is the SQL service over the connection service's test
// environment, with a tree of the given providers and ExternalSecrets.
func sqlTargets(t *testing.T, files map[string]string, tree *compiler.Tree, externals []crd.ESOExternalSecret) (*DefaultSqlService, *DefaultConnectionService, *testEnv) {
	t.Helper()
	layout := map[string]string{"projects/demo/project.yaml": "{}\n"}
	for k, v := range files {
		layout[k] = v
	}
	env := &testEnv{store: gitops.MustMemoryStore(layout), secrets: newFakeSecrets(), descriptors: &fakeDescriptors{}}
	env.deployments = gitops.NewDeployments(env.store)
	catalog, err := testContractCatalog()
	require.NoError(t, err)
	connections := NewDefaultConnectionService(env.deployments, env.secrets, env.descriptors, nil, catalog)
	source := SqlConnectionSource{
		Get:     env.deployments.GetConnection,
		Claims:  env.deployments.ListClaims,
		Tree:    func(context.Context) (*compiler.Tree, error) { return tree, nil },
		Secrets: env.secrets,
		ExternalSecrets: func(context.Context, string) ([]crd.ESOExternalSecret, error) {
			return externals, nil
		},
	}
	return NewDefaultSqlService(nil, source, nil, SqlOptions{Guard: loopbackAllowed()}), connections, env
}

func TestTheConsoleWrittenSecretIsUsed(t *testing.T) {
	ctx := context.Background()
	sql, connections, _ := sqlTargets(t, nil, &compiler.Tree{}, nil)
	_, err := connections.Create(ctx, "demo", postgresRequest())
	require.NoError(t, err)

	target, err := sql.resolveTarget(ctx, "demo", "warehouse")
	require.NoError(t, err)
	assert.Equal(t, sqlScopeProject, target.scope)
	assert.Equal(t, "db.example.com", target.values.String("host"))
	assert.Equal(t, "s3cret", string(target.credentials["password"]))
}

func TestAnUnlabelledExistingSecretIsRefused(t *testing.T) {
	ctx := context.Background()
	sql, connections, env := sqlTargets(t, nil, &compiler.Tree{}, nil)
	// An operator's superuser Secret of the namespace: named by a
	// connection, it must not log the editor in.
	env.secrets.put("demo", "pg-superuser", false, "username", "password", "host")
	req := postgresRequest()
	req.ExistingSecret = "pg-superuser"
	_, err := connections.Create(ctx, "demo", req)
	require.NoError(t, err)

	_, err = sql.resolveTarget(ctx, "demo", "warehouse")
	assert.ErrorIs(t, err, ErrSqlCredentialsRefused)
	assert.Contains(t, err.Error(), LabelConnectionCredentials)

	// Its owner opts it in.
	env.secrets.label("demo", "pg-superuser", map[string]string{LabelConnectionCredentials: "true"})
	target, err := sql.resolveTarget(ctx, "demo", "warehouse")
	require.NoError(t, err)
	assert.Equal(t, "db.example.com", target.values.String("host"))
	_, hasHost := target.credentials["host"]
	assert.False(t, hasHost, "a Secret never overrides the connection's host")
}

func TestAPlatformConnectionNeedsALabelledSecret(t *testing.T) {
	ctx := context.Background()
	sql, _, env := sqlTargets(t, map[string]string{"platform.yaml": `connections:
  shared:
    contract: database-server
    engine: postgresql
    host: pg.shared.svc
    port: 5432
    dbName: shared
    secretRef:
      name: shared-credentials
`}, &compiler.Tree{}, nil)
	// Named like the console's own Secret, managed by it: still not a
	// project connection's.
	env.secrets.put("demo", "shared-credentials", true, "username", "password")

	_, err := sql.resolveTarget(ctx, "demo", "shared")
	assert.ErrorIs(t, err, ErrSqlCredentialsRefused)

	env.secrets.label("demo", "shared-credentials", map[string]string{LabelConnectionCredentials: "true"})
	target, err := sql.resolveTarget(ctx, "demo", "shared")
	require.NoError(t, err)
	assert.Equal(t, sqlScopePlatform, target.scope)
	assert.Equal(t, "pg.shared.svc", target.values.String("host"))
}

func TestAStoreKeyIsReadThroughTheProjectsExternalSecret(t *testing.T) {
	ctx := context.Background()
	es := crd.ESOExternalSecret{}
	es.Name = "app.shared-credentials"
	es.Labels = map[string]string{"okdp.io/credentials-database-server": "true"}
	es.Spec.Target.Name = "app.shared-credentials"
	es.Spec.Data = []crd.ESOExternalSecretData{
		{SecretKey: "username", RemoteRef: crd.ESORemoteRef{Key: "platform/shared-db", Property: "username"}},
		{SecretKey: "password", RemoteRef: crd.ESORemoteRef{Key: "platform/shared-db", Property: "password"}},
	}
	other := es
	other.Name, other.Spec.Target.Name = "other", "other"
	other.Spec.Data = []crd.ESOExternalSecretData{{SecretKey: "password", RemoteRef: crd.ESORemoteRef{Key: "platform/admin"}}}
	sql, _, env := sqlTargets(t, map[string]string{"platform.yaml": `connections:
  shared:
    contract: database-server
    engine: postgresql
    host: pg.shared.svc
    port: 5432
    dbName: shared
    secretRef:
      remoteKey: platform/shared-db
`}, &compiler.Tree{}, []crd.ESOExternalSecret{other, es})
	env.secrets.put("demo", "other", false, "username", "password")
	env.secrets.label("demo", "other", map[string]string{"okdp.io/credentials-database-server": "true"})

	_, err := sql.resolveTarget(ctx, "demo", "shared")
	assert.ErrorIs(t, err, ErrSqlNoCredentials, "no instance of the project reads the key yet")

	env.secrets.put("demo", "app.shared-credentials", false, "username", "password")
	env.secrets.label("demo", "app.shared-credentials", map[string]string{"okdp.io/credentials-database-server": "true"})
	target, err := sql.resolveTarget(ctx, "demo", "shared")
	require.NoError(t, err)
	assert.Equal(t, "x", string(target.credentials["password"]))
}

// claimTree is a tree whose instance db/pg grants the claim demo.orders.
func claimTree() *compiler.Tree {
	provider := &compiler.TreeInstance{}
	provider.Inputs.Namespace, provider.Inputs.Release = "db", "pg"
	provider.Outcome.Result = &compiler.Result{Info: compiler.Info{Provides: []compiler.Output{{
		Name: "demo.orders", Contract: "database-server",
		Values:    map[string]any{"engine": "postgresql", "host": "pg-rw.db.svc", "port": 5432, "dbName": "demo_orders"},
		SecretRef: &compiler.SecretRef{Name: "demo.orders-credentials"},
	}}}}
	return &compiler.Tree{Instances: []*compiler.TreeInstance{provider}}
}

func TestANamedClaimRunsWithItsProvisionedSecret(t *testing.T) {
	ctx := context.Background()
	sql, _, env := sqlTargets(t, map[string]string{"projects/demo/project.yaml": `claims:
  orders:
    contract: database-server
  later:
    contract: database-server
`}, claimTree(), nil)
	env.secrets.put("db", "demo.orders-credentials", false, "username", "password")

	_, err := sql.resolveTarget(ctx, "demo", "orders")
	assert.ErrorIs(t, err, ErrSqlCredentialsRefused, "a provider's Secret carries okdp-compiler's credentials label")

	env.secrets.label("db", "demo.orders-credentials", map[string]string{"okdp.io/credentials-database-server": "true"})
	target, err := sql.resolveTarget(ctx, "demo", "orders")
	require.NoError(t, err)
	assert.Equal(t, sqlScopeClaim, target.scope)
	assert.Equal(t, "pg-rw.db.svc", target.values.String("host"))

	_, err = sql.resolveTarget(ctx, "demo", "later")
	assert.ErrorIs(t, err, ErrSqlUnbound)
	_, err = sql.resolveTarget(ctx, "demo", "nothing")
	assert.True(t, errors.Is(err, apperr.ErrNotFound), "neither a connection nor a claim: %v", err)
}

// --- The connection the editor opens ---

// fakePG is a PostgreSQL backend on the loopback that answers the startup
// with auth, after TLS when tlsConfig is set (else it refuses TLS). It
// records the startup parameters and any password it receives.
type fakePG struct {
	port     int
	startup  chan map[string]string
	password chan string
}

func startFakePG(t *testing.T, tlsConfig *tls.Config, auth func(be *pgproto3.Backend)) *fakePG {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	f := &fakePG{port: ln.Addr().(*net.TCPAddr).Port, startup: make(chan map[string]string, 1), password: make(chan string, 1)}
	go func() {
		raw, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = raw.Close() }()
		_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
		conn := raw
		msg, err := pgproto3.NewBackend(conn, conn).ReceiveStartupMessage()
		if err != nil {
			return
		}
		if _, ok := msg.(*pgproto3.SSLRequest); ok {
			if tlsConfig == nil {
				_, _ = raw.Write([]byte("N"))
				return
			}
			_, _ = raw.Write([]byte("S"))
			conn = tls.Server(raw, tlsConfig)
			if msg, err = pgproto3.NewBackend(conn, conn).ReceiveStartupMessage(); err != nil {
				return
			}
		}
		if s, ok := msg.(*pgproto3.StartupMessage); ok {
			f.startup <- s.Parameters
		}
		be := pgproto3.NewBackend(conn, conn)
		auth(be)
		if reply, err := be.Receive(); err == nil {
			if p, ok := reply.(*pgproto3.PasswordMessage); ok {
				f.password <- p.Password
			}
		}
	}()
	return f
}

func askCleartext(be *pgproto3.Backend) {
	be.Send(&pgproto3.AuthenticationCleartextPassword{})
	_ = be.Flush()
	_ = be.SetAuthType(pgproto3.AuthTypeCleartextPassword)
}

func askMD5(be *pgproto3.Backend) {
	be.Send(&pgproto3.AuthenticationMD5Password{Salt: [4]byte{1, 2, 3, 4}})
	_ = be.Flush()
	_ = be.SetAuthType(pgproto3.AuthTypeMD5Password)
}

func trustAnyone(be *pgproto3.Backend) {
	be.Send(&pgproto3.AuthenticationOk{})
	be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	_ = be.Flush()
}

// selfSigned is a certificate for 127.0.0.1 and the pool that trusts it.
func selfSigned(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake-pg"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}, pool
}

// editorOn is the editor, its CAs roots and guard g, and a project
// connection to the fake server with console-written credentials.
func editorOn(t *testing.T, f *fakePG, roots *x509.CertPool, g *netguard.Guard, sslMode string) *DefaultSqlService {
	t.Helper()
	connections, env, _ := newServiceUnderTest(t)
	req := postgresRequest()
	req.Values["host"] = "127.0.0.1"
	req.Values["port"] = float64(f.port)
	req.Values["password"] = "s3cr3t-from-secret"
	req.Values["sslMode"] = sslMode
	_, err := connections.Create(context.Background(), "demo", req)
	require.NoError(t, err)
	source := SqlConnectionSource{Get: env.deployments.GetConnection, Secrets: env.secrets}
	return NewDefaultSqlService(nil, source, nil, SqlOptions{Guard: g, RootCAs: roots, Limits: SqlLimits{StatementTimeout: 90 * time.Second}})
}

func noPassword(t *testing.T, f *fakePG) {
	t.Helper()
	select {
	case p := <-f.password:
		t.Fatalf("the server received the password %q", p)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestTheEditorNeverSendsACleartextOrMD5Password(t *testing.T) {
	for name, auth := range map[string]func(*pgproto3.Backend){"cleartext": askCleartext, "md5": askMD5, "trust": trustAnyone} {
		t.Run(name, func(t *testing.T) {
			serverTLS, roots := selfSigned(t)
			f := startFakePG(t, serverTLS, auth)
			sql := editorOn(t, f, roots, loopbackAllowed(), "disable")

			_, err := sql.ExecuteOnConnection(context.Background(), "demo", "warehouse", models.SqlQueryRequest{Query: "SELECT 1"})
			require.ErrorIs(t, err, ErrSqlAuthMethod)
			noPassword(t, f)

			params := <-f.startup
			assert.Equal(t, "on", params["default_transaction_read_only"], "read-only by default")
			assert.Equal(t, "90000", params["statement_timeout"])
		})
	}
}

func TestTheEditorVerifiesTheServerCertificate(t *testing.T) {
	serverTLS, _ := selfSigned(t)
	f := startFakePG(t, serverTLS, askCleartext)
	sql := editorOn(t, f, x509.NewCertPool(), loopbackAllowed(), "require")
	_, err := sql.ExecuteOnConnection(context.Background(), "demo", "warehouse", models.SqlQueryRequest{Query: "SELECT 1"})
	require.ErrorIs(t, err, ErrSqlTLS, "a certificate the platform's CAs do not sign")
	noPassword(t, f)

	f = startFakePG(t, nil, askCleartext)
	sql = editorOn(t, f, x509.NewCertPool(), loopbackAllowed(), "disable")
	_, err = sql.ExecuteOnConnection(context.Background(), "demo", "warehouse", models.SqlQueryRequest{Query: "SELECT 1"})
	require.ErrorIs(t, err, ErrSqlTLS, "sslMode disable is not honored")
	noPassword(t, f)
}

func TestTheEditorRefusesTheLoopback(t *testing.T) {
	serverTLS, roots := selfSigned(t)
	f := startFakePG(t, serverTLS, askCleartext)
	sql := editorOn(t, f, roots, netguard.Default(), "verify-full")
	_, err := sql.ExecuteOnConnection(context.Background(), "demo", "warehouse", models.SqlQueryRequest{Query: "SELECT 1"})
	require.ErrorIs(t, err, netguard.ErrRefused)
	select {
	case <-f.startup:
		t.Fatal("the server was reached")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestTheWriteFlagLeavesReadOnlyAndPassesTheGate(t *testing.T) {
	serverTLS, roots := selfSigned(t)
	f := startFakePG(t, serverTLS, askCleartext)
	sql := editorOn(t, f, roots, loopbackAllowed(), "verify-full")
	_, err := sql.ExecuteOnConnection(context.Background(), "demo", "warehouse", models.SqlQueryRequest{Query: "DELETE FROM t", Write: true})
	require.ErrorIs(t, err, ErrSqlAuthMethod)
	params := <-f.startup
	assert.NotContains(t, params, "default_transaction_read_only")

	sql.authorize = func(context.Context, string) error { return errors.New("not an editor of demo") }
	_, err = sql.ExecuteOnConnection(context.Background(), "demo", "warehouse", models.SqlQueryRequest{Query: "DELETE FROM t", Write: true})
	assert.ErrorIs(t, err, ErrSqlWriteRefused)
}

// --- Bounds ---

func TestOneUserRunsAtMostItsShareOfStatements(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"q","columns":[{"name":"n","type":"integer"}],"data":[[1]]}`))
	}))
	defer slow.Close()
	sql := sqlServiceFor(slow.URL, "trino")
	release, ok := sql.running.acquire("anonymous")
	require.True(t, ok)
	release2, ok := sql.running.acquire("anonymous")
	require.True(t, ok)

	_, err := sql.ExecuteQuery(context.Background(), "p", "t", "", models.SqlQueryRequest{Query: "SELECT 1"})
	assert.ErrorIs(t, err, ErrSqlBusy)

	release()
	res, err := sql.ExecuteQuery(context.Background(), "p", "t", "", models.SqlQueryRequest{Query: "SELECT 1"})
	require.NoError(t, err)
	assert.Equal(t, 1, res.RowCount)
	release2()
	assert.Empty(t, sql.running.n, "every slot is given back")
}

func TestTheResultSizeCapTruncates(t *testing.T) {
	trino := &fakeTrino{}
	srv := httptest.NewServer(trino)
	defer srv.Close()
	sql := sqlServiceFor(srv.URL, "trino")
	sql.limits.MaxResultBytes = 1

	res, err := sql.ExecuteQuery(context.Background(), "p", "t", "", models.SqlQueryRequest{Query: "SELECT n"})
	require.NoError(t, err)
	assert.True(t, res.Truncated)
	assert.Equal(t, 0, res.RowCount)
}

func TestTheLogKeepsTheStartOfAStatement(t *testing.T) {
	assert.Equal(t, "SELECT * FROM t WHERE a = 1", sqlStatementForLog("SELECT *\n  FROM t\tWHERE a = 1"))
	assert.Equal(t, "ALTER …", sqlStatementForLog("ALTER ROLE app PASSWORD 'hunter2'"))
	long := sqlStatementForLog("SELECT " + string(make([]byte, 200)))
	assert.LessOrEqual(t, len([]rune(long)), sqlStatementLogRunes+1)
}
