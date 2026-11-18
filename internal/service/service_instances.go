package service

import (
	"context"
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
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-gitops/treeedit"
	corev1 "k8s.io/api/core/v1"
)

// --- Instances: desired state in Git ---

// ReleaseNameTakenError is a new instance whose release name <p>-<i> another
// project's instance already uses. The handler answers 409 with its own
// code, distinct from a duplicate instance.
type ReleaseNameTakenError struct{ *treeedit.ErrReleaseTaken }

// IsReleaseNameTaken reports whether err is a release-name collision.
func IsReleaseNameTaken(err error) bool {
	var taken *ReleaseNameTakenError
	return errors.As(err, &taken)
}

// gitError maps the errors of the deployments repository to the domain
// errors the handlers turn into 404 and 409, and the refusals of a compile
// or of a delete to validation errors (400).
func gitError(err error, name string) error {
	var taken *treeedit.ErrReleaseTaken
	var compileErr *treeedit.CompileError
	var inUse *treeedit.ErrInUse
	switch {
	case err == nil:
		return nil
	case errors.As(err, &taken):
		return &ReleaseNameTakenError{taken}
	case errors.As(err, &compileErr):
		return DoesNotCompile(compileErr)
	case errors.As(err, &inUse):
		return invalid("instance %q is still referenced by %s; change them first", name, strings.Join(inUse.Users, ", "))
	case errors.Is(err, treeedit.ErrNotFound):
		return apperr.NotFound(servicesResource, name)
	case errors.Is(err, treeedit.ErrExists):
		return apperr.AlreadyExists(servicesResource, name)
	default:
		return err
	}
}

func (s *DefaultServiceService) DeployService(ctx context.Context, project string, req models.ServiceRequest) (*models.ServiceInstance, error) {
	if req.Service == "" {
		return nil, invalid("service name is required")
	}

	svc, err := s.resolvePlatformService(ctx, req.Service)
	if err != nil {
		return nil, err
	}

	deployTag := req.Tag
	if deployTag == "" {
		deployTag = svc.DefaultVersion
		if s.schemaService != nil {
			if versions, err := s.schemaService.ListPackageTags(ctx, req.Service, ""); err == nil {
				deployTag = DefaultVersion(svc.DefaultVersion, versions)
			}
		}
	}

	instanceName := req.InstanceName
	if instanceName == "" {
		instanceName = req.Service
	}
	if err := treeedit.ValidateName("instance", instanceName); err != nil {
		return nil, invalid("%v", err)
	}

	// The parameters are checked by the compile of the write: its package's
	// schema, its connection references, its claims bound to their
	// providers, the keys the context gives (global, connections, claims),
	// each problem at its field (a 400 does-not-compile).

	values := req.Parameters
	if values == nil {
		values = map[string]any{}
	}
	state := treeedit.Instance{
		Name:    instanceName,
		Project: project,
		Service: req.Service,
		Version: deployTag,
		Values:  values,
	}
	if err := state.Validate(); err != nil {
		return nil, invalid("%v", err)
	}

	revision, err := s.deployments.CreateInstance(ctx, auth.ActorFrom(ctx), state)
	if err != nil {
		return nil, gitError(err, instanceName)
	}
	s.changes.Notify(project, instanceName, "ADDED")

	instance := s.assemble(state, repository.EngineStatus{}, nil, nil, nil)
	instance.Revision = revision
	return &instance, nil
}

// UpdateServiceParameters applies a JSON Merge Patch to the parameters of an
// instance (and its chart version when req.Tag is set), checked by the
// compile of the write: the patched instance, its package's schema, its
// connection references and its claims, each problem at its field.
func (s *DefaultServiceService) UpdateServiceParameters(ctx context.Context, project, name string, req models.ServiceUpdateRequest) (*models.ServiceInstance, error) {
	state, revision, err := s.deployments.UpdateInstance(ctx, auth.ActorFrom(ctx), project, name, func(st *treeedit.Instance) error {
		if req.Tag != "" {
			st.Version = req.Tag
		}
		// JSON Merge Patch (RFC 7386): a submitted key replaces the stored
		// one, null deletes it, objects merge recursively, arrays replace.
		st.Values = MergePatch(st.Values, req.Parameters)
		return nil
	})
	if err != nil {
		return nil, gitError(err, name)
	}
	s.changes.Notify(project, name, "MODIFIED")

	instance, err := s.GetService(ctx, project, name)
	if err != nil {
		fallback := s.assemble(*state, repository.EngineStatus{}, nil, nil, nil)
		instance = &fallback
	}
	instance.Revision = revision
	return instance, nil
}

func (s *DefaultServiceService) DeleteService(ctx context.Context, project, name string) error {
	if _, err := s.deployments.DeleteInstance(ctx, auth.ActorFrom(ctx), project, name); err != nil {
		return gitError(err, name)
	}
	s.changes.Notify(project, name, "DELETED")

	// No OAuth client is deleted: the control plane made none (the
	// compiled output's oidc-dcr Jobs register them, or the platform
	// does), and a client named after the release may be anyone's.
	releaseName := compiler.ReleaseName(project, name)
	s.cleanupUserResources(ctx, project, releaseName)
	return nil
}

