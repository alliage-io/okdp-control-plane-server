package service

import (
	"context"
	"fmt"
	"io"
	"strings"

	compiler "github.com/okdp/okdp-compiler"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

type SparkService interface {
	SubmitApp(ctx context.Context, project string, req models.SparkAppRequest) (*models.SparkAppInstance, error)
	SubmitAppYAML(ctx context.Context, project string, req models.SparkAppYAMLRequest) (*models.SparkAppInstance, error)
	GetApp(ctx context.Context, project, name string) (*models.SparkAppInstance, error)
	UpdateApp(ctx context.Context, project, appName string, req models.SparkAppUpdateRequest) (*models.SparkAppInstance, error)
	ListApps(ctx context.Context, project string) ([]models.SparkAppInstance, error)
	DeleteApp(ctx context.Context, project, name string) error
	WatchApps(ctx context.Context, project string) (watch.Interface, error)
	GetDriverLogs(ctx context.Context, project, appName, container string, tailLines int64, follow bool) (io.ReadCloser, error)
	GetSparkConfig(ctx context.Context) (*models.SparkConfig, error)
	GetSparkUI(ctx context.Context, project, appName string) (*models.SparkUIInfo, error)
	GetAppSchema(ctx context.Context) (map[string]interface{}, error)
}

type DefaultSparkService struct {
	sparkRepo   repository.SparkAppRepository
	contextRepo repository.PlatformRepository
	typedClient kubernetes.Interface
	// tree is the compile of the tree: the spark-rbac instance of each
	// project (sparkPolicy).
	tree func(ctx context.Context) (*compiler.Tree, error)
}

// NewDefaultSparkService builds the Spark service; tree is the compile of
// the tree the policy of a project's SparkApplications is read from.
func NewDefaultSparkService(sparkRepo repository.SparkAppRepository, contextRepo repository.PlatformRepository, typedClient kubernetes.Interface, tree func(ctx context.Context) (*compiler.Tree, error)) *DefaultSparkService {
	return &DefaultSparkService{
		sparkRepo:   sparkRepo,
		contextRepo: contextRepo,
		typedClient: typedClient,
		tree:        tree,
	}
}

// sparkTTL is how long a finished SparkApplication is kept.
const sparkTTL = int64(3600)

// SubmitApp builds a SparkApplication from the request, checked against
// the project's policy (sparkPolicy): its driver and executors run as the
// project's spark-rbac ServiceAccount.
func (s *DefaultSparkService) SubmitApp(ctx context.Context, project string, req models.SparkAppRequest) (*models.SparkAppInstance, error) {
	policy, err := s.policy(ctx, project)
	if err != nil {
		return nil, err
	}
	if err := policy.checkSparkRequest(req); err != nil {
		return nil, err
	}
	return s.create(ctx, project, s.build(ctx, project, policy, req))
}

// SubmitAppYAML reads a SparkApplication YAML into the form's request
// (parseSparkManifest: any field the server does not build is refused) and
// submits it as SubmitApp does.
func (s *DefaultSparkService) SubmitAppYAML(ctx context.Context, project string, req models.SparkAppYAMLRequest) (*models.SparkAppInstance, error) {
	policy, err := s.policy(ctx, project)
	if err != nil {
		return nil, err
	}
	manifest, err := parseSparkManifest(req.YAML, project, policy.serviceAccount)
	if err != nil {
		return nil, err
	}
	form := manifest.request()
	if err := policy.checkSparkRequest(form); err != nil {
		return nil, err
	}
	app := s.build(ctx, project, policy, form)
	manifest.extras(app)
	return s.create(ctx, project, app)
}

func (s *DefaultSparkService) create(ctx context.Context, project string, app *crd.SparkApplication) (*models.SparkAppInstance, error) {
	if err := s.sparkRepo.Create(ctx, project, app); err != nil {
		return nil, err
	}
	return &models.SparkAppInstance{
		Name:   app.Name,
		Type:   app.Spec.Type,
		Mode:   app.Spec.Mode,
		Image:  app.Spec.Image,
		Status: "SUBMITTED",
	}, nil
}

// build is the SparkApplication of a checked request.
func (s *DefaultSparkService) build(ctx context.Context, project string, policy *sparkPolicy, req models.SparkAppRequest) *crd.SparkApplication {
	mode := req.Mode
	if mode == "" {
		mode = "cluster"
	}
	ttl := sparkTTL
	return &crd.SparkApplication{
		TypeMeta: metav1.TypeMeta{
			APIVersion: crd.SparkAppAPIVersion,
			Kind:       crd.SparkAppKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: project,
			Labels: map[string]string{
				crd.LabelProject: project,
			},
		},
		Spec: crd.SparkApplicationSpec{
			Type:                req.Type,
			Mode:                mode,
			Image:               req.Image,
			MainClass:           req.MainClass,
			MainApplicationFile: req.MainApplicationFile,
			Arguments:           req.Arguments,
			SparkVersion:        req.SparkVersion,
			SparkConf:           s.completeConf(ctx, project, req.SparkConf),
			TimeToLiveSeconds:   &ttl,
			Driver: crd.SparkPodSpec{
				Cores:          req.DriverCores,
				Memory:         req.DriverMemory,
				ServiceAccount: policy.serviceAccount,
			},
			Executor: crd.ExecutorSpec{
				SparkPodSpec: crd.SparkPodSpec{
					Cores:          req.ExecutorCores,
					Memory:         req.ExecutorMemory,
					ServiceAccount: policy.serviceAccount,
				},
				Instances: req.ExecutorInstances,
			},
		},
	}
}

// completeConf is a checked sparkConf with what the server adds: the
// Spark UI behind the project's history server, the event log on its
// volume when the project has one.
func (s *DefaultSparkService) completeConf(ctx context.Context, project string, requested map[string]string) map[string]string {
	sparkConf := map[string]string{}
	for k, v := range requested {
		sparkConf[k] = v
	}
	if _, ok := sparkConf["spark.ui.enabled"]; !ok {
		sparkConf["spark.ui.enabled"] = "true"
	}
	if _, ok := sparkConf["spark.ui.port"]; !ok {
		sparkConf["spark.ui.port"] = "4040"
	}
	if _, ok := sparkConf["spark.ui.reverseProxy"]; !ok {
		sparkConf["spark.ui.reverseProxy"] = "true"
	}
	if _, ok := sparkConf["spark.ui.reverseProxyUrl"]; !ok {
		if ingressSuffix, _ := s.contextRepo.GetIngressSuffix(ctx); ingressSuffix != "" {
			releaseName := project + "-spark-history-server"
			sparkConf["spark.ui.reverseProxyUrl"] = fmt.Sprintf("https://%s.%s", releaseName, ingressSuffix)
		}
	}

	if eventPVCName := s.findEventLogPVC(ctx, project); eventPVCName != "" {
		if _, ok := sparkConf["spark.eventLog.enabled"]; !ok {
			sparkConf["spark.eventLog.enabled"] = "true"
		}
		if _, ok := sparkConf["spark.eventLog.dir"]; !ok {
			sparkConf["spark.eventLog.dir"] = "file:///mnt/spark-events"
		}
		s.injectEventLogVolumeConf(sparkConf, eventPVCName)
	}
	return sparkConf
}

func (s *DefaultSparkService) GetApp(ctx context.Context, project, name string) (*models.SparkAppInstance, error) {
	app, err := s.sparkRepo.Get(ctx, project, name)
	if err != nil {
		return nil, err
	}

	instance := sparkAppToInstance(app)
	return &instance, nil
}

func (s *DefaultSparkService) ListApps(ctx context.Context, project string) ([]models.SparkAppInstance, error) {
	apps, err := s.sparkRepo.List(ctx, project, project)
	if err != nil {
		return nil, err
	}

	instances := make([]models.SparkAppInstance, 0, len(apps))
	for _, app := range apps {
		instances = append(instances, sparkAppToInstance(&app))
	}
	return instances, nil
}

func (s *DefaultSparkService) DeleteApp(ctx context.Context, project, name string) error {
	return s.sparkRepo.Delete(ctx, project, name)
}

func (s *DefaultSparkService) WatchApps(ctx context.Context, project string) (watch.Interface, error) {
	return s.sparkRepo.Watch(ctx, project, project)
}

func (s *DefaultSparkService) UpdateApp(ctx context.Context, project, appName string, req models.SparkAppUpdateRequest) (*models.SparkAppInstance, error) {
	policy, err := s.policy(ctx, project)
	if err != nil {
		return nil, err
	}
	if req.Image != "" {
		if err := policy.checkImage(req.Image); err != nil {
			return nil, err
		}
	}
	if req.SparkConf != nil {
		if err := checkSparkConf(req.SparkConf); err != nil {
			return nil, err
		}
	}
	app, err := s.sparkRepo.Get(ctx, project, appName)
	if err != nil {
		return nil, err
	}

	// The typed object holds only the fields the server builds: what else
	// the stored one had is dropped by the update. Its pods run as the
	// project's ServiceAccount, whatever it was.
	app.Spec.Driver.ServiceAccount = policy.serviceAccount
	app.Spec.Executor.ServiceAccount = policy.serviceAccount
	if req.Image != "" {
		app.Spec.Image = req.Image
	}
	if req.MainClass != "" {
		app.Spec.MainClass = req.MainClass
	}
	if req.MainApplicationFile != "" {
		app.Spec.MainApplicationFile = req.MainApplicationFile
	}
	if req.Arguments != nil {
		app.Spec.Arguments = req.Arguments
	}
	if req.DriverCores != nil {
		app.Spec.Driver.Cores = req.DriverCores
	}
	if req.DriverMemory != "" {
		app.Spec.Driver.Memory = req.DriverMemory
	}
	if req.ExecutorInstances != nil {
		app.Spec.Executor.Instances = req.ExecutorInstances
	}
	if req.ExecutorCores != nil {
		app.Spec.Executor.Cores = req.ExecutorCores
	}
	if req.ExecutorMemory != "" {
		app.Spec.Executor.Memory = req.ExecutorMemory
	}
	if req.SparkConf != nil {
		app.Spec.SparkConf = s.completeConf(ctx, project, req.SparkConf)
	} else if err := checkSparkConf(withoutServerConf(app.Spec.SparkConf)); err != nil {
		return nil, err
	}

	if err := s.sparkRepo.Update(ctx, project, app); err != nil {
		return nil, err
	}

	instance := sparkAppToInstance(app)
	return &instance, nil
}

func (s *DefaultSparkService) GetSparkUI(ctx context.Context, project, appName string) (*models.SparkUIInfo, error) {
	app, err := s.sparkRepo.Get(ctx, project, appName)
	if err != nil {
		return nil, err
	}

	ingressSuffix, _ := s.contextRepo.GetIngressSuffix(ctx)

	releaseName := project + "-spark-history-server"
	info := &models.SparkUIInfo{
		ServiceName: app.Status.DriverInfo.WebUIServiceName,
		Available:   false,
	}

	if ingressSuffix != "" {
		baseURL := fmt.Sprintf("https://%s.%s", releaseName, ingressSuffix)
		if app.Status.SparkApplicationID != "" {
			info.HistoryServerURL = fmt.Sprintf("%s/history/%s/jobs/", baseURL, app.Status.SparkApplicationID)
		} else {
			info.HistoryServerURL = baseURL + "/"
		}
	}

	if app.Status.AppState.State == "RUNNING" && app.Status.DriverInfo.WebUIServiceName != "" {
		if ingressSuffix != "" {
			sparkAppID := app.Status.SparkApplicationID
			if sparkAppID == "" {
				sparkAppID = appName
			}
			info.UIAddress = fmt.Sprintf("https://%s.%s/proxy/%s/jobs/", releaseName, ingressSuffix, sparkAppID)
		}
		info.Available = true
	}

	return info, nil
}

func (s *DefaultSparkService) GetDriverLogs(ctx context.Context, project, appName, container string, tailLines int64, follow bool) (io.ReadCloser, error) {
	app, err := s.sparkRepo.Get(ctx, project, appName)
	if err != nil {
		return nil, fmt.Errorf("failed to get SparkApplication: %w", err)
	}

	podName := app.Status.DriverInfo.PodName
	if podName == "" {
		return nil, fmt.Errorf("driver pod not yet available for SparkApplication %s", appName)
	}

	opts := &corev1.PodLogOptions{
		Follow: follow,
	}
	if tailLines > 0 {
		opts.TailLines = &tailLines
	}
	if container != "" {
		opts.Container = container
	}

	stream, err := s.typedClient.CoreV1().Pods(project).GetLogs(podName, opts).Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get driver pod logs: %w", err)
	}
	return stream, nil
}

