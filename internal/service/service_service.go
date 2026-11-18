package service

import (
	"context"
	"errors"
	"io"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-gitops/gitops"
	corev1 "k8s.io/api/core/v1"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
)

// Catalog management errors. Handlers map these to HTTP 400/404/409.
var (
	ErrCatalogValidation      = errors.New("invalid catalog service")
	ErrCatalogServiceExists   = errors.New("service already exists in the catalog")
	ErrCatalogServiceNotFound = errors.New("service not found in the catalog")
)

// servicesResource names the resource in the not-found and conflict errors
// the handlers map to 404 and 409.
const servicesResource = "services.okdp.io"

// Workloads is what the services read of the workloads of a project's
// namespace, through repository.Workloads.
type Workloads interface {
	ListPods(ctx context.Context, namespace, selector string) ([]corev1.Pod, error)
	ListPodNames(ctx context.Context, namespace string) ([]string, error)
	GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error)
	PodLogs(ctx context.Context, namespace, pod, container string, tailLines int64, follow bool) (io.ReadCloser, error)
	DeletePod(ctx context.Context, namespace, name string) error
	WarningEvents(ctx context.Context, namespace string) ([]corev1.Event, error)
	ListPVCNames(ctx context.Context, namespace string) ([]string, error)
	PVCExists(ctx context.Context, namespace, name string) (bool, error)
	DeletePVC(ctx context.Context, namespace, name string) error
	PodMetrics(ctx context.Context, namespace string) ([]metricsv1beta1.PodMetrics, error)
}

// ServiceService manages platform services (deploy, monitor, delete) and exposes the catalog.
//
// Desired state is written to the deployments Git repositories, the tree
// only; what each compiled instance publishes is read from their output
// (okdp.info.yaml), as okdp-gitops compile wrote it in CI (it may lag
// behind the sources), observed state from the cluster (workloads, and the
// objects of the GitOps engine).
type ServiceService interface {
	GetPlatformServices(ctx context.Context) ([]models.PlatformService, error)
	// AddPlatformService exposes a new service in the catalog (catalogs.yaml).
	AddPlatformService(ctx context.Context, svc models.PlatformService) (*models.PlatformService, error)
	// UpdatePlatformService updates an existing catalog service (name from the path).
	UpdatePlatformService(ctx context.Context, name string, svc models.PlatformService) (*models.PlatformService, error)
	// RemovePlatformService removes a service from the catalog.
	RemovePlatformService(ctx context.Context, name string) error
	DeployService(ctx context.Context, project string, req models.ServiceRequest) (*models.ServiceInstance, error)
	ListServices(ctx context.Context, project string) ([]models.ServiceInstance, error)
	GetService(ctx context.Context, project, name string) (*models.ServiceInstance, error)
	UpdateServiceParameters(ctx context.Context, project, name string, req models.ServiceUpdateRequest) (*models.ServiceInstance, error)
	DeleteService(ctx context.Context, project, name string) error
	// WatchServices streams the instances of a project as they change, until
	// ctx ends or an underlying watch closes (the channel is then closed).
	WatchServices(ctx context.Context, project string) (<-chan ServiceEvent, error)

	GetMenuCategories(ctx context.Context) ([]models.MenuCategory, error)

	GetProfileImages(ctx context.Context) (map[string][]models.ProfileImage, error)

	ListPods(ctx context.Context, project, serviceName string) ([]models.Pod, error)
	// GetPodLogs streams the logs of a pod of the instance serviceName; a pod
	// of another instance, or of none, is not found.
	GetPodLogs(ctx context.Context, project, serviceName, podName, container string, tailLines int64, follow bool) (io.ReadCloser, error)
	GetServiceMetrics(ctx context.Context, project, serviceName string) (*models.ServiceMetrics, error)
	GetProjectMetrics(ctx context.Context, project string) (map[string]*models.ServiceMetrics, error)
}

// ServiceEvent is one change of a project's instances, as streamed to the
// console: Type is ADDED, MODIFIED or DELETED.
type ServiceEvent struct {
	Type   string                 `json:"type"`
	Object models.ServiceInstance `json:"object"`
}

// PackageVersions is what a catalog's packages tell the service view:
// their versions and the catalog holding each (DefaultPackageSchemaService).
type PackageVersions interface {
	ListVersionsForServices(ctx context.Context, services []models.PlatformService) map[string][]string
	// ListPackageTags returns the versions of a service's package in the
	// catalogs, even if the service is not (yet) in the console menu.
	ListPackageTags(ctx context.Context, serviceName, repositoryOverride string) ([]string, error)
	// CatalogOf names the catalog repository of each service of the
	// catalogs.
	CatalogOf(ctx context.Context) map[string]string
}

// ServiceDeps gathers what DefaultServiceService needs.
type ServiceDeps struct {
	Deployments     *gitops.Deployments
	PlatformRepo    repository.PlatformRepository
	CatalogWriter   repository.CatalogWriterRepository
	SchemaService   PackageVersions
	Engine          repository.EngineAdapter
	Descriptors     repository.DescriptorRepository
	Workloads       Workloads
	SidecarPrefixes []string
	// Changes is shared with the other writers of the deployments repository,
	// so a live stream hears about their commits too. Created when nil.
	Changes *ChangeNotifier
}

type DefaultServiceService struct {
	deployments     *gitops.Deployments
	platformRepo    repository.PlatformRepository
	catalogWriter   repository.CatalogWriterRepository
	schemaService   PackageVersions
	engine          repository.EngineAdapter
	descriptors     repository.DescriptorRepository
	workloads       Workloads
	sidecarPrefixes []string
	changes         *ChangeNotifier
}

func NewDefaultServiceService(d ServiceDeps) *DefaultServiceService {
	if d.Changes == nil {
		d.Changes = NewChangeNotifier()
	}
	return &DefaultServiceService{
		deployments:     d.Deployments,
		platformRepo:    d.PlatformRepo,
		catalogWriter:   d.CatalogWriter,
		schemaService:   d.SchemaService,
		engine:          d.Engine,
		descriptors:     d.Descriptors,
		workloads:       d.Workloads,
		sidecarPrefixes: d.SidecarPrefixes,
		changes:         d.Changes,
	}
}