// MergePatch applies patch to target as a JSON Merge Patch (RFC 7386) and
// returns the result: a null value removes the key, a nested object merges
// into the stored one (recursively), anything else (arrays included)
// replaces it. target is not modified. The result is never nil.
func MergePatch(target, patch map[string]any) map[string]any {
	out := make(map[string]any, len(target)+len(patch))
	for k, v := range target {
		out[k] = v
	}
	for k, v := range patch {
		if v == nil {
			delete(out, k)
			continue
		}
		if patchObject, ok := v.(map[string]any); ok {
			current, _ := out[k].(map[string]any)
			out[k] = MergePatch(current, patchObject)
			continue
		}
		out[k] = v
	}
	return out
}

// declaredConnections lists the connections an instance of a project may
// reference by name: the project's (its project.yaml) and the platform's
// (platform.yaml).
func (s *DefaultServiceService) declaredConnections(ctx context.Context, project string) (map[string]bool, error) {
	names := map[string]bool{}
	for _, p := range []string{"", project} {
		connections, err := s.deployments.ListConnections(ctx, p)
		if err != nil {
			return nil, err
		}
		for _, c := range connections {
			names[c.Name] = true
		}
	}
	return names, nil
}

// walkStrings calls fn on every string of a JSON-like value.
func walkStrings(v any, fn func(string)) {
	switch t := v.(type) {
	case string:
		fn(t)
	case map[string]any:
		for _, item := range t {
			walkStrings(item, fn)
		}
	case []any:
		for _, item := range t {
			walkStrings(item, fn)
		}
	}
}

// --- Instances: observed state from the cluster ---

// projectObservation is what the cluster says about a project's releases.
type projectObservation struct {
	engine      map[string]repository.EngineStatus
	descriptors map[string]*repository.Descriptor
	// outputs are the connections the deployed instances provide, by name.
	outputs map[string]bool
	pods    []corev1.Pod
}

func (s *DefaultServiceService) observe(ctx context.Context, project string) projectObservation {
	obs := projectObservation{
		engine:      map[string]repository.EngineStatus{},
		descriptors: map[string]*repository.Descriptor{},
		outputs:     map[string]bool{},
	}
	if s.engine != nil {
		if statuses, err := s.engine.List(ctx, project); err != nil {
			slog.Warn("Could not read the GitOps engine status", "error", err, "engine", s.engine.Name())
		} else {
			obs.engine = statuses
		}
	}
	if s.descriptors != nil {
		if list, err := s.descriptors.List(ctx, project); err != nil {
			slog.Warn("Could not read what the instances publish", "error", err)
		} else {
			for i := range list {
				obs.descriptors[list[i].Release] = &list[i]
				for _, o := range list[i].Outputs {
					obs.outputs[o.Name] = true
				}
			}
		}
	}
	if s.workloads != nil {
		if pods, err := s.workloads.ListPods(ctx, project, repository.LabelAppInstance); err == nil {
			obs.pods = pods
		}
	}
	return obs
}

// assemble builds the console view of an instance from its declaration and
// what the cluster reports.
func (s *DefaultServiceService) assemble(st treeedit.Instance, engine repository.EngineStatus, descriptor *repository.Descriptor, externals map[string]bool, obs *projectObservation) models.ServiceInstance {
	release := st.Release()
	instance := models.ServiceInstance{
		Name:            st.Name,
		ReleaseName:     release,
		Service:         st.Service,
		ServiceTag:      st.Version,
		TargetNamespace: st.Project,
		Parameters:      st.Values,
	}

	instance.Status, instance.StatusMessage = instanceStatus(st, engine, s.engineName())
	if !engine.CreatedAt.IsZero() {
		instance.CreatedAt = engine.CreatedAt.UTC().Format(time.RFC3339)
	}
	if descriptor != nil {
		instance.URL = descriptor.URL
		instance.Usage = descriptor.Usage
		if instance.CreatedAt == "" && !descriptor.CreatedAt.IsZero() {
			instance.CreatedAt = descriptor.CreatedAt.UTC().Format(time.RFC3339)
		}
	}
	if obs != nil && instance.Status == repository.PhaseReady {
		instance.Status = s.checkPodHealth(obs.pods, release, instance.Status)
	}
	instance.Connections = boundConnections(st, externals, obs)
	return instance
}

func (s *DefaultServiceService) engineName() string {
	if s.engine == nil {
		return "the GitOps engine"
	}
	return s.engine.Name()
}