func (s *DefaultSparkService) GetSparkConfig(ctx context.Context) (*models.SparkConfig, error) {
	return s.contextRepo.GetSparkConfig(ctx)
}

func (s *DefaultSparkService) GetAppSchema(ctx context.Context) (map[string]interface{}, error) {
	return s.sparkRepo.GetCRDSchema(ctx)
}

func sparkAppToInstance(app *crd.SparkApplication) models.SparkAppInstance {
	createdAt := ""
	if !app.CreationTimestamp.IsZero() {
		createdAt = app.CreationTimestamp.Format("2006-01-02T15:04:05Z07:00")
	}

	completedAt := ""
	if app.Annotations != nil {
		if v, ok := app.Annotations["sparkoperator.k8s.io/completed-at"]; ok {
			completedAt = v
		}
	}

	executors := make(map[string]string)
	for k, v := range app.Status.ExecutorState {
		executors[k] = v
	}

	return models.SparkAppInstance{
		Name:          app.Name,
		Type:          app.Spec.Type,
		Mode:          app.Spec.Mode,
		Image:         app.Spec.Image,
		Status:        app.Status.AppState.State,
		ErrorMessage:  app.Status.AppState.ErrorMessage,
		DriverPodName: app.Status.DriverInfo.PodName,
		CreatedAt:     createdAt,
		CompletedAt:   completedAt,
		Executors:     executors,
	}
}

