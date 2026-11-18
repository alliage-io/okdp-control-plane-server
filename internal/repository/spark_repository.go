package repository

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// SparkAppRepository stores SparkApplications. It takes the typed object
// only, the fields the server builds (sparkPolicy): a manifest is never
// passed through.
type SparkAppRepository interface {
	Create(ctx context.Context, namespace string, app *crd.SparkApplication) error
	Get(ctx context.Context, namespace, name string) (*crd.SparkApplication, error)
	Update(ctx context.Context, namespace string, app *crd.SparkApplication) error
	List(ctx context.Context, namespace, project string) ([]crd.SparkApplication, error)
	Delete(ctx context.Context, namespace, name string) error
	// Watch streams the SparkApplications of a project as they change, until
	// ctx ends or the watch closes (the channel is then closed).
	Watch(ctx context.Context, namespace, project string) (<-chan models.SparkAppEvent, error)
	GetCRDSchema(ctx context.Context) (map[string]interface{}, error)
}

type k8sSparkAppRepository struct {
	client dynamic.Interface
	gvr    schema.GroupVersionResource
}

func NewSparkAppRepository(client dynamic.Interface) SparkAppRepository {
	return &k8sSparkAppRepository{
		client: client,
		gvr:    crd.GetSparkAppGVR(),
	}
}

func (r *k8sSparkAppRepository) Create(ctx context.Context, namespace string, app *crd.SparkApplication) error {
	unstructuredMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(app)
	if err != nil {
		return fmt.Errorf("failed to convert SparkApplication to unstructured: %w", err)
	}

	_, err = r.client.Resource(r.gvr).Namespace(namespace).Create(
		ctx, &unstructured.Unstructured{Object: unstructuredMap}, metav1.CreateOptions{},
	)
	return fromK8s(err)
}

func (r *k8sSparkAppRepository) Update(ctx context.Context, namespace string, app *crd.SparkApplication) error {
	unstructuredMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(app)
	if err != nil {
		return fmt.Errorf("failed to convert SparkApplication to unstructured: %w", err)
	}

	_, err = r.client.Resource(r.gvr).Namespace(namespace).Update(
		ctx, &unstructured.Unstructured{Object: unstructuredMap}, metav1.UpdateOptions{},
	)
	return fromK8s(err)
}

func (r *k8sSparkAppRepository) Get(ctx context.Context, namespace, name string) (*crd.SparkApplication, error) {
	u, err := r.client.Resource(r.gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fromK8s(err)
	}

	var app crd.SparkApplication
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &app); err != nil {
		return nil, fmt.Errorf("failed to convert from unstructured: %w", err)
	}
	return &app, nil
}

func (r *k8sSparkAppRepository) List(ctx context.Context, namespace, project string) ([]crd.SparkApplication, error) {
	opts := metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", crd.LabelProject, project),
	}

	list, err := r.client.Resource(r.gvr).Namespace(namespace).List(ctx, opts)
	if err != nil {
		if isCRDNotInstalled(err) {
			slog.Warn("SparkApplication CRD not installed on cluster, returning empty list")
			return []crd.SparkApplication{}, nil
		}
		return nil, fromK8s(err)
	}

	var apps []crd.SparkApplication
	for _, item := range list.Items {
		var app crd.SparkApplication
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, &app); err != nil {
			slog.Warn("Failed to convert SparkApplication from unstructured", "error", err)
			continue
		}
		apps = append(apps, app)
	}
	return apps, nil
}

func (r *k8sSparkAppRepository) Delete(ctx context.Context, namespace, name string) error {
	return fromK8s(r.client.Resource(r.gvr).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{}))
}

func (r *k8sSparkAppRepository) Watch(ctx context.Context, namespace, project string) (<-chan models.SparkAppEvent, error) {
	opts := metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", crd.LabelProject, project),
	}
	out := make(chan models.SparkAppEvent)
	w, err := r.client.Resource(r.gvr).Namespace(namespace).Watch(ctx, opts)
	if err != nil {
		if isCRDNotInstalled(err) {
			slog.Warn("SparkApplication CRD not installed on cluster, returning empty watcher")
			close(out)
			return out, nil
		}
		return nil, fromK8s(err)
	}
	go func() {
		defer close(out)
		defer w.Stop()
		for event := range w.ResultChan() {
			u, ok := event.Object.(*unstructured.Unstructured)
			if !ok {
				continue
			}
			select {
			case out <- models.SparkAppEvent{Type: string(event.Type), Object: sparkInstanceOf(u)}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// sparkInstanceOf reads a watched SparkApplication.
func sparkInstanceOf(u *unstructured.Unstructured) models.SparkAppInstance {
	appType, _, _ := unstructured.NestedString(u.Object, "spec", "type")
	mode, _, _ := unstructured.NestedString(u.Object, "spec", "mode")
	image, _, _ := unstructured.NestedString(u.Object, "spec", "image")
	state, _, _ := unstructured.NestedString(u.Object, "status", "applicationState", "state")
	errMsg, _, _ := unstructured.NestedString(u.Object, "status", "applicationState", "errorMessage")
	driverPod, _, _ := unstructured.NestedString(u.Object, "status", "driverInfo", "podName")

	executors := make(map[string]string)
	rawExec, found, _ := unstructured.NestedStringMap(u.Object, "status", "executorState")
	if found {
		executors = rawExec
	}

	createdAt := ""
	if ts := u.GetCreationTimestamp(); !ts.IsZero() {
		createdAt = ts.Format("2006-01-02T15:04:05Z07:00")
	}

	completedAt := ""
	if ann := u.GetAnnotations(); ann != nil {
		if v, ok := ann["sparkoperator.k8s.io/completed-at"]; ok {
			completedAt = v
		}
	}

	return models.SparkAppInstance{
		Name:          u.GetName(),
		Type:          appType,
		Mode:          mode,
		Image:         image,
		Status:        state,
		ErrorMessage:  errMsg,
		DriverPodName: driverPod,
		CreatedAt:     createdAt,
		CompletedAt:   completedAt,
		Executors:     executors,
	}
}

func (r *k8sSparkAppRepository) GetCRDSchema(ctx context.Context) (map[string]interface{}, error) {
	crdGVR := schema.GroupVersionResource{
		Group:    "apiextensions.k8s.io",
		Version:  "v1",
		Resource: "customresourcedefinitions",
	}

	u, err := r.client.Resource(crdGVR).Get(ctx, "sparkapplications.sparkoperator.k8s.io", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("SparkApplication CRD not found on cluster: %w", err)
	}

	versions, found, _ := unstructured.NestedSlice(u.Object, "spec", "versions")
	if !found || len(versions) == 0 {
		return nil, fmt.Errorf("no versions found in SparkApplication CRD")
	}

	firstVersion, ok := versions[0].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid version format in CRD")
	}

	specSchema, found, _ := unstructured.NestedMap(firstVersion, "schema", "openAPIV3Schema", "properties", "spec", "properties")
	if !found {
		return nil, fmt.Errorf("could not extract spec schema from CRD")
	}

	return specSchema, nil
}

func isCRDNotInstalled(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "the server could not find the requested resource") ||
		strings.Contains(msg, "no matches for kind")
}
