package service

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	compiler "github.com/okdp/okdp-compiler"
	"github.com/okdp/okdp-control-plane-server/internal/apperr"
	"github.com/okdp/okdp-control-plane-server/internal/auth"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/netguard"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-gitops/gitops"
	"github.com/okdp/okdp-gitops/rules"
	"github.com/okdp/okdp-gitops/treeedit"
)

// credentialsSecretSuffix names the Secret the console writes for a
// connection: <connection>-credentials, in the project namespace.
const credentialsSecretSuffix = "-credentials"

// valueSecretRef is the key under which a connection publishes the NAME of
// the Secret holding its credentials (never a value): secretRef.name in the
// connection's entry, the field the contract schemas declare.
const valueSecretRef = "secretRef"

// connectionsResource names the resource in not-found and conflict errors.
const connectionsResource = "connections.okdp.io"

// ConnectionService manages the connections of a project.
//
// External connections are entries of the project's project.yaml in the
// deployments Git repository (connections.<name>) plus a credentials Secret
// in the project namespace; the platform's, entries of platform.yaml, are
// written in Git by its administrators and offered to every project.
// Internal connections are the outputs the instances of the project publish
// (the provides of their compiled okdp.info.yaml), whatever their contract:
// a parameter references one by its name.
type ConnectionService interface {
	// Catalog returns the known contracts and whether connections can currently
	// be persisted.
	Catalog(ctx context.Context) models.ConnectionCatalogResponse

	List(ctx context.Context, namespace string) ([]models.ConnectionResponse, error)
	Create(ctx context.Context, namespace string, req models.ConnectionRequest) (*models.ConnectionResponse, error)
	Update(ctx context.Context, namespace, name string, req models.ConnectionRequest) (*models.ConnectionResponse, error)
	Delete(ctx context.Context, namespace, name string) error

	// Test probes a live endpoint with the submitted values. It never touches
	// the cluster, so a connection can be validated before it is created.
	Test(ctx context.Context, req models.ConnectionTestRequest) models.ConnectionTestResult

	// ListInternal returns the connections provided by the services already
	// deployed in a project, consumable by the project's other services.
	ListInternal(ctx context.Context, project string) ([]models.InternalConnection, error)

	// ListSelectable returns the connections a deployment form can offer for an
	// input of the given contract: the project's external ones, the
	// platform's, and the ones its deployed instances provide.
	ListSelectable(ctx context.Context, project, contract string) ([]models.SelectableConnection, error)

	// ListConsumers returns the services of a project bound to a connection:
	// the instances whose parameters name it.
	ListConsumers(ctx context.Context, project, name string) ([]models.ConnectionConsumer, error)
}

// InstanceLister lists the instances of a project, with their status and
// bound connections (ServiceService.ListServices).
type InstanceLister func(ctx context.Context, project string) ([]models.ServiceInstance, error)

type DefaultConnectionService struct {
	deployments *gitops.Deployments
	secrets     repository.ConnectionSecretRepository
	descriptors repository.DescriptorRepository
	instances   InstanceLister
	catalog     ContractCatalog
	// guard is the network policy of the tests (netguard.Default when nil).
	guard *netguard.Guard
	// roots verify a tested server's certificate; nil, the system's.
	roots *x509.CertPool
}

// SetNetworkGuard sets the network policy the connection tests dial
// through, the SQL editor's.
func (s *DefaultConnectionService) SetNetworkGuard(guard *netguard.Guard) { s.guard = guard }

// SetRootCAs sets the certificates a tested server is verified with: the
// system's and the platform's CA bundle (OKDP_CA_FILE).
func (s *DefaultConnectionService) SetRootCAs(roots *x509.CertPool) { s.roots = roots }

func NewDefaultConnectionService(
	deployments *gitops.Deployments,
	secrets repository.ConnectionSecretRepository,
	descriptors repository.DescriptorRepository,
	instances InstanceLister,
	catalog ContractCatalog,
) *DefaultConnectionService {
	return &DefaultConnectionService{
		deployments: deployments,
		secrets:     secrets,
		descriptors: descriptors,
		instances:   instances,
		catalog:     catalog,
	}
}

// ErrConnectionsUnavailable is returned by the SQL editor given no source
// of connections (SqlConnectionSource.Get).
var ErrConnectionsUnavailable = fmt.Errorf("no deployments repository is configured, connections cannot be stored")