func (s *DefaultSparkService) findEventLogPVC(ctx context.Context, project string) string {
	pvcName := project + "-spark-history-server-events"
	_, err := s.typedClient.CoreV1().PersistentVolumeClaims(project).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	return pvcName
}

func (s *DefaultSparkService) injectEventLogVolumeConf(sparkConf map[string]string, pvcName string) {
	prefix := "spark.kubernetes.driver.volumes.persistentVolumeClaim.spark-events."
	if _, ok := sparkConf[prefix+"options.claimName"]; !ok {
		sparkConf[prefix+"options.claimName"] = pvcName
		sparkConf[prefix+"mount.path"] = "/mnt/spark-events"
	}
	exPrefix := "spark.kubernetes.executor.volumes.persistentVolumeClaim.spark-events."
	if _, ok := sparkConf[exPrefix+"options.claimName"]; !ok {
		sparkConf[exPrefix+"options.claimName"] = pvcName
		sparkConf[exPrefix+"mount.path"] = "/mnt/spark-events"
	}
}

// withoutServerConf is a stored sparkConf without the keys the server adds
// (completeConf): the event log volume.
func withoutServerConf(conf map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range conf {
		if strings.Contains(k, ".volumes.persistentVolumeClaim.spark-events.") {
			continue
		}
		out[k] = v
	}
	return out
}
