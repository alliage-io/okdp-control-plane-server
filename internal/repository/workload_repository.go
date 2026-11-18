package repository

import (
	"context"
	"io"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
)

// Workloads reads the workloads of the projects' namespaces: their pods,
// logs, events, volumes and metrics, and removes the user objects a release
// leaves behind.
type Workloads struct {
	client  kubernetes.Interface
	metrics metricsclient.Interface
}

// NewWorkloads reads through the typed clientset and metrics-server.
func NewWorkloads(client kubernetes.Interface, metrics metricsclient.Interface) *Workloads {
	return &Workloads{client: client, metrics: metrics}
}

// ListPods lists the pods of a namespace, those of selector when set.
func (w *Workloads) ListPods(ctx context.Context, namespace, selector string) ([]corev1.Pod, error) {
	list, err := w.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// ListPodNames lists the names of the pods of a namespace.
func (w *Workloads) ListPodNames(ctx context.Context, namespace string) ([]string, error) {
	pods, err := w.ListPods(ctx, namespace, "")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(pods))
	for i := range pods {
		names = append(names, pods[i].Name)
	}
	return names, nil
}

// GetPod reads a pod.
func (w *Workloads) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	pod, err := w.client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	return pod, fromK8s(err)
}

// PodLogs streams the logs of a pod: the last tailLines lines (all when not
// positive) of container (the only one when ""), following when asked.
func (w *Workloads) PodLogs(ctx context.Context, namespace, pod, container string, tailLines int64, follow bool) (io.ReadCloser, error) {
	opts := &corev1.PodLogOptions{Follow: follow}
	if tailLines > 0 {
		opts.TailLines = &tailLines
	}
	if container != "" {
		opts.Container = container
	}
	stream, err := w.client.CoreV1().Pods(namespace).GetLogs(pod, opts).Stream(ctx)
	return stream, fromK8s(err)
}

// DeletePod deletes a pod.
func (w *Workloads) DeletePod(ctx context.Context, namespace, name string) error {
	return w.client.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

// WarningEvents lists the Warning events of a namespace.
func (w *Workloads) WarningEvents(ctx context.Context, namespace string) ([]corev1.Event, error) {
	list, err := w.client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{FieldSelector: "type=Warning"})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// ListPVCNames lists the names of the PersistentVolumeClaims of a namespace.
func (w *Workloads) ListPVCNames(ctx context.Context, namespace string) ([]string, error) {
	list, err := w.client.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, list.Items[i].Name)
	}
	return names, nil
}

// PVCExists reports whether a PersistentVolumeClaim can be read.
func (w *Workloads) PVCExists(ctx context.Context, namespace, name string) (bool, error) {
	_, err := w.client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
	return err == nil, err
}

// DeletePVC deletes a PersistentVolumeClaim.
func (w *Workloads) DeletePVC(ctx context.Context, namespace, name string) error {
	return w.client.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

// PodMetrics lists the usage metrics-server reports for the pods of a
// namespace.
func (w *Workloads) PodMetrics(ctx context.Context, namespace string) ([]metricsv1beta1.PodMetrics, error) {
	list, err := w.metrics.MetricsV1beta1().PodMetricses(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}
