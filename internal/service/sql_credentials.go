package service

import (
	"context"
	"errors"
	"fmt"

	compiler "github.com/okdp/okdp-compiler"
	"github.com/okdp/okdp-compiler/treeedit"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
)

// LabelConnectionCredentials marks a Secret named by a connection
// (existingSecret, secretRef.name) as one its owner lets the SQL editor log
// in with. Any other Secret of a namespace (an operator's superuser, a
// replicated admin Secret) is refused: naming it in a connection is not
// enough to hand its credentials to every member of the project.
const LabelConnectionCredentials = "okdp.io/connection-credentials"

// credentialsLabel is the label okdp-compiler puts on the credentials
// Secrets it renders for a contract: the Secret of a provider's output and
// the Secrets the platform's secret store fills (okdp.io/credentials-<contract>).
func credentialsLabel(contract string) string { return "okdp.io/credentials-" + contract }

var (
	// ErrSqlCredentialsRefused: the Secret a connection names is not one
	// the SQL editor may log in with.
	ErrSqlCredentialsRefused = errors.New("is not a credentials Secret the SQL editor may use")
	// ErrSqlUnbound: a named claim no provider has granted yet.
	ErrSqlUnbound = errors.New("is not bound to a provider that granted it a connection")
)

// Scopes of the connection a statement runs on.
const (
	sqlScopeProject  = "project"
	sqlScopePlatform = "platform"
	sqlScopeClaim    = "claim"
)

// sqlTarget is the database a statement runs on: the values of a
// connection declared in the tree (host, port, database, TLS mode) and
// the credentials of one of the allowed sources. Nothing of it comes from
// the request.
type sqlTarget struct {
	scope       string
	contract    string
	values      connectionValues
	credentials map[string][]byte
}

// sqlCredentialFields are the keys of a credentials Secret the drivers
// read; every other key is ignored, a Secret never overrides the host or
// the port of the connection.
var sqlCredentialFields = []string{"username", "password"}

// resolveTarget finds the connection name of a project: one the project
// declares, else one of the platform's, else a named claim of the project,
// and the credentials it may run with.
func (s *DefaultSqlService) resolveTarget(ctx context.Context, project, name string) (*sqlTarget, error) {
	for _, scope := range []struct{ project, name string }{{project, sqlScopeProject}, {"", sqlScopePlatform}} {
		connection, err := s.connections.Get(ctx, scope.project, name)
		if err == nil {
			return s.fromConnection(ctx, project, scope.name, connection)
		}
		if !errors.Is(err, treeedit.ErrNotFound) {
			return nil, err
		}
	}
	return s.fromClaim(ctx, project, name)
}