// ValidationError is a problem with what was submitted, as opposed to a failure
// of the platform. It lets the handler answer 400 rather than 500 without
// having to guess from the message.
type ValidationError struct {
	message string
	// Code tells the console what kind of refusal it is, when one is set
	// (ErrorCodeDoesNotCompile).
	Code string
	// Fields are the problems of the compile at the fields they are about
	// (compiler.FieldErrors), for a form that shows each beside its field;
	// none for another refusal.
	Fields []models.FieldProblem
}

func (e *ValidationError) Error() string { return e.message }

func invalid(format string, args ...any) error {
	return &ValidationError{message: fmt.Sprintf(format, args...)}
}

// ErrorCodeDoesNotCompile is the code of an instance the compiler refuses:
// its message is the compiler's, one line per problem.
const ErrorCodeDoesNotCompile = "does-not-compile"

// DoesNotCompile is the refusal of a compile, its problems at their
// fields, each of the instance the compile refused (CompileError's
// <project>/<instance>), the one written or another the write breaks;
// of none when the compile refused no instance (the tree, a project's
// name).
func DoesNotCompile(err *treeedit.CompileError) error {
	instance := ""
	if err.Instance != "" {
		instance = err.Project + "/" + err.Instance
	}
	return &ValidationError{message: err.Error(), Code: ErrorCodeDoesNotCompile, Fields: fieldProblems(compiler.FieldErrors(err.Err), instance)}
}

// rejected is a refusal of the compiler's rules out of a compile (a
// connection at its declaration, a claim's provider), its problems at
// their fields.
func rejected(err error) error {
	return &ValidationError{message: err.Error(), Fields: fieldProblems(compiler.FieldErrors(err), "")}
}

// fieldProblems are the compiler's problems at their fields, of instance
// ("" for none).
func fieldProblems(fields []compiler.FieldError, instance string) []models.FieldProblem {
	if len(fields) == 0 {
		return nil
	}
	out := make([]models.FieldProblem, len(fields))
	for i, f := range fields {
		out[i] = models.FieldProblem{Path: f.Path, Pointer: f.Pointer, Message: f.Message, Instance: instance}
	}
	return out
}

// ValidationFields returns the field problems of a rejected input, none
// when it has none.
func ValidationFields(err error) []models.FieldProblem {
	var validationErr *ValidationError
	if errors.As(err, &validationErr) {
		return validationErr.Fields
	}
	return nil
}

// ValidationCode returns the code of a rejected input, "" when it has none.
func ValidationCode(err error) string {
	var validationErr *ValidationError
	if errors.As(err, &validationErr) {
		return validationErr.Code
	}
	return ""
}

// storeCredentialsError turns a refused adoption into a rejected input: naming
// a Secret the control plane does not own is a bad request, not a platform
// failure, and the console can say which name to change.
func storeCredentialsError(err error) error {
	if errors.Is(err, repository.ErrForeignSecret) {
		return invalid("%v", err)
	}
	return fmt.Errorf("failed to store the credentials: %w", err)
}

// IsValidationError reports whether err is a rejected user input.
func IsValidationError(err error) bool {
	var validationErr *ValidationError
	return errors.As(err, &validationErr)
}

// IsNotFound reports whether an error from this service is a missing resource,
// so the handler can answer 404 without importing the Kubernetes error package.
func IsNotFound(err error) bool {
	return errors.Is(err, apperr.ErrNotFound)
}

// connectionGitError maps the deployments repository errors to the API ones.
func connectionGitError(err error, name string) error {
	var inUse *treeedit.ErrInUse
	var compileErr *treeedit.CompileError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &compileErr):
		// An instance that layers the connection in no longer compiles with it.
		return DoesNotCompile(compileErr)
	case errors.Is(err, treeedit.ErrNotFound):
		return apperr.NotFound(connectionsResource, name)
	case errors.Is(err, treeedit.ErrExists):
		return apperr.AlreadyExists(connectionsResource, name)
	case errors.As(err, &inUse):
		return invalid("connection %q is still referenced by %s; change them first", name, strings.Join(inUse.Users, ", "))
	default:
		return err
	}
}

func (s *DefaultConnectionService) Catalog(ctx context.Context) models.ConnectionCatalogResponse {
	return models.ConnectionCatalogResponse{
		Types: s.catalog.List(),
	}
}

