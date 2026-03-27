package repository

import (
	"context"
	"fmt"

	"github.com/sirupsen/logrus"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

// ContextWriterRepository creates, syncs, and deletes KuboCD Context CRs for per-project isolation.
type ContextWriterRepository interface {
	CreateFromDefault(ctx context.Context, projectName string) error
	SyncFromDefault(ctx context.Context, projectName string) error
	Delete(ctx context.Context, projectName string) error
}

type k8sContextWriterRepository struct {
	client           dynamic.Interface
	defaultName      string
	defaultNamespace string
}

func NewContextWriterRepository(client dynamic.Interface, defaultName, defaultNamespace string) ContextWriterRepository {
	return &k8sContextWriterRepository{
		client:           client,
		defaultName:      defaultName,
		defaultNamespace: defaultNamespace,
	}
}

// CreateFromDefault copies the default Context CR into a project-scoped Context.
func (r *k8sContextWriterRepository) CreateFromDefault(ctx context.Context, projectName string) error {
	defaultCtx, err := r.client.Resource(contextGVR).Namespace(r.defaultNamespace).Get(ctx, r.defaultName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to read default context %s/%s: %w", r.defaultNamespace, r.defaultName, err)
	}

	specContext, found, err := unstructured.NestedMap(defaultCtx.Object, "spec", "context")
	if err != nil || !found {
		return fmt.Errorf("default context has no spec.context")
	}

	projectCtx := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "kubocd.kubotal.io/v1alpha1",
			"kind":       "Context",
			"metadata": map[string]interface{}{
				"name":      projectName,
				"namespace": r.defaultNamespace,
				"labels": map[string]interface{}{
					"okdp.io/project": projectName,
					"okdp.io/source":  "default",
				},
			},
			"spec": map[string]interface{}{
				"context": specContext,
			},
		},
	}

	_, err = r.client.Resource(contextGVR).Namespace(r.defaultNamespace).Create(ctx, projectCtx, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create project context %q: %w", projectName, err)
	}

	logrus.WithField("project", projectName).Info("Created per-project Context CR")
	return nil
}

// SyncFromDefault creates the project Context if missing, or updates it from the default if it already exists.
// This ensures project contexts always reflect the latest default context (e.g. new service blocks).
func (r *k8sContextWriterRepository) SyncFromDefault(ctx context.Context, projectName string) error {
	defaultCtx, err := r.client.Resource(contextGVR).Namespace(r.defaultNamespace).Get(ctx, r.defaultName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to read default context %s/%s: %w", r.defaultNamespace, r.defaultName, err)
	}

	specContext, found, err := unstructured.NestedMap(defaultCtx.Object, "spec", "context")
	if err != nil || !found {
		return fmt.Errorf("default context has no spec.context")
	}

	existing, err := r.client.Resource(contextGVR).Namespace(r.defaultNamespace).Get(ctx, projectName, metav1.GetOptions{})
	if err != nil {
		logrus.WithField("project", projectName).Info("Project Context missing, creating from default")
		return r.CreateFromDefault(ctx, projectName)
	}

	if err := unstructured.SetNestedMap(existing.Object, specContext, "spec", "context"); err != nil {
		return fmt.Errorf("failed to set spec.context on project context %q: %w", projectName, err)
	}

	_, err = r.client.Resource(contextGVR).Namespace(r.defaultNamespace).Update(ctx, existing, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update project context %q: %w", projectName, err)
	}

	logrus.WithField("project", projectName).Info("Synced project Context from default")
	return nil
}

func (r *k8sContextWriterRepository) Delete(ctx context.Context, projectName string) error {
	err := r.client.Resource(contextGVR).Namespace(r.defaultNamespace).Delete(ctx, projectName, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("failed to delete project context %q: %w", projectName, err)
	}
	logrus.WithField("project", projectName).Info("Deleted per-project Context CR")
	return nil
}