// fromConnection is the target of a declared connection: its values, the
// credentials of its Secret when the Secret may be used.
func (s *DefaultSqlService) fromConnection(ctx context.Context, project, scope string, c *compiler.Connection) (*sqlTarget, error) {
	t := &sqlTarget{scope: scope, contract: c.Contract, values: connectionValues{}}
	for k, v := range c.Values {
		t.values[k] = v
	}
	if err := supportedSql(c.Name, c.Contract, t.values); err != nil {
		return nil, err
	}
	var err error
	switch {
	case c.RemoteKey != "":
		t.credentials, err = s.storeCredentials(ctx, project, c)
	case c.SecretRef != "":
		t.credentials, err = s.namedCredentials(ctx, project, scope, c)
	default:
		err = fmt.Errorf("connection '%s': it names no credentials Secret, its secretRef %w", c.Name, ErrSqlNoCredentials)
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// namedCredentials reads the Secret a connection names (secretRef.name,
// the console's existingSecret), in the project namespace. It is used
// when it carries LabelConnectionCredentials, or when it is the Secret the
// console wrote for this project connection from the credentials typed in
// its form (<connection>-credentials, managed by the console).
func (s *DefaultSqlService) namedCredentials(ctx context.Context, project, scope string, c *compiler.Connection) (map[string][]byte, error) {
	data, labels, found, err := s.connections.Secrets.ReadSecret(ctx, project, c.SecretRef)
	if err != nil {
		return nil, fmt.Errorf("reading the credentials of connection '%s': %w", c.Name, err)
	}
	if !found {
		return nil, fmt.Errorf("connection '%s': credentials secret '%s' %w", c.Name, c.SecretRef, ErrSqlNoCredentials)
	}
	typed := scope == sqlScopeProject && c.SecretRef == c.Name+credentialsSecretSuffix &&
		labels[crd.LabelManagedBy] == crd.ManagedByValue
	if !typed && labels[LabelConnectionCredentials] != "true" {
		return nil, fmt.Errorf("connection '%s': secret '%s' %w: label it %s: \"true\" to allow it", c.Name, c.SecretRef, ErrSqlCredentialsRefused, LabelConnectionCredentials)
	}
	return pick(data), nil
}

// storeCredentials reads the credentials of a connection whose secretRef is
// a key of the platform's secret store (remoteKey): the Secret an
// ExternalSecret of the project rendered by okdp-compiler fills from that
// key, for the instances that reference the connection. The server does
// not read the store itself.
func (s *DefaultSqlService) storeCredentials(ctx context.Context, project string, c *compiler.Connection) (map[string][]byte, error) {
	if s.connections.ExternalSecrets == nil {
		return nil, fmt.Errorf("connection '%s': its credentials are in the platform's secret store, which this server cannot look up: %w", c.Name, ErrSqlNoCredentials)
	}
	list, err := s.connections.ExternalSecrets(ctx, project)
	if err != nil {
		return nil, fmt.Errorf("reading the credentials of connection '%s': %w", c.Name, err)
	}
	label := credentialsLabel(c.Contract)
	for _, es := range list {
		if es.Labels[label] != "true" || !readsOnly(es, c.RemoteKey) {
			continue
		}
		target := es.Spec.Target.Name
		if target == "" {
			target = es.Name
		}
		data, labels, found, err := s.connections.Secrets.ReadSecret(ctx, project, target)
		if err != nil {
			return nil, fmt.Errorf("reading the credentials of connection '%s': %w", c.Name, err)
		}
		if found && labels[label] == "true" {
			return pick(data), nil
		}
	}
	return nil, fmt.Errorf("connection '%s': its credentials are in the platform's secret store (key %s), and no instance of the project reads them yet: %w", c.Name, c.RemoteKey, ErrSqlNoCredentials)
}

// readsOnly reports whether every key an ExternalSecret reads is key.
func readsOnly(es crd.ESOExternalSecret, key string) bool {
	if len(es.Spec.Data) == 0 {
		return false
	}
	for _, d := range es.Spec.Data {
		if d.RemoteRef.Key != key {
			return false
		}
	}
	return true
}

// fromClaim is the target of a named claim of the project: the connection
// its provider granted it in the compile of the tree, the provider's
// credentials Secret for it, which must carry the label okdp-compiler
// gives a provider's credentials.
func (s *DefaultSqlService) fromClaim(ctx context.Context, project, name string) (*sqlTarget, error) {
	if s.connections.Claims == nil || s.connections.Tree == nil {
		return nil, apierrors.NewNotFound(connectionsResource, name)
	}
	claims, err := s.connections.Claims(ctx, project)
	if err != nil {
		if errors.Is(err, treeedit.ErrNotFound) {
			return nil, apierrors.NewNotFound(connectionsResource, name)
		}
		return nil, err
	}
	var claim *compiler.ClaimSpec
	for i := range claims {
		if claims[i].Name == name {
			claim = &claims[i]
		}
	}
	if claim == nil {
		return nil, apierrors.NewNotFound(connectionsResource, name)
	}
	t, err := s.connections.Tree(ctx)
	if _, ok := errors.AsType[*compiler.LoadError](err); ok {
		// A tree that does not load binds no claim.
		t, err = &compiler.Tree{}, nil
	}
	if err != nil {
		return nil, err
	}
	id := project + "." + name
	for _, ti := range t.Instances {
		o := ti.Granted(id)
		if o == nil || o.Contract != claim.Contract {
			continue
		}
		target := &sqlTarget{scope: sqlScopeClaim, contract: claim.Contract, values: connectionValues{}}
		for k, v := range o.Values {
			target.values[k] = v
		}
		if err := supportedSql(name, claim.Contract, target.values); err != nil {
			return nil, err
		}
		if o.SecretRef == nil || o.SecretRef.Name == "" {
			return nil, fmt.Errorf("claim '%s': its provider publishes no credentials Secret, %w", name, ErrSqlNoCredentials)
		}
		data, labels, found, err := s.connections.Secrets.ReadSecret(ctx, ti.Namespace(), o.SecretRef.Name)
		if err != nil {
			return nil, fmt.Errorf("reading the credentials of claim '%s': %w", name, err)
		}
		if !found {
			return nil, fmt.Errorf("claim '%s': credentials secret '%s' %w", name, o.SecretRef.Name, ErrSqlNoCredentials)
		}
		if labels[credentialsLabel(claim.Contract)] != "true" {
			return nil, fmt.Errorf("claim '%s': secret '%s' %w: it is not labelled %s", name, o.SecretRef.Name, ErrSqlCredentialsRefused, credentialsLabel(claim.Contract))
		}
		target.credentials = pick(data)
		return target, nil
	}
	return nil, fmt.Errorf("claim '%s' %w", name, ErrSqlUnbound)
}

// supportedSql refuses a connection the editor has no driver for.
func supportedSql(name, contract string, values connectionValues) error {
	if contract == "database-server" && values.String("engine") == "postgresql" {
		return nil
	}
	engine := values.String("engine")
	if engine == "" {
		engine = contract
	}
	return fmt.Errorf("connection '%s' (%s) %w", name, engine, ErrSqlUnsupported)
}

// sqlTargetOf is the engine of a connection's values and whether the SQL
// editor runs statements on it (supportedSql), for the console to offer
// only those.
func sqlTargetOf(name, contract string, values map[string]any) (string, bool) {
	v := connectionValues(values)
	return v.String("engine"), supportedSql(name, contract, v) == nil
}

// pick keeps the credential keys of a Secret's data.
func pick(data map[string][]byte) map[string][]byte {
	out := map[string][]byte{}
	for _, k := range sqlCredentialFields {
		if v, ok := data[k]; ok {
			out[k] = v
		}
	}
	return out
}