// instanceStatus maps the engine's report to the console's status words.
// Anything the engine has not picked up yet is Pending: committed, waiting.
func instanceStatus(inst treeedit.Instance, engine repository.EngineStatus, engineName string) (string, string) {
	if !engine.Found {
		return repository.PhasePending, fmt.Sprintf("Committed to Git, waiting for %s to reconcile it.", engineName)
	}
	if engine.Phase == repository.PhaseReady && engine.Revision != "" && engine.Revision != inst.Version {
		return repository.PhaseUpdating, fmt.Sprintf("Moving from version %s to %s.", engine.Revision, inst.Version)
	}
	message := engine.Message
	if engine.Phase == repository.PhaseReady {
		message = ""
	}
	return engine.Phase, truncateMessage(message)
}

// boundConnections describes what an instance is wired to: the declared
// connections its values name (externals: the project's and the
// platform's), and the connections of other instances its values name. A
// connection reference is a string parameter (x-okdp-connection-ref),
// possibly nested (a catalog of a Trino), so every string of the values is
// a candidate; only known connections match. Resolved says whether the
// connection exists: declared in Git for an external one, a deployed
// provider for an internal one.
func boundConnections(st treeedit.Instance, externals map[string]bool, obs *projectObservation) []models.ServiceConnection {
	project := st.Project
	byName := map[string]models.ServiceConnection{}
	self := st.Release()
	walkStrings(st.Values, func(v string) {
		if _, seen := byName[v]; seen || v == self {
			return
		}
		switch {
		case externals[v]:
			byName[v] = models.ServiceConnection{Name: v, Namespace: project, Kind: models.ConnectionKindExternal, Resolved: true}
		case obs != nil && obs.outputs[v]:
			byName[v] = models.ServiceConnection{Name: v, Namespace: project, Kind: models.ConnectionKindInternal, Resolved: true}
		}
	})
	if len(byName) == 0 {
		return nil
	}
	out := make([]models.ServiceConnection, 0, len(byName))
	for _, c := range byName {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *DefaultServiceService) ListServices(ctx context.Context, project string) ([]models.ServiceInstance, error) {
	states, err := s.deployments.ListInstances(ctx, project)
	if err != nil {
		return nil, err
	}
	externals, err := s.declaredConnections(ctx, project)
	if err != nil {
		return nil, err
	}
	obs := s.observe(ctx, project)

	result := make([]models.ServiceInstance, 0, len(states))
	for _, st := range states {
		release := st.Release()
		result = append(result, s.assemble(st, obs.engine[release], obs.descriptors[release], externals, &obs))
	}
	return result, nil
}

func (s *DefaultServiceService) GetService(ctx context.Context, project, name string) (*models.ServiceInstance, error) {
	st, err := s.deployments.GetInstance(ctx, project, name)
	if err != nil {
		return nil, gitError(err, name)
	}
	externals, err := s.declaredConnections(ctx, project)
	if err != nil {
		return nil, err
	}
	obs := s.observe(ctx, project)
	release := st.Release()
	instance := s.assemble(*st, obs.engine[release], obs.descriptors[release], externals, &obs)
	// Only look for an explanation when the instance is not Ready and the
	// engine gave none: scanning events has a non-trivial API cost per request.
	if instance.Status != repository.PhaseReady && instance.Status != repository.PhasePending && instance.StatusMessage == "" {
		instance.StatusMessage = s.latestWarningMessage(ctx, project, release)
	}
	return &instance, nil
}

// latestWarningMessage scans Warning events in a namespace and returns the
// most recent message whose involvedObject belongs to the given release.
func (s *DefaultServiceService) latestWarningMessage(ctx context.Context, project, releaseName string) string {
	if project == "" || releaseName == "" || s.workloads == nil {
		return ""
	}
	events, err := s.workloads.WarningEvents(ctx, project)
	if err != nil {
		slog.Debug(fmt.Sprintf("latestWarningMessage: event list failed in %s", project), "error", err)
		return ""
	}

	var (
		latestAt  time.Time
		latestMsg string
	)
	for i := range events {
		involvedName := events[i].InvolvedObject.Name
		if involvedName != releaseName && !strings.HasPrefix(involvedName, releaseName+"-") {
			continue
		}
		if ts := eventTimestamp(&events[i]); ts.After(latestAt) {
			latestAt = ts
			latestMsg = truncateMessage(events[i].Message)
		}
	}
	return latestMsg
}

// eventTimestamp reads the most useful timestamp of an Event: legacy events
// carry lastTimestamp, modern ones eventTime, and metadata.creationTimestamp
// is the last resort.
func eventTimestamp(e *corev1.Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}

// truncateMessage keeps UI tooltips readable. K8s validation errors can run
// several hundred characters, we cap to 400 so the UI does not blow up.
func truncateMessage(msg string) string {
	const max = 400
	if len(msg) <= max {
		return msg
	}
	return msg[:max] + "…"
}

func (s *DefaultServiceService) resolvePlatformService(ctx context.Context, name string) (*models.PlatformService, error) {
	services, err := s.platformRepo.GetPlatformServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read platform services: %w", err)
	}

	for _, svc := range services {
		if svc.Name == name {
			return &svc, nil
		}
	}
	return nil, invalid("service %q is not available in the platform", name)
}