// --- External connections ---

func (s *DefaultConnectionService) List(ctx context.Context, namespace string) ([]models.ConnectionResponse, error) {
	connections, err := s.deployments.ListConnections(ctx, namespace)
	if err != nil {
		return nil, err
	}
	result := make([]models.ConnectionResponse, 0, len(connections))
	for i := range connections {
		result = append(result, s.toResponse(&connections[i], namespace))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

// withoutSecretIfNone drops a named Secret for a contract without secret
// fields: such a connection carries no secretRef at all.
func (s *DefaultConnectionService) withoutSecretIfNone(req models.ConnectionRequest) models.ConnectionRequest {
	if descriptor, known := s.catalog.Get(req.Type); known && len(descriptor.SecretFields()) == 0 {
		req.ExistingSecret = ""
	}
	return req
}

func (s *DefaultConnectionService) Create(ctx context.Context, namespace string, req models.ConnectionRequest) (*models.ConnectionResponse, error) {
	req = s.withoutSecretIfNone(req)
	descriptor, values, err := s.validateRequest(ctx, namespace, req, false)
	if err != nil {
		return nil, err
	}

	// The name has to be free before anything is written: the credentials Secret
	// of a live connection follows the same naming convention, and a repeated
	// create must not touch it (the commit below refuses a taken name too,
	// before any Secret is written).
	if _, err := s.deployments.GetConnection(ctx, namespace, req.Name); err == nil {
		return nil, apperr.AlreadyExists(connectionsResource, req.Name)
	} else if !errors.Is(err, treeedit.ErrNotFound) {
		return nil, err
	}
	// A level of the context adds connections, it changes none of the
	// platform's: the compile refuses a project connection of that name.
	if _, err := s.deployments.GetConnection(ctx, "", req.Name); err == nil {
		return nil, invalid("connection %q: a connection of the platform has that name", req.Name)
	} else if !errors.Is(err, treeedit.ErrNotFound) {
		return nil, err
	}

	public, secrets := splitValues(descriptor, values)
	secretName := ""
	ownSecret := req.ExistingSecret == ""
	if !ownSecret {
		// Pointing at a Secret somebody else owns: nothing is written, and the
		// credential fields of the payload are ignored on purpose.
		secretName = req.ExistingSecret
	} else if len(secrets) > 0 {
		secretName = req.Name + credentialsSecretSuffix
	}
	if err := s.checkConnection(ctx, namespace, compiler.Connection{Name: req.Name, Contract: descriptor.Name, Values: withoutSecrets(descriptor, values), SecretRef: secretName}); err != nil {
		return nil, err
	}
	writeSecret := ownSecret && len(secrets) > 0
	if writeSecret {
		if err := s.checkSecretWritable(ctx, namespace, secretName); err != nil {
			return nil, err
		}
	}

	// The connection is committed first, naming its Secret, and the Secret
	// written next: a crash in between leaves a connection whose credentials
	// are not stored yet (an update writes them), never a Secret that no
	// connection names. A create refused by the repository (the name taken
	// by a concurrent create, a push failure) writes no Secret at all.
	connection := compiler.Connection{
		Name:        req.Name,
		Contract:    descriptor.Name,
		Description: req.Description,
		Values:      public,
		SecretRef:   secretName,
	}
	actor := auth.ActorFrom(ctx)
	if _, err := s.deployments.PutConnection(ctx, actor, namespace, connection, true); err != nil {
		return nil, connectionGitError(err, req.Name)
	}
	if writeSecret {
		if err := s.secrets.CreateOrUpdateSecret(ctx, namespace, secretName, secrets); err != nil {
			// A connection naming credentials that were never stored is
			// taken back, whether or not the client is still there.
			rollback, cancel := compensationContext(ctx)
			defer cancel()
			if _, rollbackErr := s.deployments.DeleteConnection(rollback, actor, namespace, req.Name); rollbackErr != nil {
				slog.Warn("Failed to take back the connection whose credentials could not be stored", "error", rollbackErr, "connection", req.Name)
			}
			return nil, storeCredentialsError(err)
		}
	}

	response := s.toResponse(&connection, namespace)
	return &response, nil
}

func (s *DefaultConnectionService) Update(ctx context.Context, namespace, name string, req models.ConnectionRequest) (*models.ConnectionResponse, error) {
	req.Name = name
	req = s.withoutSecretIfNone(req)
	descriptor, values, err := s.validateRequest(ctx, namespace, req, true)
	if err != nil {
		return nil, err
	}

	existing, err := s.deployments.GetConnection(ctx, namespace, name)
	if err != nil {
		return nil, connectionGitError(err, name)
	}

	public, secrets := splitValues(descriptor, values)
	ownedName := name + credentialsSecretSuffix
	secretName := existing.SecretRef

	// Set when the connection moves off a Secret we owned. The old Secret is
	// dropped only once the new reference is persisted, otherwise a failed
	// update leaves the stored connection pointing at a Secret that no longer
	// exists.
	orphanedSecret := ""

	if len(descriptor.SecretFields()) == 0 {
		// No secret field, no secretRef.
		req.ExistingSecret, secretName = "", ""
		if existing.SecretRef == ownedName {
			orphanedSecret = ownedName
		}
	} else if req.ExistingSecret != "" {
		if existing.SecretRef == ownedName && req.ExistingSecret != ownedName {
			orphanedSecret = ownedName
		}
		secretName = req.ExistingSecret
	} else if len(secrets) > 0 {
		secretName = ownedName
	}
	if err := s.checkConnection(ctx, namespace, compiler.Connection{Name: name, Contract: descriptor.Name, Values: withoutSecrets(descriptor, values), SecretRef: secretName}); err != nil {
		return nil, err
	}
	// An unchanged credential is not resubmitted by the console, so the
	// Secret is written only when new values actually arrived, otherwise an
	// edit of, say, the port would blank out the password.
	writeSecret := len(descriptor.SecretFields()) > 0 && req.ExistingSecret == "" && len(secrets) > 0
	if writeSecret {
		if err := s.checkSecretWritable(ctx, namespace, ownedName); err != nil {
			return nil, err
		}
	}

	// Committed first, the Secret written next, as on a create: a crash in
	// between leaves no Secret that no connection names.
	updated := compiler.Connection{
		Name:        name,
		Contract:    descriptor.Name,
		Description: req.Description,
		Values:      public,
		SecretRef:   secretName,
	}
	actor := auth.ActorFrom(ctx)
	if _, err := s.deployments.PutConnection(ctx, actor, namespace, updated, false); err != nil {
		return nil, connectionGitError(err, name)
	}
	if writeSecret {
		if err := s.secrets.CreateOrUpdateSecret(ctx, namespace, ownedName, secrets); err != nil {
			// The connection as it was: its values and its Secret,
			// whether or not the client is still there.
			rollback, cancel := compensationContext(ctx)
			defer cancel()
			if _, rollbackErr := s.deployments.PutConnection(rollback, actor, namespace, *existing, false); rollbackErr != nil {
				slog.Warn("Failed to restore the connection whose credentials could not be stored", "error", rollbackErr, "connection", name)
			}
			return nil, storeCredentialsError(err)
		}
	}

	if orphanedSecret != "" {
		s.deleteOwnedSecret(ctx, namespace, orphanedSecret)
	}

	response := s.toResponse(&updated, namespace)
	return &response, nil
}

func (s *DefaultConnectionService) Delete(ctx context.Context, namespace, name string) error {
	existing, err := s.deployments.GetConnection(ctx, namespace, name)
	if err != nil {
		return connectionGitError(err, name)
	}
	if _, err := s.deployments.DeleteConnection(ctx, auth.ActorFrom(ctx), namespace, name); err != nil {
		return connectionGitError(err, name)
	}

	// Only a Secret this server wrote is removed with the connection. One that
	// was already there, projected from a vault, belongs to whoever put it
	// there: deleting it would take the credentials of everything else reading
	// it.
	if existing.SecretRef == "" {
		return nil
	}
	if existing.SecretRef != name+credentialsSecretSuffix {
		slog.Info("Leaving the credentials secret in place, the connection did not own it", "secret", existing.SecretRef)
		return nil
	}
	s.deleteOwnedSecret(ctx, namespace, existing.SecretRef)
	return nil
}

// checkSecretWritable refuses, before anything is committed, a credentials
// name a Secret the console did not write already holds: writing into it
// would take it over (CreateOrUpdateSecret refuses it too, after the
// commit, which is then taken back).
func (s *DefaultConnectionService) checkSecretWritable(ctx context.Context, namespace, name string) error {
	content, found, err := s.secrets.InspectSecret(ctx, namespace, name)
	if err != nil {
		return fmt.Errorf("failed to read the secret %q: %w", name, err)
	}
	if found && !content.Managed {
		return storeCredentialsError(fmt.Errorf("%w: secret %q in namespace %q", repository.ErrForeignSecret, name, namespace))
	}
	return nil
}

// deleteOwnedSecret removes a credentials Secret, only when it carries the
// console's managed-by label: the name alone is no proof, an external Secret
// being free to follow the same convention.
func (s *DefaultConnectionService) deleteOwnedSecret(ctx context.Context, namespace, name string) {
	content, found, err := s.secrets.InspectSecret(ctx, namespace, name)
	if err != nil {
		slog.Warn("Could not inspect the credentials secret, leaving it in place", "error", err, "secret", name)
		return
	}
	if !found {
		return
	}
	if !content.Managed {
		slog.Info("Leaving the credentials secret in place, the console did not write it", "secret", name)
		return
	}
	// The Secret outlives the connection only if this fails. Report nothing to
	// the user for it, the connection itself is gone.
	if err := s.secrets.DeleteSecret(ctx, namespace, name); err != nil {
		slog.Warn("Failed to delete the credentials secret of a connection", "error", err, "secret", name)
	}
}

func (s *DefaultConnectionService) Test(ctx context.Context, req models.ConnectionTestRequest) models.ConnectionTestResult {
	started := time.Now()

	result := func(success bool, reason, message string) models.ConnectionTestResult {
		return models.ConnectionTestResult{
			Success:    success,
			Reason:     reason,
			Message:    message,
			DurationMs: time.Since(started).Milliseconds(),
		}
	}

	if _, known := s.catalog.Get(req.Type); !known {
		return result(false, models.TestReasonInvalidConfig, fmt.Sprintf("Unknown contract %q.", req.Type))
	}
	// Normalize first, exactly as Create and Update do: the form does not send a
	// derived field, so validating the raw payload would demand a JDBC driver
	// nobody was asked for. Testing the normalized values is also the point of
	// the button, which is to try what will actually be stored.
	values := s.catalog.Normalize(req.Type, req.Values)
	descriptor, _ := s.catalog.Get(req.Type)
	if err := missingCredentials(descriptor, values); err != nil {
		return result(false, models.TestReasonInvalidConfig, err.Error())
	}
	if err := s.checkConnection(ctx, "", compiler.Connection{Name: "test", Contract: req.Type, Values: withoutSecrets(descriptor, values)}); err != nil {
		return result(false, models.TestReasonInvalidConfig, err.Error())
	}

	tester, ok := connectionTesters[req.Type]
	if !ok {
		return result(false, models.TestReasonInvalidConfig, "This contract cannot be tested.")
	}

	// A contract may publish several addresses. The verdict is the one a
	// workload will actually take, and the other checks travel along for
	// diagnosis: a store reachable publicly and unreachable in-cluster passes
	// and fails at once.
	guard := s.guard
	if guard == nil {
		guard = netguard.Default()
	}
	checks := tester(ctx, values, probeNetwork{guard: guard, roots: s.roots})
	outcome := result(true, "", "Connection successful.")
	outcome.Checks = checks
	for _, c := range checks {
		if !c.Decisive {
			continue
		}
		outcome.Success = c.Success
		outcome.Message = c.Message
		outcome.Reason = c.Reason
	}
	return outcome
}

// --- Internal connections ---

// outputsOf lists the connections the compiled instances of a project
// publish (okdp.info.yaml).
func (s *DefaultConnectionService) outputsOf(ctx context.Context, project string) ([]repository.Descriptor, error) {
	if s.descriptors == nil {
		return nil, nil
	}
	return s.descriptors.List(ctx, project)
}

// ListInternal returns the connections the project's own instances publish,
// their outputs: another instance references one by its name, whatever its
// contract (an instance's own output is named after its release by default).
// The connections a provider publishes for the claims bound to it, named
// after the claims' IDs, are the claimers': they are not listed.
func (s *DefaultConnectionService) ListInternal(ctx context.Context, project string) ([]models.InternalConnection, error) {
	descriptors, err := s.outputsOf(ctx, project)
	if err != nil {
		return nil, err
	}
	result := make([]models.InternalConnection, 0)
	for _, d := range descriptors {
		for _, output := range d.Outputs {
			if IsClaimID(output.Name) {
				continue
			}
			result = append(result, s.outputToInternal(d, output, project))
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func (s *DefaultConnectionService) outputToInternal(d repository.Descriptor, output repository.DescriptorOutput, project string) models.InternalConnection {
	values := output.Values
	if values == nil {
		values = map[string]any{}
	}
	entry := models.InternalConnection{
		Name:        output.Name,
		Type:        output.Contract,
		TypeDisplay: output.Contract,
		Service:     d.Service,
		ReleaseName: d.Release,
		Namespace:   project,
		// The descriptor exists: the chart rendered and the engine applied it.
		Status:  "Ready",
		Values:  values,
		Managed: true,
	}
	if descriptor, known := s.catalog.Get(output.Contract); known {
		entry.Icon = descriptor.Icon
		entry.Category = descriptor.Category
		entry.TypeDisplay = descriptor.DisplayName
	}
	entry.Endpoint = endpointFrom(values, s.catalog, output.Contract)
	// host and port stay filled when the contract has them, for the columns that
	// show them separately. Most contracts publish a URI instead.
	if host, ok := values["host"].(string); ok {
		entry.Host = host
		if port, ok := toFloat(values["port"]); ok {
			entry.Port = int32(port)
		}
	}
	if !d.CreatedAt.IsZero() {
		entry.CreatedAt = d.CreatedAt.UTC().Format(time.RFC3339)
	}
	return entry
}

// --- helpers ---

// IsClaimID reports whether an output's name is a claim's ID, the
// connection a provider publishes for a claim bound to it: DNS-1123 labels
// joined by dots (<project>.<release>.<parameter>, <project>.<name>), where
// an output named by its package is a label, the release name by default.
// Such a connection is its claimer's, reached through the claim: the
// selectable outputs leave it out.
func IsClaimID(name string) bool { return compiler.IsClaimID(name) }

// validateRequest checks a submitted connection and returns the values as they
// should be stored: derived fields filled, fields put out of play by the
// submitted engine dropped. On an edit, credentials that were not resubmitted
// are kept as they are rather than reported as missing.
func (s *DefaultConnectionService) validateRequest(ctx context.Context, namespace string, req models.ConnectionRequest, isUpdate bool) (*models.ContractDescriptor, map[string]any, error) {
	if err := treeedit.ValidateName("connection", req.Name); err != nil {
		return nil, nil, invalid("%v", err)
	}

	descriptor, known := s.catalog.Get(req.Type)
	if !known {
		return nil, nil, invalid("unknown contract %q", req.Type)
	}
	if !descriptor.External {
		return nil, nil, invalid("connections of type %q come from a deployed service and cannot be declared by hand", req.Type)
	}
	values := s.catalog.Normalize(req.Type, req.Values)
	// The Secret name is published with the values for the consumers, so a
	// client editing what it just read sends it back. No contract field
	// declares it, and the write paths set it again from the request.
	delete(values, valueSecretRef)
	if req.ExistingSecret != "" {
		// The credentials live in a Secret we do not own, so the payload is not
		// expected to carry them.
		if err := s.checkExistingSecret(ctx, namespace, req, descriptor); err != nil {
			return nil, nil, err
		}
	} else if !isUpdate {
		// The console does not resend a credential that has not changed: on
		// an edit, a missing one is left as it is.
		if err := missingCredentials(descriptor, values); err != nil {
			return nil, nil, err
		}
	}
	return descriptor, values, nil
}

// missingCredentials refuses values that lack a credential the contract
// requires (x-okdp-required of a secret field in play), or give one that
// is no string: the credentials Secret is the console's to write, the
// compile never sees it. The connection's other fields are the compiler's
// to check (checkConnection).
func missingCredentials(descriptor *models.ContractDescriptor, values map[string]any) error {
	if descriptor == nil {
		return nil
	}
	for i := range descriptor.Fields {
		f := &descriptor.Fields[i]
		if !f.Secret || !f.Applies(values) {
			continue
		}
		switch v, present := values[f.Name]; {
		case !present || v == nil || v == "":
			if f.Required {
				return invalid("%s is required", f.Name)
			}
		default:
			if _, ok := v.(string); !ok {
				return invalid("%s must be a string", f.Name)
			}
		}
	}
	return nil
}

// withoutSecrets is values but the contract's secret fields, the keys no
// field of the contract declares kept: the connection as the compile
// checks it, which refuses them.
func withoutSecrets(descriptor *models.ContractDescriptor, values map[string]any) map[string]any {
	out := make(map[string]any, len(values))
	for k, v := range values {
		if descriptor != nil {
			if f, known := descriptor.Field(k); known && f.Secret {
				continue
			}
		}
		out[k] = v
	}
	return out
}

// checkConnection refuses the connection c of project ("" for a test) as
// the compile of the tree checks it at its declaration
// (compiler.CheckConnection): its contract, its fields, none unknown, its
// required ones after completion, their types, its secretRef. A 400 of the
// compiler's message, its problems at their fields.
func (s *DefaultConnectionService) checkConnection(ctx context.Context, project string, c compiler.Connection) error {
	platform := map[string]any{}
	if s.deployments != nil {
		raw, err := s.deployments.ReadPlatformValues(ctx)
		switch {
		case errors.Is(err, treeedit.ErrNotFound):
		case err != nil:
			return err
		default:
			if platform, err = treeedit.DecodeValues(raw); err != nil {
				return fmt.Errorf("%s: %w", compiler.PlatformFile, err)
			}
		}
	}
	if err := compiler.CheckConnection(rules.Standard(), s.catalog.Contracts(), project, c, platform); err != nil {
		return rejected(err)
	}
	return nil
}

// checkExistingSecret refuses a connection pointed at a Secret that is absent or
// does not carry what the contract needs. Storing it anyway would produce a
// connection that looks healthy and whose consumers fail at pod start with
// CreateContainerConfigError, far from the form that caused it.
func (s *DefaultConnectionService) checkExistingSecret(
	ctx context.Context,
	namespace string,
	req models.ConnectionRequest,
	descriptor *models.ContractDescriptor,
) error {
	secretNamespace := namespace
	content, found, err := s.secrets.InspectSecret(ctx, secretNamespace, req.ExistingSecret)
	if err != nil {
		return fmt.Errorf("failed to read the secret %q: %w", req.ExistingSecret, err)
	}
	if !found {
		return invalid("no secret named %q in namespace %q", req.ExistingSecret, secretNamespace)
	}
	// A Secret this server wrote belongs to the connection it was created for,
	// and goes away with it. Pointing a second connection at it would leave that
	// one credentialless the day the first is deleted.
	if content.Managed {
		return invalid("secret %q belongs to another connection, name one this server does not manage", req.ExistingSecret)
	}

	present := make(map[string]bool, len(content.Keys))
	for _, key := range content.Keys {
		present[key] = true
	}
	var missing []string
	for _, field := range descriptor.SecretFields() {
		if !present[field] {
			missing = append(missing, field)
		}
	}
	if len(missing) > 0 {
		return invalid("secret %q does not carry the keys the %s contract needs: %s",
			req.ExistingSecret, descriptor.Name, strings.Join(missing, ", "))
	}
	return nil
}

// splitValues separates the values that go into the Connection spec from the
// credentials, which are stored in a Secret instead.
func splitValues(descriptor *models.ContractDescriptor, values map[string]any) (map[string]any, map[string][]byte) {
	public := map[string]any{}
	secrets := map[string][]byte{}

	for name, value := range values {
		field, known := descriptor.Field(name)
		if !known {
			continue
		}
		if field.Secret {
			if str, ok := value.(string); ok && str != "" {
				secrets[name] = []byte(str)
			}
			continue
		}
		public[name] = value
	}
	return public, secrets
}

func (s *DefaultConnectionService) toResponse(connection *compiler.Connection, namespace string) models.ConnectionResponse {
	values := map[string]any{}
	for k, v := range connection.Values {
		values[k] = v
	}
	if connection.SecretRef != "" {
		values[valueSecretRef] = connection.SecretRef
	}

	response := models.ConnectionResponse{
		Name:        connection.Name,
		Type:        connection.Contract,
		Scope:       models.ConnectionScopeProject,
		Namespace:   namespace,
		Description: connection.Description,
		// Declared in Git: what it points at is only checked by a test.
		Status: "Ready",
		Values: values,
	}
	if descriptor, known := s.catalog.Get(connection.Contract); known {
		response.SecretFields = descriptor.SecretFields()
		if connection.SecretRef != "" && len(response.SecretFields) > 0 {
			response.CredentialsSecret = &models.CredentialsSecretRef{
				Name:      connection.SecretRef,
				Namespace: namespace,
				Keys:      response.SecretFields,
				Owned:     connection.SecretRef == connection.Name+credentialsSecretSuffix,
			}
		}
	}
	return response
}

func (s *DefaultConnectionService) ListSelectable(ctx context.Context, project, contract string) ([]models.SelectableConnection, error) {
	result := []models.SelectableConnection{}
	// The project's, then the platform's (platform.yaml), which every
	// instance may reference.
	for _, scope := range []struct{ project, name string }{{project, models.ConnectionScopeProject}, {"", models.ConnectionScopePlatform}} {
		connections, err := s.deployments.ListConnections(ctx, scope.project)
		if err != nil {
			return nil, err
		}
		for _, c := range connections {
			if contract != "" && c.Contract != contract {
				continue
			}
			engine, sql := sqlTargetOf(c.Name, c.Contract, c.Values)
			result = append(result, models.SelectableConnection{
				Name:        c.Name,
				Scope:       scope.name,
				Type:        c.Contract,
				Status:      "Ready",
				Description: c.Description,
				Engine:      engine,
				Sql:         sql,
			})
		}
	}

	descriptors, err := s.outputsOf(ctx, project)
	if err != nil {
		return nil, err
	}
	for _, d := range descriptors {
		for _, output := range d.Outputs {
			if (contract != "" && output.Contract != contract) || IsClaimID(output.Name) {
				continue
			}
			// An output is no target of the SQL editor: a Trino instance's
			// statements go to .../services/:s/sql.
			engine, _ := output.Values["engine"].(string)
			result = append(result, models.SelectableConnection{
				Name:       output.Name,
				Scope:      models.ConnectionScopeProject,
				Type:       output.Contract,
				Status:     "Ready",
				Managed:    true,
				ProvidedBy: d.Release,
				Engine:     engine,
			})
		}
	}

	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

// endpointFrom returns the address a reader can use to reach a connection.
//
// Which value carries it depends on the contract: database-server publishes a
// host and a port, trino a URI, hive a thrift URI, s3 a URL. Reading host+port
// and nothing else, as this did, meant a Ready trino, hive or s3 connection
// showed "not available yet" while its address sat in its own values, two
// clicks away in the details panel. The descriptor now names the keys that
// carry it, in order of preference.
func endpointFrom(values map[string]any, catalog ContractCatalog, contractName string) string {
	if descriptor, known := catalog.Get(contractName); known {
		for _, key := range descriptor.EndpointFrom {
			if value, ok := values[key].(string); ok && value != "" {
				return value
			}
		}
	}
	// The host+port convention, kept as a fallback so a contract shaped like
	// database-server needs no declaration to be addressable.
	host, ok := values["host"].(string)
	if !ok || host == "" {
		return ""
	}
	if port, ok := toFloat(values["port"]); ok {
		return fmt.Sprintf("%s:%d", host, int32(port))
	}
	return host
}

// ListConsumers returns the services bound to a connection: the instances
// whose parameters name it, a declared connection or an output of an
// instance.
func (s *DefaultConnectionService) ListConsumers(ctx context.Context, project, name string) ([]models.ConnectionConsumer, error) {
	consumers := make([]models.ConnectionConsumer, 0)
	if s.instances == nil {
		return consumers, nil
	}
	instances, err := s.instances(ctx, project)
	if err != nil {
		return nil, err
	}
	for _, instance := range instances {
		for _, bound := range instance.Connections {
			if bound.Name != name {
				continue
			}
			consumers = append(consumers, models.ConnectionConsumer{
				Service:     instance.Name,
				ReleaseName: instance.ReleaseName,
				Status:      instance.Status,
				Effective:   bound.Resolved,
			})
			break
		}
	}
	sort.Slice(consumers, func(i, j int) bool { return consumers[i].ReleaseName < consumers[j].ReleaseName })
	return consumers, nil
}
